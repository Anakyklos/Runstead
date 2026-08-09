package main

// Issue #13 - crash/interruption boundaries at the REAL #12 coding-loop
// boundaries. A subprocess runs the REAL CLI composition against the REAL
// coding-loop fixture (real git repo, real safe writes, real `go test`
// recipe) and dies at a named boundary; the parent then resumes the SAME
// task with a NEW provider conversation and proves the final invariants:
// completed effects are never duplicated, interrupted effects are reconciled
// from authoritative state, the task history stays intact, and an uncertain
// process effect never becomes success.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// TestRuntimeCodingLoopCrashHelper is the subprocess entry point for the #13
// coding-loop crash boundaries: it installs every deterministic crash seam
// (state, write tool and process runner) and runs the real CLI.
func TestRuntimeCodingLoopCrashHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_CODING_CRASH_HELPER") == "" {
		t.Skip("coding-loop crash helper")
	}
	point := os.Getenv("RUNSTEAD_CODING_CRASH_POINT")
	after := 1
	if value := os.Getenv("RUNSTEAD_CODING_CRASH_AFTER"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			after = parsed
		}
	}
	state.SetCrashPoint(func(name string) {
		if name == point {
			after--
			if after == 0 {
				os.Exit(42)
			}
		}
	})
	tools.SetWriteCrashPoint(func(name string) {
		if name == point {
			after--
			if after == 0 {
				os.Exit(42)
			}
		}
	})
	recipe.SetCrashPoint(func(name string) {
		if name == point {
			after--
			if after == 0 {
				os.Exit(42)
			}
		}
	})
	args := strings.Split(os.Getenv("RUNSTEAD_CODING_CRASH_ARGS"), "\x1f")
	code := run(context.Background(), args, os.Stdout, os.Stderr)
	os.Exit(code)
}

// TestRuntimeRecipeCrashHelper is the recipe executable for the mid-process
// crash boundary: it writes a marker and its own pid, then blocks until the
// process group is terminated (or the test kills it).
func TestRuntimeRecipeCrashHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_RUNTIME_RECIPE_CRASH_HELPER") != "1" {
		t.Skip("runtime recipe crash helper")
	}
	if path := os.Getenv("RUNSTEAD_RUNTIME_RECIPE_MARKER"); path != "" {
		_ = os.WriteFile(path, []byte("recipe-started\n"), 0o600)
	}
	if path := os.Getenv("RUNSTEAD_RUNTIME_RECIPE_PIDFILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	time.Sleep(300 * time.Second)
}

// codingCrashRunArgs builds the real CLI run arguments for the full #12
// scenario with the given scripted provider file.
func codingCrashRunArgs(t *testing.T, scriptPath, workspace, stateDir, acceptance string) []string {
	t.Helper()
	return []string{
		"run", "--task", "Fix the calculator so the test suite passes.",
		"--workspace", workspace,
		"--scripted", scriptPath,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--recipe-policy", "test=allow",
		"--write-policy", "write_file=allow",
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
}

// codingResumeArgs builds the resume arguments continuing the same task with
// a NEW provider conversation.
func codingResumeArgs(t *testing.T, taskID, scriptPath, stateDir, acceptance string) []string {
	t.Helper()
	return []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", scriptPath,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--recipe-policy", "test=allow",
		"--write-policy", "write_file=allow",
		"--acceptance", acceptance,
		"--log-level", "error",
	}
}

// runCrashedCodingLoop spawns the crash helper and returns its exit code and
// full output (the run command prints the task id before execution).
func runCrashedCodingLoop(t *testing.T, args []string, point string, after int) (int, string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=TestRuntimeCodingLoopCrashHelper")
	command.Env = append(os.Environ(),
		"RUNSTEAD_CODING_CRASH_HELPER=1",
		"RUNSTEAD_CODING_CRASH_POINT="+point,
		"RUNSTEAD_CODING_CRASH_AFTER="+strconv.Itoa(after),
		"RUNSTEAD_CODING_CRASH_ARGS="+strings.Join(args, "\x1f"),
	)
	output, err := command.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 42 {
		return 42, string(output)
	}
	t.Fatalf("crash helper exit = %v\n%s", err, output)
	return 0, string(output)
}

