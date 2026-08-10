package omniroute

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The embedded corpus is deliberately test-only. It cannot be reached by
// production code or by a live OmniRoute client.
//
//go:embed testdata/contract
var embeddedContractCorpus embed.FS

type contractManifest struct {
	SchemaVersion      int                          `json:"schema_version"`
	ManagementDefaults map[string]string            `json:"management_defaults"`
	ErrorKindInventory []contractErrorKindInventory `json:"error_kind_inventory"`
	Scenarios          []contractScenario           `json:"scenarios"`
}

type contractErrorKindInventory struct {
	Kind       string   `json:"kind"`
	Categories []string `json:"categories"`
	Scenarios  []string `json:"scenarios"`
}

type contractScenario struct {
	Name       string                `json:"name"`
	Operation  string                `json:"operation"`
	Transport  contractTransportSpec `json:"transport,omitempty"`
	Response   *contractResponseSpec `json:"response,omitempty"`
	Management map[string]string     `json:"management,omitempty"`
	Config     contractConfigSpec    `json:"config,omitempty"`
	Request    contractRequestSpec   `json:"request,omitempty"`
	Expected   contractExpectation   `json:"expected"`
}

type contractResponseSpec struct {
	Status      int               `json:"status"`
	BodyFile    string            `json:"body_file,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	ReceiptFile string            `json:"receipt_file,omitempty"`
}

type contractTransportSpec struct {
	Kind     string `json:"kind,omitempty"`
	DelayMS  int    `json:"delay_ms,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	Location string `json:"location,omitempty"`
}

type contractConfigSpec struct {
	MaxRequestBytes  int  `json:"max_request_bytes,omitempty"`
	MaxResponseBytes int  `json:"max_response_bytes,omitempty"`
	TimeoutMS        int  `json:"timeout_ms,omitempty"`
	AttemptReceipts  bool `json:"attempt_receipts,omitempty"`
}

