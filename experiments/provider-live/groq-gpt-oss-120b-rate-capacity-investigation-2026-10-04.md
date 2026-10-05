# Groq GPT-OSS-120B Stage 3 rate/capacity investigation

**Issue:** #151

**Investigation date:** 2026-10-04
**Decision:** **OBSERVABILITY_GAP**

No provider API request was made for this investigation. The external documentation below was read over the web; no Groq endpoint, `/models`, completion, quota probe, or account console was contacted.

## IMPLEMENTED / PROVEN

The v4 canary built Runstead at `7306bc46fdd430e1c83b3d0ba386145b83ca67a4`. The issue base is `ee7461de74bcf5f601d0425eb16a868531491a9d`. A source comparison found no changes between those commits in the prompt, OpenAI-compatible adapter, classifier, governor, or provider-attempt persistence files used below.

The code path is:

1. `internal/agent/transcript.go` creates a system-contract message and a task-objective message. Before each later model call it appends the preceding assistant response and any tool observation, correction, recovery, or verification messages. It renders each as `=== runstead:<role> ===` plus content.
2. `internal/agent/loop.go` passes that full render as `provider.Request.Prompt`.
3. `internal/provider/openaicompat/client.go` sends one `POST` per governed call. `encodeChatCompletionRequest` JSON encodes one user message whose content is the complete rendered prompt, together with the configured model and `stream:false`. The model response text is appended to the transcript before parsing the next action.
4. The adapter reads HTTP status and parses `Retry-After` into transient `ResponseMetadata`. For this adapter, HTTP 429 maps to typed `rate_or_capacity`; the class does not identify a specific quota dimension.
5. The classifier passes `RetryAfter` and `ResetAt` to the governor. The governor records the selected backoff and the outcome. `provider_attempts` and events retain the class, delivery state, uncertainty, debit, and selected backoff, but do not retain the HTTP status, original `RetryAfter`, `ResetAt`, or numeric rate-limit headers.

No exact/canonical tokenizer for `openai/gpt-oss-120b` or this wire protocol was found in the v4 Go source/dependencies. Exact token counts are **unavailable**. No bytes-to-tokens conversion is used.

## V4 DURABLE EVIDENCE

### Source and retained state

- The v4 report is `groq-gpt-oss-120b-canary-v4-2026-10-04.md` at the v4 source commit above.
- The retained Stage 2 SQLite file is `/tmp/runstead-issue149-v4/stage2/state/runstead.db`.
- The retained shared Stage 3 SQLite file is `/tmp/runstead-issue149-v4/stage3/state/runstead.db`.
- Both databases were opened with SQLite URI `mode=ro` and `PRAGMA query_only=ON`. No task or database was changed.
- Stage 2 task `cli-1791148984761603981` is completed. It has two provider attempts, two governor ledger admissions, two debits, zero retries, a successful `read_file` observation `obs-000001`, and verifier decision `passed`.
- Stage 3 initial task `cli-1791149781504182558` and controlled rerun `cli-1791150335547312794` are both failed at provider turn six. Each has six attempts, six admissions/debits, zero retries, six `upstream_reached` outcomes, and zero uncertain deliveries. The final attempt in each task has `delivery_state=completed` and `provider_failure_class=rate_or_capacity`.
- Initial Stage 3 has durable observations `obs-000001` through `obs-000004` for the two `list_files` and two `read_file` actions. Its fifth `apply_patch` action failed as `invalid_patch`; the agent loop appended that failed observation to the in-memory transcript, but no `tool_results` row retained its contents or size, and no write occurred. The rerun has the same four observation types/IDs, then successful stale-state-protected `write_file` observation `obs-000005`. No recipe or verification attempt ran in either trajectory. The rerun write did not produce the reviewed expected file hash, so neither task met Stage 3 acceptance.
- The durable evidence contains task/action/tool observations and provider/governor events, but no provider prompt, assistant response text, completion usage, HTTP status field, or raw response headers.

### Stage 3 request chronology

