package verifier

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// fakeObserver is a deterministic authoritative-environment fake for verifier
// tests. It implements Observer without touching the filesystem, git or any
// process.
type fakeObserver struct {
	files     map[string]string // relative path -> content
	gitStatus string
	gitDiff   string
	gitErr    error
	hashErr   error
}

func (f *fakeObserver) FileSHA256(relative string) (string, bool, error) {
	if f.hashErr != nil {
		return "", true, f.hashErr
	}
	content, ok := f.files[relative]
	if !ok {
		return "", false, nil
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:]), true, nil
}

func (f *fakeObserver) GitStatusText() (string, bool, error) {
	return f.gitStatus, false, f.gitErr
}

func (f *fakeObserver) GitDiffText() (string, bool, error) {
	return f.gitDiff, false, f.gitErr
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func evidence(t *testing.T, id, tool string, data any) state.RecoveryEvidence {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal evidence %s: %v", id, err)
	}
	return state.RecoveryEvidence{EvidenceID: id, Tool: tool, DataJSON: string(encoded)}
}

func readEvidence(t *testing.T, id string) state.RecoveryEvidence {
	return evidence(t, id, tools.ToolReadFile, map[string]any{"path": "a.txt", "content": "alpha\n", "sha256": hashOf("alpha\n")})
}

func writeEvidence(t *testing.T, id, path, afterHash, changeKind string) state.RecoveryEvidence {
	return evidence(t, id, tools.ToolWriteFile, tools.WriteEvidence{
		Path: path, AfterHash: afterHash, ChangeKind: changeKind, Outcome: "success",
	})
}

func recipeEvidenceItem(t *testing.T, id, recipeID string, exitCode int, truncated bool) state.RecoveryEvidence {
	return evidence(t, id, tools.ToolRunRecipe, recipe.Evidence{
		RecipeID: recipeID, Executable: "go", ExitCode: exitCode, Started: true,
		StdoutTruncated: truncated, StderrTruncated: truncated,
		EvidenceID: id, NetworkIsolation: recipe.NetworkIsolationValue,
	})
}

func planWithChecks(checks ...Check) *Plan {
	return &Plan{Version: PlanVersion, Checks: checks}
}

func mustReport(t *testing.T, observer Observer, input Input) Report {
	t.Helper()
	v := New(observer, input.Plan)
	return v.Verify(input)
}

// Scenario 1: the model claims a file was created but it does not exist.
func TestVerifyRejectsClaimedFileThatDoesNotExist(t *testing.T) {
	plan := planWithChecks(Check{ID: "artifact", Type: CheckFileExists, Path: "src/main.go"})
	report := mustReport(t, &fakeObserver{files: map[string]string{}}, Input{
		TaskID: "t1", Plan: plan,
		Evidence: []state.RecoveryEvidence{readEvidence(t, "obs-000001")},
	})
	if report.Decision != DecisionFailed {
		t.Fatalf("decision = %s, want failed", report.Decision)
	}
	check := findCheck(report, "artifact")
	if check.Status != CheckFailed || check.Reason != "file_not_found" {
		t.Fatalf("artifact check = %+v", check)
	}
}

// Scenario 2: the model cites an evidence ID that does not exist. Without an
// operator acceptance plan the task can never complete anyway (fail closed),
// but the fabricated citation is still a failed grounding check.
func TestVerifyRejectsFabricatedEvidenceID(t *testing.T) {
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t2", FinalEvidence: []EvidenceClaim{{EvidenceID: "obs-999999", Tool: "read_file"}},
		Evidence: []state.RecoveryEvidence{readEvidence(t, "obs-000001")},
	})
	if report.Decision != DecisionBlocked {
		t.Fatalf("decision = %s, want blocked (no acceptance plan fails closed)", report.Decision)
	}
	check := findCheck(report, checkEvidenceGrounded)
	if check.Status != CheckFailed || check.Reason != "evidence_ids_not_found" {
		t.Fatalf("grounding check = %+v", check)
	}
}

