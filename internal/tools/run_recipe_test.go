package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

func recipeAction(id string) protocol.Action {
	return protocol.Action{
		Version: protocol.Current,
		Tool:    tools.ToolRunRecipe,
		Arguments: protocol.Arguments{
			"recipe": jsonString(id),
		},
	}
}

func testCatalog(t *testing.T, recipes ...recipe.Recipe) *recipe.Catalog {
	t.Helper()
	catalog, err := recipe.NewCatalog(recipes)
	if err != nil {
		t.Fatalf("recipe.NewCatalog() error = %v", err)
	}
	return catalog
}

// fakeRecipeRunner records the invocation and returns a canned result.
type fakeRecipeRunner struct {
	invoked bool
	recipe  recipe.Recipe
	cwd     string
	env     []string
	result  recipe.Result
}

func (f *fakeRecipeRunner) run(ctx context.Context, r recipe.Recipe, cwd string, env []string) recipe.Result {
	f.invoked = true
	f.recipe = r
	f.cwd = cwd
	f.env = env
	return f.result
}

func TestRunRecipeKnownExecutesExactArgv(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{result: recipe.Result{Started: true, ExitCode: 0}}
	catalog := testCatalog(t, recipe.Recipe{
		ID: "test", Executable: "go", Argv: []string{"test", "./..."},
		Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode},
	})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	observation := registry.Execute(context.Background(), recipeAction("test"))
	if !observation.Success {
		t.Fatalf("run_recipe failed: %+v", observation.Failure)
	}
	if !fake.invoked {
		t.Fatal("the recipe runner must be invoked")
	}
	if fake.recipe.ID != "test" || fake.recipe.Executable != "go" || len(fake.recipe.Argv) != 2 || fake.recipe.Argv[0] != "test" {
		t.Fatalf("runner received wrong recipe: %+v", fake.recipe)
	}
	if fake.cwd != workspace {
		t.Fatalf("runner cwd = %q, want %q", fake.cwd, workspace)
	}
	evidence, ok := observation.Data.(recipe.Evidence)
	if !ok {
		t.Fatalf("observation data = %T, want recipe.Evidence", observation.Data)
	}
	if evidence.RecipeID != "test" || evidence.Executable != "go" || evidence.ExitCode != 0 {
		t.Fatalf("evidence = %+v", evidence)
	}
	if evidence.NetworkIsolation != recipe.NetworkIsolationValue {
		t.Fatalf("network isolation = %q, want %q", evidence.NetworkIsolation, recipe.NetworkIsolationValue)
	}
}

