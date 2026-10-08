package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/siwcstate"
)

type resolvedStateDomain struct {
	Dir              string
	SIWC             *siwcstate.Domain
	explicitStateDir string
	identity         *provider.Identity
}

// acquireSIWCDomainLock performs the bounded OS lock and revalidates the
// locator, manifest and database while the lock is held. Non-SIWC commands
// retain their historical state-directory behavior.
func acquireSIWCDomainLock(ctx context.Context, location resolvedStateDomain) (*siwcstate.Lock, error) {
	if location.SIWC == nil {
		return nil, nil
	}
	lock, err := siwcstate.AcquireLock(ctx, *location.SIWC)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, releaseSIWCDomainLockError(lock, fmt.Errorf("cannot revalidate SIWC locator home: %w", err))
	}
	locator, err := siwcstate.DefaultLocatorPath(home, os.Getenv("XDG_STATE_HOME"))
	if err != nil {
		return nil, releaseSIWCDomainLockError(lock, err)
	}
	var overrides []string
	if location.explicitStateDir != "" {
		overrides = append(overrides, location.explicitStateDir)
	}
	if value, ok := os.LookupEnv(config.EnvStateDir); ok {
		overrides = append(overrides, value)
	}
	revalidated, err := siwcstate.Resolve(siwcstate.Options{LocatorPath: locator, Identity: location.identity, Overrides: overrides})
	if err != nil || revalidated.Dir != location.SIWC.Dir || revalidated.Identity != location.SIWC.Identity {
		if err != nil {
			return nil, releaseSIWCDomainLockError(lock, err)
		}
		return nil, releaseSIWCDomainLockError(lock, siwcstate.ErrDomainUnavailable)
	}
	return lock, nil
}

func releaseSIWCDomainLock(lock *siwcstate.Lock, errOut io.Writer, command string) {
	if lock == nil {
		return
	}
	if err := lock.Release(); err != nil {
		fmt.Fprintf(errOut, "%s: SIWC domain lock release failed: %v\n", command, err)
	}
}

func releaseSIWCDomainLockError(lock *siwcstate.Lock, cause error) error {
	if releaseErr := lock.Release(); releaseErr != nil {
		return errors.Join(cause, fmt.Errorf("release SIWC domain lock: %w", releaseErr))
	}
	return cause
}

var errInvalidStateDomainSelector = errors.New("invalid state domain selector")

func stateDomainResolveExitCode(selection string) int {
	if selection == "siwc" {
		return exitUnavailable
	}
	return exitUsage
}

func resolveCommandStateDomain(selection string, selectionSet bool, stateDir string, stateDirExplicit, legacyStateDirSet bool, identity *provider.Identity) (resolvedStateDomain, error) {
	if !selectionSet {
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
		return resolvedStateDomain{}, errInvalidStateDomainSelector
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
	explicitStateDir := ""
	if stateDirExplicit {
		explicitStateDir = stateDir
	}
	return resolvedStateDomain{Dir: domain.Dir, SIWC: &domain, explicitStateDir: explicitStateDir, identity: identity}, nil
}

// stateDomainDiagnostic returns stable, sanitized error text for operator
// commands. It intentionally omits filesystem paths and decoder/SQLite detail.
func stateDomainDiagnostic(err error, selectorSet bool) string {
	switch {
	case errors.Is(err, siwcstate.ErrDivergentPath):
		return "SIWC state directory override diverges from registered domain"
	case errors.Is(err, siwcstate.ErrUnsafePath):
		return "unsafe SIWC state path"
	case errors.Is(err, siwcstate.ErrIdentityMismatch):
		return "SIWC provider identity does not match registered domain"
	case errors.Is(err, siwcstate.ErrDomainUnavailable):
		return "SIWC state domain unavailable"
	case errors.Is(err, errInvalidStateDomainSelector):
		return "invalid state domain selector"
	default:
		if selectorSet {
			return "invalid state domain selector"
		}
		return "invalid state dir"
	}
}
