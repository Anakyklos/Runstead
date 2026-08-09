package recipe_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/recipe"
)

// TestMain doubles as the subprocess entry point for process-runner tests:
// when RUNSTEAD_PROCESS_HELPER_MODE is set, the helper logic runs directly
// and exits without the test framework (so no "PASS" output pollutes the
// captured streams). The recipe tests spawn it through os.Args[0]
// -test.run=TestRecipeRunnerHelper with the mode delivered through the
// recipe's allowlisted environment.
func TestMain(m *testing.M) {
	if mode := os.Getenv("RUNSTEAD_PROCESS_HELPER_MODE"); mode != "" {
		os.Exit(runProcessHelper(mode))
	}
	os.Exit(m.Run())
}

func runProcessHelper(mode string) int {
	switch mode {
	case "spawn-child":
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			return 1
		}
		if path := os.Getenv("RUNSTEAD_PROCESS_CHILD_PID_FILE"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		_ = child.Wait()
		return 0
	case "spawn-child-ignore-term":
		// The parent process spawns a child that ignores SIGTERM (self-exec
		// with mode "ignore-term"). When the process group receives SIGTERM the
		// parent dies, but the child must keep running until SIGKILL: this
		// proves the synchronous tree-termination barrier, because Run() must
		// not return while the SIGTERM-ignoring child is still alive.
		child := exec.Command(os.Args[0], "-test.run=TestRecipeRunnerHelper")
		child.Env = append(os.Environ(), "RUNSTEAD_PROCESS_HELPER_MODE=ignore-term")
		if err := child.Start(); err != nil {
			return 1
		}
		if path := os.Getenv("RUNSTEAD_PROCESS_CHILD_PID_FILE"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		_ = child.Wait()
		return 0
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(60 * time.Second)
		return 0
	case "output":
		outBytes, _ := strconv.Atoi(os.Getenv("RUNSTEAD_PROCESS_HELPER_OUT"))
		errBytes, _ := strconv.Atoi(os.Getenv("RUNSTEAD_PROCESS_HELPER_ERR"))
		for i := 0; i < outBytes; i++ {
			fmt.Fprint(os.Stdout, "o")
		}
		for i := 0; i < errBytes; i++ {
			fmt.Fprint(os.Stderr, "e")
		}
		return 0
	case "signal":
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
		return 0
	}
	return 1
}

// runnerRecipe builds a recipe whose executable is the test binary and whose
// argv triggers the helper with the given mode. The mode and any data files
// are delivered through the allowlisted environment, proving both that the
// allowlist works and that the runner passes exactly the declared argv.
func runnerRecipe(t *testing.T, id, mode string, allowed ...string) recipe.Recipe {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	env := append([]string{"RUNSTEAD_PROCESS_HELPER_MODE", "RUNSTEAD_PROCESS_CHILD_PID_FILE", "RUNSTEAD_PROCESS_HELPER_OUT", "RUNSTEAD_PROCESS_HELPER_ERR"}, allowed...)
	return recipe.Recipe{
		ID:                 id,
		Executable:         exe,
		Argv:               []string{"-test.run=TestRecipeRunnerHelper"},
		Capabilities:       []recipe.Capability{recipe.CapabilityExecuteRepoCode, recipe.CapabilityInheritEnvironment},
		AllowedEnvironment: env,
	}
}

func TestProcessRunnerExitZero(t *testing.T) {
	r, _ := runnerRecipe(t, "ok", "output").Normalize()
	r.OutputLimits.MaxStdoutBytes = 64 << 10
	r.OutputLimits.MaxStderrBytes = 64 << 10
	setEnv := map[string]string{"RUNSTEAD_PROCESS_HELPER_MODE": "output", "RUNSTEAD_PROCESS_HELPER_OUT": "4", "RUNSTEAD_PROCESS_HELPER_ERR": "3"}
	parent := envWith(setEnv)
	result := recipe.Run(context.Background(), r, t.TempDir(), recipe.BuildEnvironment(parent, r))
	if !result.Started {
		t.Fatalf("process must start: %+v", result)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if string(result.Stdout) != "oooo" || string(result.Stderr) != "eee" {
		t.Fatalf("stdout/stderr = %q/%q", result.Stdout, result.Stderr)
	}
	if result.StdoutBytes != 4 || result.StderrBytes != 3 {
		t.Fatalf("byte counts = %d/%d", result.StdoutBytes, result.StderrBytes)
	}
	if result.StdoutTruncated || result.StderrTruncated {
		t.Fatal("small output must not be truncated")
	}
}

func TestProcessRunnerExitNonZero(t *testing.T) {
	// A recipe whose executable exits non-zero preserves the real exit code;
	// exit code 0 is never conflated with success in the evidence.
	failRecipe := recipe.Recipe{ID: "fail", Executable: "/bin/false", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}}
	failRecipe, _ = failRecipe.Normalize()
	result := recipe.Run(context.Background(), failRecipe, t.TempDir(), nil)
	if !result.Started {
		t.Fatalf("process must start: %+v", result)
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", result.ExitCode)
	}
}

func TestProcessRunnerStartFailure(t *testing.T) {
	missing := recipe.Recipe{ID: "missing", Executable: "/nonexistent/definitely-missing-binary", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}}
	missing, _ = missing.Normalize()
	result := recipe.Run(context.Background(), missing, t.TempDir(), nil)
	if result.Started {
		t.Fatal("a missing executable must not start")
	}
	if result.Err == nil {
		t.Fatal("start failure must carry an error")
	}
}