type contractRequestSpec struct {
	Prompt          string `json:"prompt,omitempty"`
	Model           string `json:"model,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
}

type contractExpectation struct {
	ErrorKind           string         `json:"error_kind"`
	OutcomeClass        string         `json:"outcome_class,omitempty"`
	DeliveryState       string         `json:"delivery_state,omitempty"`
	UpstreamReached     *bool          `json:"upstream_reached,omitempty"`
	RetryAfterSeconds   int            `json:"retry_after_seconds,omitempty"`
	ResetAt             string         `json:"reset_at,omitempty"`
	ResponseText        string         `json:"response_text,omitempty"`
	RequestCounts       map[string]int `json:"request_counts,omitempty"`
	AttemptReceiptCount int            `json:"attempt_receipt_count,omitempty"`
}

const contractSchemaVersion = 1

var contractManagementEndpoints = map[string]string{
	"resilience":             "/api/resilience",
	"rate_limits":            "/api/rate-limits",
	"settings":               "/api/settings",
	"model_aliases":          "/api/models/alias",
	"settings_model_aliases": "/api/settings/model-aliases",
	"fallback_chains":        "/api/fallback/chains",
	"combos":                 "/api/combos",
	"model_combo_mappings":   "/api/model-combo-mappings",
	"providers":              "/api/providers",
}

var contractCategories = map[string]struct{}{
	"http_body_header":        {},
	"transport":               {},
	"pre_request":             {},
	"route_safety_management": {},
	"attempt_receipts":        {},
}

var contractOperations = map[string]struct{}{
	"complete_once": {},
	"complete":      {},
	"preflight":     {},
	"snapshot":      {},
}

var contractTransports = map[string]struct{}{
	"":                   {},
	"cancelled":          {},
	"dial_error":         {},
	"connection_reset":   {},
	"delay":              {},
	"redirect":           {},
	"oversized_response": {},
	"close_connection":   {},
}

var (
	contractCredentialFieldPattern = regexp.MustCompile(`(?i)(?:^|["'\s])(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|api[-_]key|access[-_]?token|refresh[-_]?token|session[-_]?token|id[-_]?token)(?:["'\s]|:)`)
	contractBearerPattern          = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{12,}`)
	contractAPIKeyPattern          = regexp.MustCompile(`(?i)\b(?:sk|ghp|github_pat|xox[baprs])[-_][a-z0-9_-]{12,}\b`)
	contractJWTPattern             = regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\b`)
	contractEmailPattern           = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9._%+\-]{0,63}@[a-z0-9.-]+\.[a-z]{2,}\b`)
)

func writeFixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, root, content string) {
	t.Helper()
	writeFixtureFile(t, root, "manifest.json", content)
}

func contractFixtureFS() fs.FS {
	root, err := fs.Sub(embeddedContractCorpus, "testdata/contract")
	if err != nil {
		panic(err)
	}
	return root
}

func contractFixtureBytes(name string) ([]byte, error) {
	return fs.ReadFile(contractFixtureFS(), filepath.ToSlash(name))
}

func mustReadContractFixture(name string) string {
	data, err := contractFixtureBytes(name)
	if err != nil {
		panic(fmt.Sprintf("read contract fixture %q: %v", name, err))
	}
	return string(data)
}

func scanFixtureHygiene(fsys fs.FS) error {
	return fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read fixture %q: %w", path, err)
		}
		text := string(data)
		if contractCredentialFieldPattern.MatchString(text) {
			return fmt.Errorf("fixture %q contains a credential field or header", path)
		}
		for name, pattern := range map[string]*regexp.Regexp{
			"bearer credential": contractBearerPattern,
			"API key":           contractAPIKeyPattern,
			"JWT credential":    contractJWTPattern,
			"email identifier":  contractEmailPattern,
		} {
			if pattern.MatchString(text) {
				return fmt.Errorf("fixture %q contains a secret-shaped %s", path, name)
			}
		}
		return nil
	})
}

func loadContractManifest(fsys fs.FS) (contractManifest, error) {
	if err := scanFixtureHygiene(fsys); err != nil {
		return contractManifest{}, err
	}
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return contractManifest{}, fmt.Errorf("read contract manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest contractManifest
	if err := decoder.Decode(&manifest); err != nil {
		return contractManifest{}, fmt.Errorf("decode contract manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return contractManifest{}, fmt.Errorf("contract manifest has trailing data")
	}
	if manifest.SchemaVersion != contractSchemaVersion {
		return contractManifest{}, fmt.Errorf("unsupported contract manifest schema version %d", manifest.SchemaVersion)
	}

	seenScenarios := make(map[string]struct{}, len(manifest.Scenarios))
	for index, scenario := range manifest.Scenarios {
		if strings.TrimSpace(scenario.Name) == "" {
			return contractManifest{}, fmt.Errorf("scenario %d has no name", index)
		}
		if _, exists := seenScenarios[scenario.Name]; exists {
			return contractManifest{}, fmt.Errorf("duplicate contract scenario %q", scenario.Name)
		}
		seenScenarios[scenario.Name] = struct{}{}
		if _, ok := contractOperations[scenario.Operation]; !ok {
			return contractManifest{}, fmt.Errorf("scenario %q has unknown operation %q", scenario.Name, scenario.Operation)
		}
		if _, ok := contractTransports[scenario.Transport.Kind]; !ok {
			return contractManifest{}, fmt.Errorf("scenario %q has unknown transport %q", scenario.Name, scenario.Transport.Kind)
		}
		if scenario.Response != nil {
			if err := validateFixtureReference(fsys, scenario.Response.BodyFile); err != nil {
				return contractManifest{}, fmt.Errorf("scenario %q response: %w", scenario.Name, err)
			}
			if err := validateFixtureReference(fsys, scenario.Response.ReceiptFile); err != nil {
				return contractManifest{}, fmt.Errorf("scenario %q receipt: %w", scenario.Name, err)
			}
		}
		for endpoint, fixture := range scenario.Management {
			if _, ok := contractManagementEndpoints[endpoint]; !ok {
				return contractManifest{}, fmt.Errorf("scenario %q has unknown management endpoint %q", scenario.Name, endpoint)
			}
			if err := validateFixtureReference(fsys, fixture); err != nil {
				return contractManifest{}, fmt.Errorf("scenario %q management %q: %w", scenario.Name, endpoint, err)
			}
		}
	}
	for endpoint, fixture := range manifest.ManagementDefaults {
		if _, ok := contractManagementEndpoints[endpoint]; !ok {
			return contractManifest{}, fmt.Errorf("unknown management endpoint %q", endpoint)
		}
		if err := validateFixtureReference(fsys, fixture); err != nil {
			return contractManifest{}, fmt.Errorf("management endpoint %q: %w", endpoint, err)
		}
	}

	manifestKinds := make(map[ErrorKind]struct{}, len(manifest.ErrorKindInventory))
	for index, entry := range manifest.ErrorKindInventory {
		kind := ErrorKind(strings.TrimSpace(entry.Kind))
		if kind == "" {
			return contractManifest{}, fmt.Errorf("inventory entry %d has no error kind", index)
		}
		if _, exists := manifestKinds[kind]; exists {
			return contractManifest{}, fmt.Errorf("duplicate inventory error kind %q", kind)
		}
		manifestKinds[kind] = struct{}{}
		for _, category := range entry.Categories {
			if _, ok := contractCategories[category]; !ok {
				return contractManifest{}, fmt.Errorf("inventory error kind %q has unknown category %q", kind, category)
			}
		}
		for _, scenarioName := range entry.Scenarios {
			if _, ok := seenScenarios[scenarioName]; !ok {
				return contractManifest{}, fmt.Errorf("inventory error kind %q references unknown scenario %q", kind, scenarioName)
			}
		}
	}
	sourceKinds, err := errorKindsFromSource()
	if err != nil {
		return contractManifest{}, err
	}
	for kind := range sourceKinds {
		if _, ok := manifestKinds[kind]; !ok {
			return contractManifest{}, fmt.Errorf("ErrorKind %q is missing from contract inventory", kind)
		}
	}
	for kind := range manifestKinds {
		if _, ok := sourceKinds[kind]; !ok {
			return contractManifest{}, fmt.Errorf("contract inventory contains unknown ErrorKind %q", kind)
		}
	}
	return manifest, nil
}

func validateFixtureReference(fsys fs.FS, name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return fmt.Errorf("invalid fixture path %q", name)
	}
	if _, err := fs.Stat(fsys, clean); err != nil {
		return fmt.Errorf("unknown fixture %q: %w", name, err)
	}
	return nil
}

func errorKindsFromSource() (map[ErrorKind]struct{}, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locate contract fixture test source")
	}
	path := filepath.Join(filepath.Dir(sourceFile), "errors.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse ErrorKind source: %w", err)
	}
	kinds := make(map[ErrorKind]struct{})
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			values, ok := specification.(*ast.ValueSpec)
			if !ok || values.Type == nil {
				continue
			}
			identifier, ok := values.Type.(*ast.Ident)
			if !ok || identifier.Name != "ErrorKind" {
				continue
			}
			for index, name := range values.Names {
				if index < len(values.Values) {
					literal, ok := values.Values[index].(*ast.BasicLit)
					if ok && literal.Kind == token.STRING {
						value, err := strconv.Unquote(literal.Value)
						if err == nil {
							kinds[ErrorKind(value)] = struct{}{}
							continue
						}
					}
				}
				kinds[ErrorKind(name.Name)] = struct{}{}
			}
		}
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("no ErrorKind constants found in %s", path)
	}
	return kinds, nil
}

func sortedErrorKinds(kinds map[ErrorKind]struct{}) []ErrorKind {
	result := make([]ErrorKind, 0, len(kinds))
	for kind := range kinds {
		result = append(result, kind)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func TestContractFixtureHygieneRejectsSecretShapedValues(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "responses/unsafe.json", `{"Authorization":"Bearer synthetic-secret-value-123456"}`)
	if err := scanFixtureHygiene(os.DirFS(dir)); err == nil {
		t.Fatal("scanFixtureHygiene() accepted a secret-shaped fixture")
	}
}

func TestContractFixtureHygieneAllowsSemanticTokenSignals(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "responses/safe.json", `{"error":{"code":"token_expired"},"request_id":"opaque-request-001","session":"synthetic-session-001"}`)
	if err := scanFixtureHygiene(os.DirFS(dir)); err != nil {
		t.Fatalf("scanFixtureHygiene() rejected synthetic semantic signals: %v", err)
	}
}

func TestContractManifestRejectsMalformedManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "{")
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted malformed JSON")
	}
}

func TestContractManifestRejectsUnknownFixture(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{
		"schema_version": 1,
		"management_defaults": {},
		"error_kind_inventory": [],
		"scenarios": [{
			"name": "unknown_fixture",
			"operation": "complete_once",
			"response": {"status": 200, "body_file": "responses/missing.json"},
			"expected": {"error_kind": ""}
		}]
	}`)
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted an unknown fixture reference")
	}
}

