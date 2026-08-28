package main

// Issue #93: the CLI composition layer turns governor-owned execution
// outcomes into DURABLE conservative operational profile evidence. It is the
// only place where adapter-typed evidence meets the SQLite boundary; the
// mapping itself stays provider-neutral in internal/provider/adaptive and
// the rule engine stays in internal/provider/operational_profile.go.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/adaptive"
	"github.com/RenyEnnos/Runstead/internal/provider/compat"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// profileObserver persists conservative evidence from every ADMITTED
// governed attempt of one provider identity. It is idempotent-safe across
// replays: the store boundary is monotonic and proposals the profile
// already covers more conservatively are dropped before reaching it.
type profileObserver struct {
	store    *state.Store
	identity provider.Identity
	now      func() time.Time
}

// newProfileObserver builds the durable learning observer. A nil clock
// defaults to the real wall clock.
func newProfileObserver(store *state.Store, identity provider.Identity, now func() time.Time) agent.AttemptObserver {
	if now == nil {
		now = time.Now
	}
	return &profileObserver{store: store, identity: identity, now: now}
}

// ObserveAttempt converts one admitted attempt outcome into conservative
// profile updates and persists them atomically through the monotonic store
// boundary. Errors are conservative stops (the agent executor returns them
// and never dispatches another physical attempt). Benign conditions (no
// evidence, evidence the profile already covers, or a concurrent stricter
// state) are no-ops.
func (o *profileObserver) ObserveAttempt(ctx context.Context, request governor.AttemptRequest, result governor.ExecutionResult) error {
	if o.store == nil {
		return nil
	}
	evidence := compat.Observation(result.Response, result.Err, o.now())
	// Without a valid structured reference this observation cannot be
	// audited, so it never becomes information (absence of evidence never
	// becomes evidence).
	evidence.EvidenceRef = provider.TaskEvidenceRef(request.TaskID)
	if !evidence.EvidenceRef.Valid() {
		return nil
	}
	updates := adaptive.Updates(evidence)
	if len(updates) == 0 {
		return nil
	}
	// Fail closed on an unreadable profile BEFORE proposing updates: a
	// corrupt operational profile must never receive writes (issue #93).
	profile, err := o.store.LoadOperationalProfile(ctx, o.identity)
	if err != nil {
		return fmt.Errorf("load operational profile before adaptive update: %v", err)
	}
	// Drop everything the effective profile already covers more
	// conservatively: only genuinely tightening proposals reach the store.
	updates = adaptive.ConservativeSubset(profile, updates, nil)
	if len(updates) == 0 {
		return nil
	}
	if _, err := o.store.ApplyOperationalProfileUpdates(ctx, o.identity, nil, updates); err != nil {
		if errors.Is(err, provider.ErrObservedNotConservative) {
			// A concurrent writer already made the profile at least as
			// conservative: the desired durable state exists, so this is a
			// benign no-op rather than a failure.
			return nil
		}
		return fmt.Errorf("persist adaptive operational profile update: %v", err)
	}
	return nil
}

// applyEffectiveProfileBounds makes the profile's effective bounds actually
// effective at the execution frontier: it returns a resolved provider whose
// capability profile carries the profile's effective request/output size
// bounds (configured value, or the tighter observed/authoritative value)
// for adapters that enforce size bounds at the request boundary (#91/#93).
// A corrupt or unreadable profile is a fail-closed error: execution must
// not proceed under unknown envelope state.
func applyEffectiveProfileBounds(ctx context.Context, store *state.Store, identity provider.Identity, resolved *provider.Resolved) (*provider.Resolved, error) {
	if resolved == nil || store == nil {
		return resolved, nil
	}
	profile, err := store.LoadOperationalProfile(ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("load operational profile for %s: %v", identity.ProviderID, err)
	}
	if profile == nil {
		return resolved, nil
	}
	effective := *resolved
	applied := false
	for _, bound := range []struct {
		field  provider.ProfileField
		target *int
	}{
		{provider.FieldMaxRequestBytes, &effective.Profile.MaxRequestBytes},
		{provider.FieldMaxResponseBytes, &effective.Profile.MaxResponseBytes},
	} {
		if value := profile.Effective(bound.field); value.Known() {
			// int64 -> int conversion guard: an oversized or negative
			// learned bound must never be applied (and cannot fit the
			// adapter's request-builder int on any build target).
			if value.Value <= 0 || int64(int(value.Value)) != value.Value {
				continue
			}
			*bound.target = int(value.Value)
			applied = true
		}
	}
	if !applied {
		return resolved, nil
	}
	return &effective, nil
}
