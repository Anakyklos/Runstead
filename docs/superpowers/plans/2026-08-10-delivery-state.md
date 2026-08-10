# Provider Delivery State and End-to-End Replay Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Issue #38 from `origin/main` so provider delivery evidence is transport-derived, propagated, persisted with the existing provider attempt, recovery-safe, receipt-aware, inspectable, and deterministic without inventing upstream idempotency or weakening fail-closed behavior.

**Architecture:** Add a provider-neutral `DeliveryState` value to `ResponseMetadata`, observe OmniRoute delivery with a minimal `httptrace` tracker, and keep the raw observed value separate from the governor's conservative effective value when evidence is absent. The existing provider-attempt lifecycle, governor outcome policy, TX1/TX2 boundary, and #29 receipt accounting remain authoritative. Delivery state gates same-attempt replay/recovery safety; `RetryEligible` remains the policy result for a new governed logical attempt and never bypasses admission.

**Tech Stack:** Go 1.22.2, standard-library `net/http/httptrace`, `httptest`, `database/sql`, embedded SQLite migrations, existing governor/provider/state packages, deterministic fake clocks and transports.

## Global Constraints

- Base all implementation commits on `origin/main` at `16e253f` on `feat/issue-38-delivery-state`.
- Implement exactly `not_sent`, `sent_confirmed`, `sent_unconfirmed`, `response_started`, and `completed`; zero means invalid/unobserved and is never persisted as `sent_unconfirmed`.
- `httptrace.WroteRequest` is not evidence of upstream model dispatch and must never produce `sent_confirmed`.
- The real OmniRoute adapter may leave `sent_confirmed` unobservable; provider-neutral fakes must cover it.
- `client_request_id` is correlation only. Do not add or send `Idempotency-Key`.
- Attempt receipts remain authoritative and independent. Missing or malformed receipts remain fail-closed.
- TX1 persists provider intent before `Complete`; TX2 persists raw delivery observation, outcome, receipts, and accounting after `Complete`.
- Delivery state is orthogonal to the provider-attempt lifecycle and must not create a second lifecycle or table.
- `sent_unconfirmed` and interrupted `response_started` paths must be conservative and must not authorize automatic replay.
- `RetryEligible` remains the governor's existing new-attempt policy. Every retry is a fresh governor admission with fresh accounting and a new request identity where applicable.
- Do not implement #29 producer, activate #30, add adapter retry loops, account rotation/fallback, #39 telemetry expansion, #40 health probes, #43 fixture corpus, browser/Camoufox/M6, or allowance/governor semantic changes.

---

## File map

Create these focused files:

- `internal/provider/delivery_state.go`: provider-neutral state constants, validation, stable names, and replay-safety helper.
- `internal/provider/delivery_state_test.go`: state contract and validation tests.
- `internal/provider/omniroute/delivery.go`: narrow HTTP observation tracker using `httptrace`.
- `internal/provider/omniroute/delivery_test.go`: deterministic transport-boundary tests.
- `internal/governor/delivery_state_test.go`: delivery-to-outcome/retry/receipt integration tests.
- `internal/state/migrations/0011_provider_delivery_state.sql`: one-column schema migration.
- `docs/superpowers/plans/2026-08-10-delivery-state.md`: this plan.

Modify these existing files:

- `internal/provider/provider.go`: add raw delivery state to `ResponseMetadata`.
- `internal/provider/fake.go` and provider/agent test fixtures: make test transports declare explicit delivery evidence rather than relying on text or error wording.
- `internal/provider/omniroute/client.go`, `client_transport.go`, `classifier.go`: attach the tracker, preserve metadata on every error, map observations, and never infer upstream confirmation from `WroteRequest`.
- `internal/governor/types.go`, `execute.go`, `permit.go`, `circuit.go`, `persistence.go`: carry raw delivery evidence, apply conservative effective state, preserve new-attempt retry policy, and keep receipt gates fail-closed.
- `internal/state/provider_attempts.go`, `inspect.go`, `recovery.go`: persist/load/render delivery state alongside the existing attempt lifecycle and events.
- `internal/state/inspect_test.go`, `recovery_test.go`, `migrations_test.go`, provider-attempt test helpers: cover persistence/reconstruction and crash windows.
- `internal/recovery/recovery.go` and recovery tests: ensure ambiguous delivery is never reissued and prepared rows with no TX2 evidence remain unobserved.
- `docs/persistence.md` and the minimum provider/architecture documentation: document the implemented contract without marketing language.

---

## Task 1: Establish the provider-neutral delivery-state contract

**Files:**
- Create: `internal/provider/delivery_state.go`
- Create: `internal/provider/delivery_state_test.go`
- Modify: `internal/provider/provider.go`
- Modify: `internal/provider/fake.go`
- Test: `internal/provider/fake_test.go`

**Interfaces:**
- Produces `provider.DeliveryState`, `DeliveryState.Valid() bool`, `DeliveryState.String() string`, and `DeliveryState.ReplaySafe() bool`.
- Produces `provider.ResponseMetadata.DeliveryState` containing the raw observed value.
- `DeliveryState.ReplaySafe()` returns true only for the observed `DeliveryNotSent`; zero/unobserved and all other values return false.

- [ ] **Step 1: Write failing state-contract tests.**

Add table-driven tests with these exact assertions:

