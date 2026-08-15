package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binPath is the quality-gates binary built once in TestMain so the CLI
// tests exercise the real exit codes end to end.
var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "quality-gates-cli-*")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(dir, "quality-gates")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		panic("build quality-gates binary: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func runCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), ee.ExitCode()
		}
		t.Fatal(err)
	}
	return out.String(), 0
}

func TestCLIGrowthExitCode(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "pkg", "big.go"), 5000)
	out, code := runCLI(t, "growth", "--root", root)
	if code != 1 {
		t.Fatalf("growth on an oversized tree exited %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "pkg/big.go") || !strings.Contains(out, "5000") || !strings.Contains(out, "1800") {
		t.Fatalf("growth output lacks file/observed/limit:\n%s", out)
	}
}

func TestCLIGrowthPassExitCode(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "pkg", "ok.go"), 10)
	out, code := runCLI(t, "growth", "--root", root)
	if code != 0 {
		t.Fatalf("growth on a clean tree exited %d, want 0\n%s", code, out)
	}
}

func TestCLIErrcheckExitCode(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": swallowModule})
	out, code := runCLI(t, "errcheck", "--root", root)
	if code != 1 {
		t.Fatalf("errcheck on a swallowed error exited %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "synth/synth.go") || !strings.Contains(out, "_ = failing()") {
		t.Fatalf("errcheck output lacks the finding:\n%s", out)
	}
}

func TestCLILiveConventionExitCode(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import "testing"

func TestLive(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_PKG") == "1" {
		t.Log("no skip")
	}
}
`)
	out, code := runCLI(t, "live-convention", "--root", root)
	if code != 1 {
		t.Fatalf("live-convention on a violating file exited %d, want 1\n%s", code, out)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	_, code := runCLI(t)
	if code != 2 {
		t.Fatalf("no arguments exited %d, want 2", code)
	}
	_, code = runCLI(t, "no-such-gate")
	if code != 2 {
		t.Fatalf("unknown gate exited %d, want 2", code)
	}
	_, code = runCLI(t, "growth", "--bogus-flag")
	if code != 2 {
		t.Fatalf("unknown flag exited %d, want 2", code)
	}
}
