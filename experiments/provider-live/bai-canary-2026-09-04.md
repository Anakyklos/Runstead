# B.AI live canary report

- Status: **blocked, not a compatibility success**
- Model discovery timestamp: `2026-09-04T01:08:12Z` (UTC); live smoke outcomes were collected later on 2026-09-04 UTC
- Runstead commit: `bdbc004`
- Provider ID: `bai-canary-openai`
- Protocol family: `openai_compatible`
- Endpoint: `https://api.b.ai/v1`
- Adapter version: `compatible-provider-v0.1`
- Route safety: single attempt, single-attempt guarantee, internal retries/cooldown replay/account pooling/automatic fallback/combo routing disabled

## Model discovery

A single authenticated `GET /v1/models` request returned HTTP 200 and 46 model IDs. The operator narrowed the eligible set to:

- `hy3`
- `mimo-v2.5`
- `glm-5.3-flash`
- `qwen3.8-flash`

The four eligible IDs were each exercised as an explicit, isolated selection. No further model selection was made after these results.

## Sanitized live evidence projections

The blocks below are the minimum field-level projections copied from the
captured `record.txt` and selected `inspect.txt` output of the existing
`experiments/provider-live/run.sh`. They preserve the live task-to-provider,
governed-attempt and delivery chain without copying prompts, responses or
headers. The original scratch outputs were removed after the experiment; no
live call was repeated to produce this review fix. Upstream request-id hashes
are omitted here because they are not needed to establish the attempt chain.

The existing session timestamps establish the role and order: qwen was the
first eligible selection, so it is the **primary canary run**. The later glm,
mimo and hy3 runs are **bounded manual diagnostic follow-up** after the
primary's protocol failure. They are not four canaries, not a benchmark, not
automatic fallback and not model routing.

### Primary canary run: `qwen3.8-flash`

```text
selection_role=primary_canary
started_at=2026-09-04T01:36:18Z
task_id=cli-1788485778603185383
provider_id=bai-canary-openai
protocol_family=openai_compatible
model=qwen3.8-flash
adapter_version=compatible-provider-v0.1
config_identity=provider.Config{ProviderID:"bai-canary-openai" ProtocolFamily:"openai_compatible" Endpoint:"https://api.b.ai/v1" Model:"qwen3.8-flash" AuthRequirement:"reference_required" AuthRef:true Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1, SingleAttempt:0x1, InternalRetries:0x1, CooldownReplay:0x1, AccountPooling:0x1, AutomaticFallback:0x1, ComboRouting:0x1} ConfigVersion:"bai-canary-2026-09-04-qwen3.8-flash"}
terminal_outcome=corrections_exhausted
terminal_reason=protocol correction exhausted: invalid_action_schema
smoke_exit=21
attempt_count=3
attempt=exec-000001 request=cli-1788485778603185383-0001 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000002 request=cli-1788485778603185383-0002 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000003 request=cli-1788485778603185383-0003 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
accepted_runstead_protocol_turn=false
```

### Bounded manual diagnostic follow-up: `glm-5.3-flash`

```text
selection_role=bounded_manual_diagnostic_follow_up
started_at=2026-09-04T01:37:43Z
task_id=cli-1788485863975384146
provider_id=bai-canary-openai
protocol_family=openai_compatible
model=glm-5.3-flash
adapter_version=compatible-provider-v0.1
config_identity=provider.Config{ProviderID:"bai-canary-openai" ProtocolFamily:"openai_compatible" Endpoint:"https://api.b.ai/v1" Model:"glm-5.3-flash" AuthRequirement:"reference_required" AuthRef:true Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1, SingleAttempt:0x1, InternalRetries:0x1, CooldownReplay:0x1, AccountPooling:0x1, AutomaticFallback:0x1, ComboRouting:0x1} ConfigVersion:"bai-canary-2026-09-04-glm-5.3-flash"}
terminal_outcome=provider_failure
terminal_reason=provider failure: timeout
smoke_exit=28
attempt_count=3
attempt=exec-000001 request=cli-1788485863975384146-0001 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000002 request=cli-1788485863975384146-0002 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000003 request=cli-1788485863975384146-0003 status=failed delivery_state=sent_confirmed outcome=timeout upstream_reached=true debited=1
accepted_runstead_protocol_turn=false
```