```go
func TestDeliveryStateNamesAndValidity(t *testing.T) {
    tests := []struct {
        name       string
        state      DeliveryState
        wantString string
        wantValid  bool
        wantReplay bool
    }{
        {"not sent", DeliveryNotSent, "not_sent", true, true},
        {"sent confirmed", DeliverySentConfirmed, "sent_confirmed", true, false},
        {"sent unconfirmed", DeliverySentUnconfirmed, "sent_unconfirmed", true, false},
        {"response started", DeliveryResponseStarted, "response_started", true, false},
        {"completed", DeliveryCompleted, "completed", true, false},
        {"unobserved", DeliveryState(0), "unobserved", false, false},
        {"unknown", DeliveryState(99), "unobserved", false, false},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            if got := tt.state.Valid(); got != tt.wantValid {
                t.Fatalf("Valid() = %t, want %t", got, tt.wantValid)
            }
            if got := tt.state.String(); got != tt.wantString {
                t.Fatalf("String() = %q, want %q", got, tt.wantString)
            }
            if got := tt.state.ReplaySafe(); got != tt.wantReplay {
                t.Fatalf("ReplaySafe() = %t, want %t", got, tt.wantReplay)
            }
        })
    }
}
```

Add a regression test proving a response body/error cannot change metadata:

```go
func TestResponseMetadataKeepsRawDeliveryState(t *testing.T) {
    response := Response{Text: "model says sent_confirmed", Metadata: ResponseMetadata{
        DeliveryState: DeliverySentUnconfirmed,
    }}
    if response.Metadata.DeliveryState != DeliverySentUnconfirmed {
        t.Fatal("delivery state was not retained as transport metadata")
    }
}
```

- [ ] **Step 2: Run the focused tests and verify failure.**

Run:

```bash
go test ./internal/provider -run 'TestDeliveryState|TestResponseMetadataKeepsRawDeliveryState'
```

Expected: FAIL because the type, constants, methods, and metadata field do not yet exist.

- [ ] **Step 3: Implement the smallest provider-neutral type.**

Create `internal/provider/delivery_state.go` with this shape:

```go
package provider

type DeliveryState uint8

const (
    DeliveryNotSent DeliveryState = iota + 1
    DeliverySentConfirmed
    DeliverySentUnconfirmed
    DeliveryResponseStarted
    DeliveryCompleted
)

func (s DeliveryState) Valid() bool {
    return s >= DeliveryNotSent && s <= DeliveryCompleted
}

func (s DeliveryState) String() string {
    switch s {
    case DeliveryNotSent:
        return "not_sent"
    case DeliverySentConfirmed:
        return "sent_confirmed"
    case DeliverySentUnconfirmed:
        return "sent_unconfirmed"
    case DeliveryResponseStarted:
        return "response_started"
    case DeliveryCompleted:
        return "completed"
    default:
        return "unobserved"
    }
}

func (s DeliveryState) ReplaySafe() bool { return s == DeliveryNotSent }
```

Add `DeliveryState DeliveryState` to `ResponseMetadata` without changing the existing fields or provider interface.

Keep `provider.Fake` neutral: it must return the response metadata supplied by its caller. Update its tests and shared test response constructors to set `DeliveryCompleted` for successful complete fake responses and explicit states for transport-error scenarios. Do not infer a state from response text or error strings.

- [ ] **Step 4: Run focused tests and existing provider tests.**

Run:

```bash
gofmt -w internal/provider/delivery_state.go internal/provider/provider.go internal/provider/fake.go internal/provider/delivery_state_test.go internal/provider/fake_test.go
go test ./internal/provider
```

Expected: PASS, with existing fake behavior preserved except that tests now declare their transport evidence explicitly.

- [ ] **Step 5: Commit the provider contract.**

```bash
git add internal/provider/delivery_state.go internal/provider/delivery_state_test.go internal/provider/provider.go internal/provider/fake.go internal/provider/fake_test.go
git commit -m "feat(provider): add provider-neutral delivery states"
```

---

## Task 2: Implement narrow OmniRoute transport evidence

**Files:**
- Create: `internal/provider/omniroute/delivery.go`
- Create: `internal/provider/omniroute/delivery_test.go`
- Modify: `internal/provider/omniroute/client_transport.go`
- Modify: `internal/provider/omniroute/client.go`
- Modify: `internal/provider/omniroute/classifier.go`
- Modify: `internal/provider/omniroute/client_test.go`
- Modify: `internal/provider/omniroute/attempt_receipts_test.go`
- Modify: `internal/provider/omniroute/review_regressions_test.go`

**Interfaces:**
- Produces an internal `deliveryObservation` that is local to OmniRoute and never crosses into governor/state.
- `completeOnce` returns `provider.ResponseMetadata.DeliveryState` on success and every error path.
- `httptrace.WroteRequest` records only local write evidence. It never maps to `DeliverySentConfirmed`.

- [ ] **Step 1: Write failing transport tests for each boundary.**

Use `httptest.Server` and a fake `http.RoundTripper` that can invoke trace callbacks deterministically. Create named tests with these exact assertions:

