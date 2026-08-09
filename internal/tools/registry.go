package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/recipe"
)

const (
	ToolReadFile   = "read_file"
	ToolListFiles  = "list_files"
	ToolSearchText = "search_text"
	ToolGitStatus  = "git_status"
	ToolGitDiff    = "git_diff"
	ToolWriteFile  = "write_file"
	ToolApplyPatch = "apply_patch"
	ToolRunRecipe  = "run_recipe"
)

type Limits struct {
	MaxReadBytes        int
	MaxListEntries      int
	MaxSearchMatches    int
	MaxSearchBytes      int
	MaxGitStdoutBytes   int
	MaxGitStderrBytes   int
	MaxWriteBytes       int
	MaxPatchBytes       int
	MaxPatchTargetBytes int
	MaxDiffBytes        int
	SearchTimeout       time.Duration
	GitTimeout          time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxReadBytes:        64 << 10,
		MaxListEntries:      256,
		MaxSearchMatches:    256,
		MaxSearchBytes:      128 << 10,
		MaxGitStdoutBytes:   64 << 10,
		MaxGitStderrBytes:   16 << 10,
		MaxWriteBytes:       256 << 10,
		MaxPatchBytes:       128 << 10,
		MaxPatchTargetBytes: 4 << 20,
		MaxDiffBytes:        8 << 10,
		SearchTimeout:       2 * time.Second,
		GitTimeout:          2 * time.Second,
	}
}

type CommandResult struct {
	Stdout      []byte
	Stderr      []byte
	StdoutBytes int64
	StderrBytes int64
	ExitCode    int
	Signal      string
	Err         error
}

type RGRunner func(context.Context, string, []string, string) CommandResult
type GitRunner func(context.Context, []string, string) CommandResult

// RecipeRunner executes one operator-declared recipe inside the canonical
// working directory with the allowlisted environment. It is a seam so tests
// can inject deterministic runners; the default is recipe.Run.
type RecipeRunner func(ctx context.Context, r recipe.Recipe, cwd string, env []string) recipe.Result

type Options struct {
	Workspace string
	Limits    Limits
	LookPath  func(string) (string, error)
	RGPath    string
	RunRG     RGRunner
	RunGit    GitRunner
	// Recipes is the operator-controlled recipe catalog. A nil catalog makes
	// run_recipe fail closed with no_recipes_configured.
	Recipes *recipe.Catalog
	// RunRecipe overrides the recipe process runner (test seam). A nil value
	// uses recipe.Run.
	RunRecipe RecipeRunner
	// NextEvidenceSequence is the highest evidence sequence number already
	// allocated for this task (from persisted evidence). The next observation
	// receives NextEvidenceSequence+1, so a resumed process continues the
	// task-scoped evidence ID space instead of restarting at obs-000001 and
	// colliding with persisted evidence (issue #9). Zero starts at obs-000001.
	NextEvidenceSequence int
}

type Registry struct {
	workspace workspace
	limits    Limits
	rgPath    string
	runRG     RGRunner
	runGit    GitRunner
	recipes   *recipe.Catalog
	runRecipe RecipeRunner
	nextID    atomic.Uint64
}

func NewRegistry(options Options) (*Registry, error) {
	workspace, err := newWorkspace(options.Workspace)
	if err != nil {
		return nil, err
	}
	limits, err := normalizeLimits(options.Limits)
	if err != nil {
		return nil, err
	}
	rgPath := strings.TrimSpace(options.RGPath)
	if rgPath == "" {
		lookPath := options.LookPath
		if lookPath == nil && options.RunRG == nil {
			lookPath = exec.LookPath
		}
		if lookPath != nil {
			rgPath, _ = lookPath("rg")
		} else if options.RunRG != nil {
			rgPath = "rg"
		}
	}
	runRG := options.RunRG
	if runRG == nil {
		runRG = func(ctx context.Context, executable string, args []string, dir string) CommandResult {
			return runCommand(ctx, executable, args, dir, limits.MaxSearchBytes, limits.MaxGitStderrBytes, nil)
		}
	}
	runGit := options.RunGit
	if runGit == nil {
		runGit = func(ctx context.Context, args []string, dir string) CommandResult {
			return runCommand(ctx, "git", args, dir, limits.MaxGitStdoutBytes, limits.MaxGitStderrBytes, []string{
				"GIT_OPTIONAL_LOCKS=0",
				"GIT_TERMINAL_PROMPT=0",
				"GIT_PAGER=cat",
				"GIT_CONFIG_NOSYSTEM=1",
			})
		}
	}
	runRecipe := options.RunRecipe
	if runRecipe == nil {
		runRecipe = func(ctx context.Context, r recipe.Recipe, cwd string, env []string) recipe.Result {
			return recipe.Run(ctx, r, cwd, env)
		}
	}
	registry := &Registry{
		workspace: workspace,
		limits:    limits,
		rgPath:    rgPath,
		runRG:     runRG,
		runGit:    runGit,
		recipes:   options.Recipes,
		runRecipe: runRecipe,
	}
	if options.NextEvidenceSequence > 0 {
		registry.nextID.Store(uint64(options.NextEvidenceSequence))
	}
	return registry, nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	defaults := DefaultLimits()
	if limits.MaxReadBytes == 0 {
		limits.MaxReadBytes = defaults.MaxReadBytes
	}
	if limits.MaxListEntries == 0 {
		limits.MaxListEntries = defaults.MaxListEntries
	}
	if limits.MaxSearchMatches == 0 {
		limits.MaxSearchMatches = defaults.MaxSearchMatches
	}
	if limits.MaxSearchBytes == 0 {
		limits.MaxSearchBytes = defaults.MaxSearchBytes
	}
	if limits.MaxGitStdoutBytes == 0 {
		limits.MaxGitStdoutBytes = defaults.MaxGitStdoutBytes
	}
	if limits.MaxGitStderrBytes == 0 {
		limits.MaxGitStderrBytes = defaults.MaxGitStderrBytes
	}
	if limits.SearchTimeout == 0 {
		limits.SearchTimeout = defaults.SearchTimeout
	}
	if limits.GitTimeout == 0 {
		limits.GitTimeout = defaults.GitTimeout
	}
	if limits.MaxWriteBytes == 0 {
		limits.MaxWriteBytes = defaults.MaxWriteBytes
	}
	if limits.MaxPatchBytes == 0 {
		limits.MaxPatchBytes = defaults.MaxPatchBytes
	}
	if limits.MaxPatchTargetBytes == 0 {
		limits.MaxPatchTargetBytes = defaults.MaxPatchTargetBytes
	}
	if limits.MaxDiffBytes == 0 {
		limits.MaxDiffBytes = defaults.MaxDiffBytes
	}
	if limits.MaxReadBytes < 0 || limits.MaxListEntries < 0 || limits.MaxSearchMatches < 0 || limits.MaxSearchBytes < 0 || limits.MaxGitStdoutBytes < 0 || limits.MaxGitStderrBytes < 0 || limits.SearchTimeout < 0 || limits.GitTimeout < 0 ||
		limits.MaxWriteBytes < 0 || limits.MaxPatchBytes < 0 || limits.MaxPatchTargetBytes < 0 || limits.MaxDiffBytes < 0 {
		return Limits{}, errors.New("tool limits must not be negative")
	}
	return limits, nil
}

