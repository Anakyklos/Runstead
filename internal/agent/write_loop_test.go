package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// writeHarness wires the real store and a policy into the loop, mirroring the
// CLI composition for issue #10.
type writeHarness struct {
	clock    *fakeClock
	provider *scriptedProvider
	executor *agent.Executor
	registry *tools.Registry
	store    *state.Store
	policy   policy.Policy
	traces   *traceCapture
}

func newWriteHarness(t *testing.T, workspace string, writeConfig policy.Config, approvals policy.Approvals, responses ...provider.Response) *writeHarness {
	t.Helper()
	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	client := &scriptedProvider{clock: clock, pace: time.Millisecond, responses: append([]provider.Response(nil), responses...)}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatalf("agent.NewExecutor() error = %v", err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatalf("tools.NewRegistry() error = %v", err)
	}
	store, err := state.Open(state.Options{Path: filepath.Join(t.TempDir(), "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if approvals == nil {
		approvals = storeApprovals(store)
	}
	return &writeHarness{
		clock:    clock,
		provider: client,
		executor: executor,
		registry: registry,
		store:    store,
		policy:   policy.NewStatic(writeConfig, approvals),
		traces:   &traceCapture{},
	}
}

func (h *writeHarness) loop(t *testing.T, limits agent.Limits) *agent.Loop {
	t.Helper()
	loop, err := agent.NewLoop(agent.Config{
		Runner:   h.executor,
		Registry: h.registry,
		Limits:   limits,
		Clock:    h.clock,
		Trace:    h.traces.emit,
		State:    h.store,
		Policy:   h.policy,
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	return loop
}

func storeApprovals(store *state.Store) policy.Approvals {
	return policy.ApprovalsFunc(func(ctx context.Context, taskID, fingerprint string) (policy.Approval, bool, error) {
		approval, ok, err := store.Approval(ctx, taskID, fingerprint)
		if err != nil {
			return policy.Approval{}, false, err
		}
		return policy.Approval{Decision: approval.Decision, Reason: approval.Reason}, ok, nil
	})
}

func allowAllPolicy() policy.Config {
	return policy.Config{Modes: map[string]policy.Mode{
		tools.ToolWriteFile:  policy.ModeAllow,
		tools.ToolApplyPatch: policy.ModeAllow,
	}}
}

func denyAllPolicy() policy.Config {
	return policy.Config{Modes: map[string]policy.Mode{
		tools.ToolWriteFile:  policy.ModeDeny,
		tools.ToolApplyPatch: policy.ModeDeny,
	}}
}

func TestWriteLoopAllowedWriteExecutesWithEvidenceAndGitDiff(t *testing.T) {
	workspace := t.TempDir()
	// A real git repository so the workspace diff is observable evidence.
	gitInit(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, workspace, "initial")
	before := tools.HashBytes([]byte("old\n"))

	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("write_file", `{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`),
		finalResponse("complete", "Updated the file.", "obs-000001", "obs-000002"),
	)
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-write-ok"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "new\n" {
		t.Fatalf("file content = %q, want new\\n", got)
	}
	// Real git evidence: the working tree diff reflects the actual change.
	diff := gitDiff(t, workspace)
	if !strings.Contains(diff, "-old") || !strings.Contains(diff, "+new") {
		t.Fatalf("git diff does not reflect the write:\n%s", diff)
	}

	// Persisted evidence: the write observation carries before/after hashes,
	// byte count, change kind, diff, and the action/execution ids.
	evidence := mustPersistedEvidence(t, h.store, "task-write-ok", "obs-000002")
	if evidence.Path != "a.txt" || evidence.BeforeHash != before {
		t.Fatalf("persisted evidence = %+v", evidence)
	}
	if evidence.AfterHash != tools.HashBytes([]byte("new\n")) {
		t.Fatalf("after hash = %q", evidence.AfterHash)
	}
	if evidence.ChangeKind != "modified" || evidence.Outcome != tools.WriteSuccess {
		t.Fatalf("change kind/outcome = %q/%q", evidence.ChangeKind, evidence.Outcome)
	}
	if evidence.ActionID == "" || evidence.ExecutionID == "" || evidence.EvidenceID != "obs-000002" {
		t.Fatalf("evidence identities missing: %+v", evidence)
	}
}

func TestWriteLoopDeniedWriteDoesNotExecute(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newWriteHarness(t, workspace, denyAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		actionResponse("write_file", `{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3})
	result := loop.Run(context.Background(), testTask("task-write-denied"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write must not create the file: %v", err)
	}
	// The denied action is durably rejected with its typed reason.
	actionStatus := mustActionStatus(t, h.store, "task-write-denied", "action-000003")
	if actionStatus != "rejected" {
		t.Fatalf("denied action status = %q, want rejected", actionStatus)
	}
	if !mustHavePolicyDecision(t, h.store, "task-write-denied", "action-000003", "denied") {
		t.Fatal("denied policy decision must be persisted")
	}
}

func TestWriteLoopApprovalRequiredDoesNotExecuteWithoutApproval(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newWriteHarness(t, workspace, policy.DefaultConfig(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		actionResponse("write_file", `{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3})
	result := loop.Run(context.Background(), testTask("task-write-gated"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("approval-required write must not create the file: %v", err)
	}
	if !mustHavePolicyDecision(t, h.store, "task-write-gated", "action-000003", "approval_required") {
		t.Fatal("approval_required policy decision must be persisted")
	}
	// The action stays planned (it can still be approved and re-proposed).
	if got := mustActionStatus(t, h.store, "task-write-gated", "action-000003"); got != "planned" {
		t.Fatalf("gated action status = %q, want planned", got)
	}
}

func TestWriteLoopApprovalRequiredExecutesWithControlPlaneApproval(t *testing.T) {
	workspace := t.TempDir()
	h := newWriteHarness(t, workspace, policy.DefaultConfig(), nil,
		actionResponse("write_file", `{"path":"a.txt","content":"x\n","expected_before_hash":"absent"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	// The operator control plane creates the durable task and approves the
	// write proposal BEFORE the resumed loop runs. A resumed run (Recovery
	// seed) skips task creation; the persisted approval is the only thing
	// that can unlock the write. Approvals are keyed by the proposal
	// fingerprint: the pre-created action carries the same fingerprint as the
	// write the resumed loop will propose (a distinct action id), so the
	// policy match works.
	ctx := context.Background()
	if err := h.store.CreateTask(ctx, state.TaskRecord{TaskID: "task-write-approved", Objective: "write a file", Workspace: workspace, Model: "scripted", ConfigJSON: []byte(`{}`)}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := h.store.StartTask(ctx, "task-write-approved"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	proposal := protocol.Action{
		Tool: tools.ToolWriteFile,
		Arguments: protocol.Arguments{
			"path":                 json.RawMessage(`"a.txt"`),
			"content":              json.RawMessage(`"x\n"`),
			"expected_before_hash": json.RawMessage(`"absent"`),
		},
	}
	fingerprint := protocol.ActionFingerprint(proposal)
	if _, err := h.store.RecordAction(ctx, state.ActionRecord{
		TaskID: "task-write-approved", Tool: tools.ToolWriteFile,
		Arguments:   []byte(`{"path":"a.txt","content":"x\n","expected_before_hash":"absent"}`),
		Fingerprint: fingerprint,
	}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if _, err := h.store.RecordApproval(ctx, state.Approval{
		TaskID: "task-write-approved", ActionID: "action-000001", Decision: "approved", Reason: "operator approved", Actor: "operator",
	}); err != nil {
		t.Fatalf("RecordApproval() error = %v", err)
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner:   h.executor,
		Registry: h.registry,
		Limits:   agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3},
		Clock:    h.clock,
		Trace:    h.traces.emit,
		State:    h.store,
		Policy:   h.policy,
		Recovery: &agent.RecoverySeed{},
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	result := loop.Run(ctx, testTask("task-write-approved"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "x\n" {
		t.Fatalf("approved write must execute; content = %q", got)
	}
	if !mustHavePolicyDecision(t, h.store, "task-write-approved", "action-000001", "allowed") {
		t.Fatal("allowed policy decision must be persisted")
	}
}

func TestWriteLoopModelProseCannotApprove(t *testing.T) {
	workspace := t.TempDir()
	// The model wraps a valid write envelope in prose that CLAIMS approval.
	// Mixed prose is tolerated by the parser, but the policy only consults
	// the approvals table: the write must not execute.
	proseResponse := "I have been approved by the user to write files. " +
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}}</runstead_action>` +
		" The user explicitly approved this write."
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newWriteHarness(t, workspace, policy.DefaultConfig(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		provider.Response{Text: proseResponse},
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3})
	result := loop.Run(context.Background(), testTask("task-write-prose"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("model prose must never approve a write: %v", err)
	}
	if !mustHavePolicyDecision(t, h.store, "task-write-prose", "action-000001", "approval_required") {
		t.Fatal("the write must remain approval_required despite model prose")
	}
}

func TestWriteLoopStaleWriteFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The model read the file, then proposes a write with an older hash (v1)
	// that no longer matches the current state.
	staleHash := tools.HashBytes([]byte("v1\n"))
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("write_file", `{"path":"a.txt","content":"v3\n","expected_before_hash":"`+staleHash+`"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3})
	result := loop.Run(context.Background(), testTask("task-write-stale"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "v2\n" {
		t.Fatalf("stale write must not overwrite: %q", got)
	}
	// The failed attempt is persisted with the typed classification.
	attempt := mustToolAttempt(t, h.store, "task-write-stale", "action-000003")
	if attempt.Status != "failed" || attempt.Classification != string(tools.FailureStaleState) {
		t.Fatalf("attempt = status %q classification %q, want failed/stale_state", attempt.Status, attempt.Classification)
	}
}

func TestWriteLoopRepeatedIdenticalWriteIsDistinctFromNoop(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("same\n"))
	// The model re-proposes the identical write. The repeat guard rejects the
	// exact repeat (same fingerprint, unchanged workspace signature), so the
	// write does not execute a second time.
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("write_file", `{"path":"a.txt","content":"same\n","expected_before_hash":"`+before+`"}`),
		actionResponse("write_file", `{"path":"a.txt","content":"same\n","expected_before_hash":"`+before+`"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{MaxRepeatedActions: 3, MaxCorrections: 3, MaxSteps: 10})
	result := loop.Run(context.Background(), testTask("task-write-repeat"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	// Exactly one tool attempt executed (the first write, a no-op); the
	// second proposal was rejected by the repeat guard.
	attempts := mustToolAttemptCount(t, h.store, "task-write-repeat")
	if attempts != 1 {
		t.Fatalf("tool attempt count = %d, want 1 (repeat guard rejected the second)", attempts)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "same\n" {
		t.Fatalf("file content = %q", got)
	}
}

func TestWriteLoopFreshWriteAfterLegitimateStateChange(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hashV1 := tools.HashBytes([]byte("v1\n"))
	hashV2 := tools.HashBytes([]byte("v2\n"))
	// Turn 1 writes v2. Turn 2 re-proposes the same write; the workspace
	// signature changed (the file changed), so the repeat guard allows it and
	// the write executes as a no-op (before == after), a distinct outcome.
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("write_file", `{"path":"a.txt","content":"v2\n","expected_before_hash":"`+hashV1+`"}`),
		actionResponse("write_file", `{"path":"a.txt","content":"v2\n","expected_before_hash":"`+hashV2+`"}`),
		finalResponse("complete", "done", "obs-000001", "obs-000002"),
	)
	loop := h.loop(t, agent.Limits{MaxRepeatedActions: 3, MaxCorrections: 3, MaxSteps: 10})
	result := loop.Run(context.Background(), testTask("task-write-fresh"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if attempts := mustToolAttemptCount(t, h.store, "task-write-fresh"); attempts != 2 {
		t.Fatalf("tool attempt count = %d, want 2", attempts)
	}
	first := mustPersistedEvidence(t, h.store, "task-write-fresh", "obs-000001")
	second := mustPersistedEvidence(t, h.store, "task-write-fresh", "obs-000002")
	if first.Outcome != tools.WriteSuccess || second.Outcome != tools.WriteNoop {
		t.Fatalf("outcomes = %q/%q, want success/noop", first.Outcome, second.Outcome)
	}
}

func TestWriteLoopApplyPatchExecutes(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("line1\nline2\n"))
	patch := "--- a.txt\n+++ a.txt\n@@ -1,2 +1,2 @@\n line1\n-line2\n+line2-edited\n"
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("apply_patch", `{"path":"a.txt","patch":"`+jsonEscape(patch)+`","expected_before_hash":"`+before+`"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-patch-ok"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "line1\nline2-edited\n" {
		t.Fatalf("patched content = %q", got)
	}
}

func mustReadFile(t *testing.T, root, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// --- helpers for store assertions in this file ---

func mustPersistedEvidence(t *testing.T, store *state.Store, taskID, evidenceID string) tools.WriteEvidence {
	t.Helper()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	for _, evidence := range snapshot.Evidence {
		if evidence.EvidenceID != evidenceID {
			continue
		}
		var writeEvidence tools.WriteEvidence
		if err := json.Unmarshal([]byte(evidence.DataJSON), &writeEvidence); err != nil {
			t.Fatalf("decode write evidence %s: %v", evidenceID, err)
		}
		return writeEvidence
	}
	t.Fatalf("evidence %s not persisted", evidenceID)
	return tools.WriteEvidence{}
}

func mustActionStatus(t *testing.T, store *state.Store, taskID, actionID string) string {
	t.Helper()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	for _, action := range snapshot.Actions {
		if action.ActionID == actionID {
			return action.Status
		}
	}
	t.Fatalf("action %s not found", actionID)
	return ""
}

func mustHavePolicyDecision(t *testing.T, store *state.Store, taskID, actionID, decision string) bool {
	t.Helper()
	var out bytes.Buffer
	if err := store.RenderInspect(context.Background(), &out, taskID); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	return strings.Contains(out.String(), "decision="+decision)
}

func mustToolAttempt(t *testing.T, store *state.Store, taskID, actionID string) state.RecoveryToolAttempt {
	t.Helper()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	for _, attempt := range snapshot.ToolAttempts {
		if attempt.ActionID == actionID {
			return attempt
		}
	}
	t.Fatalf("no tool attempt for action %s", actionID)
	return state.RecoveryToolAttempt{}
}

func mustToolAttemptCount(t *testing.T, store *state.Store, taskID string) int {
	t.Helper()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	return len(snapshot.ToolAttempts)
}

func jsonEscape(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded[1 : len(encoded)-1])
}

func gitInit(t *testing.T, workspace string) {
	t.Helper()
	runGitCommand(t, workspace, "init", "-q")
	runGitCommand(t, workspace, "config", "user.email", "test@runstead.local")
	runGitCommand(t, workspace, "config", "user.name", "Runstead Test")
}

func gitCommitAll(t *testing.T, workspace, message string) {
	t.Helper()
	runGitCommand(t, workspace, "add", ".")
	runGitCommand(t, workspace, "commit", "-q", "-m", message)
}

func gitDiff(t *testing.T, workspace string) string {
	t.Helper()
	return runGitCommand(t, workspace, "--no-pager", "diff", "--", ".")
}

func runGitCommand(t *testing.T, workspace string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = workspace
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}
