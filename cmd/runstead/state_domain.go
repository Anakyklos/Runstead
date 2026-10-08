package main

import (
	"fmt"
	"os"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/siwcstate"
)

type resolvedStateDomain struct {
	Dir  string
	SIWC *siwcstate.Domain
}

func stateDomainResolveExitCode(selection string) int {
	if selection == "siwc" {
		return exitUnavailable
	}
	return exitUsage
}

func resolveCommandStateDomain(selection, stateDir string, stateDirExplicit, legacyStateDirSet bool, identity *provider.Identity) (resolvedStateDomain, error) {
	if identity != nil && identity.WireContract == provider.WireResponsesSIWCV1 && selection != "siwc" {
		return resolvedStateDomain{}, fmt.Errorf("SIWC provider requires --state-domain siwc")
	}
	if selection == "" {
		if identity != nil && identity.WireContract == provider.WireResponsesSIWCV1 {
			return resolvedStateDomain{}, fmt.Errorf("SIWC provider requires --state-domain siwc")
		}
		dir, err := resolveStateDir(stateDir, legacyStateDirSet)
		if err != nil {
			return resolvedStateDomain{}, err
		}
		return resolvedStateDomain{Dir: dir}, nil
	}
	if selection != "siwc" {
		return resolvedStateDomain{}, fmt.Errorf("unsupported state domain %q", selection)
	}
	if identity != nil && identity.WireContract != provider.WireResponsesSIWCV1 {
		return resolvedStateDomain{}, fmt.Errorf("--state-domain siwc requires a responses_siwc_v1 provider")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return resolvedStateDomain{}, fmt.Errorf("cannot resolve SIWC locator home: %w", err)
	}
	locatorPath, err := siwcstate.DefaultLocatorPath(home, os.Getenv("XDG_STATE_HOME"))
	if err != nil {
		return resolvedStateDomain{}, err
	}
	var overrides []string
	if stateDirExplicit {
		overrides = append(overrides, stateDir)
	}
	if value, ok := os.LookupEnv(config.EnvStateDir); ok {
		overrides = append(overrides, value)
	}
	domain, err := siwcstate.Resolve(siwcstate.Options{LocatorPath: locatorPath, Identity: identity, Overrides: overrides})
	if err != nil {
		return resolvedStateDomain{}, err
	}
	return resolvedStateDomain{Dir: domain.Dir, SIWC: &domain}, nil
}
