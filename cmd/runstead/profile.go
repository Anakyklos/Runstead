package main

// Issue #91: the composition root persists the configured operational bounds
// of the selected provider endpoint (capability-profile MaxRequestBytes /
// MaxResponseBytes) as CONFIGURED provenance. This is metadata only: it
// feeds future governor inputs and inspection, never admission, retry or
// fallback authority. A replayed configured bound that would undo an
// observed tightening or authoritative acceptance is a benign no-op;
// anything else that prevents a durable profile fails the invocation
// closed.

import (
	"context"
	"errors"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// syncOperationalConfiguredBounds records the operator-declared capability
// bounds for one selected provider identity and persists the profile. It
// returns the resulting profile (or nil when nothing was recorded).
func syncOperationalConfiguredBounds(ctx context.Context, store *state.Store, resolved *provider.Resolved, identity provider.Identity) (*provider.OperationalProfile, error) {
	if resolved == nil {
		return nil, nil
	}
	profile := provider.NewOperationalProfile(identity)
	applied := false
	for _, bound := range []struct {
		field provider.ProfileField
		value int
	}{
		{provider.FieldMaxRequestBytes, resolved.Profile.MaxRequestBytes},
		{provider.FieldMaxResponseBytes, resolved.Profile.MaxResponseBytes},
	} {
		if bound.value <= 0 {
			continue
		}
		next, err := profile.ApplyConfigured(bound.field, int64(bound.value), nil)
		if errors.Is(err, provider.ErrProfileReplayUndo) {
			// Same unchanged identity replayed: an observed tightening or
			// authoritative acceptance stays authoritative and is never
			// undone by the same declaration.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("operational profile: %v", err)
		}
		profile = next
		applied = true
	}
	if !applied {
		return nil, nil
	}
	if err := store.SaveOperationalProfile(ctx, profile); err != nil {
		return nil, fmt.Errorf("operational profile unavailable: %v", err)
	}
	return profile, nil
}
