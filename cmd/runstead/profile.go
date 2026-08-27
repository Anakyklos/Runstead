package main

// Issue #91: the composition root records the configured operational bounds
// of the selected provider endpoint (capability-profile MaxRequestBytes /
// MaxResponseBytes) as CONFIGURED provenance. The update is applied
// MONOTONICALLY at the durable boundary (Store.ApplyOperationalProfileUpdates
// performs check-and-set inside one SQLite transaction): re-running the same
// configuration can never undo an observed tightening or authoritative
// acceptance for the same unchanged identity, and concurrent tasks cannot
// interleave a stale write that weakens conservative state. Metadata only:
// it feeds future governor inputs and inspection, never admission, retry or
// fallback authority.

import (
	"context"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// syncOperationalConfiguredBounds records the operator-declared capability
// bounds for one selected provider identity through the durable monotonic
// boundary. It returns the resulting profile (or nil when nothing was
// recorded).
func syncOperationalConfiguredBounds(ctx context.Context, store *state.Store, resolved *provider.Resolved, identity provider.Identity) (*provider.OperationalProfile, error) {
	if resolved == nil {
		return nil, nil
	}
	var updates []provider.ProfileUpdate
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
		updates = append(updates, provider.ProfileUpdate{
			Field:      bound.field,
			Value:      int64(bound.value),
			Provenance: provider.ProvenanceConfigured,
		})
	}
	if len(updates) == 0 {
		return nil, nil
	}
	profile, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, updates)
	if err != nil {
		return nil, fmt.Errorf("operational profile unavailable: %v", err)
	}
	return profile, nil
}
