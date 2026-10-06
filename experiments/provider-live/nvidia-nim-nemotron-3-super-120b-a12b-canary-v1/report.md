# NVIDIA NIM Nemotron 3 Super Gate A canary v1

**Decision: Gate A = NOT SATISFIED. Stage 1 = INCONCLUSIVE.** The one Stage 1 request returned HTTP 200 and the exact model, but the retained structural evidence is insufficient to determine which response-shape predicate failed. Stages 2–4 remain SKIPPED. No retry or controlled rerun was made.

## Identity and provenance

- Parent: [#121](https://github.com/Anakyklos/Runstead/issues/121)
- Issue: [#168](https://github.com/Anakyklos/Runstead/issues/168)
- PR: [#169](https://github.com/Anakyklos/Runstead/pull/169); the canary remained draft and unmerged through its evidence-correction review
- Source base: `c079f80321fde3a687416afb5a055cece9ed4762`
- **Live trajectory code HEAD:** `aebcf9b85023b881ac935abfa31361f7ec9c7d54`
- **Post-trajectory validator correction:** implemented offline after maintainer review; it was not used to process the consumed response. Final reviewed correction HEAD before this provenance-only update: `0ed3da9ce5d5f14cf70f25ecdc461508209e04a0`. The live request count remains one.
- Candidate: `nvidia-nim-nemotron-3-super-120b-a12b-canary-v1`
- Protocol: `openai_compatible`
- Base URL: `https://integrate.api.nvidia.com/v1`
- Model: `nvidia/nemotron-3-super-120b-a12b`
- Auth reference: `NVIDIA_API_KEY`

## Dated NVIDIA evidence — 2026-10-06

- NVIDIA's API reference lists `POST https://integrate.api.nvidia.com/v1/chat/completions`, identifies the API as OpenAI-compatible, and lists the exact model: [Nemotron 3 Super inference API reference](https://docs.api.nvidia.com/nim/reference/nvidia-nemotron-3-super-120b-a12b-infer).
- NVIDIA's model page shows the exact base URL and model in an OpenAI client example and marks the free endpoint Available: [NVIDIA model build page](https://build.nvidia.com/nvidia/nemotron-3-super-120b-a12b/build).

## Preflight immediately before the one live request

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
| 1 — auth/model | **INCONCLUSIVE** | Exactly one direct pre-task `POST /chat/completions`; HTTP `200`; returned model exact. The response was parseable enough to compare its model, but the harness retained only aggregate `response_shape_valid=false`, so structural failure details cannot be reconstructed. |
| 2 — read-only protocol/evidence/verifier | **SKIPPED** | Stage 1 PASS was not proven; no task was created. |
| 3 — bounded coding | **SKIPPED** | Stage 1 PASS was not proven; no task, write, recipe, or verifier run occurred. |
| 4 — interruption/resume | **SKIPPED** | Stage 1 PASS was not proven; no task was created or resumed. |

The one live request used the fixed model, user message `Reply with OK.`, `max_tokens=1`, and `stream=false`. It made no redirect, retry, fallback, pooling, rotation, alternate model, account, or endpoint request. The response body was deliberately discarded. This evidence does **not** prove a provider failure, an invalid compatible response, or OpenAI incompatibility; it also does not prove Stage 1 PASS. The body is unavailable, so the original structural predicates cannot be recovered.

### Offline validator correction for future use

After the live trajectory, the validator was changed to accept assistant `content` as either string or null. A present `object` field must equal `chat.completion`; the field may be omitted. The future result emits separate booleans for JSON object, completion object, choices, message, assistant role, content type, and exact returned model. It retains no body, generated text, reasoning, raw headers, or arbitrary provider fields and does not assess generated-text semantics. Offline tests cover these predicates. This correction was not exercised against, and does not retroactively classify, the consumed live response.

## Accounting and effects

- Historical NVIDIA pre-task control requests: `1`
- Provider requests added after maintainer review: `0`
- Runstead tasks / task IDs: `0` / none
- Provider attempts / admissions / debits: `0` / `0` / `0`
- Workspace writes / recipes / verifier runs: `0` / `0` / `0`
- Automatic retry / fallback / rotation: `0` / `0` / `0`
- Controlled rerun: `0`
- Inspect/resume: not used
- Delivery accounting: the direct control received HTTP 200 and was not replayed; there was no Runstead task delivery ledger

## Secret hygiene and verification

Only the existing external credential reference was used. The key was never printed, copied into the repository or state files, hashed, measured, or fingerprinted. No response body, generated text, reasoning text, arbitrary header, or prompt transcript was retained.

The post-trajectory correction passed 10 canary Stage 1 unit tests, covering nullable content and granular structural diagnostics. All 20 canary-specific Python tests and the 4 shared sanitized HTTP tests passed. On correction HEAD `0ed3da9ce5d5f14cf70f25ecdc461508209e04a0`, `gofmt`, `go test ./...`, `go vet ./...`, `go build ./cmd/runstead`, `go test -race ./...`, `bash experiments/protocol/test.sh`, and `git diff --check` passed. GitHub Actions Go CI run #309 also passed on that exact correction HEAD. This later provenance-only report update performs no provider dispatch and does not alter the live trajectory.