func TestContractManifestRejectsUnknownScenarioReference(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{
		"schema_version": 1,
		"management_defaults": {},
		"error_kind_inventory": [{"kind":"transport","categories":["transport"],"scenarios":["missing_scenario"]}],
		"scenarios": []
	}`)
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted an unknown scenario reference")
	}
}

func TestContractManifestInventoryMatchesErrorKinds(t *testing.T) {
	manifest, err := loadContractManifest(contractFixtureFS())
	if err != nil {
		t.Fatalf("loadContractManifest() error = %v", err)
	}
	sourceKinds, err := errorKindsFromSource()
	if err != nil {
		t.Fatalf("errorKindsFromSource() error = %v", err)
	}
	manifestKinds := make(map[ErrorKind]struct{}, len(manifest.ErrorKindInventory))
	for _, entry := range manifest.ErrorKindInventory {
		manifestKinds[ErrorKind(entry.Kind)] = struct{}{}
	}
	for kind := range sourceKinds {
		if _, ok := manifestKinds[kind]; !ok {
			t.Errorf("ErrorKind %q is missing from the contract inventory", kind)
		}
	}
	for kind := range manifestKinds {
		if _, ok := sourceKinds[kind]; !ok {
			t.Errorf("contract inventory contains unknown ErrorKind %q", kind)
		}
	}
}
