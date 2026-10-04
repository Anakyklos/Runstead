# Groq GPT OSS 120B live canary report

- Initial-attempt report timestamp: 2026-10-04 01:04:46 UTC
- Initial-attempt Runstead base commit: `937c91c6679b8fa637d3d432ad4ed3038e57fddf`
- Canary issue: #138; adoption tracker: #121
- Provider ID: `groq-gpt-oss-120b-canary`
- Protocol family: `openai_compatible`
- Endpoint: `https://api.groq.com/openai/v1`
- Candidate model: `openai/gpt-oss-120b`
- Auth reference: `GROQ_API_KEY`
- Initial-attempt snapshot — exact model availability: **not confirmed**
- Initial-attempt snapshot — adapter resolution/dispatch: **not attempted**

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

## Continuation — authenticated preflight and Stage 2 (2026-10-04)

- Continuation report timestamp: 2026-10-04 02:07:06 UTC.

This section continues the chronology above. The earlier HTTP 403 is retained
as historical evidence and is not replaced or reclassified. This continuation
uses a fresh branch from current `main` commit
`1d8be6162143ad4bedea4b97e63d3a4985983d10` (PR #139's report-only merge is
already included).

### Stage 1 — authenticated preflight: PASS after the sole controlled rerun

The original `GET https://api.groq.com/openai/v1/models` using Python's
default `Python-urllib/3.12` client returned HTTP 403. Root-cause investigation
kept the same key, endpoint, and model and used the one permitted controlled
rerun with `curl/8.5.0`: HTTP 200, with exact model ID
`openai/gpt-oss-120b` present in the returned list of 11 IDs. The response body
and credential were neither printed nor retained. The declaration resolved
locally through `openai_compatible` without dispatch. The rerun allowance is
consumed; no additional model discovery or preflight request was made.

### Stage 2 — governed protocol task: attempted; acceptance NOT QUALIFIED

The live trajectory was `cli-1791078122075375180` and reached the configured
Groq endpoint through the existing generic adapter
`compatible-provider-v0.1`. Durable provider identity was
`groq-gpt-oss-120b-canary` / `openai_compatible` /
`https://api.groq.com/openai/v1` / `openai/gpt-oss-120b`, with the secret
represented only by the `GROQ_API_KEY` reference. The persisted config identity
records `v1` profile/config versions and single-attempt safety with internal
retry, fallback, pooling, and combo routing disabled.

The task objective asked Runstead to read `app/calc.go`, identify its
`ParseValues` whitespace bug, make no edits or command calls, and cite the
`read_file` observation. The durable trajectory contains two completed,
governor-admitted provider attempts: request IDs
`sha256:3ee4b2cc7243a8fc` and `sha256:196602c70ea4a413`. Both reached upstream,
were accounted and debited once, and completed without uncertain delivery.
For each attempt, the durable `provider_attempt_prepared` event precedes its
completion event; the exercised Runstead path admits the attempt and commits
that prepared record before calling the provider adapter's `Complete` method.
Totals: 2 physical provider requests, 2 admissions, 2 debits, 0 retries, 0
uncertain attempts. The persisted governor task count is 2. No fallback,
rotation, or additional model was used.

The strict protocol produced a completed `read_file` action
`action-000002`, execution `exec-000003`, with successful observation
`obs-000001`. Task terminal state is `completed`, resume count 0. Verifier
`verif-000005` recorded `passed`, but the persisted acceptance plan digest
`a8b286ec2985aac9f119a21716ca1efe63e658c4d10e57e160afe30dc4158680` contained
only `fixture-readme-present` (`file_exists` at `README.md`). Therefore that
verifier result establishes README presence only; it does not independently
accept the `app/calc.go` task objective. This operator acceptance-plan mismatch
makes Stage 2 **not qualified** under this canary's contract. Preserve the task
and verifier records as observed evidence; do not rerun Stage 2.

Failure classification: **operator acceptance-plan/fixture execution error**.
The observed adapter/protocol path and accounting completed, but the acceptance
criteria did not match the objective. No deterministic Runstead defect is
demonstrated. This is the terminal canary decision under the consumed rerun
budget and stop conditions.

### Stage 3 — bounded coding fixture: NOT RUN

Stage 2 did not qualify, so no Stage 3 trajectory, coding write, recipe
execution, acceptance verifier, or terminal result exists. No coding fixture
effects were started for this stage.

### Stage 4 — interruption/resume: NOT RUN

Stage 3 did not pass, so no Stage 4 trajectory was started. No interruption,
inspect/resume operation, recovery attempt, or Stage 4 verifier exists.

### Continuation accounting and data handling

- Stage 1 control-plane requests: 2 total `GET /models` requests (initial
  HTTP 403 plus the one controlled HTTP 200 rerun); outside the Runstead
  governor.
- Stage 2: 2 physical provider requests, 2 admissions, 2 debits, 0 retries.
- Stages 3 and 4: 0 provider requests, 0 admissions, 0 debits.
- The task used only the public synthetic coding-loop fixture. No secret value,
  raw authentication header, raw live transcript, or private content was
  persisted in Git, provider JSON, SQLite, evidence, logs/traces, issue/PR, or
  model context. The key remained an environment-only reference and
  `.env.local` remains ignored.
- `docs/provider-compatibility.md` remains unchanged. Stages 2, 3, and 4 did
  not all pass, so no positive compatibility claim is supported.
- Gate A is **NOT SATISFIED**. Issue #138 records the terminal Stage 2
  acceptance mismatch; #123 remains blocked and #121 remains open. M12+ is not
  promoted.

### Validation on the continuation branch

These requested gates were run on the report-only continuation branch at
`1d8be6162143ad4bedea4b97e63d3a4985983d10` plus this report update:

- `test -z "$(gofmt -l .)"`: PASS (exit 0; no output).
- `go test ./...`: PASS (exit 0; `cmd/runstead` 313.723s; all packages
  passed).
- `go vet ./...`: PASS (exit 0; no output).
- `go build ./cmd/runstead`: PASS (exit 0; no output).
- `go test -race ./...`: PASS (exit 0; `cmd/runstead` 398.418s; all packages
  passed).
- `bash experiments/protocol/test.sh`: PASS (exit 0):
  `PASS: protocol parser and offline experiment checks`.
- `git diff --check`: PASS (exit 0; no output).

These repository gates validate the branch contents; they do not change the
Stage 2 acceptance decision or satisfy Gate A.