### Bounded manual diagnostic follow-up: `mimo-v2.5`

```text
selection_role=bounded_manual_diagnostic_follow_up
started_at=2026-09-04T01:40:03Z
task_id=cli-1788486003254888595
provider_id=bai-canary-openai
protocol_family=openai_compatible
model=mimo-v2.5
adapter_version=compatible-provider-v0.1
config_identity=provider.Config{ProviderID:"bai-canary-openai" ProtocolFamily:"openai_compatible" Endpoint:"https://api.b.ai/v1" Model:"mimo-v2.5" AuthRequirement:"reference_required" AuthRef:true Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1, SingleAttempt:0x1, InternalRetries:0x1, CooldownReplay:0x1, AccountPooling:0x1, AutomaticFallback:0x1, ComboRouting:0x1} ConfigVersion:"bai-canary-2026-09-04-mimo-v2.5"}
terminal_outcome=provider_failure
terminal_reason=provider failure: upstream_server_failure
smoke_exit=28
attempt_count=3
attempt=exec-000001 request=cli-1788486003254888595-0001 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000002 request=cli-1788486003254888595-0002 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000003 request=cli-1788486003254888595-0003 status=failed delivery_state=completed outcome=upstream_server_failure upstream_reached=true debited=1
accepted_runstead_protocol_turn=false
```

### Bounded manual diagnostic follow-up: `hy3`

```text
selection_role=bounded_manual_diagnostic_follow_up
started_at=2026-09-04T01:41:25Z
task_id=cli-1788486085626063924
provider_id=bai-canary-openai
protocol_family=openai_compatible
model=hy3
adapter_version=compatible-provider-v0.1
config_identity=provider.Config{ProviderID:"bai-canary-openai" ProtocolFamily:"openai_compatible" Endpoint:"https://api.b.ai/v1" Model:"hy3" AuthRequirement:"reference_required" AuthRef:true Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1, SingleAttempt:0x1, InternalRetries:0x1, CooldownReplay:0x1, AccountPooling:0x1, AutomaticFallback:0x1, ComboRouting:0x1} ConfigVersion:"bai-canary-2026-09-04-hy3"}
terminal_outcome=corrections_exhausted
terminal_reason=protocol correction exhausted: malformed_json
smoke_exit=21
attempt_count=3
attempt=exec-000001 request=cli-1788486085626063924-0001 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000002 request=cli-1788486085626063924-0002 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000003 request=cli-1788486085626063924-0003 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
accepted_runstead_protocol_turn=false
```

## First Runstead protocol turn

The unchanged CLI and the existing `openai_compatible` adapter reached the B.AI chat endpoint for every eligible model. Each physical attempt was admitted and debited separately by the governor. No provider fallback, hidden retry, key rotation, or account rotation occurred.

| Model | Durable live result | Attempts | Delivery evidence |
| --- | --- | ---: | --- |
| `qwen3.8-flash` | `corrections_exhausted`, `invalid_action_schema` | 3 | 3 upstream-reached responses, each `delivery_state=completed`, each debited once |
| `glm-5.3-flash` | `provider_failure`, `timeout` | 3 | 2 upstream-reached completed responses, then one upstream-reached `sent_confirmed` timeout, each debited once |
| `mimo-v2.5` | `provider_failure`, `upstream_server_failure` | 3 | 2 upstream-reached completed responses, then one upstream-reached server failure, each debited once |
| `hy3` | `corrections_exhausted`, `malformed_json` | 3 | 3 upstream-reached responses, each `delivery_state=completed`, each debited once |

The model protocol turn did not produce an accepted `runstead.protocol.v1` action or final response. The primary qwen run and the bounded glm/hy3 follow-ups produced protocol correction failures. The mimo follow-up reached an upstream server failure after protocol corrections. These are classified as model protocol/provider outcomes, not transport retries.

## Requirement traceability

The following matrix maps each acceptance requirement to the concrete public
path or repository check that was actually exercised. A blocked row is an
explicit negative result, not an inferred success.

