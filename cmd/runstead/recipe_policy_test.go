package main

import (
	"context"
	"strings"
	"testing"
)

// TestRunRejectsRecipePolicyForUnknownRecipe proves a --recipe-policy mode
// referencing a recipe that is not in the catalog is rejected fail-closed.
func TestRunRejectsRecipePolicyForUnknownRecipe(t *testing.T) {
	workspace := t.TempDir()
	recipes := writeRecipesFile(t, echoRecipes())
	script := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Run a recipe.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", recipes,
		"--recipe-policy", "does-not-exist=allow",
		"--state-dir", t.TempDir(),
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitUsage {
		t.Fatalf("run exit = %d, want %d\nstderr:\n%s", code, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "unknown recipe") {
		t.Fatalf("run diagnostic = %q, want an unknown-recipe diagnostic", errOut.String())
	}
}
