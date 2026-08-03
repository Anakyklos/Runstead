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
)

const (
	ToolReadFile   = "read_file"
	ToolListFiles  = "list_files"
	ToolSearchText = "search_text"
	ToolGitStatus  = "git_status"
	ToolGitDiff    = "git_diff"
)

type Limits struct {
	MaxReadBytes      int
	MaxListEntries    int
	MaxSearchMatches  int
	MaxSearchBytes    int
	MaxGitStdoutBytes int
	MaxGitStderrBytes int
	SearchTimeout     time.Duration
	GitTimeout        time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxReadBytes:      64 << 10,
		MaxListEntries:    256,
		MaxSearchMatches:  256,
		MaxSearchBytes:    128 << 10,
		MaxGitStdoutBytes: 64 << 10,
		MaxGitStderrBytes: 16 << 10,
		SearchTimeout:     2 * time.Second,
		GitTimeout:        2 * time.Second,
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

type Options struct {
	Workspace string
	Limits    Limits
	LookPath  func(string) (string, error)
	RGPath    string
	RunRG     RGRunner
	RunGit    GitRunner
}

type Registry struct {
	workspace workspace
	limits    Limits
	rgPath    string
	runRG     RGRunner
	runGit    GitRunner
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
	return &Registry{workspace: workspace, limits: limits, rgPath: rgPath, runRG: runRG, runGit: runGit}, nil
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
	if limits.MaxReadBytes < 0 || limits.MaxListEntries < 0 || limits.MaxSearchMatches < 0 || limits.MaxSearchBytes < 0 || limits.MaxGitStdoutBytes < 0 || limits.MaxGitStderrBytes < 0 || limits.SearchTimeout < 0 || limits.GitTimeout < 0 {
		return Limits{}, errors.New("tool limits must not be negative")
	}
	return limits, nil
}

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
	default:
		return false, nil
	}
	return true, nil
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
