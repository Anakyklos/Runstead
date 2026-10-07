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
- Implementation/report commit before PR metadata: `23b8d7abd18a5416f45989b3a4602305aabef0d1`. This report metadata update is documentation-only and does not change the live trajectory HEAD or the tested implementation.
- PR: [#171](https://github.com/Anakyklos/Runstead/pull/171), open and unmerged; title `test(provider): run NVIDIA NIM DeepSeek V4.1 Flash Gate A canary v1 (#170)`.

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

The key value was not echoed, persisted to a file, hashed, sized, fingerprinted, or committed. However, the final preflight driver copied the Python process environment containing the key into the Go config-resolver subprocess. That local subprocess received the key in its environment, contrary to the contract's Bearer-only transmission rule. The resolver used it only as a nonempty auth-reference check and printed static identity fields; no evidence shows it was logged, persisted, or transmitted elsewhere. `secret_value_emitted=false` remains accurate, but strict secret-hygiene compliance is **NOT SATISFIED**. The sole authorized external Stage 1 call used the key only as its Bearer value. No raw request/response bodies, generated text, reasoning, headers, or exception strings were retained.

### Offline key-hygiene correction after the live trajectory

The historical trajectory above remains unchanged: the Go preflight subprocess inherited the key, with no evidence of exfiltration. A later offline correction removed the Go resolver's environment-key requirement and made the Python preflight the explicit resolver invocation boundary. It invokes `go run` with a fixed argument vector, `shell=false`, and a copied environment with `NVIDIA_API_KEY` removed; `GOPROXY=off` and `GOTOOLCHAIN=local` prevent Go from using this preflight to access a module proxy or download a toolchain. The resolver validates the static identity, auth reference, required capabilities, and `SafeRouteSafety` without reading a secret. The env-file parser returns the parsed value locally and leaves `os.environ` untouched; it does not fall back to an ambient key.

Offline sentinel tests exercise successful static resolution with no key, malformed and wrong-identity config rejection, removal of a synthetic ambient key from the resolver child environment, fixed Bearer header preservation, and sanitized resolver/HTTP output. They also verify an unauthorized invocation stops before loading the configured env file. The tests use injected runners and local config files; the resolver does not construct an adapter and reports `provider_requests=0`. This correction happened after the sole live request and does not change its `URLError`, unknown HTTP status, or Stage 1 **INCONCLUSIVE** verdict. It does not establish Gate A.

Test-first evidence: before the implementation change, the focused suite was RED with two loader assertions failing because the key was written to `os.environ`, plus three errors because the resolver wrapper did not exist. The added unauthorized-invocation sentinel test was also observed RED while `main` loaded the key before checking the authorization flag. After the changes, the focused suite passed all 17 tests.

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

PR #171 was opened on 2026-10-06 and remains unmerged. GitHub Go CI run [#312](https://github.com/Anakyklos/Runstead/actions/runs/37547518860) passed on implementation/report commit `23b8d7abd18a5416f45989b3a4602305aabef0d1`. After the secret-hygiene report correction, GitHub Go CI run [#314](https://github.com/Anakyklos/Runstead/actions/runs/37550693509) passed on exact PR head `3b875f9f67128605265ee05db49b5d13cd919648`, including tests, race detector, vet/build, protocol, provider abstraction, sidecar, and quality gates. This report status correction is documentation-only; CI for its resulting PR head is checked separately on PR #171. Gate A remains **NOT SATISFIED** regardless of repository gate results because no Stage 1 response was obtained.

### P1 correction local validation (2026-10-07)

The offline correction was validated in its isolated worker worktree. No NVIDIA requests, external env-file reads, credential operations, retries, or endpoint changes were made.

| Gate | Result |
|---|---|
| `python3 -m unittest -v test_stage1_control test_provider_contract` | PASS: 17 tests, including a real offline `go run` with the key absent; malformed and wrong-model config rejected; unauthorized main invocation skips env-file loading. |
| `python3 -m unittest -v test_sanitized_http` | PASS: 4 tests. |
| `gofmt -d experiments/provider-live/nvidia-nim-deepseek-v4-1-flash-canary-v1/stage1_resolve.go` | PASS: no output. |
| `GOCACHE=/tmp/runstead-pr171-keyhygiene-gocache go test ./...` | FAIL due sandbox IPv6 listener restriction in `httptest`: `listen tcp6 [::1]:0: socket: operation not permitted`. Affected tests: `cmd/runstead/TestLearningCooldownFromRetryAfterAllFamilies/openai_compatible`, provider `anthropiccompat`, `compat`, `googlecompat`, `omniroute`, and `openaicompat` tests. Other reported packages passed or were cached. |
| `GOCACHE=/tmp/runstead-pr171-keyhygiene-gocache go vet ./...` | PASS. |
| `GOCACHE=/tmp/runstead-pr171-keyhygiene-gocache go build ./cmd/runstead` | PASS; generated local binary removed. |
| `GOCACHE=/tmp/runstead-pr171-keyhygiene-gocache go test -race ./...` | FAIL due the same sandbox IPv6 `httptest` listener restriction in the same packages; other reported packages passed or were cached. |
| `bash experiments/protocol/test.sh` | PASS: protocol parser and offline experiment checks. |
| `git diff --check` | PASS. |

Hosted CI for the P1 correction is pending publication of the reviewed worker commit to PR #171; the earlier CI runs above do not validate this later correction. Gate A remains **NOT SATISFIED**.
