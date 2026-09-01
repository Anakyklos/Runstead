package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/state"
)

func TestProfileRunPersistsAndInspectsFrozenContract(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := writeCompositionProfile(t, `{"version":1,"profile_id":"audit","profile_version":"1.0.0","packages":[{"id":"repo.read","version":"1.0.0"}]}`)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"run", "--task", "Inspect the workspace.", "--workspace", workspace,
		"--scripted", script, "--profile", profile, "--acceptance", acceptanceFor(t, "a.txt"),
		"--state-dir", stateDir, "--min-start-interval", "1ms", "--log-level", "error",
	}, &out, &errOut)
	if code != agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("profile run exit code = %d, want %d\nstderr:\n%s\nstdout:\n%s", code, agent.OutcomeCompleted.ExitCode(), errOut.String(), out.String())
	}
	taskID := extractTaskID(t, errOut.String())
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.ExecutionContractHash == "" || snapshot.Task.ExecutionContractJSON == "" {
		t.Fatalf("frozen contract not persisted: %#v", snapshot.Task)
	}
	if !strings.HasPrefix(snapshot.Task.ExecutionContractHash, "sha256:") {
		t.Fatalf("contract hash = %q, want sha256 prefix", snapshot.Task.ExecutionContractHash)
	}
	var inspectOut, inspectErr bytes.Buffer
	if code := run(context.Background(), []string{"inspect", taskID, "--state-dir", stateDir}, &inspectOut, &inspectErr); code != exitSuccess {
		t.Fatalf("inspect exit code = %d\nstderr:\n%s", code, inspectErr.String())
	}
	for _, want := range []string{"Execution contract:", "profile: audit@1.0.0", "hash: sha256:", "repo.read@1.0.0", "tools: git_diff,git_status,list_files,read_file,search_text", "compatibility:"} {
		if !strings.Contains(inspectOut.String(), want) {
			t.Fatalf("inspect output missing %q:\n%s", want, inspectOut.String())
		}
	}
}

func TestProfileRunInspectAndResumeReusesFrozenContract(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := writeCompositionProfile(t, `{"version":1,"profile_id":"controlled-write","profile_version":"1.0.0","packages":[{"id":"repo.read","version":"1.0.0"},{"id":"repo.write","version":"1.0.0"}]}`)
	acceptance := acceptanceFor(t, "out.txt")
	stateDir := t.TempDir()
	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"created\n","expected_before_hash":"absent"}}</runstead_action>`,
	)
	var runOut, runErr bytes.Buffer
	code := run(context.Background(), []string{
		"run", "--task", "Create the approved file.", "--workspace", workspace,
		"--scripted", runScript, "--profile", profile, "--acceptance", acceptance,
		"--state-dir", stateDir, "--min-start-interval", "1ms", "--log-level", "error",
	}, &runOut, &runErr)
	if code != agent.OutcomeApprovalRequired.ExitCode() {
		t.Fatalf("profile run exit code = %d, want %d (approval_required)\nstderr:\n%s\nstdout:\n%s", code, agent.OutcomeApprovalRequired.ExitCode(), runErr.String(), runOut.String())
	}
	taskID := extractTaskID(t, runErr.String())
	pendingAction := pendingActionFromOutput(t, runOut.String())
	before := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(before, "profile: controlled-write@1.0.0") || !strings.Contains(before, "repo.write@1.0.0") {
		t.Fatalf("initial inspect must show the frozen profile:\n%s", before)
	}
	frozenHash := contractHashFromInspect(t, before)
	missingProfileScript := writeScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run"}</runstead_final>`)
	var missingOut, missingErr bytes.Buffer
	missingCode := run(context.Background(), []string{
		"resume", taskID, "--state-dir", stateDir, "--scripted", missingProfileScript,
		"--acceptance", acceptance, "--log-level", "error",
	}, &missingOut, &missingErr)
	if missingCode != exitUnavailable || !strings.Contains(missingErr.String(), "requires the original --profile") {
		t.Fatalf("resume without original profile = %d, want unavailable\nstderr:\n%s\nstdout:\n%s", missingCode, missingErr.String(), missingOut.String())
	}
	driftProfile := writeCompositionProfile(t, `{"version":1,"profile_id":"changed","profile_version":"1.0.0","packages":[{"id":"repo.read","version":"1.0.0"}]}`)
	driftScript := writeScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run"}</runstead_final>`)
	var driftOut, driftErr bytes.Buffer
	driftCode := run(context.Background(), []string{
		"resume", taskID, "--state-dir", stateDir, "--profile", driftProfile,
		"--scripted", driftScript, "--acceptance", acceptance, "--log-level", "error",
	}, &driftOut, &driftErr)
	if driftCode != exitUsage || !strings.Contains(driftErr.String(), "profile composition drift") {
		t.Fatalf("drifted profile resume = %d, want usage with explicit drift\nstderr:\n%s\nstdout:\n%s", driftCode, driftErr.String(), driftOut.String())
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.ResumeCount != 0 {
		t.Fatalf("drifted profile must fail before recovery, resume count = %d", snapshot.Task.ResumeCount)
	}
	if decideCode, decideOut := runDecide(t, stateDir, taskID, pendingAction, "approved", "operator reviewed"); decideCode != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", decideCode, decideOut)
	}

	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"created\n","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"created","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"write_file"}]}</runstead_final>`,
	)
	var resumeOut, resumeErr bytes.Buffer
	resumeCode := run(context.Background(), []string{
		"resume", taskID, "--state-dir", stateDir, "--profile", profile,
		"--scripted", resumeScript, "--acceptance", acceptance, "--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode != exitSuccess {
		t.Fatalf("profile resume exit = %d\nstderr:\n%s\nstdout:\n%s", resumeCode, resumeErr.String(), resumeOut.String())
	}
	if !strings.Contains(resumeOut.String(), "outcome: completed") {
		t.Fatalf("profile resume must complete:\n%s", resumeOut.String())
	}
	content, err := os.ReadFile(filepath.Join(workspace, "out.txt"))
	if err != nil || string(content) != "created\n" {
		t.Fatalf("approved write content = %q, err = %v", content, err)
	}
	after := inspectRendered(t, stateDir, taskID)
	if got := contractHashFromInspect(t, after); got != frozenHash {
		t.Fatalf("resume changed frozen contract hash from %q to %q\n%s", frozenHash, got, after)
	}
}

func TestInvalidProfileFailsBeforeTaskBootstrap(t *testing.T) {
	workspace := t.TempDir()
	profile := writeCompositionProfile(t, `{"version":1,"profile_id":"bad","profile_version":"1.0.0","packages":[{"id":"not-installed","version":"1.0.0"}]}`)
	script := writeScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"incomplete","summary":"not run"}</runstead_final>`)
	stateDir := t.TempDir()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"run", "--task", "should fail", "--workspace", workspace, "--scripted", script,
		"--profile", profile, "--state-dir", stateDir, "--log-level", "error",
	}, &out, &errOut)
	if code != exitUsage {
		t.Fatalf("invalid profile exit code = %d, want %d\nstderr:\n%s", code, exitUsage, errOut.String())
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.LoadRecoverySnapshot(context.Background(), "any-task"); !errors.Is(err, state.ErrTaskNotFound) {
		t.Fatalf("invalid profile left task state: error = %v, want ErrTaskNotFound", err)
	}
}

func writeCompositionProfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func contractHashFromInspect(t *testing.T, rendered string) string {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "hash: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "hash: "))
		}
	}
	t.Fatalf("inspect output has no frozen contract hash:\n%s", rendered)
	return ""
}