The rolling count below is the number of governed admissions in that one Stage 3 trajectory in the preceding 60 seconds. The two trajectories are separate windows; they are about nine minutes apart. Times and backoffs are from the read-only SQLite rows.

| Attempt sequence | Task | Admission start UTC | Gap from previous | Rolling requests | Result | Selected backoff | Tool action after response |
|---:|---|---|---:|---:|---|---:|---|
| 1 | initial | 21:36:21.510 | — | 1 | success | — | `list_files` completed → `obs-000001` |
| 2 | initial | 21:36:26.513 | 5.003 s | 2 | success | — | `list_files` completed → `obs-000002` |
| 3 | initial | 21:36:31.517 | 5.004 s | 3 | success | — | `read_file` completed → `obs-000003` |
| 4 | initial | 21:36:36.518 | 5.001 s | 4 | success | — | `read_file` completed → `obs-000004` |
| 5 | initial | 21:36:41.520 | 5.002 s | 5 | success | — | `apply_patch` failed with typed `invalid_patch`; failed observation appended in memory, no durable observation |
| 6 | initial | 21:36:46.523 | 5.003 s | 6 | `rate_or_capacity`; completed; certain | 21 s | none |
| 7 | rerun | 21:45:35.557 | — | 1 | success | — | `list_files` completed → `obs-000001` |
| 8 | rerun | 21:45:40.559 | 5.002 s | 2 | success | — | `list_files` completed → `obs-000002` |
| 9 | rerun | 21:45:45.563 | 5.004 s | 3 | success | — | `read_file` completed → `obs-000003` |
| 10 | rerun | 21:45:50.567 | 5.004 s | 4 | success | — | `read_file` completed → `obs-000004` |
| 11 | rerun | 21:45:55.570 | 5.003 s | 5 | success | — | `write_file` completed → `obs-000005` |
| 12 | rerun | 21:46:00.570 | 5.000 s | 6 | `rate_or_capacity`; completed; certain | 29 s | none |

Attempts 1–5 and 7–11 each completed with an empty provider failure class. Attempts 6 and 12 each have one debit, `uncertain=0`, empty receipt error, and no receipt-aware amplification. Governor history contains exactly two rate events, at 21:36:46.590Z and 21:46:00.632Z; both are retained within the configured one-hour rate-response window.

### Offline transcript growth reconstruction

The stored data sizes below are UTF-8 byte counts of each durable `tool_results.data_json` component, not whole observation-message or prompt sizes. The hashes are SHA-256 of those serialized data components; no raw data is included. A failed `apply_patch` created no `tool_results` row. `prompt_bytes`, encoded wire-request bytes, full observation-message bytes, completion-output bytes, and cumulative prompt bytes cannot be reconstructed because the successful provider response text (which becomes each prior assistant turn) was deliberately not retained. Therefore per-attempt exact request sizes and token counts are unavailable; the rolling cumulative prompt-byte total is also unavailable.

| Sequence | Known transcript categories before request | Tool observations before request | Failed actions / failed observations / corrections before request | Durable successful tool-data bytes accumulated before request | Prompt bytes | Wire JSON bytes | Output bytes / tokens |
|---:|---|---:|---|---:|---|---|---|
| 1 | system_contract, task_objective | 0 | 0 / 0 / 0 | 0 | unavailable | unavailable | unavailable |
| 2 | system_contract, task_objective, previous_model_turn, tool_action, tool_observation | 1 | 0 / 0 / 0 | 343 | unavailable | unavailable | unavailable |
| 3 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 2 | 0 / 0 / 0 | 536 | unavailable | unavailable | unavailable |
| 4 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 3 | 0 / 0 / 0 | 2,163 | unavailable | unavailable | unavailable |
| 5 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 4 | 0 / 0 / 0 | 3,749 | unavailable | unavailable | unavailable |
| 6 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 5 | 1 / 1 / 0 | 3,749 | unavailable | unavailable | unavailable |
| 7 | system_contract, task_objective | 0 | 0 / 0 / 0 | 0 | unavailable | unavailable | unavailable |
| 8 | system_contract, task_objective, previous_model_turn, tool_action, tool_observation | 1 | 0 / 0 / 0 | 343 | unavailable | unavailable | unavailable |
| 9 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 2 | 0 / 0 / 0 | 536 | unavailable | unavailable | unavailable |
| 10 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 3 | 0 / 0 / 0 | 2,163 | unavailable | unavailable | unavailable |
| 11 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 4 | 0 / 0 / 0 | 3,749 | unavailable | unavailable | unavailable |
| 12 | system_contract, task_objective, previous_model_turns, tool_actions, tool_observations | 5 | 0 / 0 / 0 | 7,331 | unavailable | unavailable | unavailable |

