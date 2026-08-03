package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRootHelp(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), []string{"--help"}, &out, &errOut)

	if code != exitSuccess {
		t.Fatalf("help exit code = %d, want %d", code, exitSuccess)
	}
	for _, want := range []string{"Runstead", "run", "inspect", "resume", "flags > environment > defaults"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("root help does not contain %q:\n%s", want, out.String())
		}
	}
	if errOut.Len() != 0 {
		t.Fatalf("help wrote diagnostics: %s", errOut.String())
	}
}

func TestCommandHelp(t *testing.T) {
	for _, command := range []string{"run", "inspect", "resume"} {
		t.Run(command, func(t *testing.T) {
			var out, errOut bytes.Buffer

			code := run(context.Background(), []string{command, "--help"}, &out, &errOut)

			if code != exitSuccess {
				t.Fatalf("%s help exit code = %d, want %d", command, code, exitSuccess)
			}
			if !strings.Contains(out.String(), "Usage: runstead "+command) {
				t.Errorf("%s help missing usage:\n%s", command, out.String())
			}
			if !strings.Contains(out.String(), "not implemented") && command != "run" {
				t.Errorf("%s help should identify the placeholder:\n%s", command, out.String())
			}
			if errOut.Len() != 0 {
				t.Fatalf("%s help wrote diagnostics: %s", command, errOut.String())
			}
		})
	}
}

func TestUnknownCommandFailsWithDiagnostic(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), []string{"unknown"}, &out, &errOut)

	if code != exitUsage {
		t.Fatalf("unknown command exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), `unknown command "unknown"`) {
		t.Fatalf("unknown command diagnostic = %q", errOut.String())
	}
}

func TestInvalidRunFlagFailsWithDiagnostic(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), []string{"run", "--not-a-flag"}, &out, &errOut)

	if code != exitUsage {
		t.Fatalf("invalid flag exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "run: invalid flags") {
		t.Fatalf("invalid flag diagnostic = %q", errOut.String())
	}
}

func TestUnavailableCommandsFailClearly(t *testing.T) {
	for _, command := range []string{"run", "inspect", "resume"} {
		t.Run(command, func(t *testing.T) {
			var out, errOut bytes.Buffer

			code := run(context.Background(), []string{command}, &out, &errOut)

			if code != exitUnavailable {
				t.Fatalf("%s exit code = %d, want %d", command, code, exitUnavailable)
			}
			if !strings.Contains(errOut.String(), "not implemented") {
				t.Fatalf("%s diagnostic = %q", command, errOut.String())
			}
		})
	}
}

func TestCanceledRunReturnsInterruptedCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer

	code := run(ctx, []string{"run"}, &out, &errOut)

	if code != exitInterrupted {
		t.Fatalf("canceled run exit code = %d, want %d", code, exitInterrupted)
	}
	if !strings.Contains(errOut.String(), "canceled") {
		t.Fatalf("canceled run diagnostic = %q", errOut.String())
	}
}
