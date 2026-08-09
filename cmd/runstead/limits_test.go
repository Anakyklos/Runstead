package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/config"
)

func TestRunScriptedZeroCorrectionsExitsImmediately(t *testing.T) {
	workspace := t.TempDir()
	script := writeScript(t, `I refuse to use the protocol.`)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--max-corrections", "0",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeCorrectionsExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeCorrectionsExhausted.ExitCode(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "corrections=0") || !strings.Contains(errOut.String(), "attempts=1") {
		t.Fatalf("run must stop after the first attempt without any correction turn:\n%s", errOut.String())
	}
}

func TestRunScriptedZeroRepeatedActionsExitsImmediately(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
	)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--max-repeated-actions", "0",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeRepeatedAction.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeRepeatedAction.ExitCode(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "observations=1") {
		t.Fatalf("run must execute the tool exactly once:\n%s", errOut.String())
	}
}

func TestRunLimitsFromEnvironment(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
	)
	t.Setenv(config.EnvMaxSteps, "1")
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeStepsExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d (RUNSTEAD_MAX_STEPS=1)\nstderr:\n%s", code, agent.OutcomeStepsExhausted.ExitCode(), errOut.String())
	}
}

func TestRunEnvironmentZeroCorrections(t *testing.T) {
	workspace := t.TempDir()
	script := writeScript(t, `I refuse to use the protocol.`)
	t.Setenv(config.EnvMaxCorrections, "0")
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeCorrectionsExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d (RUNSTEAD_MAX_CORRECTIONS=0)\nstderr:\n%s", code, agent.OutcomeCorrectionsExhausted.ExitCode(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "attempts=1") {
		t.Fatalf("environment zero corrections must stop after one attempt:\n%s", errOut.String())
	}
}

func TestRunFlagOverridesEnvironmentLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Three turns need at least three steps; the env value alone would stop
	// after one, so a completed run proves flags take precedence.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"list_files"}]}</runstead_final>`,
	)
	t.Setenv(config.EnvMaxSteps, "1")
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--min-start-interval", "1ms",
		"--max-steps", "24",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d (flag must override env)\nstderr:\n%s", code, agent.OutcomeCompleted.ExitCode(), errOut.String())
	}
}

func TestRunEnvironmentTimeBudget(t *testing.T) {
	workspace := t.TempDir()
	script := writeScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"too late","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`)
	t.Setenv(config.EnvTimeBudget, "1ns")
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeTimeBudgetExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d (RUNSTEAD_TIME_BUDGET=1ns)\nstderr:\n%s", code, agent.OutcomeTimeBudgetExhausted.ExitCode(), errOut.String())
	}
}

func TestRunEnvironmentLimitValidation(t *testing.T) {
	workspace := t.TempDir()
	script := writeScript(t, `whatever`)
	t.Setenv(config.EnvMaxSteps, "not-a-number")
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)

	if code != exitUsage {
		t.Fatalf("run exit code = %d, want %d for an invalid environment value\nstderr:\n%s", code, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), config.EnvMaxSteps) {
		t.Fatalf("run diagnostic should name the invalid variable:\n%s", errOut.String())
	}
}