// Scenario 2b (issue #11 review blocker): the final response cites a real
// evidence id but with a WRONG claimed tool. A read_file observation is cited
// as if it were run_recipe evidence; the type mismatch is a failed check and
// the evidence cannot support the claim.
func TestVerifyRejectsTypeIncompatibleEvidence(t *testing.T) {
	plan := planWithChecks(Check{ID: "tests-pass", Type: CheckRecipeExitZero, Recipe: "test"})
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t2b", Plan: plan,
		FinalEvidence: []EvidenceClaim{{EvidenceID: "obs-000001", Tool: tools.ToolRunRecipe}},
		Evidence:      []state.RecoveryEvidence{readEvidence(t, "obs-000001")},
	})
	if report.Decision != DecisionFailed {
		t.Fatalf("decision = %s, want failed", report.Decision)
	}
	typing := findCheck(report, checkEvidenceClaimsTyped)
	if typing.Status != CheckFailed || typing.Reason != "evidence_type_mismatch" {
		t.Fatalf("typing check = %+v", typing)
	}
	if len(report.CitedEvidence) != 1 {
		t.Fatalf("cited evidence = %+v", report.CitedEvidence)
	}
	if !report.CitedEvidence[0].Exists || report.CitedEvidence[0].ToolMatches {
		t.Fatalf("cited evidence must exist but mismatch the claimed tool: %+v", report.CitedEvidence[0])
	}
	// The recipe check also cannot be satisfied: the persisted evidence is a
	// read observation, not an executed recipe.
	check := findCheck(report, "tests-pass")
	if check.Status != CheckFailed || check.Reason != "recipe_evidence_missing" {
		t.Fatalf("tests-pass check = %+v", check)
	}
}

// Scenario 2c: a citation with the correct claimed tool is accepted.
func TestVerifyAcceptsTypeCompatibleEvidence(t *testing.T) {
	plan := planWithChecks(Check{ID: "artifact", Type: CheckFileExists, Path: "a.txt"})
	observer := &fakeObserver{files: map[string]string{"a.txt": "alpha\n"}}
	report := mustReport(t, observer, Input{
		TaskID: "t2c", Plan: plan,
		FinalEvidence: []EvidenceClaim{{EvidenceID: "obs-000001", Tool: tools.ToolReadFile}},
		Evidence:      []state.RecoveryEvidence{readEvidence(t, "obs-000001")},
	})
	if report.Decision != DecisionPassed {
		t.Fatalf("decision = %s, want passed: %+v", report.Decision, report.Checks)
	}
	if findCheck(report, checkEvidenceClaimsTyped).Status != CheckPassed {
		t.Fatalf("typing check must pass: %+v", findCheck(report, checkEvidenceClaimsTyped))
	}
}

// Issue #11 review blocker: without an operator acceptance plan, completion is
// refused blocked even when every structural check passes. A completion
// proposal without task-specific acceptance criteria can never be proven
// against the task objective.
func TestVerifyWithoutAcceptancePlanBlocksCompletion(t *testing.T) {
	report := mustReport(t, &fakeObserver{files: map[string]string{"result.txt": "42\n"}}, Input{
		TaskID: "t-noplan",
		Evidence: []state.RecoveryEvidence{
			writeEvidence(t, "obs-000001", "result.txt", hashOf("42\n"), "created"),
		},
	})
	if report.Decision != DecisionBlocked {
		t.Fatalf("decision = %s, want blocked without an acceptance plan", report.Decision)
	}
	check := findCheck(report, checkAcceptanceCriteria)
	if check.Status != CheckBlocked || check.Reason != "acceptance_plan_missing" {
		t.Fatalf("acceptance criteria check = %+v", check)
	}
	if findCheck(report, checkWritesReconciled).Status != CheckPassed {
		t.Fatalf("structural checks still pass: %+v", report.Checks)
	}
}

// An explicit plan with zero checks is the same fail-closed state: no
// task-specific acceptance criterion exists.
func TestVerifyEmptyAcceptancePlanBlocksCompletion(t *testing.T) {
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t-emptyplan", Plan: EmptyPlan(),
	})
	if report.Decision != DecisionBlocked {
		t.Fatalf("decision = %s, want blocked", report.Decision)
	}
	if findCheck(report, checkAcceptanceCriteria).Reason != "acceptance_plan_missing" {
		t.Fatalf("acceptance criteria check = %+v", findCheck(report, checkAcceptanceCriteria))
	}
}

