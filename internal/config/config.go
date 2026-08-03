package config

import (
	"fmt"
	"strings"
)

const (
	EnvWorkspace = "RUNSTEAD_WORKSPACE"
	EnvLogLevel  = "RUNSTEAD_LOG_LEVEL"

	DefaultWorkspace = "."
	DefaultLogLevel  = "info"
)

type Config struct {
	Workspace string
	LogLevel  string
}

type Overrides struct {
	Workspace    string
	WorkspaceSet bool
	LogLevel     string
	LogLevelSet  bool
}

type LookupEnv func(string) (string, bool)

func Resolve(overrides Overrides, lookupEnv LookupEnv) (Config, error) {
	workspace := DefaultWorkspace
	logLevel := DefaultLogLevel
	if lookupEnv != nil {
		if value, ok := lookupEnv(EnvWorkspace); ok {
			workspace = value
		}
		if value, ok := lookupEnv(EnvLogLevel); ok {
			logLevel = value
		}
	}
	if overrides.WorkspaceSet {
		workspace = overrides.Workspace
	}
	if overrides.LogLevelSet {
		logLevel = overrides.LogLevel
	}

	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return Config{}, fmt.Errorf("workspace must not be empty")
	}
	logLevel = strings.ToLower(strings.TrimSpace(logLevel))
	if !validLogLevel(logLevel) {
		return Config{}, fmt.Errorf("log level %q must be one of debug, info, warn or error", logLevel)
	}

	return Config{Workspace: workspace, LogLevel: logLevel}, nil
}

func validLogLevel(value string) bool {
	switch value {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
