# NVIDIA NIM Nemotron 3 Super Gate A canary v1

**Decision: Gate A = NOT SATISFIED. Stage 1 was blocked before dispatch; Stages 2–4 were skipped. No NVIDIA inference request was sent.**

## Identity and source

- Parent: [#121](https://github.com/Anakyklos/Runstead/issues/121)
- Child: [#168](https://github.com/Anakyklos/Runstead/issues/168)
- PR: [#169](https://github.com/Anakyklos/Runstead/pull/169)
- PR head when opened: `9d73058e3f46040c18fcc82e2a98f02d76b72035` (the report URL metadata update will advance the branch; current final HEAD is in the PR metadata).
- Source base and preflight HEAD: `c079f80321fde3a687416afb5a055cece9ed4762` (merged PR #167)
- Candidate: `nvidia-nim-nemotron-3-super-120b-a12b-canary-v1`
- Protocol: `openai_compatible`
- Base URL: `https://integrate.api.nvidia.com/v1`
- Model: `nvidia/nemotron-3-super-120b-a12b`
- Auth reference: `NVIDIA_API_KEY`

## Dated NVIDIA evidence — 2026-10-06

- NVIDIA's API reference lists `POST https://integrate.api.nvidia.com/v1/chat/completions`, says it is OpenAI-compatible, and uses the exact model ID: [API reference](https://docs.api.nvidia.com/nim/reference/nvidia-nemotron-3-super-120b-a12b-infer).
- NVIDIA's model build page lists the exact model and base URL in its OpenAI client example; the live catalog currently marks its free endpoint available and reports 65M API calls in the last 30 days: [NVIDIA model page](https://build.nvidia.com/nvidia/nemotron-3-super-120b-a12b/build).
- This is dated catalog/API evidence. It does not prove account authorization or a successful inference request.

## Preflight

The offline preflight ran against the source SHA above. It loaded the key only from the existing external env file into the short-lived preflight process and emitted booleans only:

- env file regular: `true`
- `NVIDIA_API_KEY` declaration present: `true`
- key loaded for this preflight and non-empty: `true`
- `secret_value_emitted`: `false`
- exact provider ID, protocol, base URL, model, and auth reference resolved: `true`
- resolved route safety equals `SafeRouteSafety`: `true`
- adapter constructed for preflight: `false`
- provider requests before dispatch: `0`
- resolver exit: `0`

The resolver output retained no secret and no key-derived fingerprint, length, prefix, or suffix.

## Stage verdicts

| Stage | Verdict | Evidence |
|---|---|---|
| 1 — auth/model | **BLOCKED before dispatch** | The required control request includes `max_tokens=1`. The current adapter contract cannot encode it: `provider.Request` has no token-limit field, `chatCompletionRequest` contains only `model`, `messages`, and `stream`, and `encodeChatCompletionRequest` serializes only those fields. Sending the request with a separate direct HTTP client would bypass the required existing adapter/governor path. Adding runtime support is explicitly outside this canary. No provider request was made. |
| 2 — read-only protocol/evidence/verifier | **SKIPPED** | Stage 1 did not pass. No task was created. |
| 3 — bounded coding | **SKIPPED** | Stage 1 did not pass. No task, workspace edit, recipe, or verifier run occurred. |
| 4 — interruption/resume | **SKIPPED** | Stage 1 did not pass. No task was created or resumed. |

The adapter limitation is visible in [`internal/provider/provider.go`](../../../internal/provider/provider.go), [`internal/provider/openaicompat/wire.go`](../../../internal/provider/openaicompat/wire.go), and `internal/provider/openaicompat/client.go` (`encodeChatCompletionRequest`). It is a current contract/capability gap; this report does not change runtime behavior or weaken the requested control payload.

## Accounting and effects

- NVIDIA inference requests: `0`
- Runstead tasks / task IDs: `0` / none
- Attempts / admissions / debits: `0` / `0` / `0`
- Actions, writes, recipes, verifier runs: `0`
- Redirects, retries, fallback, rotation: `0`
- Controlled rerun: not used (`0`)
- Resume: not used
- Delivery state: not applicable; no dispatch occurred

## Secret hygiene and limits

Only the exact external credential reference was used. The key value was never printed, copied into the repository, persisted, hashed, measured, or included in diagnostics. No provider response, body, arbitrary header, prompt transcript, or output was retained. Catalog availability is not endpoint-health or account-access proof.

## Follow-up proposal

Consider a separate maintainer-scoped issue for a governed, provider-neutral generation-token limit in the existing request contract, with accounting and acceptance coverage. Do not implement that change under this canary and do not relax the Stage 1 request contract. Until that gap is resolved and a fresh bounded trajectory passes all stages, #123 remains blocked by Gate A and #124 remains downstream.

## Verification

- Offline resolver preflight: PASS (`SafeRouteSafety`, exact identity, no adapter, zero requests).
- Canary provider-contract tests: PASS (2 tests; `python3 -m unittest -v test_provider_contract.py`).
- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: PASS after rerunning with local-listener permission; the first sandboxed run was blocked by IPv6 `httptest` bind denial.
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS with local-listener permission.
- `bash experiments/protocol/test.sh`: PASS.
- `git diff --check`: PASS.
- GitHub Actions: PASS — Go CI run #305 on PR head `160f11f9668e54fc4231c364c342749d1aa67810` (2026-10-06). Recording this result advances the report-only PR head; CI for that revision is tracked in PR #169.
