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

## Continuation evidence: 2026-09-05T14:02:00Z

This section records the continuation attempt after the maintainer requested
that work resume. It does not upgrade the compatibility claim.

- Final branch head: `fbca69d` (`ba2da22` was an experimental local adapter
  change and was reverted because its causal relationship to the timeout was
  not proven; the final branch has no net runtime change over `main`).
- Fresh Stage 3 follow-up binary: built from `ba2da22` before that experiment
  was reverted. The attempted role-splitting change did not produce a
  successful task and was not retained as a runtime fix.
- One authenticated model discovery request completed at
  `2026-09-05T12:56:45Z` UTC. It returned 48 model IDs and included the
  previously selected `qwen3.8-flash`; no model sweep was performed.
- The prescribed provider identity stayed unchanged: provider ID
  `bai-canary-openaiprotocol`, family `openai_compatible`, endpoint
  `https://api.b.ai/v1`, model `qwen3.8-flash`, adapter
  `compatible-provider-v0.1`, and `auth_ref_present=true` for the external
  `BAI_API_KEY` reference.

### Fresh Stage 3 follow-up

The disposable workspace was initialized from the fixture as its own Git
repository. Its tracked diff was zero at task bootstrap; operator-controlled
provider/configuration and run/state paths were untracked by design, so this
negative attempt does not claim a clean Git worktree.

```text
task_id=cli-1788614513382085071
workspace=/home/pedro/.jcode/scratch/stage3
provider_id=bai-canary-openaiprotocol
protocol_family=openai_compatible
model=qwen3.8-flash
config_identity=provider.Config{ProviderID:"bai-canary-openaiprotocol" ProtocolFamily:"openai_compatible" Endpoint:"https://api.b.ai/v1" Model:"qwen3.8-flash" AuthRequirement:"reference_required" AuthRef:true Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1, SingleAttempt:0x1, InternalRetries:0x1, CooldownReplay:0x1, AccountPooling:0x1, AutomaticFallback:0x1, ComboRouting:0x1} ConfigVersion:"bai-canary-2026-09-05-qwen3.8-flash-stage3"}
terminal_outcome=provider_failure
terminal_reason=provider failure: timeout
attempt_count=1
governor_admission=admitted
attempt=exec-000001 request=cli-1788614513382085071-0001 status=failed delivery_state=sent_confirmed outcome=timeout upstream_reached=true debited=1
observed_tools=none
observed_effects=none
evidence_ids=none
independent_verifier=not_reached
```

`runstead inspect cli-1788614513382085071` independently inspected the
durable SQLite row and reported the same provider, family, exact model and
configuration identity. It reported no tool attempts, process attempts,
verification or Git observation because the first model-effect request timed
out before the model proposed an action. The attempt lasted approximately the
adapter's fixed 60-second per-call bound. No timeout, attempt, RPM,
concurrency, retry or governor ceiling was increased.

A separate minimal, direct qwen request to the documented Chat Completions
endpoint returned HTTP 200 in approximately 7.5 seconds. That diagnostic was
not a Runstead task, was not used as acceptance evidence, and did not change
the selected model or configuration. It shows that the endpoint and
credential were reachable at that moment, but it does not explain the
long-context Runstead timeout or prove Stage 3.

### Repeated 400 diagnostic reported during continuation

The operator also reported two identical `400 Bad Request` responses for
`glm-5.3-flash`. The displayed client used auth reference
`JCODE_PROVIDER_BAI_API_KEY`, not the required `BAI_API_KEY` reference. The
message was an upstream `invalid_request` / `Invalid request body` response.
This is not counted as #122 evidence: it used a different model and secret
reference, and the message format is not emitted by the current Runstead
adapter. A 400 means the server received the request but rejected its body or
endpoint/model parameters; it is not evidence of a transient timeout. No
automatic model rotation or retry was performed.

### Updated stage result

| Stage | Continuation evidence | Result |
| --- | --- | --- |
| Stage 3 | Fresh task `cli-1788614513382085071`, SQLite inspection, one admitted physical attempt `exec-000001` | **Blocked**: timeout before inspect/write/recipe/verifier effects |
| Stage 4 | No positively completed Stage 3 task exists to use as the prerequisite trajectory | **Not executed** |
| Final #122 acceptance | Required Stage 3 completion plus same-config interruption/resume and no-replay proof | **Not met** |

The timeout is now observed on a fresh Stage 3 trajectory as well as the
earlier continuation after real successful provider turns. The evidence is
consistent with recurring provider/model latency near the existing 60-second
adapter bound, but it is not sufficient to claim a deterministic Runstead
defect. The run was not replayed after the uncertain delivery. Stage 4 was not
attempted behind the failed Stage 3 gate.

No raw model transcript, private prompt, private response, Authorization
header, credential value or unredacted upstream request ID was added to this
report. No credential was persisted in the repository, provider declaration,
SQLite state, logs, evidence or documentation.

## Current execution preflight: 2026-09-05T15:09:51Z

This issue branch was created from `main` at Runstead commit `971644a`
(`fix(protocol): make the Runstead envelope contract self-describing (#127)`).
The mandatory deterministic preflight was run with the locally installed Go
1.22.12 toolchain:

- `go build ./cmd/runstead`: passed;
- `go test ./...`: passed;
- `go vet ./...`: passed;
- `bash experiments/protocol/test.sh`: passed;
- formatting and `git diff --check`: clean.

The required external `BAI_API_KEY` secret reference was absent from the
process environment. Per the canary contract, no alternate reference was
invented, the credential value supplied out of band was not copied into the
environment or any command, and no unauthenticated or otherwise substituted
model-list request was made. Therefore the required single authenticated
`GET https://api.b.ai/v1/models` preflight could not be executed in this
environment.

### Gate result for this execution

| Stage | Result | Evidence |
| --- | --- | --- |
| Authenticated model discovery | **Blocked** | `BAI_API_KEY` absent; zero model-list requests made by this execution |
| Stage 3 | **Blocked before start** | No fresh workspace task, provider attempt, tool effect, recipe, verifier result or new evidence ID was created |
| Stage 4 | **Not executed** | Stage 3 prerequisite was not positively met |
| Compatibility documentation | Unchanged | No new live operational claim is justified |

This section is an environment blocker, not a provider failure and not a
Runstead defect. The earlier sanitized Stage 2 and Stage 3 observations above
remain historical evidence with the limitations already stated. A future
rerun must provide the existing external `BAI_API_KEY` reference, repeat the
single model discovery request, and then independently satisfy both remaining
gates. No secret, raw model transcript, private prompt/response, request
header or credential-shaped value was added to this report or persisted by
this execution.