The four common successful tool-data components were 343, 193, 1,627, and 1,586 bytes; their SHA-256 values, in that order, are `624813a623d03bc44fc3c5e8ed84ea2a5590bfbd0b83ec760bdff8c52cbb06b2`, `1d56a1d36754b60d3564b2aa60d4af6a83c88483bfe6e7c798b8cd95ff125aa2`, `b607d272884b2a56120a65ac5883d478cf9c01eb2e91b6eb7c122c49bb023491`, and `b571a598503893880e4d89bcbadaadb10221c9d0cabf34e5b41454deb802bcaa`. The rerun's successful write observation was 3,582 bytes with SHA-256 `9c8e3fa9471e92adcefeed45caebb1028427ef225e59eb9a72ebe162c52277e9`.

This proves that Runstead repeatedly resends an expanding transcript structure and that durable observation data increased before the failure turn, especially to 7,331 bytes of observation data before attempt 12. It does not prove the full growth amount or token usage: prior model-turn bytes and observation framing/arguments are missing from a complete serialized request record. This remains consistent with input-token pressure, not proof of it.

### Retry-After and governor backoff

- Attempt 6's selected 21 s is **origin indeterminate**. On the first rate event the governor uses a 15 s baseline plus nonnegative jitter (up to 22.5 s); a valid `Retry-After` can replace that value. Both paths can produce 21 s, and the parsed header value was not persisted.
- Attempt 12's selected 29 s is attributable to `Retry-After` by reconstruction from durable state and v4 code. It is the second retained rate event inside the one-hour window, whose jitter baseline is 30 s and is clamped to at least 30 s. The OpenAI-compatible adapter does not populate `ResetAt`; the only input that can lower the selected duration to 29 s is a positive parsed `Retry-After`. The exact header text/value was not separately persisted, but its normalized duration was 29 s.
- In both cases selected backoff is the governor's chosen wait, not independently retained header evidence. Only the second event's source can be reconstructed from the implementation plus the retained second-event count and persisted value.

## EXTERNAL GROQ EVIDENCE

Official Groq documentation was retrieved on **2026-10-04**:

