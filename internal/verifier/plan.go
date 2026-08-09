// Package verifier implements the independent control-plane verification
// boundary of Runstead (issue #11).
//
// The invariant this package exists to enforce:
//
//	model proposes completion
//	→ protocol parses the final response
//	→ Runstead verifier independently observes authoritative state
//	→ acceptance checks evaluated
//	→ completion gate: PASS → completed, FAIL → structured verification
//	  evidence returned to execution, UNCERTAIN/BLOCKED → completion refused
//
// The verifier is control plane, not a tool whose result the model controls.
// It consumes authoritative inputs only: persisted task history (actions,
// tool attempts, evidence), real filesystem observations through the tools
// workspace resolver, real bounded Git observations through the same git seam
// as the tools, the operator-provided acceptance plan, and the evidence IDs
// cited by the final response. Model prose, summaries, reasoning and exit
// code 0 are never inputs.
//
// The verifier performs no process execution. Any recipe that must run as part
// of a task (including test/build recipes) runs through the #26 bounded
// process runner and capability policy; the verifier only reads the persisted
// recipe.Evidence produced by that boundary. It never runs os/exec, sh -c or
// bash -c directly.
package verifier

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// PlanVersion is the version of the acceptance plan schema.
const PlanVersion = 1

// CheckType is the typed acceptance check. No expression engine, scripting or
// DSL: the verifier implements exactly this small typed set.
type CheckType string

const (
	// CheckFileExists requires the file at path to exist.
	CheckFileExists CheckType = "file_exists"
	// CheckFileAbsent requires the file at path to be absent.
	CheckFileAbsent CheckType = "file_absent"
	// CheckFileHash requires the complete file at path to match sha256.
	CheckFileHash CheckType = "file_hash"
	// CheckRecipeExitZero requires one executed run_recipe evidence for the
	// recipe id with started=true, exit code 0, no timeout, no cancellation
	// and no terminating signal. Truncation is recorded explicitly; by
	// default a truncated process still satisfies the exit-status check
	// (truncation only concerns the output content, not the exit status).
	// require_untruncated=true fails the check when stdout or stderr was
	// truncated, because the operator's conclusion depends on the full
	// output.
	CheckRecipeExitZero CheckType = "recipe_exit_zero"
)

// AllCheckTypes is the deterministic sorted list of recognized check types.
var AllCheckTypes = []CheckType{
	CheckFileExists,
	CheckFileAbsent,
	CheckFileHash,
	CheckRecipeExitZero,
}

// ParseCheckType validates a check type name strictly.
func ParseCheckType(value string) (CheckType, error) {
	for _, checkType := range AllCheckTypes {
		if string(checkType) == value {
			return checkType, nil
		}
	}
	return "", fmt.Errorf("unknown acceptance check type %q", value)
}

// Check is one typed acceptance check with a stable id.
type Check struct {
	// ID is the stable operator-chosen identifier, unique within a plan.
	ID string `json:"id"`
	// Type is the typed check kind.
	Type CheckType `json:"type"`
	// Path is the relative workspace path for file checks.
	Path string `json:"path,omitempty"`
	// SHA256 is the expected complete-file hash for file_hash checks.
	SHA256 string `json:"sha256,omitempty"`
	// Recipe is the recipe id for recipe_exit_zero checks.
	Recipe string `json:"recipe,omitempty"`
	// RequireUntruncated fails a recipe_exit_zero check when the process
	// output was truncated. Default false: truncation is recorded in the
	// report but does not by itself invalidate an exit-status conclusion.
	RequireUntruncated bool `json:"require_untruncated,omitempty"`
}

// Validate checks the static shape of one check. It never observes the
// environment.
func (c Check) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("acceptance check id must not be empty")
	}
	switch c.Type {
	case CheckFileExists, CheckFileAbsent, CheckFileHash:
		if strings.TrimSpace(c.Path) == "" {
			return fmt.Errorf("check %q: file checks require a path", c.ID)
		}
		if c.Type == CheckFileHash && !validHexSHA256(c.SHA256) {
			return fmt.Errorf("check %q: file_hash requires a sha256", c.ID)
		}
	case CheckRecipeExitZero:
		if strings.TrimSpace(c.Recipe) == "" {
			return fmt.Errorf("check %q: recipe_exit_zero requires a recipe id", c.ID)
		}
	default:
		return fmt.Errorf("check %q: unknown type %q", c.ID, c.Type)
	}
	return nil
}

func validHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Plan is the operator-provided acceptance specification of one task. It is
// versioned, typed and read once at task start; the model can never invent or
// modify its own acceptance criteria after execution.
type Plan struct {
	// Version is the plan schema version (PlanVersion).
	Version int `json:"version"`
	// Checks is the ordered list of mandatory acceptance checks.
	Checks []Check `json:"checks"`
}

// ParsePlan decodes an acceptance plan with a strict decoder: unknown fields,
// duplicate keys, trailing content, an unsupported version and duplicate
// check ids are all rejected fail-closed. Whitespace-only trailing content is
// accepted.
func ParsePlan(data []byte) (*Plan, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("decode acceptance plan: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var plan Plan
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode acceptance plan: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode acceptance plan: trailing JSON value after the plan")
		}
		return nil, fmt.Errorf("decode acceptance plan: trailing content: %w", err)
	}
	if plan.Version != PlanVersion {
		return nil, fmt.Errorf("decode acceptance plan: unsupported version %d (want %d)", plan.Version, PlanVersion)
	}
	seen := make(map[string]struct{}, len(plan.Checks))
	for _, check := range plan.Checks {
		if err := check.Validate(); err != nil {
			return nil, err
		}
		if _, exists := seen[check.ID]; exists {
			return nil, fmt.Errorf("decode acceptance plan: duplicate check id %q", check.ID)
		}
		seen[check.ID] = struct{}{}
	}
	return &plan, nil
}

// EmptyPlan returns a plan with no acceptance checks. Verification still runs
// the structural completion checks (evidence grounding, uncertain effects,
// pending approvals, write/filesystem reconciliation), but because no
// task-specific acceptance criterion exists, completion is refused blocked
// (fail closed, issue #11 review): a completion proposal without operator
// acceptance criteria cannot be proven against the task objective.
func EmptyPlan() *Plan {
	return &Plan{Version: PlanVersion}
}

// Digest returns a stable SHA-256 of the plan: version plus sorted
// check-id=canonical-definition pairs. It is persisted with the task
// configuration so resume rejects plan drift fail-closed.
func (p *Plan) Digest() string {
	if p == nil {
		return ""
	}
	lines := make([]string, 0, len(p.Checks)+1)
	lines = append(lines, fmt.Sprintf("version=%d", p.Version))
	ids := make([]string, 0, len(p.Checks))
	for _, check := range p.Checks {
		ids = append(ids, check.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, check := range p.Checks {
			if check.ID != id {
				continue
			}
			encoded, err := json.Marshal(check)
			if err != nil {
				continue
			}
			lines = append(lines, id+"="+string(encoded))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// rejectDuplicateJSONKeys rejects duplicate object keys anywhere in the JSON
// document so a duplicated field can never silently override another.
func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= 64 {
		return errors.New("JSON nesting is too deep")
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate object key %q", name)
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	}
	return nil
}