func TestProcessRunnerTerminatingSignal(t *testing.T) {
	r, _ := runnerRecipe(t, "sig", "signal").Normalize()
	parent := envWith(map[string]string{"RUNSTEAD_PROCESS_HELPER_MODE": "signal"})
	result := recipe.Run(context.Background(), r, t.TempDir(), recipe.BuildEnvironment(parent, r))
	if !result.Started {
		t.Fatalf("process must start: %+v", result)
	}
	if result.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1 for signal termination", result.ExitCode)
	}
	if result.Signal != "killed" {
		t.Fatalf("signal = %q, want killed", result.Signal)
	}
}

func TestProcessRunnerTimeoutKillsFullTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	r, _ := runnerRecipe(t, "tree", "spawn-child", "RUNSTEAD_PROCESS_CHILD_PID_FILE").Normalize()
	r.TimeoutNanos = int64(3 * time.Second)
	parent := envWith(map[string]string{
		"RUNSTEAD_PROCESS_HELPER_MODE":    "spawn-child",
		"RUNSTEAD_PROCESS_CHILD_PID_FILE": pidFile,
	})
	result := recipe.Run(context.Background(), r, dir, recipe.BuildEnvironment(parent, r))
	if !result.Started {
		t.Fatalf("process must start: %+v", result)
	}
	if !result.TimedOut {
		t.Fatalf("timed_out = false, want true (result %+v)", result)
	}
	// The child process (sleep 60) must be dead: the whole process group was
	// terminated, not just the direct parent.
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file not written: %v", err)
	}
	childPID, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if childPID <= 0 {
		t.Fatalf("invalid child pid %q", raw)
	}
	waitForDead(t, childPID)
}

// TestProcessRunnerSynchronousTreeTerminationBarrier is the deterministic
// regression test for the reviewer blocker: a child that ignores SIGTERM must
// be dead before Run() returns. The direct process (parent) spawns a child
// that ignores SIGTERM; when the timeout fires the parent dies on SIGTERM but
// the child keeps running. Run() must not return until the whole group was
// SIGKILLed, so immediately after Run() returns the child is already dead.
func TestProcessRunnerSynchronousTreeTerminationBarrier(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	r, _ := runnerRecipe(t, "tree", "spawn-child-ignore-term", "RUNSTEAD_PROCESS_CHILD_PID_FILE").Normalize()
	r.TimeoutNanos = int64(500 * time.Millisecond)
	parent := envWith(map[string]string{
		"RUNSTEAD_PROCESS_HELPER_MODE":    "spawn-child-ignore-term",
		"RUNSTEAD_PROCESS_CHILD_PID_FILE": pidFile,
	})
	start := time.Now()
	result := recipe.Run(context.Background(), r, dir, recipe.BuildEnvironment(parent, r))
	if !result.Started {
		t.Fatalf("process must start: %+v", result)
	}
	if !result.TimedOut {
		t.Fatalf("timed_out = false, want true (result %+v)", result)
	}
	// The SIGTERM-ignoring child must already be dead the moment Run returns:
	// the barrier guarantees the whole group was terminated before TX2.
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file not written: %v", err)
	}
	childPID, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if childPID <= 0 {
		t.Fatalf("invalid child pid %q", raw)
	}
	waitForDead(t, childPID)
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("Run returned after %v, but the SIGTERM->grace->SIGKILL barrier requires >= 2s", elapsed)
	}
}

func TestProcessRunnerCancellationKillsFullTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	r, _ := runnerRecipe(t, "tree", "spawn-child", "RUNSTEAD_PROCESS_CHILD_PID_FILE").Normalize()
	r.TimeoutNanos = int64(30 * time.Second)
	parent := envWith(map[string]string{
		"RUNSTEAD_PROCESS_HELPER_MODE":    "spawn-child",
		"RUNSTEAD_PROCESS_CHILD_PID_FILE": pidFile,
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	result := recipe.Run(ctx, r, dir, recipe.BuildEnvironment(parent, r))
	if !result.Started {
		t.Fatalf("process must start: %+v", result)
	}
	if !result.Canceled {
		t.Fatalf("canceled = false, want true (result %+v)", result)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file not written: %v", err)
	}
	childPID, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	waitForDead(t, childPID)
}

func TestProcessRunnerBoundedOutputIndependent(t *testing.T) {
	r, _ := runnerRecipe(t, "out", "output", "RUNSTEAD_PROCESS_HELPER_OUT", "RUNSTEAD_PROCESS_HELPER_ERR").Normalize()
	r.OutputLimits.MaxStdoutBytes = 1024
	r.OutputLimits.MaxStderrBytes = 2048
	parent := envWith(map[string]string{
		"RUNSTEAD_PROCESS_HELPER_MODE": "output",
		"RUNSTEAD_PROCESS_HELPER_OUT":  "100000",
		"RUNSTEAD_PROCESS_HELPER_ERR":  "100000",
	})
	result := recipe.Run(context.Background(), r, t.TempDir(), recipe.BuildEnvironment(parent, r))
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if len(result.Stdout) != 1024 {
		t.Fatalf("retained stdout = %d, want 1024", len(result.Stdout))
	}
	if len(result.Stderr) != 2048 {
		t.Fatalf("retained stderr = %d, want 2048", len(result.Stderr))
	}
	if result.StdoutBytes != 100000 || result.StderrBytes != 100000 {
		t.Fatalf("observed bytes = %d/%d, want 100000/100000", result.StdoutBytes, result.StderrBytes)
	}
	if !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("truncation flags = %t/%t, want true/true", result.StdoutTruncated, result.StderrTruncated)
	}
}

func TestProcessRunnerShellMetacharactersStayLiteral(t *testing.T) {
	// The executable is /bin/echo; the argv contains shell metacharacters. A
	// shell would interpret them; the runner must pass them literally.
	echo := recipe.Recipe{
		ID:           "echo",
		Executable:   "/bin/echo",
		Argv:         []string{";", "&&", "|", "$(touch escape)", "`touch escape2`", ">", "<"},
		Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode},
	}
	echo, _ = echo.Normalize()
	dir := t.TempDir()
	result := recipe.Run(context.Background(), echo, dir, nil)
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	output := string(result.Stdout)
	for _, literal := range []string{";", "&&", "|", "$(touch escape)", "`touch escape2`", ">", "<"} {
		if !strings.Contains(output, literal) {
			t.Fatalf("shell metacharacter %q was not passed literally; output = %q", literal, output)
		}
	}
	// No files may have been created by shell interpretation.
	if _, err := os.Stat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("shell interpretation created a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape2")); !os.IsNotExist(err) {
		t.Fatalf("shell interpretation created a file: %v", err)
	}
}

func TestProcessRunnerNoRecipesConfiguredFailsClosed(t *testing.T) {
	// The default runner with a recipe that has no declared capabilities is
	// fine; the fail-closed check for missing catalog lives in the tools
	// layer. Here we just verify /bin/echo runs with no environment at all.
	echo := recipe.Recipe{ID: "echo", Executable: "/bin/echo", Argv: []string{"hi"}, Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}}
	echo, _ = echo.Normalize()
	result := recipe.Run(context.Background(), echo, t.TempDir(), nil)
	if result.ExitCode != 0 || string(result.Stdout) != "hi\n" {
		t.Fatalf("echo result = %+v", result)
	}
}

func envWith(values map[string]string) []string {
	env := append([]string(nil), os.Environ()...)
	for name, value := range values {
		env = append(env, name+"="+value)
	}
	return env
}

func waitForDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive after the group termination", pid)
}