- [Rate Limits](https://console.groq.com/docs/rate-limits) lists Free-plan `openai/gpt-oss-120b` at **30 RPM, 1K RPD, 8K TPM, and 200K TPD**. It labels the table a high-level summary, warns that exceptions exist, and directs users to the account limits page for exact organization limits. It says rate limits apply at organization level and some organizations have separate ITPM/OTPM limits.
- The same page documents `retry-after` and `x-ratelimit-limit/remaining/reset-{requests,tokens}` response headers. Its table maps the request headers to RPD and token headers to TPM, and says a 429 is returned when a rate limit is exceeded.
- [API Error Codes and Responses](https://console.groq.com/docs/errors) describes 429 as too many requests in a time frame. It also documents a separate 498 Flex capacity-exceeded response; v4 did not use Flex, and Runstead's OpenAI-compatible classifier does not map 498 to the recorded `rate_or_capacity` result.

These published values are mutable external evidence. They do not establish the actual organization/project limits or remaining allowance used by the v4 key at request time.

## INFERENCE

| Hypothesis | Evidence for | Evidence against / missing | Status |
|---|---|---|---|
| RPM | A rate response occurred on request six in each trajectory. | Admission starts have about a 5 s inter-arrival cadence (~12 RPM). Counting all six starts over the ~25 s first-to-last interval gives 14.4/min; both estimates are below the published 30 RPM Free value. Actual org/project limit and other concurrent usage are unknown. | indeterminate |
| TPM | Transcript structure grows each turn; before rerun attempt 12, known durable observation-data components total 7,331 bytes, plus system/task text, prior model responses, wrappers and arguments. Both trajectories failed at the same turn count. | Full prompts and token counts are unavailable; the two task histories do not establish cumulative tokens from other callers or token-window counters. | indeterminate |
| ITPM | Separate organization-level input-token limits may exist. | No exact input token count or ITPM account limit was retained; documented headers expose combined tokens, not a separate ITPM counter. | indeterminate |
| OTPM | Repeated model responses consume output tokens and may grow as the task proceeds. | Assistant response text and output usage counts were not retained; separate OTPM configuration is unknown. | indeterminate |
| RPD | Each trajectory contributes six requests. | The public Free value is 1K/day, but account history and remaining requests before the canary are unknown; six local requests cannot prove the day's starting allowance. | indeterminate |
| TPD | Requests cumulatively consume tokens during the day. | Exact input/output tokens and prior daily usage are unavailable; the published 200K/day value is not the verified account limit. | indeterminate |
| generic provider capacity | Two sixth-turn responses had the same typed rate/capacity outcome and completed delivery. | The adapter's `rate_or_capacity` specifically comes from HTTP 429; no capacity subtype or independent capacity signal survives. | indeterminate |

The two sixth-turn failures are compatible with a rolling input-token window, particularly because complete transcript history is resent, but neither the request size nor token counter needed to prove that mechanism survives. Similar failure position alone is not attribution. RPM against the public Free limit is a weaker explanation, but actual account limits and shared usage prevent rejecting it for the canary account. RPD/TPD likewise cannot be rejected without prior usage evidence.

## OBSERVABILITY AUDIT

| Signal | Adapter sees it? | Reaches governor? | Survives in current durable attempt state? |
|---|---|---|---|
| HTTP status | Yes, transient `ResponseMetadata.StatusCode` and typed error | Classifier maps 429 to `rate_or_capacity`; numeric status is not passed into governor outcome | No numeric status field; the closed provider failure class survives |
| `Retry-After` | Parsed to a bounded duration | Yes, as `Outcome.RetryAfter`; it can set/override cooldown selection | Original duration does not survive separately; only selected backoff and cooldown projection survive |
| `ResetAt` | No; the OpenAI-compatible adapter does not parse a reset header | Zero value | No |
| Rate-limit limit / remaining / reset headers | No normalized fields are read from response headers | No | No |
| Delivery, failure class, debit, uncertainty, selected backoff | Yes | Yes | Yes, in `provider_attempts` and outcome events |

The numeric Groq headers cited above are discarded by the compatible adapter. Current state cannot tell whether the 429 exhausted TPM versus RPD, nor whether separately configured ITPM/OTPM applied. This is an **OBSERVABILITY_GAP**.

## RUNSTEAD DECISION

**Classification: `OBSERVABILITY_GAP`**

Recommendation: create a separate, narrowly scoped provider-neutral observability issue and have it reviewed before another Groq Gate A canary. Preserve this report as the diagnosis; do not alter pacing, retry policy, provider configuration, token/context handling, or Gate A based on the TPM hypothesis.

The future extension should normalize only a small allowlist of validated numeric counters and durations into provider-neutral metadata (for example, limit, remaining, and reset for known request/token dimensions). It should reject malformed, negative, overflowing, or unbounded values; never persist arbitrary headers, bodies, prompts, model output, or secrets; and leave retry eligibility, governor accounting, and provider identity untouched. Do not implement that extension in #151.

## Scope confirmation

- live provider requests = **0**
- provider configuration changed = **no**
- retry behavior changed = **no**
- Gate A changed = **no**
- #123 started = **no**
- provider compatibility claim added = **no**