// Limits.MaxChecks is enforced: a plan with more checks than the attempt
// budget is refused blocked with check_budget_exceeded, never partially
// proven.
func TestVerifyMaxChecksBudgetExceededBlocks(t *testing.T) {
	plan := &Plan{Version: PlanVersion}
	for index := 0; index < DefaultLimits().MaxChecks+8; index++ {
		plan.Checks = append(plan.Checks, Check{
			ID: fmt.Sprintf("c-%04d", index), Type: CheckFileExists, Path: fmt.Sprintf("f-%04d.txt", index),
		})
	}
	observer := &fakeObserver{}
	report := mustReport(t, observer, Input{TaskID: "t-budget", Plan: plan})
	if report.Decision != DecisionBlocked {
		t.Fatalf("decision = %s, want blocked (check budget exceeded)", report.Decision)
	}
	if findCheck(report, checkBudgetExceeded).Status != CheckBlocked {
		t.Fatalf("budget check = %+v", findCheck(report, checkBudgetExceeded))
	}
	if len(report.Checks) > DefaultLimits().MaxChecks+2 {
		t.Fatalf("report must stay bounded: %d checks", len(report.Checks))
	}
}

// Limits.MaxObservedChars is enforced: a description longer than the bound is
// truncated with an explicit marker, never persisted in full.
func TestVerifyBoundedDescriptions(t *testing.T) {
	longFailure := strings.Repeat("x", DefaultLimits().MaxObservedChars*2)
	observer := &fakeObserver{gitErr: errors.New(longFailure)}
	report := mustReport(t, observer, Input{TaskID: "t-bounds"})
	check := findCheck(report, checkGitObserved)
	if len(check.Observed) > DefaultLimits().MaxObservedChars+len("[truncated]") {
		t.Fatalf("observed description must be bounded, got %d chars", len(check.Observed))
	}
	if !strings.Contains(check.Observed, "[truncated]") {
		t.Fatalf("truncation must be explicit: %q", check.Observed)
	}
	for _, limitation := range report.Limitations {
		if len(limitation) > DefaultLimits().MaxObservedChars+len("[truncated]") {
			t.Fatalf("limitation must be bounded: %d chars", len(limitation))
		}
	}
}

// A truncated git baseline is recorded explicitly: the report carries the flag
// and the limitation so pre-existing changes outside the truncated window are
// never silently attributed as during_task (issue #11 review).
func TestVerifyBaselineTruncationRecorded(t *testing.T) {
	observer := &fakeObserver{gitStatus: "?? a.txt\n"}
	report := mustReport(t, observer, Input{
		TaskID:                     "t-baseline-trunc",
		Plan:                       planWithChecks(Check{ID: "a", Type: CheckFileExists, Path: "a.txt"}),
		BaselineGitStatusTruncated: true,
	})
	if report.Git == nil || !report.Git.Available {
		t.Fatalf("git = %+v", report.Git)
	}
	if !report.Git.BaselineTruncated {
		t.Fatalf("baseline truncation must be recorded: %+v", report.Git)
	}
	found := false
	for _, limitation := range report.Limitations {
		if strings.Contains(limitation, "baseline truncated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("baseline truncation limitation must be visible: %+v", report.Limitations)
	}
}

// Scenario 4: the model claims tests passed with no executed recipe.
func TestVerifyRejectsTestsPassedWithoutRecipe(t *testing.T) {
	plan := planWithChecks(Check{ID: "tests-pass", Type: CheckRecipeExitZero, Recipe: "test"})
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t4", Plan: plan, Evidence: nil,
	})
	if report.Decision != DecisionFailed {
		t.Fatalf("decision = %s, want failed", report.Decision)
	}
}

// Scenario 5: the test recipe executes and returns non-zero.
func TestVerifyFailsRecipeNonZeroExit(t *testing.T) {
	plan := planWithChecks(Check{ID: "tests-pass", Type: CheckRecipeExitZero, Recipe: "test"})
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t5", Plan: plan,
		Evidence: []state.RecoveryEvidence{recipeEvidenceItem(t, "obs-000001", "test", 1, false)},
	})
	if report.Decision != DecisionFailed {
		t.Fatalf("decision = %s, want failed", report.Decision)
	}
	check := findCheck(report, "tests-pass")
	if check.Status != CheckFailed || check.Reason != "recipe_exit_nonzero" {
		t.Fatalf("tests-pass check = %+v", check)
	}
}

