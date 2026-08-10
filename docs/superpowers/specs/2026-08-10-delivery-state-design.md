# Issue #38 design: provider delivery state and end-to-end replay safety

**Date:** 2026-08-10  
**Branch:** `feat/issue-38-delivery-state`  
**Baseline:** `origin/main` at `16e253f`  
**Status:** approved design, implementation plan follows

## Goal

Make the delivery-state contract from ADR 0001 observable, provider-neutral,
persisted, and testable on the provider path without introducing a competing
provider-attempt lifecycle, an invented upstream idempotency key, a producer for
#29, or activation of #30.

The provider attempt lifecycle remains the authoritative persisted lifecycle:
`planned -> prepared -> running -> completed|failed|uncertain|reconciled|...`.
Delivery state is transport evidence about the same request and remains
orthogonal to that lifecycle.

## Invariants

1. The only delivery states are:
   - `not_sent`
   - `sent_confirmed`
   - `sent_unconfirmed`
   - `response_started`
   - `completed`
2. The zero value is invalid and means **unobserved**, not
   `sent_unconfirmed`. It may be retained in TX1/pre-TX2 rows and in any result
   for which the provider supplied no delivery evidence.
3. Runtime safety decisions treat an invalid/unobserved value conservatively as
   an ambiguous delivery. They must not persist `sent_unconfirmed` merely
   because evidence is absent.
4. `client_request_id` remains the Runstead correlation identity and is sent
   through the existing request header. No `Idempotency-Key` is added.
5. `receipt_attempt_id` remains upstream evidence identity and is never used as
   `execution_id` or `client_request_id`.
6. Attempt receipts remain independent, authoritative accounting evidence.
   Delivery state cannot make a missing or malformed receipt set acceptable.
7. TX1 persists intent and correlation before the provider call. TX2 persists
   the observed delivery state, classified outcome, receipt evidence, and
   governor accounting. A crash before TX2 leaves the delivery state
   unobserved and recovery must not manufacture one.
8. No retry or replay bypasses a new governor admission.

## Provider-neutral contract

Add a small `provider.DeliveryState` type with explicit constants and methods
for validation/string rendering. `ResponseMetadata` gains:

```go
DeliveryState DeliveryState
```

The provider-neutral type has no OmniRoute-specific fields. The zero value has
no public state name and is rendered by persistence/inspect as `unobserved`.

`Outcome` carries the raw observed delivery state so the governor can classify
it, while `ExecutionResult.Response.Metadata` remains the transport source of
truth. `Execute` overwrites any classifier-provided delivery state with the
value from `ResponseMetadata`; text, status, duration, and error narratives
cannot fabricate delivery evidence.

When the raw state is invalid, governor decisions use an internal conservative
effective state equivalent to an ambiguous dispatch, but TX2 stores the raw
invalid value as unobserved.

## Transport evidence and state mapping

Instrument only the narrow OmniRoute completion path with standard
`net/http/httptrace` callbacks. No general telemetry, metrics, first-token
latency, adapter-version, or health-probe work is included.

The adapter keeps a monotonic local observation for one `Complete` call:

| Observation | Final state | Reason |
| --- | --- | --- |
| Context/validation/body/URL failure before invoking `Do` | `not_sent` | Runstead can prove no HTTP dispatch was attempted. |
| `Do` invoked, no response or authoritative dispatch confirmation | `sent_unconfirmed` | The HTTP/gateway boundary may have been reached, but no proof exists that an upstream model attempt occurred. This includes transport reset, timeout, or cancellation after the possible-dispatch boundary when no stronger evidence exists. |
| A provider contract/authoritative receipt or equivalent signal explicitly proves the upstream model attempt was dispatched, without a response having started | `sent_confirmed` | `httptrace.WroteRequest` alone is not sufficient and never produces this state. |
| Response headers/first response byte are observed, but body delivery is interrupted | `response_started` | The provider effect occurred, but final outcome remains conservative. |
| The complete response body is read, including a complete HTTP error body, empty body, or malformed JSON body | `completed` | Delivery is terminal; classification still decides provider success/failure. |

The real OmniRoute adapter may not be able to produce `sent_confirmed` today.
That is acceptable. Its `httptrace` evidence can distinguish pre-dispatch
`not_sent`, ambiguous dispatch `sent_unconfirmed`, response-started, and
completed delivery. A provider-neutral fake can directly supply
`sent_confirmed` for deterministic contract tests. The adapter must not promote
`WroteRequest` to `sent_confirmed`.

Every error return from `completeOnce` preserves the response metadata already
observed. In particular, a body read error does not erase `response_started`,
and a transport error does not erase a preceding authoritative confirmation or
response-start observation.

## Classification and governor integration

The existing outcome classifier remains responsible for provider outcome
classification. Delivery evidence constrains safety but does not become a
second lifecycle or a replacement for the governor's outcome policy.

The effective delivery state is applied as follows:

- `not_sent`: preserve a deterministic pre-dispatch cancellation/failure and
  leave the existing governor outcome retry policy available. If a retry is
  selected, it is a new `AttemptRequest`, re-admitted by the governor, and uses
  a new request identity where required.
- `sent_unconfirmed`: classify conservatively as `OutcomeUncertainReached`,
  keep the conservative accounting, and do not mark the result retry eligible
  for automatic replay.
