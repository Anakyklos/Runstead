package main

// Issue #14 (Part 7) live-smoke canary: the opt-in script
// (experiments/provider-live/run.sh) must (a) refuse to run without explicit
// opt-in, (b) survive a FAILING live attempt and still produce the sanitized
// record/inspection evidence, and (c) exit non-zero on missing required
// arguments. The canary fakes the runstead binary so no real endpoint
// traffic ever happens.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLiveBin writes a fake runstead binary: `run` always fails with exit 3
// after printing a task id, `inspect` renders the sanitized identity fields.
func fakeLiveBin(t *testing.T, invokedMarker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-runstead")
	content := `#!/usr/bin/env bash
if [[ "$1" == "run" ]]; then
  : > "` + invokedMarker + `"
  echo "task: fake-task-123" >&2
  echo "outcome: failed"
  exit 3
fi
if [[ "$1" == "inspect" ]]; then
  cat <<'EOF'
Provider identity:
  provider_id=fake-provider
  protocol_family=openai_compatible
  model=fake-model
  config_identity=provider.Config{fake-sanitized}
  profile_version=v1
  adapter_version=compatible-provider-v0.1
Provider attempts:
  exec-1 request=req-1 provider=fake-provider family=openai_compatible model=fake-model
    delivery_state=completed
EOF
  exit 0
fi
exit 0
`
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func liveScriptPath() string {
	return filepath.Join("..", "..", "experiments", "provider-live", "run.sh")
}

func runLiveSmoke(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	command := exec.Command("bash", append([]string{liveScriptPath()}, args...)...)
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run live smoke: %v", err)
	}
	return code, string(output)
}

func TestProviderLiveSmokeScriptOptInAndFailureEvidence(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	providersFile := writeProvidersFile(t, e2eFamilies[0], map[string]string{"fake-provider": "http://127.0.0.1:1/v1"})

	// 1) Without explicit opt-in the script refuses and never invokes the
	//    binary: zero live traffic.
	marker := filepath.Join(t.TempDir(), "invoked")
	binary := fakeLiveBin(t, marker)
	code, output := runLiveSmoke(t,
		[]string{"RUNSTEAD_BIN=" + binary},
		"--providers", providersFile, "--provider-id", "fake-provider",
		"--workspace", workspace, "--output", filepath.Join(t.TempDir(), "out"),
	)
	if code != 2 {
		t.Fatalf("opt-in refusal exit = %d, want 2\n%s", code, output)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("bin invoked without opt-in (live traffic without consent)")
	}

	// 2) Missing required arguments fail with a non-zero usage exit.
	code, output = runLiveSmoke(t, []string{"RUNSTEAD_LIVE_SMOKE=1", "RUNSTEAD_BIN=" + binary}, "--providers", providersFile)
	if code != 2 {
		t.Fatalf("missing-args exit = %d, want 2\n%s", code, output)
	}

	// 3) With opt-in, a FAILING live attempt still produces the sanitized
	//    record and inspection, and the run exit code is propagated.
	outputDir := filepath.Join(t.TempDir(), "evidence")
	code, output = runLiveSmoke(t,
		[]string{"RUNSTEAD_LIVE_SMOKE=1", "RUNSTEAD_BIN=" + binary},
		"--providers", providersFile, "--provider-id", "fake-provider",
		"--workspace", workspace, "--output", outputDir,
	)
	if code != 3 {
		t.Fatalf("live failure exit = %d, want 3 (propagated run exit)\n%s", code, output)
	}
	record, err := os.ReadFile(filepath.Join(outputDir, "record.txt"))
	if err != nil {
		t.Fatalf("record.txt missing: %v", err)
	}
	for _, want := range []string{
		"live smoke exit=3",
		"provider_id: fake-provider",
		"protocol_family: openai_compatible",
		"model: fake-model",
		"config_identity: provider.Config{fake-sanitized}",
		"adapter_version: compatible-provider-v0.1",
		"task: fake-task-123",
	} {
		if !strings.Contains(string(record), want) {
			t.Fatalf("record missing %q:\n%s", want, record)
		}
	}
	if _, err := os.Stat(filepath.Join(outputDir, "inspect.txt")); err != nil {
		t.Fatalf("inspect.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "trace.stderr.log")); err != nil {
		t.Fatalf("trace.stderr.log missing: %v", err)
	}
}