- `TestCompleteOnceCanceledBeforeDoIsNotSent`: cancel the context before calling `completeOnce`, assert the fake transport call count is zero and the returned metadata state is `DeliveryNotSent`.
- `TestCompleteOnceRoundTripErrorWithoutAuthorityIsSentUnconfirmed`: invoke the transport and return a reset/transport error without a response or contract confirmation, assert `DeliverySentUnconfirmed`.
- `TestWroteRequestDoesNotConfirmUpstreamModelDispatch`: invoke the trace's `WroteRequest` callback with a nil error and then return a transport error, assert `DeliverySentUnconfirmed`.
- `TestAuthoritativeFakeConfirmationCanProduceSentConfirmed`: return a provider-neutral fake response whose metadata explicitly contains `DeliverySentConfirmed`, and assert the value is preserved without any OmniRoute inference.
- `TestPartialResponseIsResponseStarted`: invoke `GotFirstResponseByte`, return a body that fails before EOF, and assert `DeliveryResponseStarted`.
- `TestCompleteValidResponseIsCompleted`: return a full 2xx JSON completion body and assert `DeliveryCompleted`.
- `TestCompleteHTTPFailureBodyIsCompleted`: return a full 4xx or 5xx body and assert `DeliveryCompleted` even though classification returns an HTTP failure.
- `TestCompleteEmptyAndMalformedBodiesAreCompleted`: return an empty body and malformed JSON body that both reach EOF, assert `DeliveryCompleted` for both.
- `TestCancelBeforeAndAfterPossibleDispatchHaveDifferentStates`: assert pre-`Do` cancellation is `DeliveryNotSent`, while cancellation after invoking the transport without stronger evidence is `DeliverySentUnconfirmed`.
- `TestClientRequestIDPropagatesWithoutIdempotencyKey`: assert `X-Runstead-Client-Request-Id` equals the request ID and `Idempotency-Key` is empty.

The tests must use `completeOnce` where the receipt gate would otherwise obscure transport behavior, and use `Complete` separately for receipt-aware missing/invalid regressions.

- [ ] **Step 2: Run the focused OmniRoute tests and verify failure.**

Run:

```bash
go test ./internal/provider/omniroute -run 'TestCompleteOnceCanceledBeforeDo|TestCompleteOnceRoundTripError|TestWroteRequest|TestAuthoritativeFakeConfirmation|TestPartialResponse|TestCompleteValidResponse|TestCompleteHTTPFailure|TestCompleteEmpty|TestCancelBeforeAndAfter|TestClientRequestID'
```

Expected: FAIL because transport observation and metadata propagation are not implemented.

- [ ] **Step 3: Implement the monotonic transport tracker.**

Create `internal/provider/omniroute/delivery.go` with an internal tracker using this exact state model:

```go
type deliveryObservation struct {
    wroteRequest    bool
    responseStarted bool
}

func (o *deliveryObservation) trace() *httptrace.ClientTrace {
    return &httptrace.ClientTrace{
        WroteRequest: func(_ httptrace.WroteRequestInfo) {
            o.wroteRequest = true
        },
        GotFirstResponseByte: func() { o.responseStarted = true },
    }
}

func (o *deliveryObservation) stateBeforeDo() provider.DeliveryState {
    return provider.DeliveryNotSent
}

func (o *deliveryObservation) stateAfterError() provider.DeliveryState {
    if o.responseStarted {
        return provider.DeliveryResponseStarted
    }
    return provider.DeliverySentUnconfirmed
}

func (o *deliveryObservation) stateAfterBody(readComplete bool) provider.DeliveryState {
    if readComplete {
        return provider.DeliveryCompleted
    }
    if o.responseStarted {
        return provider.DeliveryResponseStarted
    }
    return provider.DeliverySentUnconfirmed
}
```

The implementation must set `doCalled=true` immediately before invoking `httpClient.Do`, attach the trace with `httptrace.WithClientTrace`, and set `responseStarted=true` whenever a non-nil response is returned even if the custom transport did not invoke `GotFirstResponseByte`.

Do not add any `sent_confirmed` path to OmniRoute based on `WroteRequest`, status code, duration, or error narrative. The adapter has no upstream confirmation source in this issue; provider-neutral fake tests cover the state today.

- [ ] **Step 4: Thread state through all `completeOnce` returns.**

Apply these exact rules in `client_transport.go`:

```go
if err := ctx.Err(); err != nil {
    return provider.Response{Metadata: provider.ResponseMetadata{
        DeliveryState: provider.DeliveryNotSent,
        Endpoint: logicalEndpoint(requestURL),
        Model: model,
    }}, contextError(err, false)
}
```

For request/body/URL construction failures before `Do`, use `DeliveryNotSent`. For a `Do` error, use `observation.stateAfterError()`. For a non-nil response, set `response_started` before receipt parsing/body reads, return `response_started` if reading fails, and set `completed` only after `readBody` returns nil. Then classify HTTP status and JSON/body content without changing the completed delivery state.

When the receipt header is invalid and the adapter closes the body before reading it, preserve `response_started`; when the body was fully consumed first, preserve `completed`. Preserve all existing sanitized status/request/session metadata and never include raw error text.

- [ ] **Step 5: Update OmniRoute classifier to carry but not invent state.**

Set `Outcome.DeliveryState = response.Metadata.DeliveryState`. Derive `Outcome.UpstreamReached` conservatively from the raw state: `DeliveryNotSent` means false, a valid non-not-sent state means true, and zero/invalid means true for safety. Keep the existing error-kind classification and retry hints. Do not set `DeliveryState` from `StatusCode`, `Duration`, response text, or `Error.UpstreamReached`.

