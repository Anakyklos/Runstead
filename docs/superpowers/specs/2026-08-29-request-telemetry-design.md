# Minimal Request Telemetry for Provider Metadata Design

**Date:** 2026-08-29
**Issue:** #39
**Base:** `origin/main` at `97bcf21` (post-#93 conservative provider learning)

## Goal

Extend the provider-neutral sanitized `provider.ResponseMetadata` with a small,
typed request-telemetry surface so trace output and governor ledger events can
carry the minimal facts needed to reason about account health and protocol
drift, without ever storing or rendering prompts, response bodies, credentials,
raw headers or raw session identity. The three compatibility adapters
(#87/#88/#89) and the legacy OmniRoute adapter populate the fields only from
evidence they can prove; every unmeasurable field stays absent or zero-valued
rather than guessed.

## Scope

Implemented now (provider-generic core of #39):

- `AdapterVersion` — pinned adapter/composition version, shared with the
  existing `compat.AdapterVersion` identity so execution evidence and
  telemetry never disagree;
- `Transport` — stable transport identifier per adapter family;
- `FirstTokenLatency` — first-response-byte latency when the adapter's
  `httptrace` observation proves it (zero otherwise);
- `SessionFingerprint` semantics — the existing sanitized `SessionID` field
  is the sha256 fingerprint; raw session/connection identity never enters
  metadata;
- `RetryCount` and `Fallback` — always zero/false on the protected lane: no
  retry or fallback exists outside the governor (#92), and the fields exist
  so no future transport can hide amplification;
- `UsageEstimated` — false from every current adapter (none emits usage);
  reserved so a transport that reports estimated usage (e.g. the OmniRoute
  web path's upstream-reported estimates) must declare it;
- governor `Event` attempts carry the sanitized metadata and the trace sink
  renders it;
- redaction tests cover the new fields.

Deferred to the plugin track (with #80/#82/#83/#74 and #86): the
browser-provider outcome refinements (`incomplete`, `needs_user_action`,
`session_lost`, `adapter_drift`, request/assistant fingerprints, opaque
conversation/session references, bounded retention). No current adapter can
produce those outcomes; adding unused states would be speculative. The
metadata contract remains provider-neutral so the deferred surface can extend
it without breaking the core.

## Architecture

`provider.ResponseMetadata` (in `internal/provider/provider.go`) gains six
typed fields with conservative zero values. Each adapter stamps its identity
(version + transport) into every metadata record it constructs, including
pre-dispatch refusal records, and measures `FirstTokenLatency` from the same
`httptrace` observation it already uses for delivery state, using the
injected clock (`Options.Now` / `c.now`) so tests stay deterministic.

Version identity stays in one place: `internal/provider` owns
`CompatAdapterVersion` as the canonical constant, `compat.AdapterVersion`
becomes an alias of it (existing callers keep working), and the OmniRoute
adapter owns its own pinned `AdapterVersion` constant. No import cycles are
introduced: adapters already depend on `internal/provider`, and neither
`compat` nor the adapters import each other's version constants.

The governor flow already classifies the outcome from the full `provider.Response`
inside `Execute`. The design propagates evidence through the existing typed
records: `Outcome` gains a sanitized `Metadata` copy set by `Execute` right
after classification, `governor.Event` gains the same sanitized copy, the
permit `attempt_finished` emitters copy it from the outcome, and
`trace.PolicySink` renders a single sanitized `attempt` group. `RouteSafety`,
receipts, persistence, budgets, circuits, policies and the durable
`ProviderFinished` projection are unchanged: telemetry is event/trace-level
evidence, not durable task truth.

## Sanitization contract

- `AdapterVersion` and `Transport` are fixed strings pinned by the adapter
  build; they carry no remote influence.
- `SessionID` is populated only from `hashOpaque(...)` (sha256 truncated,
  `sha256:` prefixed) and is documented as the session fingerprint. A
  non-empty `SessionID` that is not a `sha256:` digest is a contract
  violation covered by redaction tests.
- `FirstTokenLatency` is a duration; absent observation keeps it zero. No
  adapter synthesizes a latency from wall-clock guessing.
- `RetryCount`, `Fallback`, `UsageEstimated` are zero/false on the protected
  lane; nothing in the adapters, governor or trace may ever set them to
  non-zero values, and no current adapter reports usage at all.
- Traces render only sanitized metadata: no prompt, response body,
  authorization value, API key, cookie, raw session/connection id or
  arbitrary remote error text.

## Execution gates

Telemetry never gates execution: it is evidence after admission. `RouteSafety`
remains the executable declaration for amplification and attempt accounting;
no telemetry field can admit, refuse, retry or route. The governor
`attempt_finished` events become richer only. Nothing in this change can
increase governor authority, bypass receipts, or turn estimated usage into
accounting.

## Testing

- Unit: zero-value policy — the zero `ResponseMetadata` renders absent/zero
  for every new field.
- Unit: per-adapter metadata population from a fake response — version and
  transport stamped on success, transport error and pre-dispatch refusal
  paths; `FirstTokenLatency` measured when the `httptrace` observation fires
  and zero otherwise.
- Unit: governor propagation — `Execute` fills `Outcome.Metadata` and the
  `attempt_finished` event carries the sanitized copy.
- Unit: trace redaction — `trace.PolicySink` output for populated metadata
  contains the telemetry fields, never forbidden words, and renders
  `SessionID`/`RequestID` only in hash form.
- Integration: live-path metadata shape from the OmniRoute adapter using the
  existing contract mock (`internal/provider/omniroute/testdata/contract`),
  asserting hash-formatted session fingerprint, first-token latency policy
  and zero retry/fallback/usage fields.
- Keep fixture-hygiene and all existing suites green; no new fixture
  framework, dependency or schema migration.