// toolAttemptCounts loads the durable snapshot and counts tool attempts by
// tool and status.
func toolAttemptCounts(t *testing.T, stateDir, taskID string) map[string]int {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer store.Close()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot: %v", err)
	}
	counts := make(map[string]int)
	for _, attempt := range snapshot.ToolAttempts {
		key := attempt.Tool + "/" + attempt.Status
		counts[key]++
	}
	return counts
}

// TestCodingLoopCrashAfterFirstWriteReconcilesAndCompletes crashes right
// after the FIRST scoped write effect (the wrong fix) committed to the
// filesystem but before its result was persisted. Resume reconciles the write
// from the filesystem state, continues the loop with a new conversation, and
// completes without ever re-executing the wrong-fix write.
func TestCodingLoopCrashAfterFirstWriteReconcilesAndCompletes(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
	wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
	wrongHash := hashOfBytes([]byte(wrongFix))
	correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
	correctHash := hashOfBytes([]byte(correctFix))
	acceptance := codingLoopAcceptance(t, correctHash)

	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc_test.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(wrongFix)+`,"expected_before_hash":"`+initialHash+`"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedCodingLoop(t, codingCrashRunArgs(t, runScript, workspace, stateDir, acceptance), "write_after_effect", 1)
	if code != 42 {
		t.Fatalf("crash exit = %d, want 42", code)
	}
	taskID := taskIDFromOutput(t, output)

	// The wrong-fix write effect DID happen (crash after the effect): the
	// filesystem holds the wrong fix, the attempt is prepared.
	content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if hashOfBytes(content) != wrongHash {
		t.Fatalf("workspace must hold the wrong fix after the first write crash")
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("the interrupted write attempt must stay prepared:\n%s", rendered)
	}

	// Resume with a NEW conversation: the reconciled write is completed (never
	// re-executed), the loop continues with the corrective trajectory.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(correctFix)+`,"expected_before_hash":"`+wrongHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"read_file"},{"evidence_id":"obs-000004","tool":"write_file"},{"evidence_id":"obs-000005","tool":"run_recipe"},{"evidence_id":"obs-000006","tool":"read_file"},{"evidence_id":"obs-000007","tool":"write_file"},{"evidence_id":"obs-000008","tool":"run_recipe"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), codingResumeArgs(t, taskID, resumeScript, stateDir, acceptance), &out, &errOut)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", out.String())
	}
	content, err = os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if hashOfBytes(content) != correctHash {
		t.Fatalf("final workspace must hold the corrected implementation")
	}
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "write_effect_completed") {
		t.Fatalf("the interrupted write must be reconciled as completed:\n%s", rendered)
	}
	// Exactly ONE wrong-fix write attempt exists, reconciled; exactly ONE
	// corrective write attempt was executed after resume.
	counts := toolAttemptCounts(t, stateDir, taskID)
	if counts["write_file/reconciled"] != 1 || counts["write_file/completed"] != 1 {
		t.Fatalf("write attempt projections = %v, want one reconciled wrong fix + one completed correction", counts)
	}
}

