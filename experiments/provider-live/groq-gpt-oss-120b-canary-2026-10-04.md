# Groq GPT OSS 120B live canary report

- Execution/report timestamp: 2026-10-04 01:04:46 UTC
- Runstead base commit: `937c91c6679b8fa637d3d432ad4ed3038e57fddf`
- Canary issue: #138; adoption tracker: #121
- Provider ID: `groq-gpt-oss-120b-canary`
- Protocol family: `openai_compatible`
- Endpoint: `https://api.groq.com/openai/v1`
- Candidate model: `openai/gpt-oss-120b`
- Auth reference: `GROQ_API_KEY`
- Exact model availability: **not confirmed**
- Adapter resolution/dispatch: **not attempted**

## Stage 1 — preflight: BLOCKED

One preflight `GET /models` returned HTTP 403. The observed result is
classified as an auth/account/model-access denial; the exact cause is unknown.
The response body was not retained, and the exact candidate model was not
confirmed. The base `main` build passed before this GET.

This was one control-plane preflight request outside the Runstead task
governor. No retry, alternate model, adapter resolution or dispatch, task, or
governed provider request was attempted. No task admission, provider debit,
stage 2 task, evidence, or verifier ID exists.

## Stage 2 — governed protocol turn: NOT RUN

Stage 1 did not confirm the exact model. There was no Runstead adapter
resolution/dispatch, governed task request, task configuration persisted to
SQLite, governor admission, debit, protocol parse, or verifier result. Strict
parsing, verifier authority, fallback behavior, and per-attempt accounting
were not exercised.

## Stage 3 — bounded coding fixture: NOT RUN

No disposable fixture trajectory was created. Containment, tool inspection,
scoped writes, before/after evidence, recipe execution/result, task attempt
accounting, independent verifier, and terminal completion were not exercised.

## Stage 4 — interruption/resume: NOT RUN

There was no eligible trajectory to interrupt or resume. Same-config recovery,
no-replay, budget/evidence preservation, uncertain-delivery handling, and
terminal verifier completion were not exercised.

No canary rerun, quota probe, concurrency probe, key/account/model rotation, or
fallback was performed.

## Request and data accounting

- Preflight: 1 `GET /models` request, HTTP 403, outside the task governor.
- Governed task requests: 0.
- Task admissions: 0; provider debits: 0.
- No secret value or authentication header was persisted by this experiment in
  Git, Runstead config/SQLite, evidence, logs/traces, issue/PR, or model context.
  The raw response body was not retained, and no private content was sent.
- Compatibility documentation remains unchanged. This canary does not
  establish live compatibility for `openai_compatible` or this Groq model.

## Validation

The requested gates were run against base commit
`937c91c6679b8fa637d3d432ad4ed3038e57fddf`. The worker's isolated sandbox
used a worktree-local `GOCACHE`; its loopback socket restriction caused the
first full and race suite attempts to fail. The Maestro then ran those same
two gates in the workspace executor on this unchanged branch.

- `test -z "$(gofmt -l .)"`: PASS (exit 0; no output).
- `go test ./...` in the worker sandbox: FAIL (exit 1). Tests in these packages could not start
  `httptest` loopback listeners because the sandbox denied `listen tcp6
  [::1]:0` with `socket: operation not permitted`:
  - `cmd/runstead`: `TestLearningCooldownFromRetryAfterAllFamilies/openai_compatible`
  - `internal/provider/anthropiccompat`:
    `TestBaseURLIsConfigurableAndPreservesPrefixes`
  - `internal/provider/compat`:
    `TestMatrixIdentityDistinctFromFamilyAndSingleRequestPerAdmission/openai_compatible`
  - `internal/provider/googlecompat`:
    `TestBaseURLIsConfigurableAndPreservesPrefixes`
  - `internal/provider/omniroute`:
    `TestCompleteConsumesFinalAttemptReceiptHeader`
  - `internal/provider/openaicompat`:
    `TestBaseURLIsConfigurableAndPreservesPrefixes`
  Other package results shown in the completed output passed. This is recorded
  as a sandbox/environment failure.
- `go test ./...` in the workspace executor: PASS (exit 0; 321.413s for
  `cmd/runstead`, full suite exit 0).
- `go vet ./...`: PASS (exit 0; no output).
- `go build ./cmd/runstead`: PASS (exit 0; no output).
- `go test -race ./...` in the worker sandbox: FAIL (exit 1), with the same sandbox loopback-listener
  denial and affected tests listed above. Other package results shown in the
  completed output passed.
- `go test -race ./...` in the workspace executor: PASS (exit 0; 463.054s for
  `cmd/runstead`, full race suite exit 0).
- `bash experiments/protocol/test.sh`: PASS (exit 0):
  `PASS: protocol parser and offline experiment checks`.
- `git diff --check`: PASS (exit 0; no output).
