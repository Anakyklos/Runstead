# Conservative operational envelope learning (issue #93)

Runstead adapts its operational envelopes to a provider *passively,
conservatively and durably*: normal governed execution produces typed
evidence, and only evidence that can be proven and that tightens safety
becomes part of the durable **OperationalProfile** (issue #91). The
governor (issue #92) remains the only enforcement authority; the adaptive
layer never issues requests, never retries on its own, and never relaxes a
bound.

This document states what can be learned, what can never be learned, how
stale/restart/identity rules work, and what is deliberately out of scope.

## Architecture

```mermaid
flowchart LR
  A[adapter: typed Error + metadata] --> B[compat.Observation]
  B --> C[adaptive.Evidence: kinds, proven numbers, closed options]
  C --> D[adaptive.Updates: deterministic conservative ProfileUpdates]
  D --> E[agent.Executor AttemptObserver seam]
  E --> F[store.ApplyOperationalProfileUpdates: monotonic SQLite check-and-set]
  F --> G[OperationalProfile]
  G --> H[run/resume: effective bounds + cooldown inputs]
  H --> I[adapters enforce request bounds / retry backoff (#92)]
```

- `internal/provider/adaptive` is a pure provider-neutral mapping. It
  imports no adapters, governor, agent, store or CLI code.
- `internal/provider/compat.Observation` is the only translator from
  adapter-typed failures into adaptive evidence. It copies no free text,
  headers, prompts, responses or request ids.
- `agent.AttemptObserver` is the only execution seam: every **admitted**
  governed attempt is observed exactly once, after the governor's durable
  finish and before any retry decision. A failed observation is a
  conservative stop: no further physical attempt may start.
- The CLI observer (`cmd/runstead`) assigns the attempt's task evidence
  reference and persists through the monotonic store boundary.

## What can be learned (live today)

| Evidence | Proven signal | Profile field | Direction |
| --- | --- | --- | --- |
| `rate_limited` with a proven wait | sanitized `Retry-After` / future reset | `cooldown_millis` | higher is more conservative |
| `rate_limited` with a proven per-minute ceiling | typed numeric limit (no adapter exposes one today) | `requests_per_minute` | lower |
| `request_too_large` with a proven byte limit | typed numeric limit (no adapter exposes one today) | `max_request_bytes` | lower |
| `output_too_large` with a proven byte limit | typed numeric limit (no adapter exposes one today) | `max_response_bytes` | lower |
| `capacity_restricted` with a proven ceiling | typed numeric ceiling (no adapter exposes one today) | `concurrency_ceiling` | lower |
| `unsupported_option` for a closed option | typed `unsupported_response_format` (Anthropic/Google adapters) | `unsupported_options` bitmask | setting more bits |

Only the **cooldown** row has a live producer today (the `Retry-After`
signal parsed by all three adapter families). The other numeric channels
exist in the contract and are fully unit-tested, but no adapter fabricates
a number, so production never guesses an RPM, a byte limit or a ceiling.
"Absence of evidence never becomes information."

## What can never be learned

- Successes. A successful attempt is evidence of nothing envelope-shaped;
  success can never raise or change any limit.
- Ambiguous outcomes (unknown kinds, wrapped errors, timeouts, auth
  failures, delivery-unsafe attempts). They never produce updates.
- Anything without a valid structured evidence reference (`task:cli-...`,
  `evidence:...`, `execution:...`, `verification:...`).
- Any numeric limit that was not proven by a machine-readable signal.
- Secret or private content: prompts, responses, raw headers, request ids,
  endpoint URLs are not representable in `adaptive.Evidence` (integers,
  closed enums and a structured reference only).

## Enforcement frontier

The profile is metadata that feeds existing frontiers:

- **Request/output size bounds**: `applyEffectiveProfileBounds` loads the
  profile's effective values into the resolved provider used to build the
  adapter, so a proven observed `max_request_bytes` really constrains the
  request envelope. A transcript that exceeds the effective bound is
  refused before dispatch and the run fails conservatively (the operator
  re-tasks or relaxes via configured/authoritative input).
- **Cooldown**: the retry backoff of issue #92 takes
  `max(selected backoff, profile cooldown, governor cooldown window)`, so a
  learned `cooldown_millis` dominates pacing after restarts.
- **Concurrency**: the governor contract requires `MaxInFlight == 1`
  (serialized lanes). Learning a lower `concurrency_ceiling` is conservative
  profile metadata; the runtime is already at least as strict, and #93 does
  not add parallelism.

## Stale evidence rule

- There is **no time-based expiry**: a conservative observed/authoritative
  value stays authoritative for the identity it was learned under. This is
  the safe direction (a stale relaxation could never be trusted; a stale
  tightening only costs pacing, which the governor bounds anyway).
- Relaxation is impossible through observations and is refused for
  configured replays on the same unchanged identity
  (`ErrProfileReplayUndo`): the operator must change the identity or use
  authoritative input.
- Identity changes (provider, protocol family, model, config identity)
  produce a **different profile key** (issue #91): incompatible learned
  state is never inherited. Config identity includes sanitized option
  values, so a changed endpoint or non-secret option also isolates state.
- Historical evidence stays in SQLite for audit; recovery reconstructs the
  current profile state from the store without replaying any model request.

## Restart and recovery

- The profile lives in the same SQLite store as governor/task state, so a
  restart (new process) or `runstead resume` re-reads the effective
  envelope (including bounds for client construction and cooldown for
  retry pacing).
- Unreadable or corrupt profile state fails closed before execution:
  `run`/`resume` refuse to start, and mid-run persistence failures stop the
  attempt loop conservatively (the physical effect may already be durable,
  so no new attempt may start under state the observer could not account
  for).

## Governor authority

The adaptive layer has no execution authority:

- It never probes, preflights, benchmark-tests or calibrates by dispatching
  provider requests. Every `wire.count() == 1` assertion in the E2E suite
  re-checks this: learning adds zero physical requests.
- It never retries. Retry dispatch remains owned by the issue #92 executor
  loop; observer failures stop the loop, they do not restart it.
- It never opens circuits, debits budgets or changes admission decisions.
  It only proposes profile field values through the monotonic boundary.

## Limitations and future work

- No adapter currently proves numeric RPM / byte / concurrency limits, so
  those rows are contract-only (unit-tested, zero producers). Adding a
  sanitized numeric header parser later (for example a per-minute limit
  with a proven 60s window) activates those rows without changing the
  mapping.
- `unsupported_options` currently has one closed bit
  (`response_format`); new options must be added to the closed enum in
  `internal/provider/adaptive` with their own bit, never as free text.
  The bit is **metadata-only** (durable evidence and provenance): no
  runtime consumer exists yet, because the CLI never sends
  `response_format` today. A future option-sending path must read the bit
  before composing requests; until then nothing else reads it.
- Concurrency learning is metadata-only until the governor supports lanes
  with `MaxInFlight > 1` (an explicit non-goal of #93).

## Test coverage

- `internal/provider/adaptive`: mapping rules (success never learns,
  absence never becomes information, proven numbers only, closed options,
  determinism, direction guarantees, conservative subset).
- `internal/provider/compat`: family-neutral observation translation,
  metadata fallbacks, ambiguity, no sensitive text.
- `internal/provider`: `unsupported_options` bitmask accumulation,
  configured-replay refusal, authoritative relaxation.
- `internal/agent`: observer called exactly once per admitted attempt,
  not called on denials, observer error stops retry, runs with retry
  disabled, cancel during backoff.
- `cmd/runstead` E2E: cooldown learning across all three protocol
  families with structured task references and zero extra requests;
  success writes nothing; enforced bound refuses oversized transcripts
  pre-dispatch; restart preserves evidence and learned cooldown paces
  retries; identity isolation; corrupt-profile fail-closed; no
  prompt/secret text ever reaches the profile table.