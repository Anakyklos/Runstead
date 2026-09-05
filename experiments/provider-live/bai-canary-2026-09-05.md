# B.AI live canary rerun report

- Status: **partially exercised, blocked at Stage 3; not a compatibility completion**
- Model discovery timestamp: `2026-09-05T00:48:47Z` (UTC)
- Runstead commit: `92640c8`
- Provider ID: `bai-canary-openaiprotocol`
- Protocol family: `openai_compatible`
- Endpoint: `https://api.b.ai/v1`
- Selected model: `qwen3.8-flash`
- Adapter version: `compatible-provider-v0.1`
- Provider configuration: `auth_requirement=reference_required`, `auth_ref=BAI_API_KEY`, `auth_ref_present=true`; no credential value was placed in the provider document
- Route safety: single attempt, single-attempt guarantee, internal retries/cooldown replay/account pooling/automatic fallback/combo routing disabled

## Model discovery

One authenticated `GET https://api.b.ai/v1/models` request returned HTTP 200 and 47 model IDs. The previously recorded primary model `qwen3.8-flash` was present and was selected explicitly. No other model was queried or exercised during this rerun.

## Sanitized live evidence projections

The projections below were derived from the existing `experiments/provider-live/run.sh` `record.txt` and selected structured fields from `inspect.txt`. They omit prompts, model responses, authorization headers, request bodies, cookies and private identifiers.

### Primary canary protocol turn

```text
selection_role=primary_canary
started_at=2026-09-05T00:50:11Z
task_id=cli-1788569411062735925
provider_id=bai-canary-openaiprotocol
protocol_family=openai_compatible
model=qwen3.8-flash
config_identity=provider_id:bai-canary-openaiprotocol protocol_family:openai_compatible endpoint:https://api.b.ai/v1 model:qwen3.8-flash auth_requirement:reference_required auth_ref_present:true profile_version:v1 config_version:bai-canary-2026-09-05-qwen3.8-flash
adapter_version=compatible-provider-v0.1
terminal_outcome=completed
terminal_reason=verification passed: completion verified: acceptance check passed (readme)
attempt_count=2
governor_admission=admitted
debit_accounting=each_recorded_attempt_debited_once
attempt=exec-000001 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
attempt=exec-000004 status=completed delivery_state=completed outcome=success upstream_reached=true debited=1
tool_effect=exec-000003 tool=read_file status=completed evidence_id=obs-000001
verifier_checks=evidence_claims_typed:passed,evidence_grounded:passed,git_observed:passed,no_pending_approvals:passed,no_uncertain_attempts:passed,readme:passed,writes_reconciled:passed
accepted_runstead_protocol_turn=true
```

The first local invocation with the same explicit model omitted an operator acceptance plan and therefore ended as `verification_blocked` after two successful provider deliveries. It is retained as a configuration-correction observation, not counted as a protocol success or as a model retry. The primary projection above is the corrected run with the minimal acceptance plan required by the existing verifier.

### Coding task

```text
selection_role=primary_canary_stage3
started_at=2026-09-05T00:50:41Z
task_id=cli-1788569441815152041
provider_id=bai-canary-openaiprotocol
protocol_family=openai_compatible
model=qwen3.8-flash
config_identity=provider_id:bai-canary-openaiprotocol protocol_family:openai_compatible endpoint:https://api.b.ai/v1 model:qwen3.8-flash auth_requirement:reference_required auth_ref_present:true profile_version:v1 config_version:bai-canary-2026-09-05-qwen3.8-flash
adapter_version=compatible-provider-v0.1
terminal_outcome=provider_failure
terminal_reason=provider failure: timeout
attempt_count=14
governor_admission=admitted
debit_accounting=each_recorded_attempt_debited_once
completed_delivery_attempts=13
final_attempt=exec-000040 status=failed delivery_state=sent_confirmed outcome=timeout upstream_reached=true debited=1
observed_tool_evidence=obs-000001,obs-000002,obs-000003,obs-000004,obs-000006,obs-000007,obs-000009,obs-000010,obs-000011,obs-000012,obs-000013
observed_effects=list_files,read_file,write_file,run_recipe
independent_verifier=not_reached_after_provider_timeout
```

The coding run reached the real Runstead tools and durable SQLite state. It recorded read/list/write effects and recipe attempts, but the provider timed out before a verified terminal completion. No automatic fallback, model switch, account rotation or hidden retry was used. The run was not blindly replayed.

## Stage traceability

| Stage | Public path and evidence | Result |
| --- | --- | --- |
| Model discovery | Authenticated `GET /v1/models` | **Passed**: HTTP 200; 47 IDs; `qwen3.8-flash` present |
| Provider resolution and first protocol turn | `runstead run` through `openai_compatible`, followed by `runstead inspect` | **Passed**: accepted action/final path, verifier passed, durable outcome `completed` |
| Coding task | Existing CLI with disposable workspace, acceptance plan, `fixtures/coding-loop/recipes.json`, write policy and recipe policy | **Blocked**: real tools and durable effects occurred, then provider timeout after 14 governed attempts |
| Interruption/resume | Same provider/model/config/state after deliberate interruption | **Not executed**: Stage 3 did not reach a verified coding completion, so no interruption/resume success is claimed |
| Final #122 acceptance | Coding verifier and recovery proof | **Not met** |

## Safety and evidence handling

The B.AI promotional/free status observed during this test is upstream mutable state and is not a permanent Runstead provider/model capability.

No API key, Authorization header, private account identifier, raw private prompt or raw private response was committed as evidence. The credential was used only in the temporary process environment. The temporary provider document contained only `auth_ref: BAI_API_KEY`, and the scratch artifacts were removed after extracting these projections.

`docs/provider-compatibility.md` was not updated with a positive B.AI compatibility claim. Issue #122 remains open. The observed result proves the exact exercised provider-resolution, adapter, governor, protocol and verifier path for the selected Stage 2 task, but it does not prove the complete inspect/write/recipe/verify plus interruption/resume acceptance sequence.
