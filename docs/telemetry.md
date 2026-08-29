# Request telemetry (issue #39)

Runstead keeps its own minimal, sanitized per-attempt request telemetry so
operators can reason about account health and protocol drift without relying
on transport-side claims.

## Contract

Every `provider.ResponseMetadata` record may carry:

- `adapter_version`: pinned adapter/composition version
  (`provider.CompatAdapterVersion` for the compatible families, an adapter-owned
  constant for legacy transports);
- `transport`: stable transport identifier (`openaicompat-http`,
  `anthropiccompat-http`, `googlecompat-http`, `omniroute-http`);
- `session_fingerprint`: sha256 fingerprint of the opaque session/connection
  identity, never the raw identity;
- `first_byte_latency`: request-start to first observed response byte (HTTP
  response-header arrival) when the adapter's transport observation proves
  it. First-TOKEN latency is never claimed: the non-streaming lane cannot
  observe a model token separately from the body, so any such number would
  be a guess;
- `retry_count` and `fallback`: the protected lane has no retries or fallbacks
  outside the governor (#92), so both are always zero;
- `usage_estimated`: false today (no adapter emits usage); reserved so a
  transport that reports estimated usage must declare it.

## Zero-value rule

Unmeasurable fields are absent or zero; Runstead never guesses latency, usage
or amplification. This is enforced by adapter unit tests, the governor
zero-outcome test and the trace redaction coverage.

## Surface

Governor `attempt_finished` events carry the sanitized metadata and
`trace.PolicySink` renders it as an `attempt` group. The fields are
event/trace-level evidence: they are not durable task truth, and they never
gate admission, retries, policy or accounting.

## Redaction

Traces and metadata exclude prompts, response bodies, credentials, cookies,
raw headers and raw session/connection identity. `RequestID` and `SessionID`
are sanitized (hash-formatted); redaction tests cover the telemetry fields.

## Deferred

The browser-provider outcome refinements (incomplete responses, user-action
needs, session loss, adapter drift, request/assistant fingerprints, opaque
conversation references, bounded retention) are deferred to the
plugin/composable-provider track (#80, #82, #83, #74, #86).