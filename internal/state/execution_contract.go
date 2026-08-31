package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ErrExecutionContractCorrupt means the task's frozen composition is missing,
// malformed or does not match its persisted hash. It is a fail-closed state
// error: resume must not reconstruct a different contract.
var ErrExecutionContractCorrupt = errors.New("corrupt frozen execution contract")

func validateExecutionContractPair(data, hash string) error {
	data = strings.TrimSpace(data)
	hash = strings.TrimSpace(hash)
	if data == "" && hash == "" {
		return nil
	}
	if data == "" || hash == "" {
		return fmt.Errorf("%w: JSON and hash must be present together", ErrExecutionContractCorrupt)
	}
	if !validExecutionContractHash(hash) {
		return fmt.Errorf("%w: invalid SHA-256 format", ErrExecutionContractCorrupt)
	}
	if err := validateStrictJSONObject([]byte(data)); err != nil {
		return fmt.Errorf("%w: %v", ErrExecutionContractCorrupt, err)
	}
	sum := sha256.Sum256([]byte(data))
	if hash != "sha256:"+hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%w: hash does not match persisted JSON", ErrExecutionContractCorrupt)
	}
	return nil
}

func validExecutionContractHash(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

// validateStrictJSONObject rejects duplicate keys and trailing content. The
// state layer deliberately validates only structural integrity; composition
// semantics remain in internal/composition and the resume composition root.
func validateStrictJSONObject(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode contract: %v", err)
	}
	if token != json.Delim('{') {
		return fmt.Errorf("contract must be a JSON object")
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode contract key: %v", err)
		}
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("contract object key is not a string")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate contract object key %q", name)
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode contract value: %v", err)
		}
		if !json.Valid(value) {
			return fmt.Errorf("contract contains invalid JSON value")
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return fmt.Errorf("contract object is not closed")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("contract has trailing content")
	}
	return nil
}

// LoadExecutionContract returns the exact frozen bytes and their persisted
// hash. Both are revalidated on every load so a corrupted task cannot resume
// under an unverified composition.
func (s *Store) LoadExecutionContract(ctx context.Context, taskID string) ([]byte, string, error) {
	var data, hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT execution_contract_json, execution_contract_hash FROM tasks WHERE task_id = ?`, taskID).
		Scan(&data, &hash)
	if err == sql.ErrNoRows {
		return nil, "", ErrTaskNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("load execution contract: %w", err)
	}
	if err := validateExecutionContractPair(data, hash); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(data) == "" {
		return nil, "", nil
	}
	return []byte(data), hash, nil
}

type inspectContract struct {
	ContractVersion  int    `json:"contract_version"`
	RuntimeIdentity  string `json:"runtime_identity"`
	ProtocolIdentity string `json:"protocol_identity"`
	Profile          struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"profile"`
	Packages []struct {
		ID         string `json:"id"`
		Version    string `json:"version"`
		Provenance string `json:"provenance"`
	} `json:"packages"`
	Provider struct {
		ProviderID     string `json:"provider_id"`
		ProtocolFamily string `json:"protocol_family"`
		Model          string `json:"model"`
		ConfigIdentity string `json:"config_identity"`
	} `json:"provider"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
	ToolSchemaDigest string `json:"tool_schema_digest"`
	RecipeCatalog    struct {
		Digest    string   `json:"digest"`
		RecipeIDs []string `json:"recipe_ids"`
	} `json:"recipe_catalog"`
}

func renderExecutionContract(builder *strings.Builder, data, hash string) {
	builder.WriteString("\nExecution contract:\n")
	if strings.TrimSpace(data) == "" && strings.TrimSpace(hash) == "" {
		builder.WriteString("  (none recorded)\n")
		return
	}
	// loadInspectTask has already checked the pair's structural integrity. A
	// second bounded decode keeps the renderer defensive and never dumps the
	// stored JSON blob if it is unexpectedly not renderable.
	var contract inspectContract
	if err := json.Unmarshal([]byte(data), &contract); err != nil {
		builder.WriteString("  compatibility: invalid persisted contract\n")
		return
	}
	fmt.Fprintf(builder, "  profile: %s@%s\n", contract.Profile.ID, contract.Profile.Version)
	fmt.Fprintf(builder, "  hash: %s\n", hash)
	fmt.Fprintf(builder, "  schema: %d runtime=%s protocol=%s\n", contract.ContractVersion, contract.RuntimeIdentity, contract.ProtocolIdentity)
	sort.Slice(contract.Packages, func(i, j int) bool {
		if contract.Packages[i].ID == contract.Packages[j].ID {
			return contract.Packages[i].Version < contract.Packages[j].Version
		}
		return contract.Packages[i].ID < contract.Packages[j].ID
	})
	builder.WriteString("  packages:")
	for _, pkg := range contract.Packages {
		fmt.Fprintf(builder, " %s@%s (%s)", pkg.ID, pkg.Version, Redact(pkg.Provenance))
	}
	builder.WriteString("\n")
	if contract.Provider.ProviderID != "" {
		fmt.Fprintf(builder, "  provider: %s family=%s model=%s config_identity=%s\n", contract.Provider.ProviderID,
			contract.Provider.ProtocolFamily, contract.Provider.Model, Redact(contract.Provider.ConfigIdentity))
	} else {
		builder.WriteString("  provider: scripted/legacy\n")
	}
	toolNames := make([]string, 0, len(contract.Tools))
	for _, tool := range contract.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	sort.Strings(toolNames)
	fmt.Fprintf(builder, "  tools: %s\n", strings.Join(toolNames, ","))
	fmt.Fprintf(builder, "  tool_schema_digest: %s\n", contract.ToolSchemaDigest)
	if contract.RecipeCatalog.Digest != "" {
		ids := append([]string(nil), contract.RecipeCatalog.RecipeIDs...)
		sort.Strings(ids)
		fmt.Fprintf(builder, "  recipe_catalog: digest=%s ids=%s\n", contract.RecipeCatalog.Digest, strings.Join(ids, ","))
	} else {
		builder.WriteString("  recipe_catalog: none\n")
	}
	builder.WriteString("  compatibility: frozen contract validated from durable state\n")
}
