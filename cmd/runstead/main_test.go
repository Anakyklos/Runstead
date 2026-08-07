package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
)

// withStateDir appends a fresh deterministic state directory so tests never
// touch the user's real state location.
func withStateDir(t *testing.T, args []string) []string {
	t.Helper()
	return append(append([]string{}, args...), "--state-dir", t.TempDir())
}
func TestRootHelp(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{"--help"}), &out, &errOut)

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
			if errOut.Len() != 0 {
				t.Fatalf("%s help wrote diagnostics: %s", command, errOut.String())
			}
		})
	}
}

func TestResumeHelpIdentifiesPlaceholder(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), []string{"resume", "--help"}, &out, &errOut)

	if code != exitSuccess {
		t.Fatalf("resume help exit code = %d, want %d", code, exitSuccess)
	}
	if !strings.Contains(out.String(), "placeholder") {
		t.Fatalf("resume help should identify the placeholder:\n%s", out.String())
	}
}

func TestUnknownCommandFailsWithDiagnostic(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{"unknown"}), &out, &errOut)

	if code != exitUsage {
		t.Fatalf("unknown command exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), `unknown command "unknown"`) {
		t.Fatalf("unknown command diagnostic = %q", errOut.String())
	}
}

func TestInvalidRunFlagFailsWithDiagnostic(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{"run", "--not-a-flag"}), &out, &errOut)

	if code != exitUsage {
		t.Fatalf("invalid flag exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "run: invalid flags") {
		t.Fatalf("invalid flag diagnostic = %q", errOut.String())
	}
}

func TestRunWithoutProviderFailsClearly(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{"run", "--task", "inspect the repo"}), &out, &errOut)

	if code != exitUnavailable {
		t.Fatalf("run exit code = %d, want %d", code, exitUnavailable)
	}
	if !strings.Contains(errOut.String(), "no provider configured") {
		t.Fatalf("run diagnostic = %q", errOut.String())
	}
}

func TestRunRequiresTask(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{"run"}), &out, &errOut)

	if code != exitUsage {
		t.Fatalf("run exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "task prompt is required") {
		t.Fatalf("run diagnostic = %q", errOut.String())
	}
}

func TestResumePlaceholderFailsClearly(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), []string{"resume"}, &out, &errOut)

	if code != exitUnavailable {
		t.Fatalf("resume exit code = %d, want %d", code, exitUnavailable)
	}
	if !strings.Contains(errOut.String(), "not implemented") {
		t.Fatalf("resume diagnostic = %q", errOut.String())
	}
}

func TestCanceledRunReturnsInterruptedCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer

	code := run(ctx, withStateDir(t, []string{"run", "--task", "inspect the repo"}), &out, &errOut)

	if code != agent.OutcomeCanceled.ExitCode() {
		t.Fatalf("canceled run exit code = %d, want %d", code, agent.OutcomeCanceled.ExitCode())
	}
	if !strings.Contains(errOut.String(), "canceled") {
		t.Fatalf("canceled run diagnostic = %q", errOut.String())
	}
}

func writeScript(t *testing.T, responses ...string) string {
	t.Helper()
	var builder strings.Builder
	for _, response := range responses {
		encoded, err := json.Marshal(struct {
			Text string `json:"text"`
		}{Text: response})
		if err != nil {
			t.Fatal(err)
		}
		builder.Write(encoded)
		builder.WriteString("\n")
	}
	path := filepath.Join(t.TempDir(), "script.jsonl")
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunScriptedCompletesGroundedTask(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Inspected the workspace.","evidence":["obs-000001","obs-000002"]}</runstead_final>`,
	)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeCompleted.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("stdout missing outcome: %s", out.String())
	}
	if !strings.Contains(out.String(), "evidence: obs-000001") || !strings.Contains(out.String(), "evidence: obs-000002") {
		t.Fatalf("stdout missing grounded evidence: %s", out.String())
	}
	trace := errOut.String()
	for _, want := range []string{"runstead: attempt", "runstead: action", "runstead: observation", "runstead: stop"} {
		if !strings.Contains(trace, want) {
			t.Errorf("stderr trace missing %q:\n%s", want, trace)
		}
	}
	if strings.Contains(trace, "alpha") {
		t.Error("trace leaked repository content")
	}
}

func TestRunScriptedFabricatedEvidenceExitCode(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fabricated.","evidence":["obs-999999"]}</runstead_final>`,
	)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeFinalNotGrounded.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeFinalNotGrounded.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: final_not_grounded") {
		t.Fatalf("stdout missing outcome: %s", out.String())
	}
}

func TestRunScriptedCorrectionsExhaustedExitCode(t *testing.T) {
	workspace := t.TempDir()
	script := writeScript(t,
		`I refuse to use the protocol.`,
		`I refuse to use the protocol.`,
		`I refuse to use the protocol.`,
	)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--max-corrections", "2",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeCorrectionsExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeCorrectionsExhausted.ExitCode(), errOut.String())
	}
}

func TestRunScriptedStepsExhaustedExitCode(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two actions exhaust a one-step budget before any final arrives.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
	)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--max-steps", "1",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeStepsExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeStepsExhausted.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: steps_exhausted") {
		t.Fatalf("stdout missing outcome: %s", out.String())
	}
}

func TestRunScriptedRepeatedActionExitCode(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The same action three times with one allowed repeated-action correction
	// must stop with repeated_action after a single tool execution.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
	)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--max-repeated-actions", "1",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeRepeatedAction.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeRepeatedAction.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: repeated_action") {
		t.Fatalf("stdout missing outcome: %s", out.String())
	}
}

func TestRunScriptedProviderBudgetExitCode(t *testing.T) {
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
		"--provider-budget", "1",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeProviderBudgetExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeProviderBudgetExhausted.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: provider_budget_exhausted") {
		t.Fatalf("stdout missing outcome: %s", out.String())
	}
}

func TestRunScriptedTimeBudgetExitCode(t *testing.T) {
	workspace := t.TempDir()
	// A 1ns time budget is already elapsed before the first turn, so the loop
	// stops deterministically with time_budget_exhausted before any provider
	// attempt or tool execution.
	script := writeScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"too late","evidence":["obs-000001"]}</runstead_final>`)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--time-budget", "1ns",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeTimeBudgetExhausted.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeTimeBudgetExhausted.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: time_budget_exhausted") {
		t.Fatalf("stdout missing outcome: %s", out.String())
	}
	if strings.Contains(errOut.String(), "runstead: attempt") {
		t.Fatalf("time budget should stop before any provider attempt:\n%s", errOut.String())
	}
}

func TestRunScriptedFinalIncompleteExitCode(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"incomplete","summary":"I could not answer fully.","evidence":["obs-000001"]}</runstead_final>`,
	)
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)

	if code != agent.OutcomeFinalIncomplete.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeFinalIncomplete.ExitCode(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: final_incomplete") {
		t.Fatalf("stdout missing outcome: %s", out.String())
	}
}
