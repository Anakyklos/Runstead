package policy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

type fakeApprovals struct {
	decisions map[string]string // actionID -> decision
	err       error
}

func (f *fakeApprovals) Approval(ctx context.Context, taskID, fingerprint string) (policy.Approval, bool, error) {
	if f.err != nil {
		return policy.Approval{}, false, f.err
	}
	decision, ok := f.decisions[fingerprint]
	if !ok {
		return policy.Approval{}, false, nil
	}
	return policy.Approval{Decision: decision}, true, nil
}

func request(tool, fingerprint string) policy.Request {
	return policy.Request{TaskID: "task-1", ActionID: "action-000001", Tool: tool, Fingerprint: fingerprint, Workspace: "/ws"}
}

func TestPolicyAllowExecutesWrites(t *testing.T) {
	static := policy.NewStatic(policy.Config{Modes: map[string]policy.Mode{
		tools.ToolWriteFile: policy.ModeAllow,
	}}, nil)
	outcome := static.Evaluate(context.Background(), request(tools.ToolWriteFile, "action-000001"))
	if outcome.Decision != policy.Allowed {
		t.Fatalf("decision = %q, want allowed", outcome.Decision)
	}
	if outcome.Reason != "policy_allow" {
		t.Fatalf("reason = %q, want policy_allow", outcome.Reason)
	}
}

func TestPolicyDenyNeverExecutes(t *testing.T) {
	static := policy.NewStatic(policy.Config{Modes: map[string]policy.Mode{
		tools.ToolWriteFile:  policy.ModeDeny,
		tools.ToolApplyPatch: policy.ModeDeny,
	}}, nil)
	for _, tool := range []string{tools.ToolWriteFile, tools.ToolApplyPatch} {
		outcome := static.Evaluate(context.Background(), request(tool, "action-000001"))
		if outcome.Decision != policy.Denied {
			t.Fatalf("%s decision = %q, want denied", tool, outcome.Decision)
		}
	}
}

func TestPolicyApprovalRequiredWithoutApprovalDoesNotExecute(t *testing.T) {
	static := policy.NewStatic(policy.DefaultConfig(), &fakeApprovals{decisions: map[string]string{}})
	outcome := static.Evaluate(context.Background(), request(tools.ToolWriteFile, "action-000001"))
	if outcome.Decision != policy.ApprovalRequired {
		t.Fatalf("decision = %q, want approval_required", outcome.Decision)
	}
}

func TestPolicyApprovalRequiredWithoutStoreFailsClosed(t *testing.T) {
	// A nil approvals store must never allow an approval-required write.
	static := policy.NewStatic(policy.DefaultConfig(), nil)
	outcome := static.Evaluate(context.Background(), request(tools.ToolWriteFile, "action-000001"))
	if outcome.Decision != policy.ApprovalRequired {
		t.Fatalf("decision = %q, want approval_required (fail closed)", outcome.Decision)
	}
}

func TestPolicyApprovalLookupFailureFailsClosed(t *testing.T) {
	static := policy.NewStatic(policy.DefaultConfig(), &fakeApprovals{err: errors.New("db down")})
	outcome := static.Evaluate(context.Background(), request(tools.ToolWriteFile, "action-000001"))
	if outcome.Decision != policy.ApprovalRequired {
		t.Fatalf("decision = %q, want approval_required (lookup failure)", outcome.Decision)
	}
}

func TestPolicyApprovedByOperatorExecutes(t *testing.T) {
	static := policy.NewStatic(policy.DefaultConfig(), &fakeApprovals{decisions: map[string]string{
		"action-000001": "approved",
	}})
	outcome := static.Evaluate(context.Background(), request(tools.ToolWriteFile, "action-000001"))
	if outcome.Decision != policy.Allowed {
		t.Fatalf("decision = %q, want allowed", outcome.Decision)
	}
	if outcome.Reason != "approved_by_operator" {
		t.Fatalf("reason = %q, want approved_by_operator", outcome.Reason)
	}
}

