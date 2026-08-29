package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
)

// workUnitsFileFor writes the operator Work Unit definitions for the e2e
// scenarios and returns the path.
func workUnitsFileFor(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workunits.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWorkUnitRunSerialExecutesBothUnitsThenParent is the serial e2e: two
// operator-defined Work Units (second depends on the first) run in order
// through the real governed loop, then the parent task run completes. Each
// unit carries its own acceptance plan (file_exists) so completion is
// evidence-backed.
func TestWorkUnitRunSerialExecutesBothUnitsThenParent(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"list the workspace","dependencies":["wu-a"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	// 6 model turns: wu-a action+final, wu-b action+final, parent
	// action+final. Every final (including the parent's) must cite at least
	// one evidence entry (protocol contract), so the parent inspects b.txt
	// and cites obs-000003.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit a done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit b done","evidence":[{"evidence_id":"obs-000002","tool":"list_files"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000003","tool":"read_file"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "complete the parent task",
		"--workspace", workspace,
		"--scripted", script,
		"--workunits", workUnitsFile,
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)
	if code != agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("run exit = %d, want 0\nstderr:\n%s\nstdout:\n%s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("stdout missing completion:\n%s", out.String())
	}
}

// TestWorkUnitRecoveryWithNewConversation is the central recovery gate
// (issue #106): wu-a completes with evidence, wu-b is interrupted mid-run;
// resume with a brand-new provider conversation must NOT replay wu-a or its
// effects, must reconcile wu-b, and must reach parent completion.
func TestWorkUnitRecoveryWithNewConversation(t *testing.T) {
	workspace := crashWorkspace(t)
	// The parent must inspect a file it has NOT read before: the resumed
	// repeat guard covers past actions, so reading a.txt again would be
	// flagged as a repeated action.
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"finish the workspace scan","dependencies":["wu-a"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)

	// Run A: wu-a completes (read + final), wu-b starts and performs one
	// read, then the process is crashed after the tool result TX2.
	scriptA := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit a done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t,
		append(crashRunArgs(scriptA, workspace, stateDir), "--workunits", workUnitsFile,
			"--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms"),
		"tool_tx2_after", 2)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// Run B: a completely different provider conversation resumes. The
	// registry continues obs numbering from the persisted maximum (2), so
	// the resumed wu-b list_files becomes obs-000003 and the parent read of
	// b.txt becomes obs-000004. wu-b's final cites past evidence from the
	// recovery seed plus its own new observation.
	scriptB := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit b continued in a new session","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"read_file"},{"evidence_id":"obs-000003","tool":"list_files"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000004","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", scriptB, "--workunits", workUnitsFile,
		"--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s\nstdout:\n%s", resumeCode, resumeErr, resumeOut)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completion:\n%s", resumeOut)
	}
	// No replay: wu-a's read (obs-000001) ran exactly once; wu-b's
	// interrupted read (a.txt) ran once; the resumed wu-b added obs-000003
	// (list_files); the parent read of b.txt added obs-000004. tool_results
	// total must be 4.
	if got := countRowsFor(t, stateDir, taskID, "tool_results"); got != 4 {
		t.Fatalf("tool_results = %d, want 4 (no replay of completed effects)", got)
	}
	// Inspection exposes the durable Work Unit state without secrets.
	var out, errOut strings.Builder
	inspectCode := run(context.Background(), []string{
		"inspect", taskID, "--state-dir", stateDir,
	}, &out, &errOut)
	if inspectCode != exitSuccess {
		t.Fatalf("inspect exit = %d\n%s", inspectCode, errOut.String())
	}
	for _, want := range []string{"Work Units:", "wu-a [completed]", "wu-b [completed]", "obs-000001"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("inspect missing %q:\n%s", want, out.String())
		}
	}
	for _, forbidden := range []string{"sk-live", "Bearer ", "private prompt", "api_key"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("inspect leaks %q", forbidden)
		}
	}
}