// TestCodingLoopCrashAfterFailingTestResumesWithoutRerun crashes right after
// the FIRST failing test run's evidence was persisted, before the next model
// turn. Resume continues from the failing observation with a new
// conversation: the failing test is NOT re-run, the diagnosis proceeds and
// the task completes.
func TestCodingLoopCrashAfterFailingTestResumesWithoutRerun(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
	wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
	wrongHash := hashOfBytes([]byte(wrongFix))
	correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
	correctHash := hashOfBytes([]byte(correctFix))
	acceptance := codingLoopAcceptance(t, correctHash)

	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedCodingLoop(t, codingCrashRunArgs(t, runScript, workspace, stateDir, acceptance), "tool_tx2_after", 2)
	if code != 42 {
		t.Fatalf("crash exit = %d, want 42", code)
	}
	taskID := taskIDFromOutput(t, output)

	// The failing test evidence was persisted before the crash.
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "recipe=test exit=1") {
		t.Fatalf("the failing test evidence must be persisted:\n%s", rendered)
	}

	// Resume with a new conversation CONTINUING from the diagnosis: the next
	// tool call is the test-file read, never a re-run of the failing test.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc_test.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(wrongFix)+`,"expected_before_hash":"`+initialHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(correctFix)+`,"expected_before_hash":"`+wrongHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"read_file"},{"evidence_id":"obs-000004","tool":"write_file"},{"evidence_id":"obs-000005","tool":"run_recipe"},{"evidence_id":"obs-000006","tool":"read_file"},{"evidence_id":"obs-000007","tool":"write_file"},{"evidence_id":"obs-000008","tool":"run_recipe"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), codingResumeArgs(t, taskID, resumeScript, stateDir, acceptance), &out, &errOut)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", out.String())
	}
	content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if hashOfBytes(content) != correctHash {
		t.Fatalf("final workspace must hold the corrected implementation")
	}
	// Exactly three run_recipe attempts in the whole task history: the
	// crashed failing run, the post-write failing run, the passing rerun. The
	// failing test was never re-run after resume.
	counts := toolAttemptCounts(t, stateDir, taskID)
	if counts["run_recipe/completed"] != 3 {
		t.Fatalf("run_recipe attempts = %v, want exactly 3 (fail, fail, pass)", counts)
	}
	if counts["read_file/completed"] != 3 {
		t.Fatalf("read_file attempts = %v, want exactly 3 (no re-inspection of the crashed turn)", counts)
	}
}