func TestRunRecipeUnknownFailsWithoutStarting(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{}
	catalog := testCatalog(t, recipe.Recipe{ID: "test", Executable: "go", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	observation := registry.Execute(context.Background(), recipeAction("nope"))
	if observation.Success {
		t.Fatal("unknown recipe must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureUnknownRecipe {
		t.Fatalf("failure = %+v, want unknown_recipe", observation.Failure)
	}
	if fake.invoked {
		t.Fatal("unknown recipe must never start a process")
	}
}

func TestRunRecipeNoCatalogFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	observation := registry.Execute(context.Background(), recipeAction("test"))
	if observation.Success {
		t.Fatal("run_recipe without a catalog must fail closed")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureNoRecipes {
		t.Fatalf("failure = %+v, want no_recipes_configured", observation.Failure)
	}
	if fake.invoked {
		t.Fatal("no catalog must never start a process")
	}
}

func TestRunRecipeModelCannotSupplyArgv(t *testing.T) {
	workspace := t.TempDir()
	catalog := testCatalog(t, recipe.Recipe{ID: "test", Executable: "go", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	// Extra arguments (argv, command, shell) are rejected by validation.
	action := protocol.Action{
		Version: protocol.Current,
		Tool:    tools.ToolRunRecipe,
		Arguments: protocol.Arguments{
			"recipe":  jsonString("test"),
			"argv":    jsonString("rm -rf /"),
			"command": jsonString("anything"),
		},
	}
	if registered, err := registry.ValidateArguments(action.Tool, action.Arguments); registered && err == nil {
		t.Fatal("run_recipe must reject model-supplied argv/command arguments")
	}
}

func TestRunRecipeWorkingDirectoryBoundary(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	// Symlink escape: "sub" is swapped to a symlink pointing outside.
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fake := &fakeRecipeRunner{result: recipe.Result{Started: true, ExitCode: 0}}
	catalog := testCatalog(t, recipe.Recipe{
		ID: "escape", Executable: "go", WorkingDirectory: "link",
		Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode},
	})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	observation := registry.Execute(context.Background(), recipeAction("escape"))
	if observation.Success {
		t.Fatal("a symlink-escape working directory must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureSymlinkEscape {
		t.Fatalf("failure = %+v, want symlink_escape", observation.Failure)
	}
	if fake.invoked {
		t.Fatal("an escaping working directory must never start a process")
	}

	// Valid subdirectory: the runner receives the canonical absolute path.
	validFake := &fakeRecipeRunner{result: recipe.Result{Started: true, ExitCode: 0}}
	validCatalog := testCatalog(t, recipe.Recipe{
		ID: "ok", Executable: "go", WorkingDirectory: "sub",
		Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode},
	})
	validRegistry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: validCatalog, RunRecipe: validFake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	observation = validRegistry.Execute(context.Background(), recipeAction("ok"))
	if !observation.Success {
		t.Fatalf("valid cwd failed: %+v", observation.Failure)
	}
	if validFake.cwd != filepath.Join(workspace, "sub") {
		t.Fatalf("runner cwd = %q, want %q", validFake.cwd, filepath.Join(workspace, "sub"))
	}
	evidence := observation.Data.(recipe.Evidence)
	if evidence.WorkingDirectory != "sub" {
		t.Fatalf("evidence working directory = %q, want sub", evidence.WorkingDirectory)
	}
}

func TestRunRecipeMissingWorkingDirectoryFails(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{}
	catalog := testCatalog(t, recipe.Recipe{
		ID: "missing", Executable: "go", WorkingDirectory: "does-not-exist",
		Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode},
	})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	observation := registry.Execute(context.Background(), recipeAction("missing"))
	if observation.Success {
		t.Fatal("a missing working directory must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailurePathNotFound {
		t.Fatalf("failure = %+v, want path_not_found", observation.Failure)
	}
	if fake.invoked {
		t.Fatal("a missing working directory must never start a process")
	}
}

func TestRunRecipeCredentialEnvNotPassed(t *testing.T) {
	workspace := t.TempDir()
	os.Setenv("OPENAI_API_KEY", "sk-secret-fixture")
	os.Setenv("OMNIROUTE_API_KEY", "secret-fixture")
	os.Setenv("RUNSTEAD_ALLOWED_MARKER", "visible")
	defer os.Unsetenv("OPENAI_API_KEY")
	defer os.Unsetenv("OMNIROUTE_API_KEY")
	defer os.Unsetenv("RUNSTEAD_ALLOWED_MARKER")
	fake := &fakeRecipeRunner{result: recipe.Result{Started: true, ExitCode: 0}}
	// Credential names cannot even be declared in the recipe (catalog
	// validation refuses them); the recipe allowlists a benign marker and the
	// parent carries credential fixtures that must never be inherited.
	catalog := testCatalog(t, recipe.Recipe{
		ID: "test", Executable: "go", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode, recipe.CapabilityInheritEnvironment},
		AllowedEnvironment: []string{"RUNSTEAD_ALLOWED_MARKER"},
	})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	observation := registry.Execute(context.Background(), recipeAction("test"))
	if !observation.Success {
		t.Fatalf("run_recipe failed: %+v", observation.Failure)
	}
	for _, entry := range fake.env {
		if strings.HasPrefix(entry, "OPENAI_API_KEY=") || strings.HasPrefix(entry, "OMNIROUTE_API_KEY=") {
			t.Fatalf("credential env leaked to child: %v", fake.env)
		}
	}
	foundMarker := false
	for _, entry := range fake.env {
		if entry == "RUNSTEAD_ALLOWED_MARKER=visible" {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatalf("allowed marker must be inherited: %v", fake.env)
	}
	if len(fake.env) == 0 {
		t.Fatal("the child env must at least contain PATH and the recipe marker")
	}
}

func TestRunRecipeStartFailureCarriesNoEvidence(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{result: recipe.Result{Started: false, Err: recipe.StartError}}
	catalog := testCatalog(t, recipe.Recipe{ID: "test", Executable: "go", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	observation := registry.Execute(context.Background(), recipeAction("test"))
	if observation.Success {
		t.Fatal("start failure must not be a success")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureRecipeStart {
		t.Fatalf("failure = %+v, want recipe_start_failed", observation.Failure)
	}
	if observation.Data != nil {
		t.Fatalf("start failure must carry no evidence: %+v", observation.Data)
	}
}

func TestRunRecipeNonZeroExitIsCitableEvidence(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{result: recipe.Result{Started: true, ExitCode: 4, Stdout: []byte("failure output\n"), StdoutBytes: 16}}
	catalog := testCatalog(t, recipe.Recipe{ID: "test", Executable: "go", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	observation := registry.Execute(context.Background(), recipeAction("test"))
	if !observation.Success {
		t.Fatalf("a run that produced evidence must be a success observation: %+v", observation.Failure)
	}
	evidence := observation.Data.(recipe.Evidence)
	if evidence.ExitCode != 4 {
		t.Fatalf("exit code = %d, want 4", evidence.ExitCode)
	}
	if !strings.Contains(evidence.Stdout, "failure output") {
		t.Fatalf("stdout evidence = %q", evidence.Stdout)
	}
	if observation.Metadata.ExitCode != 4 {
		t.Fatalf("metadata exit code = %d, want 4", observation.Metadata.ExitCode)
	}
}

func TestRunRecipeValidationShape(t *testing.T) {
	workspace := t.TempDir()
	catalog := testCatalog(t, recipe.Recipe{ID: "test", Executable: "go", Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}})
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	cases := []protocol.Arguments{
		{},                         // missing recipe
		{"recipe": jsonString("")}, // empty recipe
		{"recipe": jsonString("test"), "extra": jsonString("x")}, // unknown field
	}
	for _, args := range cases {
		action := protocol.Action{Version: protocol.Current, Tool: tools.ToolRunRecipe, Arguments: args}
		if registered, err := registry.ValidateArguments(action.Tool, action.Arguments); registered && err == nil {
			t.Fatalf("arguments %v must be rejected", args)
		}
	}
}

// TestRunRecipeEffectiveCatalogSurfaceRejectsUnselected proves the M10
// recipe_ids blocker fix at the execution boundary (issue #54 review): a
// registry whose catalog was replaced by the Profile-selected effective
// surface rejects a recipe present in the ORIGINAL configured catalog but
// absent from the selection, with unknown_recipe, before any process starts.
func TestRunRecipeEffectiveCatalogSurfaceRejectsUnselected(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{result: recipe.Result{Started: true, ExitCode: 0}}
	full := testCatalog(t,
		recipe.Recipe{ID: "go-test", Executable: "go", Argv: []string{"test", "./..."}, Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}},
		recipe.Recipe{ID: "deploy", Executable: "sh", Argv: []string{"deploy.sh"}, Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}},
	)
	effective, err := full.Select([]string{"go-test"})
	if err != nil {
		t.Fatalf("full.Select() error = %v", err)
	}
	base, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: full, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	registry := base.WithRecipes(effective)

	// The unselected recipe exists in the configured catalog but must fail
	// exactly like an unknown recipe in the effective surface.
	observation := registry.Execute(context.Background(), recipeAction("deploy"))
	if observation.Success {
		t.Fatal("deploy must fail on the effective surface")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureUnknownRecipe {
		t.Fatalf("deploy failure = %+v, want unknown_recipe", observation.Failure)
	}
	if fake.invoked {
		t.Fatal("deploy must never start a process")
	}
	// The selected recipe stays available and executes normally.
	observation = registry.Execute(context.Background(), recipeAction("go-test"))
	if !observation.Success {
		t.Fatalf("go-test failed on the effective surface: %+v", observation.Failure)
	}
	if !fake.invoked || fake.recipe.ID != "go-test" {
		t.Fatalf("runner must have executed go-test, got %+v", fake.recipe)
	}
}

// TestRunRecipeRestrictedUnitViewKeepsEffectiveSurface proves Work Unit
// containment (issue #54 review requirement): a Restricted view derived from
// the effective registry inherits ONLY the Profile-selected recipe surface.
// A unit can never regain a recipe the parent task surface does not own.
func TestRunRecipeRestrictedUnitViewKeepsEffectiveSurface(t *testing.T) {
	workspace := t.TempDir()
	fake := &fakeRecipeRunner{result: recipe.Result{Started: true, ExitCode: 0}}
	full := testCatalog(t,
		recipe.Recipe{ID: "go-test", Executable: "go", Argv: []string{"test", "./..."}, Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}},
		recipe.Recipe{ID: "deploy", Executable: "sh", Argv: []string{"deploy.sh"}, Capabilities: []recipe.Capability{recipe.CapabilityExecuteRepoCode}},
	)
	effective, err := full.Select([]string{"go-test"})
	if err != nil {
		t.Fatalf("full.Select() error = %v", err)
	}
	base, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: full, RunRecipe: fake.run})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	unitRegistry, err := base.WithRecipes(effective).Restricted([]string{tools.ToolRunRecipe}, "")
	if err != nil {
		t.Fatalf("Restricted() error = %v", err)
	}
	observation := unitRegistry.Execute(context.Background(), recipeAction("deploy"))
	if observation.Success {
		t.Fatal("a Work Unit view must not resolve a recipe outside the parent surface")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureUnknownRecipe {
		t.Fatalf("unit deploy failure = %+v, want unknown_recipe", observation.Failure)
	}
	if fake.invoked {
		t.Fatal("the unselected recipe must never start through a Work Unit view")
	}
	observation = unitRegistry.Execute(context.Background(), recipeAction("go-test"))
	if !observation.Success {
		t.Fatalf("go-test failed through the unit view: %+v", observation.Failure)
	}
}