- [ ] **Step 6: Run adapter tests and RouteSafety regressions.**

Run:

```bash
gofmt -w internal/provider/omniroute/delivery.go internal/provider/omniroute/delivery_test.go internal/provider/omniroute/client_transport.go internal/provider/omniroute/client.go internal/provider/omniroute/classifier.go
go test ./internal/provider/omniroute
```

Expected: PASS, including all existing receipt and RouteSafety tests. Production execution must remain blocked when authoritative receipts are unavailable.

- [ ] **Step 7: Commit transport evidence.**

```bash
git add internal/provider/omniroute
git commit -m "feat(omniroute): observe conservative delivery boundaries"
```

---

## Task 3: Integrate delivery evidence with governor classification without replacing retry policy

**Files:**
- Create: `internal/governor/delivery_state_test.go`
- Modify: `internal/governor/types.go`
- Modify: `internal/governor/execute.go`
- Modify: `internal/governor/permit.go`
- Modify: `internal/governor/circuit.go`
- Modify: `internal/governor/persistence.go`
- Modify: `internal/governor/attempt_receipts_test.go`
- Modify: `internal/governor/governor_test.go`

**Interfaces:**
- `governor.Outcome` gains `DeliveryState provider.DeliveryState` containing the raw observation.
- `governor.FinishResult` gains `DeliveryState provider.DeliveryState` for callers/tests; zero remains unobserved.
- `governor.ProviderFinished` gains `DeliveryState provider.DeliveryState` for TX2 persistence.
- Internal helpers `effectiveDeliveryState` and `applyDeliveryEvidence` use conservative behavior without mutating the raw field.

- [ ] **Step 1: Write failing governor tests for raw/effective state separation.**

Add `TestExecuteClassifierCannotFabricateDeliveryState` with a `provider.NewFake`
that returns text claiming completion but metadata `DeliverySentUnconfirmed`.
Pass a classifier that returns `OutcomeSuccess` and a fabricated
`DeliveryCompleted`. Execute through the existing fast governor fixture, then
assert the response metadata remains `DeliverySentUnconfirmed`, the completion
is `OutcomeUncertainReached`, and `Completion.RetryEligible` is false.

Add these additional named tests with concrete assertions:

- `TestUnobservedDeliveryRemainsUnobservedButIsConservative`: return a response with zero metadata state, assert `Completion.Outcome == OutcomeUncertainReached`, `Completion.RetryEligible == false`, and the persistence fake receives zero rather than `DeliverySentUnconfirmed`.
- `TestNotSentCancellationIsNotConvertedToUncertain`: return `OutcomeCancelledBeforeUpstream` with `DeliveryNotSent`, assert the completion remains `OutcomeCancelledBeforeUpstream` and `UpstreamReached == false`.
- `TestResponseStartedForcesConservativeOutcome`: return `DeliveryResponseStarted` with a connection/reset outcome, assert `OutcomeUncertainReached` and `RetryEligible == false`.
- `TestCompletedFailureKeepsExistingNewAttemptRetryPolicy`: return a complete `DeliveryCompleted` rate/capacity or server failure with the existing budget/circuit setup, assert the pre-existing `RetryEligible` policy result is unchanged.
- `TestAmbiguousDeliveryDoesNotBypassNewAdmission`: finish an ambiguous request, then submit a second request with a distinct `client_request_id`; assert the second path still goes through admission and receives its own attempt sequence.

Add a receipt-aware regression proving a missing or malformed receipt remains fail-closed even when delivery metadata is present:

Add `TestDeliveryStateDoesNotSubstituteForMissingReceipts` using the existing
receipt-aware governor fixture. Configure `RequireAttemptReceipts=true`, use a
receipt-aware client that returns a complete response with
`DeliveryCompleted` but no receipt set, execute one request, and assert
`OutcomeUncertainReached`, `RetryEligible == false`, and
`governor.Snapshot().Telemetry.Unsafe == true`.

- [ ] **Step 2: Run focused governor tests and verify failure.**

```bash
go test ./internal/governor -run 'TestExecuteClassifierCannotFabricateDeliveryState|TestUnobservedDelivery|TestNotSentCancellation|TestResponseStarted|TestCompletedFailure|TestAmbiguousDelivery|TestDeliveryStateDoesNotSubstitute'
```

Expected: FAIL because the structs and delivery-aware normalization do not exist.

- [ ] **Step 3: Add raw state fields and conservative helpers.**

In `internal/governor/types.go` add `DeliveryState provider.DeliveryState` to `Outcome`, `FinishResult`, and `ProviderFinished`. Add helpers with these exact semantics:

```go
func effectiveDeliveryState(state provider.DeliveryState) provider.DeliveryState {
    if state.Valid() {
        return state
    }
    return provider.DeliverySentUnconfirmed
}

func deliveryUpstreamReached(state provider.DeliveryState) bool {
    return effectiveDeliveryState(state) != provider.DeliveryNotSent
}
```

Add a helper that only constrains outcome when delivery proves ambiguity:

```go
func applyDeliveryEvidence(outcome Outcome) Outcome {
    effective := effectiveDeliveryState(outcome.DeliveryState)
    outcome.UpstreamReached = deliveryUpstreamReached(outcome.DeliveryState)
    switch effective {
    case provider.DeliverySentUnconfirmed, provider.DeliveryResponseStarted:
        outcome.Class = OutcomeUncertainReached
    case provider.DeliveryNotSent:
        if outcome.Class == OutcomeCancelledBeforeUpstream {
            return outcome
        }
    }
    return outcome
}
```

