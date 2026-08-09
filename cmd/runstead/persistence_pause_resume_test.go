package main

// Issue #13 review regression: a persistence failure at TX 2 (the result
// commit after a potentially executed effect) must NOT finalize the task
// terminally. The real CLI run pauses durably resumable
// (persistence_paused), and `runstead resume` reconciles the prepared write
// attempt from observable filesystem state without re-executing it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// TestRuntimePersistenceFaultHelper is the subprocess entry point for the
// injected-persistence-failure CLI regression: it installs the store fault
// seam (a real persistence ERROR, not a crash) and runs the real CLI run
// command.
func TestRuntimePersistenceFaultHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_PERSISTENCE_FAULT_HELPER") == "" {
		t.Skip("persistence fault helper")
	}
	point := os.Getenv("RUNSTEAD_PERSISTENCE_FAULT_POINT")
	after := 1
	if value := os.Getenv("RUNSTEAD_PERSISTENCE_FAULT_AFTER"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			after = parsed
		}
	}
	state.SetFaultPoint(func(name string) error {
		if name == point {
			after--
			if after == 0 {
				return fmt.Errorf("injected persistence fault at %s", name)
			}
		}
		return nil
	})
	args := strings.Split(os.Getenv("RUNSTEAD_PERSISTENCE_FAULT_ARGS"), "\x1f")
	code := run(context.Background(), args, os.Stdout, os.Stderr)
	os.Exit(code)
}

// TestRunPersistenceTX2FailurePausesAndResumeReconciles runs the real CLI
// with a deterministic TX 2 persistence failure injected after the write
// effect executed. The run must exit with the persistence_paused code (NOT a
// terminal failure), the task must stay resumable with the prepared attempt
// intact, and `runstead resume` must reconcile the write from the filesystem
// (never re-execute it) and complete the task.
func TestRunPersistenceTX2FailurePausesAndResumeReconciles(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("alpha\n"))

	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"a.txt","content":"bravo\n","expected_before_hash":"`+before+`"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unreachable","evidence":[]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	args := []string{
		"run", "--task", "Modify a.txt.",
		"--workspace", workspace,
		"--scripted", runScript,
		"--write-policy", "write_file=allow",
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}

	command := exec.Command(os.Args[0], "-test.run=TestRuntimePersistenceFaultHelper")
	command.Env = append(os.Environ(),
		"RUNSTEAD_PERSISTENCE_FAULT_HELPER=1",
		"RUNSTEAD_PERSISTENCE_FAULT_POINT=tool_tx2",
		"RUNSTEAD_PERSISTENCE_FAULT_AFTER=2",
		"RUNSTEAD_PERSISTENCE_FAULT_ARGS="+strings.Join(args, "\x1f"),
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("fault helper failed to run: %v\n%s", err, output)
	}
	code := 0
	if exitErr != nil {
		code = exitErr.ExitCode()
	}
	// The run exits with the typed persistence_paused code (36), NOT a
	// terminal failure code and NOT success.
	if code != agent.OutcomePersistencePaused.ExitCode() {
		t.Fatalf("run exit = %d, want %d (persistence_paused)\n%s", code, agent.OutcomePersistencePaused.ExitCode(), output)
	}
	if !strings.Contains(string(output), "outcome: persistence_paused") {
		t.Fatalf("run must print the typed persistence_paused outcome:\n%s", output)
	}
	taskID := taskIDFromOutput(t, string(output))

	// The write effect DID execute (TX 2 failed after it): the file holds the
	// new content, the attempt stays prepared, no citable write evidence
	// exists, and the task is resumable, not finalized.
	content, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil || string(content) != "bravo\n" {
		t.Fatalf("file content = %q, want bravo\\n (err %v)", content, err)
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("the write attempt must stay prepared:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Status: running") {
		t.Fatalf("the task must stay resumable:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Outcome: persistence_paused") {
		t.Fatalf("inspect must show the persistence pause:\n%s", rendered)
	}

	// Resume with a NEW provider conversation: recovery reconciles the write
	// from the current filesystem state and the task completes without ever
	// re-executing the write.
	resumeScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--write-policy", "write_file=allow",
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--log-level", "error",
	}, &out, &errOut)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want success (task must be resumable)\nstderr:\n%s", resumeCode, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", out.String())
	}

	// The effect was not duplicated: exactly one write attempt, reconciled
	// from the filesystem, and the file holds the single write.
	content, err = os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil || string(content) != "bravo\n" {
		t.Fatalf("file content = %q, want bravo\\n exactly once (err %v)", content, err)
	}
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "write_effect_completed") {
		t.Fatalf("inspect must show the reconciled write:\n%s", rendered)
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, attempt := range snapshot.ToolAttempts {
		counts[attempt.Tool+"/"+attempt.Status]++
	}
	if counts["write_file/prepared"] != 0 || counts["write_file/reconciled"] != 1 || counts["write_file/completed"] != 0 {
		t.Fatalf("write attempt projections = %v, want exactly one reconciled attempt", counts)
	}
}