// ValidateArguments checks the typed argument shape of one tool proposal. For
// run_recipe it validates the shape only; recipe existence is enforced at
// execution with the typed unknown_recipe failure, so the protocol parser
// accepts the envelope and the attempt records the typed failure.
func (r *Registry) ValidateArguments(tool string, arguments protocol.Arguments) (bool, error) {
	if r == nil {
		return false, nil
	}
	switch tool {
	case ToolReadFile, ToolListFiles:
		if len(arguments) != 1 {
			return true, newFailure(FailureInvalidArguments)
		}
		value, err := stringArgument(arguments, "path")
		if err != nil {
			return true, err
		}
		_, failure := normalizeRelativePath(value)
		if failure != nil {
			return true, failure
		}
	case ToolSearchText:
		if len(arguments) != 2 {
			return true, newFailure(FailureInvalidArguments)
		}
		query, err := stringArgument(arguments, "query")
		if err != nil || query == "" || strings.IndexByte(query, 0) >= 0 {
			return true, newFailure(FailureInvalidArguments)
		}
		path, err := stringArgument(arguments, "path")
		if err != nil {
			return true, err
		}
		_, failure := normalizeRelativePath(path)
		if failure != nil {
			return true, failure
		}
	case ToolGitStatus, ToolGitDiff:
		if len(arguments) != 0 {
			return true, newFailure(FailureInvalidArguments)
		}
	case ToolWriteFile, ToolApplyPatch:
		if len(arguments) != 3 {
			return true, newFailure(FailureInvalidArguments)
		}
		path, err := stringArgument(arguments, "path")
		if err != nil {
			return true, err
		}
		if _, failure := normalizeRelativePath(path); failure != nil {
			return true, failure
		}
		expected, err := stringArgument(arguments, "expected_before_hash")
		if err != nil || !validBeforeHash(expected) {
			return true, newFailure(FailureInvalidArguments)
		}
		switch tool {
		case ToolWriteFile:
			content, contentFailure := stringArgumentAllowEmpty(arguments, "content")
			if contentFailure != nil || len(content) > r.limits.MaxWriteBytes {
				return true, newFailure(FailureInvalidArguments)
			}
		case ToolApplyPatch:
			patch, patchFailure := stringArgumentAllowEmpty(arguments, "patch")
			if patchFailure != nil || strings.TrimSpace(patch) == "" || len(patch) > r.limits.MaxPatchBytes {
				return true, newFailure(FailureInvalidArguments)
			}
		}
	case ToolRunRecipe:
		if len(arguments) != 1 {
			return true, newFailure(FailureInvalidArguments)
		}
		if _, err := stringArgument(arguments, "recipe"); err != nil {
			return true, err
		}
	default:
		return false, nil
	}
	return true, nil
}

// RecipeCatalog returns the operator-controlled recipe catalog (possibly nil).
func (r *Registry) RecipeCatalog() *recipe.Catalog {
	if r == nil {
		return nil
	}
	return r.recipes
}

// Recipe returns the recipe with the given id from the configured catalog.
func (r *Registry) Recipe(id string) (recipe.Recipe, bool) {
	if r == nil || r.recipes == nil {
		return recipe.Recipe{}, false
	}
	return r.recipes.Get(id)
}

// IsWriteTool reports whether the tool is a policy-gated write tool.
func (r *Registry) IsWriteTool(tool string) bool {
	return tool == ToolWriteFile || tool == ToolApplyPatch
}

// IsRecipeTool reports whether the tool is the policy-gated process recipe
// runner.
func (r *Registry) IsRecipeTool(tool string) bool {
	return tool == ToolRunRecipe
}

// IsPolicyGated reports whether the tool is gated by the control-plane policy
// before execution.
func (r *Registry) IsPolicyGated(tool string) bool {
	return r.IsWriteTool(tool) || r.IsRecipeTool(tool)
}

func stringArgument(arguments protocol.Arguments, name string) (string, *Failure) {
	raw, ok := arguments[name]
	if !ok {
		return "", newFailure(FailureInvalidArguments)
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", newFailure(FailureInvalidArguments)
	}
	return value, nil
}