Do not change `RetryEligible` globally. The existing `recordOutcomeLocked` policy remains responsible for recoverable outcome classes. Because ambiguous/partial delivery is converted to `OutcomeUncertainReached`, the existing policy naturally returns false for those cases. A completed rate/capacity/server failure keeps its existing new-attempt policy.

- [ ] **Step 4: Make `Execute` use metadata as the only delivery source.**

After the classifier returns, overwrite only the delivery field from the response metadata and then apply the conservative outcome helper:

```go
outcome := classifier(response, callErr)
outcome.DeliveryState = response.Metadata.DeliveryState
outcome = applyDeliveryEvidence(outcome)
```

Do not copy a classifier-provided `DeliveryState`, `UpstreamReached`, or response-text claim over the transport metadata. Preserve the existing cancellation handling only when the effective state is not `DeliveryNotSent`; a proven pre-dispatch cancellation must remain `OutcomeCancelledBeforeUpstream`.

Populate `ProviderFinished.DeliveryState` from `response.Metadata.DeliveryState`, not from the effective fallback. Populate `FinishResult.DeliveryState` with the raw value.

- [ ] **Step 5: Update permit/circuit accounting while preserving new retry semantics.**

In `recordOutcomeLocked`, remove the unconditional conversion of `OutcomeCancelledBeforeUpstream` to `OutcomeUncertainReached`. Replace it with:

```go
if outcome.Class == OutcomeCancelledBeforeUpstream &&
    effectiveDeliveryState(outcome.DeliveryState) != provider.DeliveryNotSent {
    outcome.Class = OutcomeUncertainReached
}
```

Ensure `Finish` and `FinishWithAttemptReceipts` retain the raw `DeliveryState` in their `FinishResult`. Keep receipt validation and `finishReceiptFailureLocked` unchanged in authority: missing/malformed receipts still mark unsafe and conservatively account. A valid receipt outcome may remain a normal classified outcome, but `DeliveryState.ReplaySafe()` is the only same-attempt replay safety signal.

Do not add a retry loop. Document on `RetryEligible` that it indicates a new governed logical attempt and cannot authorize reusing the same `client_request_id`.

- [ ] **Step 6: Run focused and existing governor tests.**

```bash
gofmt -w internal/governor/types.go internal/governor/execute.go internal/governor/permit.go internal/governor/circuit.go internal/governor/persistence.go internal/governor/delivery_state_test.go
go test ./internal/governor
```

Expected: PASS, including existing rate/capacity/backoff, security, receipt, RouteSafety, and accounting tests.

- [ ] **Step 7: Commit governor integration.**

```bash
git add internal/governor
git commit -m "feat(governor): classify delivery evidence conservatively"
```

---

## Task 4: Persist delivery state and expose it through recovery and inspect

**Files:**
- Create: `internal/state/migrations/0011_provider_delivery_state.sql`
- Modify: `internal/state/provider_attempts.go`
- Modify: `internal/state/inspect.go`
- Modify: `internal/state/recovery.go`
- Modify: `internal/state/inspect_test.go`
- Modify: `internal/state/recovery_test.go`
- Modify: `internal/state/migrations_test.go`
- Modify: `internal/recovery/recovery.go`
- Modify: `internal/recovery/recovery_test.go`

**Interfaces:**
- `provider_attempts.delivery_state` stores `''` for unobserved and one of the five valid names for observed state.
- `state.RecoveryProviderAttempt` gains raw `DeliveryState provider.DeliveryState`.
- `state.inspectProviderAttempt` gains raw `DeliveryState provider.DeliveryState`.
- `RecordProviderFinished` persists raw state, never the conservative fallback.

- [ ] **Step 1: Write failing migration/persistence tests.**

Add tests that assert the latest schema has the column and constraint:

```go
func TestProviderAttemptsHaveDeliveryStateColumn(t *testing.T) {
    store := openTestStore(t)
    rows, err := store.db.Query(`PRAGMA table_info(provider_attempts)`)
    if err != nil {
        t.Fatal(err)
    }
    defer rows.Close()
    found := false
    for rows.Next() {
        var cid int
        var name, columnType string
        var notNull, primaryKey int
        var defaultValue any
        if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
            t.Fatal(err)
        }
        if name == "delivery_state" {
            found = true
            if columnType != "TEXT" || notNull != 1 || primaryKey != 0 {
                t.Fatalf("delivery_state schema = type %q notNull %d pk %d", columnType, notNull, primaryKey)
            }
        }
    }
    if err := rows.Err(); err != nil {
        t.Fatal(err)
    }
    if !found {
        t.Fatal("provider_attempts.delivery_state is missing")
    }
}
```

Add `TestProviderAttemptPersistsObservedAndUnobservedDeliveryState` in the
existing `state` package test file. Create a task, call the existing prepared
attempt helper, query `provider_attempts.delivery_state`, and assert TX1 is an
empty string. Finish that same attempt with a `governor.ProviderFinished` whose
raw state is `provider.DeliveryCompleted`, query again, and assert the stored
value is `completed`. Create a second prepared/finished attempt with a zero raw
state and assert its stored value remains empty rather than
`sent_unconfirmed`. Load the last journal event and assert it includes only the
sanitized delivery state/outcome/accounting fields, not model text,
authorization, response bodies, or raw error text.