func TestPolicyRejectedByOperatorDenies(t *testing.T) {
	static := policy.NewStatic(policy.DefaultConfig(), &fakeApprovals{decisions: map[string]string{
		"action-000001": "rejected",
	}})
	outcome := static.Evaluate(context.Background(), request(tools.ToolWriteFile, "action-000001"))
	if outcome.Decision != policy.Denied {
		t.Fatalf("decision = %q, want denied", outcome.Decision)
	}
	if outcome.Reason != "rejected_by_operator" {
		t.Fatalf("reason = %q, want rejected_by_operator", outcome.Reason)
	}
}

func TestPolicyUngovernedToolDenied(t *testing.T) {
	static := policy.NewStatic(policy.DefaultConfig(), nil)
	outcome := static.Evaluate(context.Background(), request("some_future_tool", "action-000001"))
	if outcome.Decision != policy.Denied {
		t.Fatalf("decision = %q, want denied for ungoverned tool", outcome.Decision)
	}
}

func TestPolicyModelContentCannotInfluenceDecision(t *testing.T) {
	// The request carries no model content by construction: only control-plane
	// identifiers. Two identical proposals with different claimed "approval"
	// text are the same request. The fake approvals only grants when the
	// action was approved through the control plane.
	static := policy.NewStatic(policy.DefaultConfig(), &fakeApprovals{decisions: map[string]string{}})
	outcome := static.Evaluate(context.Background(), request(tools.ToolWriteFile, "action-000001"))
	if outcome.Decision == policy.Allowed {
		t.Fatal("policy must not allow without a persisted operator approval")
	}
}

func TestParseConfig(t *testing.T) {
	config, err := policy.ParseConfig("write_file=allow,apply_patch=deny")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if config.Modes[tools.ToolWriteFile] != policy.ModeAllow || config.Modes[tools.ToolApplyPatch] != policy.ModeDeny {
		t.Fatalf("parsed modes = %+v", config.Modes)
	}

	if _, err := policy.ParseConfig("write_file=allow,apply_patch=bogus"); err == nil {
		t.Fatal("bogus mode must be rejected")
	}
	if _, err := policy.ParseConfig("unknown_tool=allow"); err == nil {
		t.Fatal("unknown tool must be rejected")
	}
	if _, err := policy.ParseConfig("write_file"); err == nil {
		t.Fatal("missing =MODE must be rejected")
	}
	if _, err := policy.ParseConfig(""); err == nil {
		t.Fatal("empty value must be rejected")
	}
}

func TestDefaultConfigIsApprovalRequiredForAllWriteTools(t *testing.T) {
	config := policy.DefaultConfig()
	for _, tool := range []string{tools.ToolWriteFile, tools.ToolApplyPatch} {
		if config.Modes[tool] != policy.ModeApprovalRequired {
			t.Fatalf("default mode for %s = %q, want approval_required", tool, config.Modes[tool])
		}
	}
}

func recipeRequest(recipeID, fingerprint string) policy.Request {
	return policy.Request{TaskID: "task-1", ActionID: "action-000001", Tool: "run_recipe", Fingerprint: fingerprint, Recipe: recipeID, Workspace: "/ws"}
}

func recipePolicyConfig(modes map[string]policy.Mode) policy.Config {
	return policy.Config{Modes: map[string]policy.Mode{}, RecipeModes: modes}
}

func TestPolicyRecipeAllowExecutes(t *testing.T) {
	static := policy.NewStatic(recipePolicyConfig(map[string]policy.Mode{"test": policy.ModeAllow}), nil)
	outcome := static.Evaluate(context.Background(), recipeRequest("test", "fp"))
	if outcome.Decision != policy.Allowed {
		t.Fatalf("decision = %q, want allowed", outcome.Decision)
	}
}

func TestPolicyRecipeDenyNeverStarts(t *testing.T) {
	static := policy.NewStatic(recipePolicyConfig(map[string]policy.Mode{"test": policy.ModeDeny}), nil)
	outcome := static.Evaluate(context.Background(), recipeRequest("test", "fp"))
	if outcome.Decision != policy.Denied {
		t.Fatalf("decision = %q, want denied", outcome.Decision)
	}
}

func TestPolicyRecipeDefaultIsApprovalRequired(t *testing.T) {
	// A recipe with no configured mode defaults to approval_required.
	static := policy.NewStatic(recipePolicyConfig(map[string]policy.Mode{}), nil)
	outcome := static.Evaluate(context.Background(), recipeRequest("test", "fp"))
	if outcome.Decision != policy.ApprovalRequired {
		t.Fatalf("decision = %q, want approval_required (default)", outcome.Decision)
	}
}