// TestCodingLoopCrashAfterCorrectiveWriteReconcilesAndReruns crashes right
// after the CORRECTIVE write effect committed, before its result persisted.
// Resume reconciles it from the filesystem and the passing rerun + verified
// completion finish the task without duplicating the corrective write.
func TestCodingLoopCrashAfterCorrectiveWriteReconcilesAndReruns(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
	wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
	wrongHash := hashOfBytes([]byte(wrongFix))
	correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
	correctHash := hashOfBytes([]byte(correctFix))
	acceptance := codingLoopAcceptance(t, correctHash)

	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc_test.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(wrongFix)+`,"expected_before_hash":"`+initialHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(correctFix)+`,"expected_before_hash":"`+wrongHash+`"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedCodingLoop(t, codingCrashRunArgs(t, runScript, workspace, stateDir, acceptance), "write_after_effect", 2)
	if code != 42 {
		t.Fatalf("crash exit = %d, want 42", code)
	}
	taskID := taskIDFromOutput(t, output)

	content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if hashOfBytes(content) != correctHash {
		t.Fatalf("workspace must hold the corrective fix after the crash")
	}

	// Resume with a new conversation: the corrective write is reconciled as
	// completed (obs-000007) and only the passing rerun + final remain.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"read_file"},{"evidence_id":"obs-000004","tool":"write_file"},{"evidence_id":"obs-000005","tool":"run_recipe"},{"evidence_id":"obs-000006","tool":"read_file"},{"evidence_id":"obs-000007","tool":"write_file"},{"evidence_id":"obs-000008","tool":"run_recipe"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), codingResumeArgs(t, taskID, resumeScript, stateDir, acceptance), &out, &errOut)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", out.String())
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "write_effect_completed") {
		t.Fatalf("the corrective write must be reconciled as completed:\n%s", rendered)
	}
	counts := toolAttemptCounts(t, stateDir, taskID)
	// Two write attempts total: wrong fix completed, corrective fix
	// reconciled. The passing rerun ran exactly once after resume.
	if counts["write_file/completed"] != 1 || counts["write_file/reconciled"] != 1 {
		t.Fatalf("write attempt projections = %v, want 1 completed + 1 reconciled", counts)
	}
	if counts["run_recipe/completed"] != 3 {
		t.Fatalf("run_recipe attempts = %v, want 3 (fail, fail, pass)", counts)
	}
}

// TestCodingLoopCrashAfterPassingVerifierResumesWithoutReExecution crashes
// after the passing verification was persisted but before the task was
// finalized. Resume completes from the durable state: no tool effect is
// re-executed and the task history stays intact.
func TestCodingLoopCrashAfterPassingVerifierResumesWithoutReExecution(t *testing.T) {
	workspace := copyCodingLoopFixture(t)
	initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
	wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
	wrongHash := hashOfBytes([]byte(wrongFix))
	correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
	correctHash := hashOfBytes([]byte(correctFix))
	acceptance := codingLoopAcceptance(t, correctHash)

	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc_test.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(wrongFix)+`,"expected_before_hash":"`+initialHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":`+mustQuote(correctFix)+`,"expected_before_hash":"`+wrongHash+`"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"read_file"},{"evidence_id":"obs-000004","tool":"write_file"},{"evidence_id":"obs-000005","tool":"run_recipe"},{"evidence_id":"obs-000006","tool":"read_file"},{"evidence_id":"obs-000007","tool":"write_file"},{"evidence_id":"obs-000008","tool":"run_recipe"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedCodingLoop(t, codingCrashRunArgs(t, runScript, workspace, stateDir, acceptance), "verification_recorded_after", 1)
	if code != 42 {
		t.Fatalf("crash exit = %d, want 42", code)
	}
	taskID := taskIDFromOutput(t, output)

	// The passing verification was persisted; the task was not finalized.
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "decision=passed") {
		t.Fatalf("the passing verification must be persisted:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Status: running") {
		t.Fatalf("the task must stay running before finalize:\n%s", rendered)
	}

	// Resume goes straight to the same grounded final: completion is decided
	// by the verifier from the persisted history, no tool effect re-runs.
	resumeScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"read_file"},{"evidence_id":"obs-000004","tool":"write_file"},{"evidence_id":"obs-000005","tool":"run_recipe"},{"evidence_id":"obs-000006","tool":"read_file"},{"evidence_id":"obs-000007","tool":"write_file"},{"evidence_id":"obs-000008","tool":"run_recipe"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), codingResumeArgs(t, taskID, resumeScript, stateDir, acceptance), &out, &errOut)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", out.String())
	}
	content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if hashOfBytes(content) != correctHash {
		t.Fatalf("final workspace must hold the corrected implementation")
	}
	// No tool effect was re-executed: the tool attempt counts are unchanged
	// after the resume (3 reads, 2 writes, 3 recipes).
	counts := toolAttemptCounts(t, stateDir, taskID)
	if counts["read_file/completed"] != 3 || counts["write_file/completed"] != 2 || counts["run_recipe/completed"] != 3 {
		t.Fatalf("tool attempt projections after resume = %v, want 3/2/3 with zero re-execution", counts)
	}
	// The finalize gate accepted the persisted passing verification; the
	// verification history holds both the crashed and the resumed attempts.
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	attempts, err := store.VerificationAttempts(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("verification attempts = %d, want 2 (crashed + resumed)", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Decision != "passed" {
			t.Fatalf("verification decision = %s, want passed", attempt.Decision)
		}
	}
}