Add a crash-window test proving TX1 never fabricates delivery:

```go
func TestProviderTx1CrashLeavesPreparedDeliveryUnobserved(t *testing.T) {
    store := openTestStore(t)
    mustTask(t, store, "task-1")
    mustProviderAttemptPreparedOnly(t, store, "task-1", "request-1", 1)
    var delivery string
    if err := store.db.QueryRow(`SELECT delivery_state FROM provider_attempts WHERE task_id = ? AND client_request_id = ?`, "task-1", "request-1").Scan(&delivery); err != nil {
        t.Fatal(err)
    }
    if delivery != "" {
        t.Fatalf("prepared delivery_state = %q, want empty", delivery)
    }
}
```

- [ ] **Step 2: Run migration/persistence tests and verify failure.**

```bash
go test ./internal/state -run 'TestProviderAttemptsHaveDeliveryState|TestProviderAttemptPersists|TestProviderTx1CrashLeavesPrepared'
```

Expected: FAIL because migration and SQL projection fields do not exist.

- [ ] **Step 3: Add migration 0011.**

Create `internal/state/migrations/0011_provider_delivery_state.sql`:

```sql
-- Provider delivery evidence for issue #38. Empty is intentional: TX1 has
-- not observed transport delivery yet, and recovery must not fabricate it.
ALTER TABLE provider_attempts ADD COLUMN delivery_state TEXT NOT NULL DEFAULT '' CHECK (
    delivery_state IN ('', 'not_sent', 'sent_confirmed', 'sent_unconfirmed',
                       'response_started', 'completed')
);
```

Do not add a delivery-state table or modify provider-attempt status values.

- [ ] **Step 4: Update TX1/TX2 persistence with raw-state serialization.**

In `internal/state/provider_attempts.go`, add:

```go
func persistedDeliveryState(state provider.DeliveryState) string {
    if !state.Valid() {
        return ""
    }
    return state.String()
}
```

Import `internal/provider`, update `RecordProviderFinished` to set
`delivery_state = ?` from `persistedDeliveryState(record.DeliveryState)`, and add
`delivery_state` to the journal payload using `record.DeliveryState.String()` so
unobserved renders as `unobserved`. Keep TX1's inserted value empty explicitly
or rely on the migration default while retaining the SQL comment that TX1 has
no observation.

Add `delivery_state` to the update statement without changing the existing
provider status mapping. Keep receipts inserted in the same TX2 transaction.

- [ ] **Step 5: Load and render the state deterministically.**

In `inspect.go`, add the field and SQL projection:

```go
Append `DeliveryState provider.DeliveryState` to the existing
`inspectProviderAttempt` struct without removing or reordering the existing
fields used by deterministic rendering.
```

Scan the string into a temporary variable and parse only the six accepted
storage values (`''` maps to zero). Return a descriptive error for any value
outside the migration check. Render:

```text
  delivery_state=completed
```

or:

```text
  delivery_state=unobserved
```

Keep the lifecycle `status=`, `outcome=`, `uncertain=`, `debited=`, receipt, and
recovery lines unchanged. Do not render prompts, bodies, headers, or raw errors.

In `recovery.go`, load and parse the same raw value into
`RecoveryProviderAttempt.DeliveryState`. A `prepared` row with zero state stays
in the existing conservative recovery class. A terminal persisted `not_sent`
row is not re-executed; any future logical attempt still enters the normal
agent/governor path.

- [ ] **Step 6: Update recovery tests and context assertions.**

Add tests for:

- `TestRecoveryPreservesSentUnconfirmedAndNeverReissuesIt`: seed a persisted attempt with `status=uncertain`, `delivery_state=sent_unconfirmed`, run recovery, assert the provider attempt is reconciled conservatively and the recovery plan contains no historical provider call to execute.
- `TestRecoveryPreparedWithoutTx2KeepsDeliveryUnobserved`: seed a TX1-only prepared attempt, run recovery, assert `DeliveryState` is zero/unobserved in the loaded snapshot and the existing conservative reconciliation path is selected.
- `TestRecoveryNotSentIsTerminalEvidenceNotHistoricalReplay`: seed a terminal failed/canceled attempt with `delivery_state=not_sent`, run the continuation path, and assert any subsequent provider request is a new governed admission with a distinct client request ID rather than a re-execution of the historical attempt.

Keep the existing conservative debit behavior for receipt-aware prepared rows.
Do not use delivery state to bypass `GovernorBlocks` or the normal agent loop.

- [ ] **Step 7: Run state/recovery/inspect tests repeatedly.**

```bash
gofmt -w internal/state/provider_attempts.go internal/state/inspect.go internal/state/recovery.go internal/state/inspect_test.go internal/state/recovery_test.go internal/recovery/recovery.go internal/recovery/recovery_test.go
for i in $(seq 1 20); do
  go test ./internal/state ./internal/recovery || exit 1
done
```

Expected: PASS on every iteration, with deterministic inspect output and no fabricated state in prepared rows.

- [ ] **Step 8: Commit persistence and recovery.**

```bash
git add internal/state internal/recovery
git commit -m "feat(state): persist provider delivery evidence (#38)"
```

---

## Task 5: Complete deterministic end-to-end coverage and documentation

