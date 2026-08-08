package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// TOCTOU race tests (issue #10 review): an external mutation landing between
// the initial before-state validation and the filesystem effect must abort the
// write fail-closed without destroying the external state. The deterministic
// race seam (tools.SetWriteRaceHook) injects the mutation at
// "write_effect_revalidate", immediately before the effect-boundary
// revalidation.

func withRaceHook(t *testing.T, fn func(string)) {
	t.Helper()
	tools.SetWriteRaceHook(fn)
	t.Cleanup(func() { tools.SetWriteRaceHook(nil) })
}

func TestWriteFileRaceExternalModificationAbortsStale(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("original\n"))
	registry := mustRegistry(t, workspace)

	// An external process modifies the file after the before-hash check and
	// before the effect.
	withRaceHook(t, func(name string) {
		if name != "write_effect_revalidate" {
			return
		}
		if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("external-change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	observation := registry.Execute(context.Background(), writeAction("a.txt", "runstead-overwrite\n", before))
	if observation.Success {
		t.Fatal("a write racing an external modification must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureStaleState {
		t.Fatalf("failure = %+v, want stale_state", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "external-change\n" {
		t.Fatalf("external modification must be preserved, got %q", got)
	}
}

func TestWriteFileRaceConcurrentCreationAbortsStale(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)

	// An external process creates the originally-absent target after the
	// before-state check and before the effect: the write must not overwrite it.
	withRaceHook(t, func(name string) {
		if name != "write_effect_revalidate" {
			return
		}
		if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("created-by-other\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	observation := registry.Execute(context.Background(), writeAction("new.txt", "runstead\n", tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("a write racing concurrent creation of an absent target must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureStaleState {
		t.Fatalf("failure = %+v, want stale_state", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "new.txt"); got != "created-by-other\n" {
		t.Fatalf("the concurrently-created file must be preserved, got %q", got)
	}
}

func TestWriteFileRaceSymlinkSwapCannotEscapeWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sub", "target.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	registry := mustRegistry(t, workspace)

	// An attacker swaps the parent directory component to a symlink pointing
	// outside the workspace after the before-state check and before the effect.
	withRaceHook(t, func(name string) {
		if name != "write_effect_revalidate" {
			return
		}
		realDir := filepath.Join(workspace, "sub-real")
		if err := os.Rename(filepath.Join(workspace, "sub"), realDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "sub")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	})
	observation := registry.Execute(context.Background(), writeAction("sub/target.txt", "escaped\n", before))
	if observation.Success {
		t.Fatal("a write racing a symlink swap must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureSymlinkEscape {
		t.Fatalf("failure = %+v, want symlink_escape", observation.Failure)
	}
	if got := mustReadFile(t, outside, "target.txt"); got != "outside\n" {
		t.Fatalf("the outside file must never be overwritten, got %q", got)
	}
}

func TestApplyPatchRaceExternalModificationAbortsStale(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("line1\nline2\n"))
	registry := mustRegistry(t, workspace)
	patch := "--- a.txt\n+++ a.txt\n@@ -1,2 +1,2 @@\n line1\n-line2\n+line2-edited\n"

	withRaceHook(t, func(name string) {
		if name != "write_effect_revalidate" {
			return
		}
		if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("external\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
	if observation.Success {
		t.Fatal("a patch racing an external modification must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureStaleState {
		t.Fatalf("failure = %+v, want stale_state", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "external\n" {
		t.Fatalf("external modification must be preserved, got %q", got)
	}
}

func TestWriteFileRaceHookOffByDefault(t *testing.T) {
	// Without a hook installed, the write succeeds normally.
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)
	observation := registry.Execute(context.Background(), writeAction("ok.txt", "fine\n", tools.AbsentBeforeHash))
	if !observation.Success {
		t.Fatalf("write without a race hook must succeed: %+v", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "ok.txt"); got != "fine\n" {
		t.Fatalf("content = %q", got)
	}
}
