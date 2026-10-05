# Groq GPT-OSS-120B Gate A canary v5 — 2026-10-05

## Identity and starting state

- Issue: #155 — `[P0][PROVIDER CANARY] Groq GPT-OSS-120B Gate A canary v5`
- Branch: `issue-155-groq-gpt-oss-120b-canary-v5`
- Base / starting HEAD: `origin/main` / `486c8baa7dfe376ee8884429d35d657f3431400b` (merge commit for PR #154)
- Provider declaration required by the issue:

  ```text
  provider_id: groq-gpt-oss-120b-canary-v5
  protocol_family: openai_compatible
  base_url: https://api.groq.com/openai/v1
  model: openai/gpt-oss-120b
  auth_requirement: reference_required
  auth_ref: GROQ_API_KEY
  ```

## Decision summary

**Gate A = NOT SATISFIED.** No provider request was made. The required `GROQ_API_KEY` reference was absent from the canary process environment at preflight. The order requires stopping before the first live request if the reference is absent. Stage 1 therefore stopped before authenticated model discovery; Stages 2, 3, and 4 were skipped. Classification: **operator/environment prerequisite missing**, not a provider failure, model-protocol failure, or Runstead defect.

No task IDs, governed attempts, admissions, debits, actions, tool results, observations, verifier attempts, provider failure classes, rate-limit observations, governor outcomes, or delivery states exist for this v5 trajectory. These are not inferred from prior canaries.

## Stage preflight and outcome

### Stage 1 — auth/model: BLOCKED before live dispatch

- Objective: resolve the fixed declaration through the existing `openai_compatible` family; prove reference-only auth and the exact authenticated model; no alternate selection or persisted credential.
- Allowed effects: local config resolution; at most the one authenticated exact-model control needed after confirming the external secret reference.
- Prohibited effects: any task/model request before the required secret reference exists; model sweep; fallback/rotation; credential persistence.
- Acceptance: exact config resolves with no provider dispatch; `GROQ_API_KEY` reference is externally available without displaying its value; no more than one authenticated `GET /models` proves `openai/gpt-oss-120b`; no alternate selection/fallback or credential persistence.
- Secret check: `GROQ_API_KEY` was absent from the process environment and the optional `RUNSTEAD_GROQ_ENV_FILE` reference was also absent. Neither a credential value nor a local environment file was read or loaded. Per the issue contract, execution stopped before authenticated model discovery.
- The exact declaration resolved through the existing `openai_compatible` path with `adapter_constructed=false` and `provider_dispatches=0`. The authenticated model's availability to this account and auth handling in a live request remain unproven.

### Stage 2 — read-only protocol/evidence/verifier: SKIPPED

Conditional on Stage 1 PASS. No fixture copy or task was created. Required task objective and acceptance remain frozen in #155. Task ID: none. Attempts/admissions/debits: 0/0/0. Actions/tools/evidence/verifiers: none.

### Stage 3 — bounded coding fixture: SKIPPED

Conditional on Stage 2 PASS. No coding workspace or task was created; no recipe or process ran. Task ID: none. Attempts/admissions/debits: 0/0/0. Writes/hashes/verifiers: none. No v5 rate/capacity failure occurred, so all provider rate observations are not applicable/unknown; no rerun was considered or used.

### Stage 4 — interruption/resume: SKIPPED

Conditional on Stage 3 PASS. No task, interruption, inspect, or resume occurred. Task ID: none. Recovery, effect replay, and uncertain-delivery behavior were not exercised in v5.

## External Groq documentation (checked 2026-10-05)

- Groq's [OpenAI compatibility documentation](https://console.groq.com/docs/openai) documents use of the `https://api.groq.com/openai/v1` base URL with OpenAI client libraries.
- Groq's [supported models documentation](https://console.groq.com/docs/models) lists `openai/gpt-oss-120b` and describes the authenticated `/models` endpoint. This public documentation does not establish that the model was available to this account; the required authenticated check did not run.
- Groq's [rate limits documentation](https://console.groq.com/docs/rate-limits) describes rate-limit dimensions and response headers, including request/token counters and resets. Published plan values and account limits are mutable external information, not Runstead invariants. No v5 request was made, so no live headers or provider limits were observed.

## Secret hygiene

- The canary process environment did not contain the `GROQ_API_KEY` reference at preflight.
- No credential value was read, printed, logged, requested, persisted, or added to the repository.
- No live request, provider body, private transcript, or response header exists for this trajectory.
- Exact-value retained-state scanning is unavailable without the external key; this report makes no claim based on a key value that was not present.

## Offline validation

Results are recorded after executing the pre-dispatch and repository gates on this branch. The required CI gate inventory is taken from `.github/workflows/ci.yml`; v5 offline harness checks are additional where present.

| Gate | Result | Evidence |
|---|---|---|
| `test -z "$(gofmt -l .)"` | PASS | exit 0 on final code |
| `go test ./...` | PASS | exit 0 on final code; CLI package completed in 314.8 s |
| `go vet ./...` | PASS | exit 0 on final code |
| `go build ./cmd/runstead` | PASS | exit 0 on final code; generated binary removed |
| `go test -race ./...` | PASS | final run exit 0; CLI package completed in 501.9 s. One earlier run timed out after 735.8 s; all packages passed on this final run |
| `bash experiments/protocol/test.sh` | PASS | `PASS: protocol parser and offline experiment checks` |
| provider-abstraction Go test/vet/build | PASS | all three commands exit 0 |
| sidecar install, tests, lint, compile | PASS | temporary Python 3.11 environment; 36 tests passed; Ruff and compileall passed |
| cookie-encryption residue scan | PASS | exit 0 |
| standalone browser-substrate checks | PASS | all deterministic checks passed |
| protocol golden-corpus gate | PASS | `go test -count=1 ...` exit 0 |
| quality tool build, self-tests, vet, growth, errcheck, live-convention | PASS | all commands exit 0 |
| v5 no-dispatch resolver | PASS | exact config; `adapter_constructed=false`; `provider_dispatches=0` |
| v5 model-control guard and offline harness/audit tests | PASS | missing explicit dispatch flag yields 0 requests; 12 Python unit tests pass with mock only |
| `git diff --check` | PASS | staged diff exits 0 |

## Limitations and next step

This v5 trajectory provides no live provider compatibility or Gate A evidence. It stops solely because the required external secret reference was unavailable in the execution environment. A future attempt requires a fresh, explicitly authorized child issue and branch/state trajectory with `GROQ_API_KEY` injected externally before Stage 1; this report and issue #155 must remain an honest failed/preflight-blocked record. #123 remains blocked. No provider failure, rate-limit diagnosis, or Runstead defect is claimed.