- `sent_confirmed`: never authorize replay merely because a provider error was
  returned. If independent authoritative outcome evidence exists, the normal
  outcome classifier may retain it; absent such evidence, the transport error
  remains conservative. `RetryEligible` is not an authorization to reuse the
  same request or bypass admission.
- `response_started`: an interrupted response becomes conservative/uncertain;
  there is no automatic replay of that attempt.
- `completed`: preserve the normal outcome classification. A valid non-empty
  response can be `OutcomeSuccess`; a complete HTTP failure, empty response, or
  malformed response can be a provider failure. Delivery completion does not
  imply task/provider success.

`RetryEligible` keeps its existing meaning as governor policy for a **new,
governed logical attempt**, including existing rate/capacity/backoff behavior.
It is never a same-attempt replay permission. Delivery/recovery code separately
uses `DeliveryState.ReplaySafe()` (true only for observed `not_sent`) when it
needs to decide whether the interrupted intent itself is replay-safe. No
adapter-level retry loop is added.

The provider finished record and sanitized governor/journal event include the
raw observed delivery state. An unobserved state remains unobserved in those
records even when runtime classification was conservative.

## Attempt receipts and RouteSafety

Receipt-aware execution remains fail-closed:

1. TX1 is committed before `Complete`.
2. `Complete` and the adapter receipt gate remain outside SQLite transactions.
3. `FinishWithAttemptReceipts` still validates correlation, schema, route,
   timestamps, finalization, duplication, and amplification rules.
4. Missing/malformed/replayed receipts still produce conservative accounting
   and unsafe governor state as required by #29.
5. Delivery state is recorded alongside receipt evidence; it never substitutes
   for receipts and never enables the live path when receipts are absent.

`RouteSafety` values and OmniRoute production gating are unchanged. #29's
producer and #30 activation remain outside this issue and fail-closed.

## Persistence and recovery

Add one versioned migration to `provider_attempts`:

```sql
delivery_state TEXT NOT NULL DEFAULT '' CHECK (
  delivery_state IN ('', 'not_sent', 'sent_confirmed', 'sent_unconfirmed',
                     'response_started', 'completed')
)
```

TX1 inserts the empty value. TX2 updates the value to the raw observed state,
which can remain empty when no transport observation was available. Existing
pre-migration rows and rows left `prepared` after a crash remain unobserved.
No second table or lifecycle is introduced.

Provider recovery loads the state as evidence:

- terminal TX2 outcomes remain terminal and are not replayed;
- a persisted `sent_unconfirmed` or ambiguous/partial attempt is reconciled
  conservatively and never auto-reissued;
- a `prepared` row with empty delivery state remains the existing ambiguous
  crash window and is never converted to `not_sent` or `sent_unconfirmed` by
  recovery;
- a persisted `not_sent` outcome may be followed by a new, normally admitted
  attempt when the surrounding task policy permits it, but recovery never
  re-executes the historical provider call in place.

`runstead inspect` renders a deterministic sanitized `delivery_state=` line for
each provider attempt, using `unobserved` for the empty value. It also keeps the
existing lifecycle status, outcome, uncertainty, debit, receipt, and recovery
fields so delivery state cannot be mistaken for task completion or accounting.
Journal payloads use the same sanitized state spelling and never include raw
headers, response bodies, credentials, prompts, or error text.

## Test design

Use deterministic fake providers, fake `RoundTripper`/`httptrace` callbacks,
and `httptest.Server`; no network credentials or real OmniRoute access.

Required coverage:

- provider-neutral validation and all five states;
- pre-dispatch cancellation/validation error -> `not_sent`;
- ambiguous dispatch, timeout, cancellation, and connection reset ->
  `sent_unconfirmed` when no stronger evidence exists;
- provider-contract-confirmed dispatch -> `sent_confirmed`, proving
  `WroteRequest` alone is insufficient;
- response beginning followed by reset/short read -> `response_started` and
  conservative outcome;
- complete valid 2xx -> `completed` plus success;
- complete HTTP failure -> `completed` plus classified failure;
- complete empty/malformed response -> `completed` plus provider failure;
- cancellation before and after the possible-dispatch boundary;
- request/client ID propagation without `Idempotency-Key`;
- ambiguous delivery is not replay eligible, while existing new governed
  retry policy remains intact for complete rate/capacity/backoff outcomes;
- TX1/TX2 persistence, migration, reload, and recovery reconstruction;
- crash before TX2 preserves `prepared` plus unobserved delivery state;
- deterministic sanitized `runstead inspect` output;
- missing/malformed receipts remain fail-closed and delivery state does not
  substitute for receipt accounting;
- model text, response text, HTTP status, duration, and error wording cannot
  manufacture delivery state;
- existing RouteSafety and OmniRoute production-gating regressions remain
  unchanged.

## Documentation updates

Update the minimum persistence/provider documentation to state:

- delivery state is transport evidence, not task completion;
- delivery state does not replace attempt receipts;
- `sent_unconfirmed` is fail-closed and not replayable;
- `client_request_id` is correlation only, not an upstream idempotency promise;
- a `completed` delivery can still be a provider failure;
- no exactly-once execution guarantee is introduced.

## Explicit non-goals

This design does not implement the #29 producer, activate #30, change
allowance/governor semantics, add fallback/account rotation, add adapter retry
loops, add health probes, implement telemetry expansion for #39, build fixture
corpus #43, alter browser/Camoufox/M6 behavior, or weaken any existing
fail-closed path.
