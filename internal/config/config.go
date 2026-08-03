package config

import (
	"fmt"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/policy"
)

const (
	EnvWorkspace = "RUNSTEAD_WORKSPACE"
	EnvLogLevel  = "RUNSTEAD_LOG_LEVEL"

	DefaultWorkspace = "."
	DefaultLogLevel  = "info"
)

type Config struct {
	Workspace     string
	LogLevel      string
	AccountPolicy *policy.Config
}

type Overrides struct {
	Workspace        string
	WorkspaceSet     bool
	LogLevel         string
	LogLevelSet      bool
	AccountPolicy    *policy.Config
	AccountPolicySet bool
}

type LookupEnv func(string) (string, bool)

func Resolve(overrides Overrides, lookupEnv LookupEnv) (Config, error) {
	workspace := DefaultWorkspace
	logLevel := DefaultLogLevel
	var accountPolicy *policy.Config
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
	if overrides.AccountPolicySet {
		if overrides.AccountPolicy == nil {
			return Config{}, fmt.Errorf("account policy must be provided when configured")
		}
		copy := *overrides.AccountPolicy
		if err := copy.Validate(); err != nil {
			return Config{}, fmt.Errorf("invalid account policy: %w", err)
		}
		accountPolicy = &copy
	}

	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return Config{}, fmt.Errorf("workspace must not be empty")
	}
	logLevel = strings.ToLower(strings.TrimSpace(logLevel))
	if !validLogLevel(logLevel) {
		return Config{}, fmt.Errorf("log level %q must be one of debug, info, warn or error", logLevel)
	}

	return Config{Workspace: workspace, LogLevel: logLevel, AccountPolicy: accountPolicy}, nil
}

func validLogLevel(value string) bool {
	switch value {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
