# Provider compatibility matrix — v0.1 acceptance gate (issue #14)

Issue #14 closes the provider-neutral acceptance gap: the same authoritative
Runstead runtime executes through configured endpoints of all three supported
protocol families without provider-specific control-plane behavior. This
document separates, explicitly and honestly:

- **deterministic contract proof** — what the shared compatibility suite and
  the E2E coding loop prove with local `httptest` endpoints;
- **live operational proof** — what has been exercised against real
  configured endpoints with operator credentials;
- **unproven claims** — anything asserted but not backed by the evidence
  above;
- **unsupported features** — what the v0.1 baseline deliberately does not do.

## Terminology

- **Provider identity** (`provider_id`): the operator-chosen name of one
  configured endpoint. Two different provider IDs may share one protocol
  adapter.
- **Protocol family**: the compatibility wire contract the endpoint speaks —
  `openai_compatible`, `anthropic_compatible` or `google_compatible`.
- Identity and family are distinct concepts. Neither is derived from the
  other, and the agent loop never branches on either ((#79/#86)).

## Compatibility matrix (three families)

| Property | `openai_compatible` | `anthropic_compatible` | `google_compatible` |
| --- | --- | --- | --- |
| Adapter | `internal/provider/openaicompat` (#87) | `internal/provider/anthropiccompat` (#88) | `internal/provider/googlecompat` (#89) |
| Deterministic contract proof (httptest) | proven | proven | proven |
| Identity vs family distinct | proven | proven | proven |
| Two provider IDs share one adapter (runtime) | proven | proven | proven |
| 1 governor admission -> 1 physical request | proven | proven | proven |
| No hidden retry / fallback / fan-out | proven | proven | proven |
| Redirect never followed into a second request | proven | proven | proven |
| Cancel before dispatch -> zero requests | proven | proven | proven |
| Cancel after possible dispatch -> conservative delivery, no replay | proven | proven | proven |
| Pre-dispatch fail-closed config/capability/auth | proven | proven | proven |
| Secrets absent from errors/identity/metadata/durable state | proven | proven | proven |
| Full inspect-edit-test-fix E2E (real runtime, real git, real recipes) | proven | proven | proven |
| Crash/interruption + resume through the same provider | proven | proven | proven |
| Wire details never become task truth | proven | proven | proven |
| Live operational proof (real endpoint + real credentials) | **operationally unproven** | **operationally unproven** | **operationally unproven** |

**Live status:** no family has been exercised against a real configured
endpoint in the environment where this gate was run, because no operator
credentials/endpoint access were available. All three families are therefore
reported **operationally unproven**. Mocks and `httptest` are deterministic
contract proof, not live operational proof. See
[Live opt-in smoke](#live-opt-in-smoke-procedure).

## Deterministic contract proof

The shared provider-neutral suite (`internal/provider/compat/matrix_test.go`)
runs one harness across the three families against local `httptest` endpoints
that speak each family's wire subset. It proves:

- provider ID and protocol family are distinct; two IDs share one adapter
  with no agent-loop change;
- exactly one physical model-effect HTTP request per normal governed
  completion (requests are counted on the synthetic server);
- no retry, no fallback, no redirect following (a 3xx is refused after
  exactly one request);
- cancellation before dispatch produces provably zero requests;
- timeout/cancel after possible dispatch stays conservatively uncertain
  (never `not_sent`) and never auto-replays;
- fail-closed resolution before dispatch for: unknown provider ID, unknown
  protocol family, missing required capability, missing model, invalid
  endpoint, endpoint carrying credentials, required-but-missing auth
  reference, unknown auth requirement, incompatible route safety and
  credential-shaped options;
- secrets never appear in errors, sanitized identity, response metadata or
  fixtures; a missing secret refuses before any request.

The runtime E2E (`cmd/runstead/provider_e2e_test.go`) runs the SAME
#12 inspect-edit-test-fix trajectory through each family: real agent loop,
real account governor, real tools, real `go test` recipe failures and passes,
real scoped writes, real Git observation, real acceptance verifier and real
SQLite durability. The only synthetic seam is the provider-shaped HTTP
endpoint that carries the deterministic `runstead.protocol.v1` text inside
the family response envelope.

Attempt accounting across the E2E is asserted by counting physical requests:
9 scripted model turns -> exactly 9 requests per family run, and exactly 9
across crash+resume (one per governed admission, no replay of completed
effects).

## Configuration surface

The operator declares configured endpoints in one providers file
(`--providers FILE` / `RUNSTEAD_PROVIDERS`) and selects exactly one
(`--provider-id ID` / `RUNSTEAD_PROVIDER_ID`). The document maps one-to-one
onto the #79 contract (`provider.Config`, `CapabilityProfile`,
`RouteSafety`); resolution happens through the existing registry before any
dispatch. The document is explicitly versioned: only `version: 1` is
accepted, and absent/unknown/future versions or trailing JSON are rejected.

```json
{
  "version": 1,
  "providers": [
    {
      "provider_id": "local-gateway-openai",
      "protocol_family": "openai_compatible",
      "base_url": "http://127.0.0.1:8080/v1",
      "model": "my-model",
      "auth_requirement": "reference_required",
      "auth_ref": "MY_ENDPOINT_TOKEN", 
      "options": {},
      "config_version": "v1",
      "profile": {
        "profile_version": "v1",
        "capabilities": ["text_turn", "runstead_protocol"],
        "route_safety": {
          "attempt_accounting": "single",
          "single_attempt": "guaranteed",
          "internal_retries": "disabled",
          "cooldown_replay": "disabled",
          "account_pooling": "disabled",
          "automatic_fallback": "disabled",
          "combo_routing": "disabled"
        }
      }
    }
  ]
}
```

Fields:

- `provider_id` — stable operator identity of this endpoint.
- `protocol_family` — one of `openai_compatible`, `anthropic_compatible`,
  `google_compatible`. Unknown values fail closed.
- `base_url` — exact endpoint root (http/https, no userinfo/query/fragment).
- `model` — the exact model identifier sent with every request.
- `auth_requirement` — explicit `reference_required` or `none`; never
  inferred from the family. `none` plus an `auth_ref` is refused.
- `auth_ref` — NON-SECRET reference to externally held credentials (for
  example an environment variable name). The value itself is resolved at
  dispatch time by the adapter's secret-resolver seam and never enters
  state, metadata, traces or model context.
- `options` — strictly necessary NON-SECRET protocol options. The
  anthropic-compatible baseline requires `max_tokens` and
  `anthropic_version`; option VALUES never appear in configuration identity.
- `profile.profile_version` — explicit versioned capability profile
  (`v1` today).
- `profile.capabilities` — at least `text_turn` and `runstead_protocol` are
  required for execution; missing required capabilities fail closed.
- `profile.route_safety` — the endpoint's executable attempt-safety
  declaration, and it is MANDATORY: absence of evidence cannot be promoted to
  a safe-route guarantee, so omission fails closed before any dispatch.
  Unknown enum values and incompatible declarations also fail closed.
- `config_version` — operator-maintained configuration identity, bumped
  when the meaning changes.

Run commands:

```bash
runstead run --task "..." \
  --workspace /path/to/repo \
  --providers providers.json \
  --provider-id local-gateway-openai \
  --acceptance plan.json \
  --recipes recipes.json --recipe-policy test=allow \
  --write-policy write_file=allow \
  --state-dir /path/to/state
```

The provider-neutral identity of the selected endpoint (provider ID, family,
exact model, sanitized config identity, adapter version) is persisted with
the task configuration and rendered by `runstead inspect` and by the
`Verified runtime result:` projection of a completed run. Every governed
provider attempt also persists `protocol_family`, the sanitized
`config_identity` and the upstream `request_id` ONLY when actually observed,
in its adapter-sanitized (hashed) form; missing/unknown stays empty and is
never guessed.

`runstead resume <task-id>` continues an interrupted provider-backed task
only through the SAME provider declarations and provider ID. Re-supplying a
different provider ID, model or configuration identity fails closed before
the recovery pipeline ("resume never switches providers silently").

## Operational profiles (issue #91)

`runstead run`/`resume` through a configured provider also persists a
**durable, versioned operational profile** keyed by the sanitized
config identity + exact model + protocol family (`profile_key` is a SHA-256
of those three). The profile is operational metadata with evidence
provenance; it is not task truth, not policy, not approval, not retry
authority and not verifier state.

Each variable field (currently: `max_request_bytes`, `max_response_bytes`,
`requests_per_minute`, `cooldown_millis`, `concurrency_ceiling`,
`timeout_millis`) carries:

- the effective value;
- its provenance — `configured` (operator declaration), `observed` (concrete
  runtime evidence actually produced, with a sanitized evidence reference) or
  `authoritative` (accepted through an explicitly typed, contract-reviewed
  path); the honest absent state is **unknown**, never a guessed value;
- the sanitized evidence reference and update timestamp when they exist.

Update rules (enforced by code and tests):

- the conservative direction is defined **per field**: `max_request_bytes`,
  `max_response_bytes`, `requests_per_minute` and `concurrency_ceiling` are
  lower-is-conservative; `cooldown_millis` is higher-is-conservative (a
  longer Retry-After wait is safer, a shorter observation never weakens the
  profile); `timeout_millis` has NO automatic direction (observations are
  never auto-applied; configured/authoritative values are still
  representable);
- observed evidence may only move a field toward its conservative direction
  (or fill an unknown one from a specific produced observation);
- ordinary successful requests never change any value;
- re-supplying the same unchanged configured bounds never undoes an observed
  tightening or authoritative acceptance — updates are applied
  **monotonically at the durable SQLite boundary** inside the same
  transaction that reads the current row (check-and-set), so concurrent
  tasks cannot interleave a stale write that weakens conservative state;
- moving a value in the non-conservative direction for the same unchanged
  identity requires the operator configuration path or the explicitly typed
  authoritative path;
- a config/family/model change derives a different profile key, so old
  learning is never inherited silently.

The profile defines **no admission policy of its own**: it carries no
invented global caps, represents values with provenance for any
configuration the existing #79 contract considers valid, and leaves all
enforcement to the governor and the adapters' existing contracts. Zero is
the single representation of unknown/absence and is never persisted for any
writable provenance.

Persistence: migration `0013_operational_profiles.sql` (additive; existing
databases upgrade without loss). Records are sanitized (no credentials,
prompts, response bodies or headers), survive restarts, and are reconstructed
from durable state without any provider request. Corrupted/inconsistent rows
(key/identity mismatch, invalid provenance, over-cap values) fail closed.
`runstead inspect` renders the section `Operational profile:` with effective
value, provenance, sanitized identity, exact model and protocol family;
absent fields render as `unknown`.

The profile currently records what the runtime genuinely knows
(configured capability bounds); the observation/authoritative ingestion
paths are contract-ready for #92 (bounded retry/cooldown inputs) and #93
(conservative adaptation), which remain out of scope here. The profile cannot
execute retries, grant admission or change provider/model/fallback; the
governor stays the only admission authority.

## Execution rules (unchanged invariants)

- an arbitrary configured compatible endpoint has UNKNOWN upstream
  allowance semantics unless the operator declares them explicitly: the
  provider-neutral run surface defaults to the conservative `unknown`
  allowance profile instead of the historical ChatGPT-Web `plus_go_instant`
  published-quota contract (the legacy scripted/OmniRoute lanes keep their
  historical default);
- local durable state is authoritative;
- providers/models/sessions are replaceable infrastructure;
- the model proposes actions; Runstead validates, authorizes, executes and
  verifies;
- completion depends on observable evidence and the verifier, never on model
  text;
- every physical attempt passes governor admission: one normal governed
  completion -> one physical model-effect request;
- no hidden retries, no automatic fallback (provider/model/key/account/
  session);
- timeout/cancel after possible dispatch stays conservatively uncertain;
- credentials never enter SQLite task truth, evidence, logs, traces,
  fixtures, config identity or model context;
- protocol-family wire types never cross the provider boundary into the
  agent loop, governor, tools or verifier (structural regression:
  `internal/provider/compat/neutrality_test.go`);
- recovery reconstructs state and never blindly re-executes historical
  effects.

## Live opt-in smoke procedure

Live provider tests are explicit opt-in and run **zero traffic** in normal
CI. A live smoke may only run when the operator explicitly enables it and
the external configuration/credentials exist.

```bash
# 1. Build the runtime
go build ./cmd/runstead        # produces ./runstead (or set RUNSTEAD_BIN)

# 2. Declare one endpoint per family you can exercise with real credentials
#    (official vendors, gateways, third-party inference or local services
#    that satisfy the family contract are equally valid).

# 3. Run the opt-in smoke for the exercised family. It fails closed unless
#    RUNSTEAD_LIVE_SMOKE=1 and produces a sanitized live record.
RUNSTEAD_LIVE_SMOKE=1 experiments/provider-live/run.sh \
  --providers providers.json \
  --provider-id my-openai-endpoint \
  --workspace /path/to/repo \
  --acceptance plan.json \
  --output ./live-evidence
```

The live record contains the task id, the sanitized provider ID/family/model/
config identity/adapter version and the durable inspection (governor
admission, attempts and delivery outcomes) — no credentials, no raw request/
response bodies.

Prohibited in live smoke: quota probing, concurrency escalation until
failure, traffic to discover limits, calibration loops, key/account/model
rotation, fallback, rate-limit workarounds, or inventing success when a
credential does not exist. A family that cannot be exercised is reported as
**operationally unproven**; mocks are not live proof.

In the environment where this gate was executed, no family could be
exercised live (no endpoint credentials available). All three families are
therefore recorded as operationally unproven.

## Unsupported / not asserted

- automatic provider/model/key/account/session fallback or rotation;
- provider marketplaces, discovery or automatic model routing;
- generic OpenAI-compatible proxy/server behavior;
- streaming and native tool calling (not required by the #79 baseline);
- full API parity with upstream vendors (only the minimal family wire subset
  is implemented and proven);
- ChatGPT Web / OmniRoute in the v0.1 critical path — deferred
  plugin/composable-provider work (historical research preserved under
  `docs/research/` as provenance).

## Evidence

- `internal/provider/compat/matrix_test.go` — shared deterministic suite;
- `internal/provider/compat/neutrality_test.go` — neutral-runtime regression;
- `cmd/runstead/provider_e2e_test.go` — cross-family E2E + two-identity +
  crash/resume + selection fail-closed;
- `internal/config/providers.go` + `internal/config/providers_test.go` —
  operator declarations;
- `internal/state/migrations/0012_provider_identity.sql` +
  `internal/state/provider_identity_test.go` — durable sanitized identity;
- `experiments/provider-live/run.sh` — opt-in live smoke procedure.
