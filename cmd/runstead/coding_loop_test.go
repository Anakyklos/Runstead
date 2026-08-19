package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
)

// codingLoopFixture is the committed deterministic sample repository of the
// #12 inspect-edit-test-fix loop. Tests run with the package directory as the
// working directory, so ../../fixtures/coding-loop resolves from cmd/runstead
// to the repository root.
const codingLoopFixture = "../../fixtures/coding-loop"

func codingLoopFixtureFile(t *testing.T, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(codingLoopFixture, relative))
	if err != nil {
		t.Fatalf("read coding-loop fixture %s: %v", relative, err)
	}
	return string(content)
}

// copyCodingLoopFixture copies the committed fixture into a fresh workspace
// and initializes it as a REAL git repository with a committed baseline, so
// the runtime's git observations and the verifier's change attribution are
// real, not mocks.
func copyCodingLoopFixture(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	src, err := filepath.Abs(codingLoopFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(workspace, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode())
	}); err != nil {
		t.Fatalf("copy coding-loop fixture: %v", err)
	}
	initGitRepo(t, workspace)
	return workspace
}

// initGitRepo initializes a real git repository with one committed baseline
// commit, using explicit identity and disabled signing so no user git config
// can influence the deterministic test.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	env := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_NOGLOBAL=1",
		"GIT_CONFIG_COUNT=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Runstead Test",
		"GIT_AUTHOR_EMAIL=test@runstead.invalid",
		"GIT_COMMITTER_NAME=Runstead Test",
		"GIT_COMMITTER_EMAIL=test@runstead.invalid",
	)
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-b", "main", ".")
	runGit("add", "-A")
	runGit("-c", "commit.gpgsign=false", "commit", "-m", "fixture baseline")
}

func hashOfBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// codingLoopAcceptance writes the operator acceptance plan of the full
// scenario: the test recipe must have a real zero-exit execution AND the
// corrected file must match the exact fixed content.
func codingLoopAcceptance(t *testing.T, fixedHash string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acceptance.json")
	plan := fmt.Sprintf(`{"version":1,"checks":[{"id":"tests-pass","type":"recipe_exit_zero","recipe":"test","require_untruncated":true},{"id":"fix-hash","type":"file_hash","path":"app/calc.go","sha256":%q}]}`, fixedHash)
	if err := os.WriteFile(path, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCodingLoopDeterministicScenarioEndToEnd is the #12 required
// deterministic scenario through the REAL CLI composition: a real git
// repository, real safe writes, a real `go test` recipe that fails once (and
// once more after the first, insufficient fix), diagnosis from the real
// bounded process evidence, a corrective write, a passing rerun, and final
// independent verification with git attribution. The provider is the only
// scripted seam; filesystem, git, process and verifier are real.
func TestCodingLoopDeterministicScenarioEndToEnd(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
	wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
	wrongHash := hashOfBytes([]byte(wrongFix))
	correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
	correctHash := hashOfBytes([]byte(correctFix))
	acceptance := codingLoopAcceptance(t, correctHash)

	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc_test.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(wrongFix)+`,"expected_before_hash":"`+initialHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(correctFix)+`,"expected_before_hash":"`+wrongHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed the calculator.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"read_file"},{"evidence_id":"obs-000004","tool":"write_file"},{"evidence_id":"obs-000005","tool":"run_recipe"},{"evidence_id":"obs-000006","tool":"read_file"},{"evidence_id":"obs-000007","tool":"write_file"},{"evidence_id":"obs-000008","tool":"run_recipe"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Fix the calculator so the test suite passes.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--recipe-policy", "test=allow",
		"--write-policy", "write_file=allow",
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("run must complete:\n%s", out.String())
	}
	// The verified summary is produced by the verifier from the acceptance
	// checks; the model's free text is only an unverified note.
	if !strings.Contains(out.String(), "tests-pass") || !strings.Contains(out.String(), "fix-hash") {
		t.Fatalf("verified summary must name the acceptance checks:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "note (unverified): Fixed the calculator.") {
		t.Fatalf("the model text must be surfaced as an unverified note:\n%s", out.String())
	}
	for _, want := range []string{
		"Verified runtime result:",
		"status: completed",
		"outcome: completed",
		"verifier: passed",
		"check=tests-pass type=recipe_exit_zero status=passed",
		"during-task changes: app/calc.go ( M)",
		"Git diff (bounded):",
		"recipe=test exit=1 evidence=obs-000002",
		"recipe=test exit=1 evidence=obs-000005",
		"recipe=test exit=0 evidence=obs-000008",
		"truncated=stdout:false/stderr:false",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("normal run output missing authoritative %q:\n%s", want, out.String())
		}
	}

	// The real workspace holds the corrected implementation (real safe write).
	content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if hashOfBytes(content) != correctHash {
		t.Fatalf("workspace does not hold the corrected implementation")
	}
	// Real git observation: app/calc.go is modified against the baseline.
	statusCmd := exec.Command("git", "-C", workspace, "status", "--short", "--no-renames", "--", ".")
	statusCmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_NOGLOBAL=1", "GIT_CONFIG_COUNT=0", "GIT_TERMINAL_PROMPT=0")
	statusOutput, err := statusCmd.CombinedOutput()

	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusOutput), " M app/calc.go") {
		t.Fatalf("git must observe the modified implementation:\n%s", statusOutput)
	}

	// The durable execution history is inspectable: the failing and passing
	// process attempts, the passed verification with its checks, and the git
	// change attribution.
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "decision=passed") {
		t.Fatalf("inspect must show the passed verification:\n%s", rendered)
	}
	if !strings.Contains(rendered, "check=tests-pass type=recipe_exit_zero status=passed") {
		t.Fatalf("inspect must show the passed recipe check:\n%s", rendered)
	}
	// Both real process outcomes are persisted: exit=1 (failing run) and
	// exit=0 (passing rerun).
	if !strings.Contains(rendered, "recipe=test exit=1") || !strings.Contains(rendered, "recipe=test exit=0") {
		t.Fatalf("inspect must render the failing and passing process attempts:\n%s", rendered)
	}
	// Real git observation with change attribution: app/calc.go is a
	// during-task change, not pre-existing.
	if !strings.Contains(rendered, "Git observation:\n") || !strings.Contains(rendered, "during-task changes: app/calc.go ( M)") {
		t.Fatalf("inspect must render the git change attribution:\n%s", rendered)
	}
	if !strings.Contains(rendered, "pre-existing changes: (none)") {
		t.Fatalf("inspect must render no pre-existing changes:\n%s", rendered)
	}
	// The execution history is durable and inspectable after completion: two
	// scoped write attempts to app/calc.go, the second superseding the first.
	if !strings.Contains(rendered, "tool=write_file") {
		t.Fatalf("inspect must render the write attempts:\n%s", rendered)
	}
}

