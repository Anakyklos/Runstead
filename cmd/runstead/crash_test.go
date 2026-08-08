package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// Full-runtime crash-window tests: the real `runstead run` composition dies
// at a named persistence boundary in a subprocess, and the parent reopens
// the database with `runstead inspect` to prove which durable state survived.

// TestRuntimeCrashHelper is the subprocess entry point. It installs the
// store crash seam and runs the real CLI run command.
func TestRuntimeCrashHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_RUNTIME_CRASH_HELPER") == "" {
		t.Skip("runtime crash helper")
	}
	point := os.Getenv("RUNSTEAD_RUNTIME_CRASH_POINT")
	state.SetCrashPoint(func(name string) {
		if name == point {
			os.Exit(42)
		}
	})
	args := strings.Split(os.Getenv("RUNSTEAD_RUNTIME_CRASH_ARGS"), "\x1f")
	code := run(context.Background(), args, os.Stdout, os.Stderr)
	os.Exit(code)
}

func crashRunArgs(scriptPath, workspace, stateDir string) []string {
	return []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", scriptPath,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
}

// runCrashedRun spawns the helper and returns the exit code and stderr.
func runCrashedRun(t *testing.T, args []string, point string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestRuntimeCrashHelper")
	cmd.Env = append(os.Environ(),
		"RUNSTEAD_RUNTIME_CRASH_HELPER=1",
		"RUNSTEAD_RUNTIME_CRASH_POINT="+point,
		"RUNSTEAD_RUNTIME_CRASH_ARGS="+strings.Join(args, "\x1f"),
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("crash helper failed to run: %v\n%s", err, output)
	}
	code := 0
	if exitErr != nil {
		code = exitErr.ExitCode()
	}
	return code, string(output)
}

// inspectRendered renders one task through the store renderer.
func inspectRendered(t *testing.T, stateDir, taskID string) string {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("reopen state: %v", err)
	}
	defer store.Close()
	var out bytes.Buffer
	if err := store.RenderInspect(context.Background(), &out, taskID); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	return out.String()
}

func taskIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "task: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "task: "))
		}
	}
	t.Fatalf("crash helper did not print a task id:\n%s", output)
	return ""
}

// TestCrashAfterProviderTX1LeavesDurableIntent runs the real loop and kills
// the process after the provider TX 1 commit. The database must retain the
// prepared provider attempt and the inspect command must render it.
func TestCrashAfterProviderTX1LeavesDurableIntent(t *testing.T) {
	workspace := t.TempDir()
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRun(t, crashRunArgs(script, workspace, stateDir), "provider_tx1_after")
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// `runstead inspect` must work for the interrupted task and render the
	// prepared provider attempt.
	var out, errOut strings.Builder
	inspectCode := run(context.Background(), []string{"inspect", taskID, "--state-dir", stateDir}, &out, &errOut)
	if inspectCode != exitSuccess {
		t.Fatalf("inspect exit = %d\nstderr:\n%s", inspectCode, errOut.String())
	}
	if !strings.Contains(out.String(), "status=prepared") {
		t.Fatalf("inspect must render the prepared provider attempt:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "uncertain=prepared: the upstream may have been reached") {
		t.Fatalf("inspect must flag the prepared provider attempt:\n%s", out.String())
	}
	// The run stopped mid-turn: no terminal outcome is recorded yet.
	if !strings.Contains(out.String(), "Status: running") {
		t.Fatalf("inspect must show the interrupted task as running:\n%s", out.String())
	}
}

// TestCrashBeforeToolTX2LeavesPreparedEffectReturned kills the process after
// the tool effect returned but before the result commit: the attempt must
// stay prepared and no citable evidence may exist.
func TestCrashBeforeToolTX2LeavesPreparedEffectReturned(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRun(t, crashRunArgs(script, workspace, stateDir), "tool_tx2_before")
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "tool=read_file action=") || !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("inspect must show the prepared tool attempt:\n%s", rendered)
	}
	if strings.Contains(rendered, "evidence=obs-000001") {
		t.Fatalf("no citable evidence may exist before TX 2:\n%s", rendered)
	}
}

// TestCrashMidProviderEffect is the subprocess for the mid-effect window: the
// provider call is provably in flight when the process dies. The durable
// state is the TX 1 'prepared' row: ambiguous by design, never proof that
// the upstream was not reached.
func TestCrashMidProviderEffect(t *testing.T) {
	stateDir := os.Getenv("RUNSTEAD_MID_EFFECT_STATE_DIR")
	if stateDir == "" {
		t.Skip("mid-effect subprocess")
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{TaskID: "task-mid", Objective: "o", Workspace: "/ws", Model: "scripted"}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-mid"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}

	config := governor.DefaultInstantConfig("policy-mid", "scripted", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Millisecond
	accountGovernor, err := governor.New(config, governor.Options{Persistence: store})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	blocking := provider.NewBlockingFake()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The blocking fake waits on ctx.Done, so the effect is "running"
		// until this goroutine dies with the process.
		accountGovernor.Execute(ctx, governor.AttemptRequest{
			TaskID:          "task-mid",
			ClientRequestID: "task-mid-0001",
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, blocking, nil)
	}()
	waitForProviderCall(t, blocking)
	os.Exit(42)
	<-done
}

func waitForProviderCall(t *testing.T, fake *provider.Fake) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fake.Attempts() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("provider call never started")
}

// TestCrashMidProviderEffectParent spawns the mid-effect subprocess and
// asserts the durable evidence afterwards.
func TestCrashMidProviderEffectParent(t *testing.T) {
	stateDir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashMidProviderEffect")
	cmd.Env = append(os.Environ(), "RUNSTEAD_MID_EFFECT_STATE_DIR="+stateDir)
	if err := cmd.Run(); err == nil {
		t.Fatal("mid-effect subprocess must die")
	}

	rendered := inspectRendered(t, stateDir, "task-mid")
	if !strings.Contains(rendered, "Status: running") {
		t.Fatalf("task must remain running after the mid-effect crash:\n%s", rendered)
	}
	// The mid-effect crash leaves the provider attempt 'prepared': the
	// ambiguous state that must never be reinterpreted as success or
	// failure, and never auto-retried.
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("provider attempt must stay prepared:\n%s", rendered)
	}
	if !strings.Contains(rendered, "uncertain=prepared: the upstream may have been reached") {
		t.Fatalf("inspect must flag the ambiguous prepared attempt:\n%s", rendered)
	}
}
