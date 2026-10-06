# Mistral Small 4 Gate A canary v1 — 2026-10-05

## Identity and base

- Parent issue: #121 (OPEN)
- Canary issue: #166 — `[P0][PROVIDER CANARY] Mistral Small 4 Gate A canary v1`
- Branch: `issue-166-mistral-small-4-canary-v1`
- Base SHA: `17b9fb09a4b2952c49970d47a5bcd9a34ef4f43c`
- Provider ID: `mistral-small-4-canary-v1`
- Protocol family: `openai_compatible`
- Base URL: `https://api.mistral.ai/v1`
- Model: `mistral-small-2603`
- Auth reference: `MISTRAL_API_KEY`
- API/free-mode availability is mutable upstream state, not a Runstead invariant. The official setup guide describes Free mode access with usage and rate limits; this canary's HTTP result does not establish account entitlement.

## Official documentation revalidation

Checked 2026-10-05 immediately before the only Stage 1 dispatch:

- [Mistral Small 4 model card](https://docs.mistral.ai/models/mistral-small-4-0-26-03): model ID `mistral-small-2603`; Chat Completions support at `/v1/chat/completions`.
- [Mistral migration guide](https://docs.mistral.ai/resources/migration-guides): OpenAI-compatible base URL `https://api.mistral.ai/v1` and compatible Chat Completions request structure.
- [Chat API reference](https://docs.mistral.ai/api): `POST /v1/chat/completions`.
- [Studio API key setup](https://docs.mistral.ai/getting-started/quickstarts/studio/activate-and-generate-api-key): Free mode access is described with usage/rate limits; this is upstream account state and not a Runstead guarantee.

## Preflight — PASS, zero provider requests

- `env_file_regular=true`
- `mistral_key_declaration_present=true`
- `mistral_key_loaded=true`
- `mistral_key_nonempty=true`
- `secret_value_emitted=false`
- Candidate declaration resolved exactly through `openai_compatible` with current `SafeRouteSafety`.
- Adapter constructed: false.
- Provider requests before Stage 1: 0.
- No key value or derivation was printed, copied into the worktree, or placed in the declaration/report.

## Stage results

| Stage | Result | Evidence |
| --- | --- | --- |
| 1 — exact auth/model control | FAIL | One sanitized HTTP observation below; no 2xx response body was available to validate. |
| 2 — read/evidence/verifier | SKIPPED | Stage 1 did not PASS; no Runstead task created. |
| 3 — bounded coding | SKIPPED | Stage 2 did not PASS; no Runstead task created. |
| 4 — interruption/resume | SKIPPED | Stage 3 did not PASS; no Runstead task created. |

Stage 1 request contract: one `POST /v1/chat/completions`, model `mistral-small-2603`, one user message `Reply with OK.`, `max_tokens=1`, `stream=false`, with no temperature, reasoning flags, or extra body options.

Sanitized HTTP observation (the only retained transport fields):

```text
request_count=1
method=POST
path=/v1/chat/completions
http_status=401
error_type=HTTPError
```

The sanitized control preserved `HTTPError.code`. `response_shape_valid=false` and `returned_model_matches=false`: with a 401, no successful chat completion response was available. This does not prove a specific credential, account, entitlement, or upstream configuration cause.

## Runstead task and accounting evidence

- Task IDs: none; Stage 1 was the direct one-request control and failed before any Runstead task.
- Provider requests: 1 total (Stage 1).
- Runstead attempts / admissions / debits: 0 / 0 / 0.
- Actions/effects/tool observations/verifiers: none.
- No task state or SQLite rows were created by this canary trajectory.

## Rerun, resume, and secret hygiene

- Controlled rerun: none. HTTP 401 is evidence and did not authorize replay.
- Resume: not applicable; there was no Runstead task.
- No fallback, rotation, pooling, provider/model/key/account switch, retry, or special pacing was performed.
- The operator-owned env file was read only by the preflight and Stage 1 control. The control emits only the requested booleans and sanitized response metadata, clears the loaded process environment entry after dispatch, and does not write request/response bodies or headers. No key value, length, hash, prefix, suffix, or other derivation was retained.

## Local gates and GitHub CI

- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: PASS.
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS.
- `bash experiments/protocol/test.sh`: PASS.
- `python3 -m unittest -v test_sanitized_http.py`: PASS (4 tests).
- Mistral offline harness: PASS (6 tests).
- `git diff --check` and staged diff check: PASS.
- GitHub CI: pending at report creation; results will be appended after PR creation.

## Decision and limits

**Gate A: NOT SATISFIED.** Stage 1 failed and Stages 2–4 were skipped. No PASS is inferred from the provider's HTTP response or offline resolution.

The available evidence is limited to preflight success and one HTTP 401. It does not identify why the endpoint rejected the request, establish model access, or qualify any read/evidence/verifier, coding, or recovery behavior.

Next step: review this preserved negative evidence. Do not unlock #123. Any future live attempt must be a newly selected child trajectory with fresh issue/branch and its own authorized preflight; do not replay this issue's HTTP 401.