// TestCodingLoopPrematureCompletionFailsThenCompletes is regression C through
// the real CLI: the model proposes complete while the suite is still red; the
// verifier rejects (recipe_exit_nonzero) and the run CONTINUES with the
// corrective trajectory; the next completion proposal passes. This proves a
// failed verification returns the task to active execution.
func TestCodingLoopPrematureCompletionFailsThenCompletes(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
	correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
	correctHash := hashOfBytes([]byte(correctFix))
	acceptance := codingLoopAcceptance(t, correctHash)

	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		// Premature completion while the suite is red: the verifier refuses.
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"tests pass","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"}]}</runstead_final>`,
		// The loop continues after the failed verification: corrective write
		// and passing rerun.
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(correctFix)+`,"expected_before_hash":"`+initialHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"tests pass","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"write_file"},{"evidence_id":"obs-000004","tool":"run_recipe"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Fix the calculator so the test suite passes.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--recipe-policy", "test=allow",
		"--write-policy", "write_file=allow",
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	// Two verification attempts: the premature proposal failed, the corrected
	// proposal passed. The task never completed before the checks passed.
	if !strings.Contains(rendered, "decision=failed") || !strings.Contains(rendered, "decision=passed") {
		t.Fatalf("inspect must show the failed then passed verification attempts:\n%s", rendered)
	}
	if !strings.Contains(rendered, "reason: recipe_exit_nonzero") {
		t.Fatalf("the failed verification must carry the typed recipe failure:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Status: completed") {
		t.Fatalf("task must be completed after the corrective trajectory:\n%s", rendered)
	}
}

// TestCodingLoopResumeAfterInterruptionContinues is regression G through the
// real CLI: the run is killed mid-loop after REAL progress (inspection, a
// failing test run, and the first insufficient write), and `runstead resume`
// continues the SAME task from durable state. Completed effects are not
// repeated, the recipe is not reconstructed as if it never happened, evidence
// stays available, and the resumed trajectory completes with the corrective
// write and passing rerun.
func TestCodingLoopResumeAfterInterruptionContinues(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
	wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
	wrongHash := hashOfBytes([]byte(wrongFix))
	correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
	correctHash := hashOfBytes([]byte(correctFix))
	acceptance := codingLoopAcceptance(t, correctHash)

	// Run 1: inspect, failing test run, first (insufficient) write, second
	// failing test run. The process is killed after the fourth completed tool
	// attempt (tool TX 2), so all four attempts are durable verified history.
	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(wrongFix)+`,"expected_before_hash":"`+initialHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	runArgs := []string{
		"run", "--task", "Fix the calculator so the test suite passes.",
		"--workspace", workspace,
		"--scripted", runScript,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--recipe-policy", "test=allow",
		"--write-policy", "write_file=allow",
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
	code, output := runCrashedRunAfter(t, runArgs, "tool_tx2_after", 4)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "Status: running") {
		t.Fatalf("the interrupted task must remain running:\n%s", rendered)
	}
	// Real progress was durable: the failing recipe evidence exists BEFORE
	// the resume.
	if !strings.Contains(rendered, "recipe=test exit=1") {
		t.Fatalf("the failing recipe run must be durable before resume:\n%s", rendered)
	}

	// Resume: the workspace now holds the wrong fix; the model re-inspects,
	// applies the corrective write, reruns the recipe and proposes completion.
	// Evidence IDs continue from the persisted maximum (obs-000004).
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(correctFix)+`,"expected_before_hash":"`+wrongHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed the calculator.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"write_file"},{"evidence_id":"obs-000004","tool":"run_recipe"},{"evidence_id":"obs-000005","tool":"read_file"},{"evidence_id":"obs-000006","tool":"write_file"},{"evidence_id":"obs-000007","tool":"run_recipe"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--min-start-interval", "1ms",
		"--log-level", "error",
	)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstdout:\n%s\nstderr:\n%s", resumeCode, resumeOut, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", resumeOut)
	}
	for _, want := range []string{
		"Verified runtime result:",
		"status: completed",
		"outcome: completed",
		"verifier: passed",
		"recipe=test exit=1 evidence=obs-000002",
		"recipe=test exit=1 evidence=obs-000004",
		"recipe=test exit=0 evidence=obs-000007",
		"during-task changes: app/calc.go ( M)",
	} {
		if !strings.Contains(resumeOut, want) {
			t.Fatalf("resume output missing authoritative %q:\n%s", want, resumeOut)
		}
	}
	// The final workspace state is the corrected implementation.
	content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if hashOfBytes(content) != correctHash {
		t.Fatalf("workspace does not hold the corrected implementation after resume")
	}
	// Durable history after the resume: exactly three recipe executions (two
	// failures before the interruption, one passing rerun), exactly two write
	// attempts (the first insufficient write plus the corrective one), and a
	// passed verification. Nothing was duplicated or reconstructed.
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "Status: completed") {
		t.Fatalf("task must be completed after the resume:\n%s", rendered)
	}
	if !strings.Contains(rendered, "decision=passed") {
		t.Fatalf("resume must end with a passed verification:\n%s", rendered)
	}
	if strings.Count(rendered, "recipe=test exit=1") != 2 {
		t.Fatalf("the two historical failing recipe runs must both be durable (no reconstruction):\n%s", rendered)
	}
	if strings.Count(rendered, "recipe=test exit=0") != 1 {
		t.Fatalf("exactly one passing rerun must be durable:\n%s", rendered)
	}
	// Tool attempt rows carry "tool=write_file action="; policy decision rows
	// also mention the tool name, so count the attempt rows specifically.
	if strings.Count(rendered, " tool=write_file action=") != 2 {
		t.Fatalf("exactly two write attempts must be durable (no duplication):\n%s", rendered)
	}
	if !strings.Contains(rendered, "Resumes: 1") {
		t.Fatalf("inspect must record the resume (Resumes: 1):\n%s", rendered)
	}
}