// Scenario 6: process exits zero but the required artifact is absent.
func TestVerifyZeroExitButMissingArtifact(t *testing.T) {
	plan := planWithChecks(
		Check{ID: "tests-pass", Type: CheckRecipeExitZero, Recipe: "test"},
		Check{ID: "artifact", Type: CheckFileExists, Path: "dist/app"},
	)
	report := mustReport(t, &fakeObserver{files: map[string]string{}}, Input{
		TaskID: "t6", Plan: plan,
		Evidence: []state.RecoveryEvidence{recipeEvidenceItem(t, "obs-000001", "test", 0, false)},
	})
	if report.Decision != DecisionFailed {
		t.Fatalf("decision = %s, want failed", report.Decision)
	}
	if findCheck(report, "tests-pass").Status != CheckPassed {
		t.Fatalf("exit-zero recipe must pass: %+v", findCheck(report, "tests-pass"))
	}
	if findCheck(report, "artifact").Status != CheckFailed {
		t.Fatalf("missing artifact must fail: %+v", findCheck(report, "artifact"))
	}
}

// Scenario 7: truncated output cannot support a conclusion that depends on
// the missing part (require_untruncated).
func TestVerifyTruncatedOutputCannotSupportConclusion(t *testing.T) {
	plan := planWithChecks(Check{ID: "tests-full", Type: CheckRecipeExitZero, Recipe: "test", RequireUntruncated: true})
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t7", Plan: plan,
		Evidence: []state.RecoveryEvidence{recipeEvidenceItem(t, "obs-000001", "test", 0, true)},
	})
	if report.Decision != DecisionFailed {
		t.Fatalf("decision = %s, want failed", report.Decision)
	}
	check := findCheck(report, "tests-full")
	if check.Status != CheckFailed || check.Reason != "truncated_output_cannot_support_conclusion" {
		t.Fatalf("tests-full check = %+v", check)
	}
	if len(report.TruncatedEvidence) != 1 || report.TruncatedEvidence[0] != "obs-000001" {
		t.Fatalf("truncation must be recorded explicitly: %+v", report.TruncatedEvidence)
	}
}

// Scenario 7b: truncation alone does not invalidate an exit-status-only
// conclusion; it is recorded, not silently ignored.
func TestVerifyTruncationRecordedButExitZeroStillValid(t *testing.T) {
	plan := planWithChecks(Check{ID: "tests-pass", Type: CheckRecipeExitZero, Recipe: "test"})
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t7b", Plan: plan,
		Evidence: []state.RecoveryEvidence{recipeEvidenceItem(t, "obs-000001", "test", 0, true)},
	})
	if report.Decision != DecisionPassed {
		t.Fatalf("decision = %s, want passed (truncation recorded, exit status valid)", report.Decision)
	}
	if len(report.TruncatedEvidence) != 1 {
		t.Fatalf("truncation must be recorded: %+v", report.TruncatedEvidence)
	}
}

// Scenario 8: the requested change was only partially implemented: the
// persisted write evidence does not match the current filesystem. The task
// also has no acceptance plan, so the run would additionally be refused
// blocked; the failed write reconciliation is still reported.
func TestVerifyPartialImplementationFails(t *testing.T) {
	report := mustReport(t, &fakeObserver{files: map[string]string{"src/main.go": "partial\n"}}, Input{
		TaskID: "t8",
		Evidence: []state.RecoveryEvidence{
			writeEvidence(t, "obs-000001", "src/main.go", hashOf("complete\n"), "modified"),
		},
	})
	if report.Decision != DecisionBlocked {
		t.Fatalf("decision = %s, want blocked (no acceptance plan fails closed)", report.Decision)
	}
	check := findCheck(report, checkWritesReconciled)
	if check.Status != CheckFailed || check.Reason != "write_evidence_does_not_match_filesystem" {
		t.Fatalf("writes check = %+v", check)
	}
	if len(report.WriteReconciliation) != 1 || report.WriteReconciliation[0].Matches {
		t.Fatalf("reconciliation = %+v", report.WriteReconciliation)
	}
}

