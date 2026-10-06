# NVIDIA NIM DeepSeek V4.1 Flash Gate A canary v1

## Decision

**Gate A: NOT SATISFIED.** Stage 1 is **INCONCLUSIVE**: the single authorized HTTP control produced a sanitized `URLError` with no HTTP status. The error does not establish whether the request reached NVIDIA or identify a root cause. No Stage 1 retry was made. Stages 2–4 were skipped because Stage 1 did not pass.

## Identity and source state

- Parent issue: #121; child issue: #170.
- Provider: `nvidia-nim-deepseek-v4-1-flash-canary-v1` (`openai_compatible`).
- Base URL: `https://integrate.api.nvidia.com/v1`.
- Model: `deepseek-ai/deepseek-v4.1-flash`.
- Auth reference: `NVIDIA_API_KEY`; secret source: the configured external NVIDIA_API_KEY env file.
- Branch: `issue-170-nvidia-nim-deepseek-v4-1-flash-canary-v1`.
- Base: `2cfb46ffbc934c238f220e1c1e0ca1551a9c9c4c`.
- Live trajectory HEAD: `2cfb46ffbc934c238f220e1c1e0ca1551a9c9c4c`. The one Stage 1 HTTP control occurred against this source state; no Runstead task, adapter, governor, or task accounting was involved.
- Report/harness commit: recorded in the PR HEAD. These post-trajectory artifacts do not change the live trajectory HEAD or its result.
- PR: pending creation; requested title `test(provider): run NVIDIA NIM DeepSeek V4.1 Flash Gate A canary v1 (#170)`.

## NVIDIA documentation revalidated

Rechecked on 2026-10-06, immediately before Stage 1 dispatch:

- NVIDIA's model page identifies `deepseek-ai/deepseek-v4.1-flash`, shows the free API endpoint, and reports **Free Endpoint: Available**: [NVIDIA model page](https://build.nvidia.com/deepseek-ai/deepseek-v4.1-flash/build).
- NVIDIA's API reference specifies `POST https://integrate.api.nvidia.com/v1/chat/completions`, labels it OpenAI compatible, and defaults to the exact model ID: [NVIDIA Chat Completions API reference](https://docs.api.nvidia.com/nim/reference/nvidia-deepseek-v4_1-flash-infer).
- NVIDIA's model card lists text input/text output and use cases including coding and tool-using agentic applications: [NVIDIA model card](https://docs.api.nvidia.com/nim/re/reference/nvidia-deepseek-v4_1-flash).

This documentation is a dated external observation, not a permanent availability guarantee. The API reference accepts `max_tokens` from 1 to 1,048,576; the canary contract's value of 64 was used exactly, with no optional sampling or vendor-specific fields.

## Preflight and secret handling

Immediately before dispatch:

- Env file regular: true.
- Exactly one `NVIDIA_API_KEY` declaration: true.
- Key loaded and nonempty: true.
- `secret_value_emitted=false`.
- Exact provider config resolved through `config.LoadProvidersFile` then `Registry.Resolve(..., provider.SafeRouteSafety())`: PASS.
- Adapter constructed: false.
- Provider requests before dispatch: 0.

The key was held only in process memory and sent only as the required Bearer value for the single authorized Stage 1 request. It was not echoed, persisted, hashed, sized, fingerprinted, or committed. No raw request/response bodies, generated text, reasoning, headers, or exception strings were retained.

## Stage results

| Stage | Verdict | Evidence |
|---|---|---|
| 1 — auth/model control | INCONCLUSIVE | One request; sanitized observation: `request_count=1`, `method=POST`, `path=/chat/completions`, `http_status=unknown`, `error_type=URLError`. No response shape/model diagnostics passed. No retry. |
| 2 — read-only runtime/evidence/verifier | SKIPPED | Stage 1 did not pass; no task dispatched. |
| 3 — bounded coding | SKIPPED | Stage 2 did not pass; no task dispatched. |
| 4 — interruption/resume | SKIPPED | Stage 3 did not pass; no task dispatched. |

## Runtime, accounting, and recovery

- Runstead task IDs: none.
- Adapter dispatches: 0.
- Runstead provider attempts / governor admissions / debits: 0 / 0 / 0.
- Actions, effects, observations, verifier decisions, recipes, and task deliveries: none.
- Controlled reruns: 0. Stage 1's sole request was not repeated.
- Resume/inspect: none; no task existed to resume.
- No runtime or trust-boundary code was changed.

## Verification and publication

Local gates were run on implementation commit `b238295fe6e5cc88a397a675335376e3f69dd7c8`, before this post-campaign report-only update. The canary code and runtime source are unchanged by that later documentation commit.

| Gate | Result |
|---|---|
| `test -z "$(gofmt -l .)"` | PASS |
| `go test ./...` | FAIL in sandbox: `httptest.NewServer` cannot bind `[::1]:0` (`socket: operation not permitted`) in `cmd/runstead/TestLearningCooldownFromRetryAfterAllFamilies` and OpenAI-compatible provider tests; unrelated packages passed. |
| `go vet ./...` | PASS |
| `go build ./cmd/runstead` | PASS; generated local binary removed. |
| `go test -race ./...` | FAIL on the same sandbox loopback-listener restriction in `cmd/runstead` and OpenAI-compatible provider tests; other reported packages passed. |
| `bash experiments/protocol/test.sh` | PASS: protocol parser and offline experiment checks. |
| `python3 -m unittest -v test_sanitized_http` from `experiments/provider-live/` | PASS: 4 tests. |
| `python3 -m unittest -v test_stage1_control test_provider_contract` from this canary directory | PASS: 12 tests. |
| `git diff --check` | PASS. |

The focused Stage 1 harness and provider-contract tests passed after replacing a loopback redirect test that cannot bind sockets in this sandbox with a no-network test of the injected no-redirect handler. The test-first request-contract assertion was observed RED against the copied Nemotron values before the implementation was adapted to DeepSeek and `max_tokens=64`.

PR and GitHub CI state are recorded here after publication. Gate A remains **NOT SATISFIED** regardless of repository gate results because no Stage 1 response was obtained.