// TestCodingLoopLivePathFailsClosed proves the live OmniRoute path of the
// #12 scenario stays fail-closed while #29/#30/#4 are not satisfied: even
// with a declared safe route and credentials, a run never reaches a provider
// and exits with the typed unavailable code and the documented blocker. This
// is the deterministic test of the live gate; the opt-in manual live harness
// remains experiments/protocol (documented in docs/coding-loop.md).
func TestCodingLoopLivePathFailsClosed(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Fix the calculator so the test suite passes.",
		"--workspace", workspace,
		"--omniroute-base-url", "http://127.0.0.1:1/v1",
		"--omniroute-api-key", "not-a-real-key",
		"--omniroute-safe-route",
		"--state-dir", stateDir,
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitUnavailable {
		t.Fatalf("run exit = %d, want %d (live path fails closed)\nstderr:\n%s", code, exitUnavailable, errOut.String())
	}
	if !strings.Contains(errOut.String(), "OMNIROUTE_CONNECTION_ID") {
		t.Fatalf("the diagnostic must name the connection-pin live blocker:\n%s", errOut.String())
	}
}

func mustQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// TestCodingLoopConsecutiveFailureFlagStops proves the
// --max-consecutive-failures flag is wired through the CLI: with a
// one-failure allowance, two consecutive failing observations stop the run
// with the typed consecutive_failures_exhausted outcome and exit code.
func TestCodingLoopConsecutiveFailureFlagStops(t *testing.T) {
	workspace := t.TempDir()
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"missing-a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"missing-b.txt"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--max-consecutive-failures", "1",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != agent.OutcomeConsecutiveFailuresExhausted.ExitCode() {
		t.Fatalf("run exit = %d, want %d\nstderr:\n%s", code, agent.OutcomeConsecutiveFailuresExhausted.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: consecutive_failures_exhausted") {
		t.Fatalf("run output must show the typed outcome:\n%s", out.String())
	}
}

// TestCodingLoopVerificationRetryFlagStops proves the
// --max-verification-retries flag is wired through the CLI: with a
// one-retry allowance, two failed completion verifications stop the run with
// the typed verification_failures_exhausted outcome and exit code.
func TestCodingLoopVerificationRetryFlagStops(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"artifact","type":"file_exists","path":"never.txt"}]}`)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Create never.txt.",
		"--workspace", workspace,
		"--scripted", script,
		"--acceptance", acceptance,
		"--max-verification-retries", "1",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != agent.OutcomeVerificationFailuresExhausted.ExitCode() {
		t.Fatalf("run exit = %d, want %d\nstderr:\n%s", code, agent.OutcomeVerificationFailuresExhausted.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: verification_failures_exhausted") {
		t.Fatalf("run output must show the typed outcome:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Verified runtime result:") {
		t.Fatalf("failed verification must not receive a completed report:\n%s", out.String())
	}
}