// TestCodingLoopCrashMidProcessRequiresHumanReview crashes right after the
// recipe process STARTED: the effect is provably in flight, the result is
// unknown. Recovery must not re-run the process and must not turn it into
// success; the task stops with human_review_required.
func TestCodingLoopCrashMidProcessRequiresHumanReview(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "recipe.marker")
	pidFile := filepath.Join(t.TempDir(), "recipe.pid")
	// The recipe child needs these variables; they flow through the helper
	// process environment into the recipe allowlist.
	previous, existed := os.LookupEnv("RUNSTEAD_RUNTIME_RECIPE_CRASH_HELPER")
	_ = os.Setenv("RUNSTEAD_RUNTIME_RECIPE_CRASH_HELPER", "1")
	_ = os.Setenv("RUNSTEAD_RUNTIME_RECIPE_MARKER", marker)
	_ = os.Setenv("RUNSTEAD_RUNTIME_RECIPE_PIDFILE", pidFile)
	t.Cleanup(func() {
		_ = os.Unsetenv("RUNSTEAD_RUNTIME_RECIPE_MARKER")
		_ = os.Unsetenv("RUNSTEAD_RUNTIME_RECIPE_PIDFILE")
		if existed {
			_ = os.Setenv("RUNSTEAD_RUNTIME_RECIPE_CRASH_HELPER", previous)
		} else {
			_ = os.Unsetenv("RUNSTEAD_RUNTIME_RECIPE_CRASH_HELPER")
		}
	})
	catalog := filepath.Join(t.TempDir(), "recipes.json")
	catalogJSON := fmt.Sprintf(`[{"id":"hang","executable":%q,"argv":["-test.run=TestRuntimeRecipeCrashHelper"],"timeout_nanos":60000000000,"output_limits":{"max_stdout_bytes":4096,"max_stderr_bytes":4096},"capabilities":["execute_repository_code","inherit_environment"],"allowed_environment":["RUNSTEAD_RUNTIME_RECIPE_CRASH_HELPER","RUNSTEAD_RUNTIME_RECIPE_MARKER","RUNSTEAD_RUNTIME_RECIPE_PIDFILE"]}]`, exe)
	if err := os.WriteFile(catalog, []byte(catalogJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"hang"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	args := []string{
		"run", "--task", "Run the hang recipe.",
		"--workspace", workspace,
		"--scripted", runScript,
		"--recipes", catalog,
		"--recipe-policy", "hang=allow",
		"--write-policy", "write_file=allow",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
	code, output := runCrashedCodingLoop(t, args, "process_started_after", 1)
	if code != 42 {
		t.Fatalf("crash exit = %d, want 42", code)
	}
	taskID := taskIDFromOutput(t, output)

	// The durable state is the prepared process attempt: the effect may have
	// started, its result is unknown, and no evidence exists.
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("the interrupted process attempt must stay prepared:\n%s", rendered)
	}

	// Clean up the orphaned recipe process (the crash happened right after
	// start; the child outlives runstead because os.Exit skips group teardown).
	waitForFile(t, pidFile)
	if raw, readErr := os.ReadFile(pidFile); readErr == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil && pid > 0 {
			killProcessGroup(pid)
		}
	}

	// Resume must NOT re-run the process and must NOT turn it into success:
	// recovery class 4 escalates the prepared process attempt to
	// human_review_required and stops the task.
	dummyScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unused","evidence":[]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", dummyScript,
		"--recipes", catalog,
		"--recipe-policy", "hang=allow",
		"--write-policy", "write_file=allow",
		"--log-level", "error",
	}, &out, &errOut)
	if resumeCode != exitHumanReview {
		t.Fatalf("resume exit = %d, want %d (human review)\nstderr:\n%s", resumeCode, exitHumanReview, errOut.String())
	}
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "human_review_required") {
		t.Fatalf("the task must require human review:\n%s", rendered)
	}
	if !strings.Contains(rendered, "unreconcilable_effect") {
		t.Fatalf("the process attempt must carry the unreconcilable reason:\n%s", rendered)
	}
	// The process was never re-run: exactly one run_recipe attempt exists and
	// it was escalated to human_review_required by recovery; no completed
	// recipe attempt was ever fabricated.
	counts := toolAttemptCounts(t, stateDir, taskID)
	if counts["run_recipe/human_review_required"] != 1 || counts["run_recipe/completed"] != 0 {
		t.Fatalf("run_recipe attempts = %v, want exactly one human-review attempt", counts)
	}
	// The marker may or may not exist (the effect was in flight), but the
	// ORIGINAL child was never given a second chance to become evidence.
	if _, err := os.Stat(marker); err != nil && !os.IsNotExist(err) {
		t.Fatalf("marker stat: %v", err)
	}
}

// killProcessGroup kills the orphaned recipe process group after the crash
// (the crash exits runstead before the runner's tree-termination barrier can
// run, so the child outlives it; the test cleans it up deterministically).
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// waitForFile waits for a file to appear (bounded, for the recipe child's
// pid file after the crash).
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared", path)
}