| Requirement | Concrete check | Observed result |
| --- | --- | --- |
| Discover the real model catalog | One authenticated `GET https://api.b.ai/v1/models` before model selection | HTTP 200; 46 IDs returned; only the four operator-authorized IDs were used afterward |
| Use one real selected model through the existing adapter | Existing `experiments/provider-live/run.sh` with the built CLI and one explicit provider declaration per model | All four selections reached B.AI through `protocol_family=openai_compatible`; no B.AI adapter or provider branch was added |
| Complete the first `runstead.protocol.v1` turn | Real `runstead run` followed by durable `runstead inspect` for each live task | **Not met**: no eligible model produced an accepted action or final response; parser/provider outcomes are recorded above |
| Prove governor admission and physical attempt accounting | Inspect durable attempt records and the sanitized live records emitted by the official harness | Every recorded physical attempt was admitted and debited once; each model stopped after three governed attempts; no hidden retry, fallback or rotation was observed |
| Preserve delivery uncertainty and classify failures honestly | Review the real adapter delivery fields and terminal outcomes, including timeout and upstream failure cases | Completed, `sent_confirmed` timeout, and upstream server failure remained distinct; protocol failures were not converted into transport retries |
| Complete `inspect → scoped write → declared recipe/test → process result → independent verification → durable evidence` | Run the real CLI coding path with acceptance, write policy and recipe inputs | **Not executed**: the prerequisite accepted protocol action never occurred, so no coding-loop or verifier evidence is claimed |
| Prove interruption/resume without replay | `runstead resume <task-id>` using the identical provider, model, config, profile and state directory after a durable coding effect | **Not executed**: there was no honest durable coding effect to interrupt; no resume success is claimed |
| Keep durable state authoritative and secrets out of persistence | `runstead inspect`, sanitized harness records, repository diff scan, environment/file cleanup | Provider identity, model, adapter, attempts and delivery were persisted without a credential value; the temporary credential environment was unset and scratch file removed; no secret-shaped value or authorization header is in the branch |
| Keep normal CI opt-in and offline | `go test ./...`, `go vet ./...`, `go build ./cmd/runstead`, `go test -race ./...`, `bash experiments/protocol/test.sh`, and the harness opt-in tests | All required offline gates passed; the live script refuses to invoke the binary without explicit opt-in, so normal tests make zero live calls |
| Avoid provider-specific redesign and preserve exact claims | `git diff`, `git diff --check`, compatibility-document review and PR metadata | The only committed change is this sanitized report; `docs/provider-compatibility.md` has no positive B.AI claim; PR #126 targets `main`, is not merged, and references #122 |

The live acceptance path was therefore exercised through the real CLI,
governor, existing adapter, B.AI endpoint and durable inspection boundary, not
through a synthetic HTTP server. The downstream coding and recovery interfaces
were deliberately left unclaimed because their live prerequisite was blocked.

## Coding loop and recovery

- Inspect, scoped write, declared recipe, process result and independent verification: **not executed**. No model produced an accepted first protocol action.
- Interruption/resume: **not executed**. There was no durable coding effect to interrupt and resume honestly.
- Verifier result: **none**. No accepted action produced an observation, so no evidence IDs were generated.
- Durable state: each smoke task persisted the sanitized provider identity, exact model, adapter version, governed attempt count and delivery outcome. The persisted configuration exposed only the presence of an external authentication reference. No credential value was present.

## Classification and limitation

This experiment proves that the current Runstead path can resolve the configured provider, admit and account for physical OpenAI-compatible requests, reach `https://api.b.ai/v1`, classify delivery outcomes and persist sanitized state. It does **not** prove the required B.AI canary because no eligible model completed the Runstead protocol turn, and the coding and interruption/resume stages were therefore not reached.

Two preliminary model selections made before the operator corrected the eligible set returned HTTP 403 and are excluded from the canary evidence. They are not used to claim model compatibility.

The B.AI promotional/free status observed during this test is upstream mutable state and is not a permanent Runstead provider/model capability.

No API key, Authorization header, private account identifier, raw private prompt or raw private response was committed as evidence.

No provider-specific Runstead code was added. `docs/provider-compatibility.md` was not updated with a positive claim because the Definition of Done was not met.
