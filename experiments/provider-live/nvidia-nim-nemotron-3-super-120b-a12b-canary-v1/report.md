# NVIDIA NIM Nemotron 3 Super Gate A canary v1

**Decision: Gate A = NOT SATISFIED.** Stage 1 returned HTTP 200 and the exact model, but its response did not pass the required response-shape check. Stages 2–4 were skipped as required by #168. No retry or controlled rerun was made.

## Identity and provenance

- Parent: [#121](https://github.com/Anakyklos/Runstead/issues/121)
- Issue: [#168](https://github.com/Anakyklos/Runstead/issues/168)
- PR: [#169](https://github.com/Anakyklos/Runstead/pull/169), kept open and unmerged
- Source base: `c079f80321fde3a687416afb5a055cece9ed4762`
- Code HEAD used for the trajectory: `aebcf9b85023b881ac935abfa31361f7ec9c7d54`
- Candidate: `nvidia-nim-nemotron-3-super-120b-a12b-canary-v1`
- Protocol: `openai_compatible`
- Base URL: `https://integrate.api.nvidia.com/v1`
- Model: `nvidia/nemotron-3-super-120b-a12b`
- Auth reference: `NVIDIA_API_KEY`

## Dated NVIDIA evidence — 2026-10-06

- NVIDIA's API reference lists `POST https://integrate.api.nvidia.com/v1/chat/completions`, identifies the API as OpenAI-compatible, and lists the exact model: [Nemotron 3 Super inference API reference](https://docs.api.nvidia.com/nim/reference/nvidia-nemotron-3-super-120b-a12b-infer).
- NVIDIA's model page shows the exact base URL and model in an OpenAI client example and marks the free endpoint Available: [NVIDIA model build page](https://build.nvidia.com/nvidia/nemotron-3-super-120b-a12b/build).
- These catalog/API facts do not establish account authorization or successful inference for the configured key.

## Preflight immediately before dispatch

- External env file regular: `true`
- Exact `NVIDIA_API_KEY` declaration present: `true`
- Key loaded and non-empty in the short-lived process: `true`
- `secret_value_emitted`: `false`
- Exact provider identity and `SafeRouteSafety` resolved: `true`
- Adapter constructed during preflight: `false`
- Provider requests before Stage 1: `0`
- Resolver exit: `0`

## Stage verdicts

| Stage | Verdict | Evidence |
|---|---|---|
| 1 — auth/model | **FAIL** | Exactly one direct pre-task `POST /chat/completions`; HTTP `200`; response-shape validation `false`; exact returned model validation `true`. |
| 2 — read-only protocol/evidence/verifier | **SKIPPED** | Stage 1 did not pass; no task was created. |
| 3 — bounded coding | **SKIPPED** | Stage 1 did not pass; no task, write, recipe, or verifier run occurred. |
| 4 — interruption/resume | **SKIPPED** | Stage 1 did not pass; no task was created or resumed. |

The Stage 1 control used the fixed model, user message `Reply with OK.`, `max_tokens=1`, and `stream=false`. Redirects were disabled. No retry, fallback, pooling, rotation, alternate model, account, or endpoint was used. Only the sanitized HTTP observation and separate shape/model booleans were emitted; the response body was not retained. This direct control does not prove the runtime adapter.

The runtime request contract's lack of a `max_tokens` field is not classified as a Gate A blocker. No runtime change was made. Stages 2–4 would have exercised the real adapter/governor/runtime, but the ordered gate did not authorize them after Stage 1 failed.

## Accounting and effects

- NVIDIA pre-task control requests: `1`
- Runstead tasks / task IDs: `0` / none
- Provider attempts / admissions / debits: `0` / `0` / `0`
- Workspace writes / recipes / verifier runs: `0` / `0` / `0`
- Automatic retry / fallback / rotation: `0` / `0` / `0`
- Controlled rerun: not used (`0`)
- Inspect/resume: not used
- Delivery accounting: not applicable to a Runstead task; the one direct control received HTTP 200 and was not replayed

## Secret hygiene and limits

Only the existing external credential reference was used. The key was never printed, copied into repository or state files, hashed, measured, or fingerprinted. No provider response body, arbitrary header, prompt transcript, or generated content was retained. Stage 1's valid HTTP status and returned model did not compensate for the failed response-shape check.

## Verification

- Offline exact provider resolver: PASS (`SafeRouteSafety`, adapter not constructed, zero requests).
- Shared sanitized HTTP and Nemotron-specific Python tests: PASS (19 tests).
- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: PASS.
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS.
- `bash experiments/protocol/test.sh`: PASS.
- `git diff --check`: PASS before this report-only result update.
- GitHub Actions Go CI run #307: PASS on code HEAD `aebcf9b85023b881ac935abfa31361f7ec9c7d54`. This result predates the report-only follow-up commit and is not claimed as CI for that later HEAD.