**Files:**
- Modify: `internal/provider/fake_test.go`
- Modify: `internal/agent/loop_test.go` and other scripted provider fixtures found by `grep -R "type scriptedProvider\|provider.Response{" internal/agent cmd internal`
- Modify: `internal/provider/omniroute/classifier_test.go`
- Modify: `internal/provider/omniroute/attempt_receipts_test.go`
- Modify: `internal/governor/attempt_receipts_test.go`
- Modify: `internal/state/inspect_test.go`
- Modify: `cmd/runstead/inspect_test.go`
- Modify: `docs/persistence.md`
- Modify: `docs/architecture.md` only if the provider boundary section needs the implemented contract

**Interfaces:**
- All deterministic provider fixtures declare delivery evidence explicitly.
- CLI inspect assertions include the sanitized delivery state without leaking response content or IDs outside existing safe metadata.
- Documentation reflects implementation, not future promises.

- [ ] **Step 1: Locate and update all in-repository provider test fixtures.**

Run:

```bash
grep -R -n -E 'type scriptedProvider|provider.Response\{|NewFake\(|NewErrorFake\(' internal cmd --include='*.go'
```

For every successful complete fake response, set:

```go
Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}
```

For pre-dispatch cancellation tests, set `DeliveryNotSent`. For ambiguous
transport errors, set `DeliverySentUnconfirmed`. Do not set a state from the
model's returned text or the string form of an error.

- [ ] **Step 2: Add cross-package regression tests.**

Cover these exact assertions in named tests:

- `TestFullResponseDoesNotMeanTaskSuccess`: exercise complete empty, malformed, and HTTP-error responses; assert delivery is `completed` while the provider outcome is the corresponding failure class.
- `TestModelTextCannotManufactureDeliveryState`: return text containing every delivery-state spelling while metadata contains one fixed state; assert metadata and completion retain only the transport state.
- `TestHTTPStatusCannotManufactureSentConfirmed`: return complete HTTP responses with each status class and assert the final state is `completed`, never `sent_confirmed`.
- `TestDurationCannotManufactureDeliveryState`: vary the injected clock duration while keeping transport observations identical; assert the delivery state is unchanged.
- `TestNoIdempotencyKeyIsSent`: capture the outgoing request and assert `Idempotency-Key` is absent while the existing client request header is present.
- `TestReceiptAwareMissingAndMalformedRemainFailClosed`: run both missing and malformed receipt cases and assert unsafe telemetry, conservative outcome, and no automatic replay permission.
- `TestRouteSafetyRemainsFailClosed`: retain the existing RouteSafety table and assert unknown/mutated safety values are still rejected.

- [ ] **Step 3: Add CLI inspect output assertions.**

Extend `internal/state/inspect_test.go` and `cmd/runstead/inspect_test.go` to
assert deterministic lines such as:

```text
request=task-1-0001 provider=scripted model=scripted status=failed
delivery_state=completed
outcome=empty_response upstream_reached=true
```

and for a TX1-only row:

```text
delivery_state=unobserved
uncertain=prepared: the upstream may have been reached; never auto-retry
```

Render the task twice and compare complete output byte-for-byte.

- [ ] **Step 4: Update minimal documentation.**

In `docs/persistence.md`, add a concise section near the existing provider/
recovery discussion:

```markdown
### Provider delivery state (#38)

`delivery_state` is transport evidence orthogonal to the provider-attempt
lifecycle. It does not mean task completion and it never replaces authoritative
attempt receipts. `sent_unconfirmed` is fail-closed: it is treated as an
uncertain effect and is not automatically replayed. `client_request_id` is the
Runstead correlation identity, not a promise that OmniRoute or the model
upstream honors an idempotency key. A `completed` delivery may still classify as
provider failure when the HTTP result, response content, or envelope is invalid.
An empty value means delivery was not observed before TX2 and is never converted
to a stronger state during recovery.
```

If `docs/architecture.md` has a provider-boundary description that would now be
misleading, add only the same semantic facts. Do not update roadmap claims for
#29/#30 or add marketing language.

- [ ] **Step 5: Run all focused package tests repeatedly.**

```bash
for i in $(seq 1 20); do
  go test ./internal/provider/... ./internal/governor/... ./internal/state ./internal/recovery ./cmd/runstead || exit 1
done
```

Expected: PASS in every iteration.

- [ ] **Step 6: Commit integration tests and documentation.**

```bash
git add internal cmd docs/persistence.md docs/architecture.md
git commit -m "test: cover provider delivery boundaries and replay safety"
```

---

## Task 6: Full verification, maintainer review, and PR delivery

**Files:**
- Modify only files already in the Issue #38 diff after review.
- Create: `/tmp/issue-38-pr-body.md` or `$JCODE_SCRATCH_DIR/issue-38-pr-body.md` for the PR body, not committed.

**Interfaces:**
- Verification must cover formatting, unit/integration tests, race, vet, build, protocol experiment, and whitespace.
- PR targets `main`, title `feat(provider): persist delivery state and block ambiguous retries`, and body contains `Closes #38` plus explicit out-of-scope confirmations.

- [ ] **Step 1: Review the complete diff against origin/main.**

Run:

```bash
git diff --stat origin/main...HEAD
git diff --name-status origin/main...HEAD
git diff origin/main...HEAD -- docs/ internal/provider internal/governor internal/state internal/recovery cmd
```

