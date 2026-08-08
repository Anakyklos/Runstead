package policy_test

import (
	"context"
	"errors"
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