// Scenario 9: pre-existing repository changes are not attributed to the task.
func TestVerifyPreExistingChangesNotAttributed(t *testing.T) {
	observer := &fakeObserver{gitStatus: " M pre-existing.txt\n?? during-task.txt\n"}
	report := mustReport(t, observer, Input{
		TaskID: "t9", BaselineGitStatus: " M pre-existing.txt\n",
	})
	if report.Git == nil || !report.Git.Available {
		t.Fatalf("git = %+v", report.Git)
	}
	preExisting := changedPaths(report.Git.PreExisting)
	if len(preExisting) != 1 || preExisting[0] != "pre-existing.txt" {
		t.Fatalf("pre-existing = %v, want [pre-existing.txt]", preExisting)
	}
	during := changedPaths(report.Git.DuringTask)
	if len(during) != 1 || during[0] != "during-task.txt" {
		t.Fatalf("during-task = %v, want [during-task.txt]", during)
	}
}

// Scenario 10: a write attempt remains uncertain -> completion blocked.
func TestVerifyUncertainAttemptBlocksCompletion(t *testing.T) {
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t10",
		ToolAttempts: []state.RecoveryToolAttempt{{
			ExecutionID: "exec-000001", Tool: tools.ToolWriteFile, Status: "uncertain",
		}},
	})
	if report.Decision != DecisionUncertain {
		t.Fatalf("decision = %s, want uncertain", report.Decision)
	}
	if len(report.UncertainAttempts) != 1 || report.UncertainAttempts[0].ExecutionID != "exec-000001" {
		t.Fatalf("uncertain attempts = %+v", report.UncertainAttempts)
	}
}

// Scenario 11: a required approval remains pending -> completion blocked.
func TestVerifyPendingApprovalBlocksCompletion(t *testing.T) {
	report := mustReport(t, &fakeObserver{}, Input{
		TaskID: "t11", PendingApprovals: []string{"action-000001"},
	})
	if report.Decision != DecisionBlocked {
		t.Fatalf("decision = %s, want blocked", report.Decision)
	}
	if len(report.PendingApprovals) != 1 {
		t.Fatalf("pending approvals = %+v", report.PendingApprovals)
	}
}

// Scenario 12: valid final + correct filesystem + real git evidence + recipe
// passed + acceptance checks satisfied -> completion accepted.
func TestVerifyFullPass(t *testing.T) {
	plan := planWithChecks(
		Check{ID: "artifact", Type: CheckFileExists, Path: "dist/app"},
		Check{ID: "tests-pass", Type: CheckRecipeExitZero, Recipe: "test"},
	)
	observer := &fakeObserver{
		files:     map[string]string{"dist/app": "binary\n", "src/main.go": "complete\n"},
		gitStatus: " M src/main.go\n?? dist/app\n",
	}
	report := mustReport(t, observer, Input{
		TaskID: "t12", Plan: plan, BaselineGitStatus: "",
		Evidence: []state.RecoveryEvidence{
			recipeEvidenceItem(t, "obs-000001", "test", 0, false),
			writeEvidence(t, "obs-000002", "src/main.go", hashOf("complete\n"), "modified"),
		},
	})
	if report.Decision != DecisionPassed {
		t.Fatalf("decision = %s, want passed\n%+v", report.Decision, report.Checks)
	}
	if findCheck(report, "artifact").Status != CheckPassed || findCheck(report, "tests-pass").Status != CheckPassed {
		t.Fatalf("checks = %+v", report.Checks)
	}
	if findCheck(report, checkWritesReconciled).Status != CheckPassed {
		t.Fatalf("writes must reconcile: %+v", findCheck(report, checkWritesReconciled))
	}
}

// Scenario 16/17: the report explains the decision with typed expected vs
// observed; model text is never an input and cannot change a failed check.
func TestVerifyModelTextCannotChangeFailedCheck(t *testing.T) {
	plan := planWithChecks(Check{ID: "artifact", Type: CheckFileExists, Path: "missing.txt"})
	observer := &fakeObserver{files: map[string]string{}}
	report := mustReport(t, observer, Input{TaskID: "t17", Plan: plan})
	if report.Decision != DecisionFailed {
		t.Fatalf("decision = %s, want failed", report.Decision)
	}
	check := findCheck(report, "artifact")
	if check.Expected == "" || check.Observed == "" || check.Reason == "" {
		t.Fatalf("check must explain expected/observed/reason: %+v", check)
	}
	// The same failed check is identical no matter what the model claims; the
	// Input type carries no model text at all.
	if check.Status != CheckFailed {
		t.Fatal("a failed check cannot be altered")
	}
}