func TestPolicyRecipeApprovalRequiredNeedsOperatorApproval(t *testing.T) {
	static := policy.NewStatic(recipePolicyConfig(map[string]policy.Mode{"test": policy.ModeApprovalRequired}), &fakeApprovals{decisions: map[string]string{}})
	outcome := static.Evaluate(context.Background(), recipeRequest("test", "fp-test"))
	if outcome.Decision != policy.ApprovalRequired {
		t.Fatalf("decision = %q, want approval_required", outcome.Decision)
	}
	// A persisted operator approval unlocks the recipe.
	approved := policy.NewStatic(recipePolicyConfig(map[string]policy.Mode{"test": policy.ModeApprovalRequired}), &fakeApprovals{decisions: map[string]string{"fp-test": "approved"}})
	outcome = approved.Evaluate(context.Background(), recipeRequest("test", "fp-test"))
	if outcome.Decision != policy.Allowed {
		t.Fatalf("decision = %q, want allowed after operator approval", outcome.Decision)
	}
}

func TestPolicyRecipeRejectionPersists(t *testing.T) {
	static := policy.NewStatic(recipePolicyConfig(map[string]policy.Mode{"test": policy.ModeApprovalRequired}), &fakeApprovals{decisions: map[string]string{"fp-test": "rejected"}})
	outcome := static.Evaluate(context.Background(), recipeRequest("test", "fp-test"))
	if outcome.Decision != policy.Denied || outcome.Reason != "rejected_by_operator" {
		t.Fatalf("decision/reason = %q/%q, want denied/rejected_by_operator", outcome.Decision, outcome.Reason)
	}
}

func TestPolicyRecipeModelContentCannotInfluence(t *testing.T) {
	// The request carries no model content; the fake approvals only grants a
	// persisted operator decision.
	static := policy.NewStatic(recipePolicyConfig(map[string]policy.Mode{"test": policy.ModeApprovalRequired}), &fakeApprovals{decisions: map[string]string{}})
	outcome := static.Evaluate(context.Background(), recipeRequest("test", "fp"))
	if outcome.Decision == policy.Allowed {
		t.Fatal("model content must never unlock a recipe")
	}
}

func TestParseRecipePolicyAndRoundTrip(t *testing.T) {
	config, err := policy.ParseRecipePolicy("test=allow,vet=deny")
	if err != nil {
		t.Fatalf("ParseRecipePolicy() error = %v", err)
	}
	if config.RecipeModes["test"] != policy.ModeAllow || config.RecipeModes["vet"] != policy.ModeDeny {
		t.Fatalf("parsed modes = %+v", config.RecipeModes)
	}
	// Unknown modes and empty values are rejected.
	if _, err := policy.ParseRecipePolicy("test=bogus"); err == nil {
		t.Fatal("bogus mode must be rejected")
	}
	if _, err := policy.ParseRecipePolicy(""); err == nil {
		t.Fatal("empty value must be rejected")
	}
	if _, err := policy.ParseRecipePolicy("test"); err == nil {
		t.Fatal("missing =MODE must be rejected")
	}
	// RecipeSpec renders the full effective policy including defaults.
	spec := config.RecipeSpec([]string{"test", "vet", "lint"})
	if !strings.Contains(spec, "test=allow") || !strings.Contains(spec, "vet=deny") || !strings.Contains(spec, "lint=approval_required") {
		t.Fatalf("spec = %q", spec)
	}
	// RecipeEqual treats missing modes as approval_required.
	other := policy.Config{RecipeModes: map[string]policy.Mode{"test": policy.ModeAllow, "vet": policy.ModeDeny, "lint": policy.ModeApprovalRequired}}
	if !policy.RecipeEqual(config, other, []string{"test", "vet", "lint"}) {
		t.Fatal("identical effective recipe policies must be equal")
	}
	widened := policy.Config{RecipeModes: map[string]policy.Mode{"test": policy.ModeAllow, "vet": policy.ModeAllow, "lint": policy.ModeApprovalRequired}}
	if policy.RecipeEqual(config, widened, []string{"test", "vet", "lint"}) {
		t.Fatal("a widened recipe policy must not compare equal")
	}
}