Remove any changes that are unrelated to #38, #29 producer behavior, #30
activation, #39 telemetry, account rotation/fallback, or aesthetic refactors.
Confirm no `Idempotency-Key`, `execution_id` reuse, receipt replacement, or
RouteSafety relaxation appears anywhere in the diff.

- [ ] **Step 2: Run formatting and static checks.**

```bash
gofmt -w $(git diff --name-only --diff-filter=ACM origin/main...HEAD -- '*.go')
git diff --check
go vet ./...
go build ./cmd/runstead
```

Expected: all commands exit 0 and `git diff --check` emits no output.

- [ ] **Step 3: Run the required full test gates.**

```bash
go test ./...
go test -race ./...
```

Expected: both commands pass. If any gate fails, debug and fix it before
claiming completion.

- [ ] **Step 4: Run the protocol experiment gate.**

```bash
bash experiments/protocol/test.sh
```

Expected: the script exits 0 without credentials or network access.

- [ ] **Step 5: Re-run timing-sensitive focused tests.**

```bash
for i in $(seq 1 30); do
  go test -race ./internal/provider/omniroute ./internal/governor ./internal/state ./internal/recovery || exit 1
done
```

Expected: PASS on all 30 iterations, covering cancellation, body interruption,
HTTP transport resets, TX2 crash windows, and recovery.

- [ ] **Step 6: Review persisted and rendered evidence manually.**

Use focused tests or a temporary test database to verify:

```text
TX1 provider_attempts.status=prepared, delivery_state=''
TX2 completed response.delivery_state='completed'
TX2 ambiguous response.delivery_state='sent_unconfirmed'
inspect prints delivery_state=unobserved/completed/sent_unconfirmed deterministically
journal payload contains only sanitized delivery state and accounting fields
```

Confirm receipts are still independently stored and validated, and a prepared
crash row is not rewritten with a guessed state.

- [ ] **Step 7: Commit any final verification-only fixes.**

```bash
git status --short
git diff --check
git commit -am "chore: finalize issue 38 verification"  # only when a real fix remains
```

Do not create an empty commit. Re-run the affected focused test after any fix.

- [ ] **Step 8: Create the PR against main.**

Write the PR body to `$JCODE_SCRATCH_DIR/issue-38-pr-body.md`:

```markdown
Closes #38

## Summary

- Implemented provider-neutral delivery states: not_sent, sent_confirmed,
  sent_unconfirmed, response_started, and completed.
- Added narrow OmniRoute transport observation with httptrace. WroteRequest is
  intentionally not treated as upstream confirmation.
- Persisted raw delivery evidence with provider attempts in TX2 and exposed it
  through journal, recovery, and sanitized inspect output.
- Kept provider lifecycle and governor outcome policy separate from transport
  evidence. Ambiguous delivery remains fail-closed and cannot replay the same
  attempt.
- Kept client_request_id as correlation only. No Idempotency-Key was invented.
- Preserved independent attempt-receipt validation and accounting.

## Semantics

- not_sent may support a new governed attempt when budgets and admission allow.
- sent_unconfirmed is conservative/uncertain and never auto-replayed.
- sent_confirmed is not produced from WroteRequest and does not authorize same-
  attempt replay.
- response_started is conservative when the response is interrupted.
- completed means delivery completed, not provider/task success. Complete HTTP,
  empty, and malformed responses can still fail classification.

## Verification

- `gofmt`
- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `go build ./cmd/runstead`
- `bash experiments/protocol/test.sh`
- `git diff --check`
- repeated focused provider/governor/state/recovery tests

## Limitations and scope

The current OmniRoute adapter cannot independently prove upstream model dispatch
from HTTP write evidence alone, so it does not force a real `sent_confirmed`
observation. Provider-neutral fakes cover that contract state. No exactly-once
execution claim is introduced.

#29 producer and #30 activation remain fail-closed and outside this PR. No
fallback/account rotation, telemetry expansion, health probe, browser/Camoufox,
M6, or adapter retry loop was added.
```

Create the PR with the repository's available authenticated GitHub CLI or API
integration:

```bash
gh pr create --base main --head feat/issue-38-delivery-state \
  --title "feat(provider): persist delivery state and block ambiguous retries" \
  --body-file "$JCODE_SCRATCH_DIR/issue-38-pr-body.md"
```

If the environment has no authenticated PR tool, leave the branch and commit
history ready and report the exact limitation rather than claiming a PR exists.

---

## Plan self-review checklist

- [x] Every approved spec section maps to at least one implementation task.
- [x] `WroteRequest` is explicitly prevented from producing `sent_confirmed`.
- [x] The real adapter is not forced to emit all five states.
- [x] Zero/unobserved remains distinct from runtime conservative treatment and is persisted as empty.
- [x] `RetryEligible` remains a new-governed-attempt policy; same-attempt replay safety is represented by `DeliveryState.ReplaySafe()` and recovery rules.
- [x] Attempt receipts and RouteSafety remain fail-closed.
- [x] TX1/TX2 and prepared-crash semantics are explicit.
- [x] Tests cover all five states, cancellation/reset/timeout boundaries, complete failures, malformed/empty responses, request correlation, persistence, inspect, recovery, receipts, and regression protection.
- [x] No placeholder tasks, unspecified files, or out-of-scope feature work remain.
