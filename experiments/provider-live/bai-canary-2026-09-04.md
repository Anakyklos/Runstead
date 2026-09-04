# B.AI live canary report

- Status: **blocked, not a compatibility success**
- Observed at: 2026-09-04 UTC
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

## First Runstead protocol turn

The unchanged CLI and the existing `openai_compatible` adapter reached the B.AI chat endpoint for every eligible model. Each physical completion was admitted and debited separately by the governor. No provider fallback, hidden retry, key rotation, or account rotation occurred.

| Model | Durable live result | Attempts | Delivery evidence |
| --- | --- | ---: | --- |
| `qwen3.8-flash` | `corrections_exhausted`, `invalid_action_schema` | 3 | 3 upstream-reached responses, each `delivery_state=completed`, each debited once |
| `glm-5.3-flash` | `provider_failure`, `timeout` | 3 | 2 upstream-reached completed responses, then one upstream-reached `sent_confirmed` timeout, each debited once |
| `mimo-v2.5` | `provider_failure`, `upstream_server_failure` | 3 | 2 upstream-reached completed responses, then one upstream-reached server failure, each debited once |
| `hy3` | `corrections_exhausted`, `malformed_json` | 3 | 3 upstream-reached responses, each `delivery_state=completed`, each debited once |

The model protocol turn did not produce an accepted `runstead.protocol.v1` action or final response. The qwen and glm runs produced protocol correction failures. The hy3 run produced a malformed JSON protocol failure. The mimo run reached an upstream server failure after protocol corrections. These are classified as model protocol/provider outcomes, not transport retries.

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