// Scenario: a blocked check (file observation failure) is distinct from a
// failed check and does not return to the agent.
func TestVerifyBlockedFileObservation(t *testing.T) {
	plan := planWithChecks(Check{ID: "artifact", Type: CheckFileExists, Path: "x.txt"})
	observer := &fakeObserver{hashErr: errors.New("observation unavailable")}
	report := mustReport(t, observer, Input{TaskID: "t18", Plan: plan})
	if report.Decision != DecisionBlocked {
		t.Fatalf("decision = %s, want blocked", report.Decision)
	}
}

func findCheck(report Report, id string) CheckResult {
	for _, check := range report.Checks {
		if check.ID == id {
			return check
		}
	}
	return CheckResult{}
}

func changedPaths(files []ChangedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}

// Issue #12: a corrective write legitimately supersedes an earlier write to
// the same path (the coding-loop trajectory writes a wrong fix, then
// overwrites it with the correct fix). The latest write must match the
// current filesystem; the superseded intermediate state is recorded in the
// report instead of failing the writes_reconciled check.
func TestVerifySupersededWriteDoesNotFail(t *testing.T) {
	first := writeEvidence(t, "obs-000001", "src/calc.go", hashOf("wrong\n"), "modified")
	second := writeEvidence(t, "obs-000002", "src/calc.go", hashOf("right\n"), "modified")
	report := mustReport(t, &fakeObserver{files: map[string]string{"src/calc.go": "right\n"}}, Input{
		TaskID:   "t-superseded",
		Evidence: []state.RecoveryEvidence{first, second},
	})
	check := findCheck(report, checkWritesReconciled)
	if check.Status != CheckPassed {
		t.Fatalf("writes check = %+v, want passed (the latest write matches)", check)
	}
	if len(report.WriteReconciliation) != 1 {
		t.Fatalf("reconciliation = %+v, want exactly one entry per path", report.WriteReconciliation)
	}
	entry := report.WriteReconciliation[0]
	if entry.EvidenceID != "obs-000002" || !entry.Matches {
		t.Fatalf("reconciliation = %+v, want the latest evidence matching the filesystem", entry)
	}
	if len(entry.Superseded) != 1 || entry.Superseded[0] != "obs-000001" {
		t.Fatalf("superseded = %v, want [obs-000001]", entry.Superseded)
	}
}

// The superseded semantics never hide a real mismatch: when the LATEST write
// does not match the current filesystem (for example an external edit after
// the corrective write), the writes_reconciled check still fails.
func TestVerifyLatestWriteMismatchStillFails(t *testing.T) {
	first := writeEvidence(t, "obs-000001", "src/calc.go", hashOf("wrong\n"), "modified")
	second := writeEvidence(t, "obs-000002", "src/calc.go", hashOf("right\n"), "modified")
	report := mustReport(t, &fakeObserver{files: map[string]string{"src/calc.go": "external edit\n"}}, Input{
		TaskID:   "t-superseded-mismatch",
		Evidence: []state.RecoveryEvidence{first, second},
	})
	check := findCheck(report, checkWritesReconciled)
	if check.Status != CheckFailed || check.Reason != "write_evidence_does_not_match_filesystem" {
		t.Fatalf("writes check = %+v, want failed for the latest write mismatch", check)
	}
}

// Issue #12: `git status --short --branch` emits a branch header line
// ("## main...origin/main") that is NOT a changed file. It must never be
// attributed as a pre-existing or during-task change in the final evidence
// report.
func TestVerifyBranchHeaderNotAttributed(t *testing.T) {
	observer := &fakeObserver{gitStatus: "## main...origin/main\n M app/calc.go\n"}
	report := mustReport(t, observer, Input{
		TaskID: "t-branch", BaselineGitStatus: "## main...origin/main\n",
	})
	if report.Git == nil || !report.Git.Available {
		t.Fatalf("git = %+v", report.Git)
	}
	if got := changedPaths(report.Git.PreExisting); len(got) != 0 {
		t.Fatalf("pre-existing = %v, want none (the branch header is not a file)", got)
	}
	during := changedPaths(report.Git.DuringTask)
	if len(during) != 1 || during[0] != "app/calc.go" {
		t.Fatalf("during-task = %v, want [app/calc.go]", during)
	}
}
