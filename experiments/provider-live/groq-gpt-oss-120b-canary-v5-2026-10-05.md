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

**Gate A = NOT SATISFIED.** Stage 1 PASS: the one authorized authenticated `GET /models` returned HTTP 200 with exact model `openai/gpt-oss-120b`. Stage 2 PASS: task `cli-1791215619229247194` completed with real `read_file` evidence and independent verifier PASS. Stage 3 FAIL: task `cli-1791215744924042767` received provider HTTP 429, classified as `rate_or_capacity`, after seven governed attempts; replay safety was not proven, so the single controlled rerun allowance was not used. Stage 4 SKIPPED. Classification: **provider failure**, with no specific rate-limit dimension inferred; no Runstead defect observed.

## Historical checkpoint — external reference supplied, key unavailable

The initial checkpoint below is preserved as historical evidence. It was an environment-propagation failure: the external file existed, but the canary process did not receive `RUNSTEAD_GROQ_ENV_FILE`. That checkpoint had zero provider requests, attempts, admissions, and debits and consumed no controlled rerun.

On continuation, the same branch and PR #156 were reused. Boolean checks proved the expected file is a regular file and that `RUNSTEAD_GROQ_ENV_FILE` matched the expected path in the isolated loader process. `load_key_into_process()` was called without authorizing dispatch. The loader found the `GROQ_API_KEY` declaration but did not produce a nonempty value (`key_loaded=false`); no credential value was emitted. The sanitized source reference was `external_env_file`, but Stage 1 cannot proceed until the external file yields a nonempty key.

Provider requests before and after the environment-reference correction: **0**. Controlled rerun consumed: **NO**. No SQLite state/task or external provider effect was created. Stage 1 remains blocked and Stages 2–4 remain skipped. No value from the external file was added to Git, SQLite, logs, or this report.

No task IDs, governed attempts, admissions, debits, actions, tool results, observations, verifier attempts, provider failure classes, rate-limit observations, governor outcomes, or delivery states exist for this v5 trajectory. These are not inferred from prior canaries.

## Stage preflight and outcome

### Stage 1 — auth/model: PASS

- Objective: resolve the fixed declaration through the existing `openai_compatible` family; prove reference-only auth and the exact authenticated model; no alternate selection or persisted credential.
- Allowed effects: local config resolution; at most the one authenticated exact-model control needed after confirming the external secret reference.
- Prohibited effects: any task/model request before the required secret reference exists; model sweep; fallback/rotation; credential persistence.
- Acceptance: exact config resolves with no provider dispatch; `GROQ_API_KEY` reference is externally available without displaying its value; no more than one authenticated `GET /models` proves `openai/gpt-oss-120b`; no alternate selection/fallback or credential persistence.
- Boolean-only secret preflight confirmed an external regular env file, a nonempty loaded key, and `secret_value_emitted=false`; sanitized source: `external_env_file`. Exact provider resolution used the existing `openai_compatible` path with `adapter_constructed=false` and `provider_dispatches=0` before dispatch.
- Exactly one authenticated control request was sent: `GET /models`, `request_count=1`, HTTP 200, and exact `openai/gpt-oss-120b` present. No other model was selected. Stage 1 acceptance passed; no key was printed, logged, committed, or persisted.

### Stage 2 — read-only protocol/evidence/verifier: PASS

Task `cli-1791215619229247194` ran on a fresh no-Git fixture copy and fresh SQLite state. Attempts/admissions/debits: 2/2/2; retries: 0. One successful `read_file` of `app/calc.go` produced real result/observation `obs-000001`; the grounded final response and independent verifier both cite `obs-000001`. Writes: 0; recipes/processes: 0. Verifier PASS; terminal `completed`; sanitized retained-state secret scan clean. Artifacts are preserved under `/tmp/runstead-v5-stage2-worker/run-1/`.

### Stage 3 — bounded coding fixture: FAIL — provider failure

- Preflight: the unchanged objective, allowed/prohibited effects, and acceptance from `stage3-preflight.json` were checked before dispatch. A fresh Git workspace copied from the committed fixture started clean at `d6d06f1507c642f9d064406f8872d993ce8dfa28`; initial `app/calc.go` SHA-256 was `b8a1bd5…6986d94`, expected accepted fix hash was `1c5aa56…883b03`. The declared recipe was `go test ./...` in `app/`, timeout 120s.
- Task `cli-1791215744924042767` ended `failed/provider_failure`, stop reason `rate_or_capacity`. Attempts/admissions/debits: 7/7/7; retries: 0. Attempts 1–6 succeeded (HTTP 200); attempt 7 failed on HTTP 429. The provider response was fully observed (`delivery_state=completed`, `uncertain=0`, `upstream_reached=1`).
- The task performed two `list_files` and two `read_file` actions, including actual inspection of `app/calc.go` and `app/calc_test.go`; four corresponding tool results were recorded. It made no write, ran no recipe, attempted no verifier, and the terminal task did not complete. The fixture remained clean at its baseline commit and original source hash. No runtime defect was observed.
- **Provider observations and governor decisions are separate.** The following allowlisted fields were read from durable typed attempt evidence. Missing values are `unknown`. Durations are provider-reported reset hints, not interpreted quota dimensions.

| Attempt | Provider `http_status` | `observed_retry_after` | `observed_reset_at` | `limit_requests / remaining_requests / reset_requests` | `limit_tokens / remaining_tokens / reset_tokens` | Governor `selected_backoff` | `provider_failure_class` | `delivery_state` |
|---:|---:|---:|---:|---|---|---:|---|---|
| 1 | 200 | unknown | unknown | 1000 / 998 / 172800ms | 8000 / 6475 / 11437ms | 0s | unknown | completed |
| 2 | 200 | unknown | unknown | 1000 / 997 / 259200ms | 8000 / 5531 / 18517ms | 0s | unknown | completed |
| 3 | 200 | unknown | unknown | 1000 / 996 / 345600ms | 8000 / 4771 / 24217ms | 0s | unknown | completed |
| 4 | 200 | unknown | unknown | 1000 / 995 / 432000ms | 8000 / 3549 / 33382ms | 0s | unknown | completed |
| 5 | 200 | unknown | unknown | 1000 / 994 / 518400ms | 8000 / 2022 / 44835ms | 0s | unknown | completed |
| 6 | 200 | unknown | unknown | 1000 / 993 / 604800ms | 8000 / 1587 / 48097ms | 0s | unknown | completed |
| 7 | 429 | 3s | unknown | 1000 / 993 / 604800ms | 8000 / 3036 / 37230ms | 3s | rate_or_capacity | completed |

- The HTTP 429 alone does not establish RPM, TPM, ITPM, OTPM, RPD, TPD, or generic capacity exhaustion. No rate-limit dimension is inferred. Attempt 7 was certain and fully delivered; the failure is classified as **provider failure: rate_or_capacity**.

#### Controlled rerun decision

No controlled rerun was dispatched; the one-run allowance remains unused. Query-only inspection of the preserved SQLite state showed attempt 7 had `uncertain=0`, `delivery_state=completed`, `upstream_reached=1`, no receipt-aware retry, a closed circuit, and an elapsed 3-second cooldown. However, the existing `DeliveryState.ReplaySafe()` contract proves replay safety only for `not_sent`; this request reached the provider and completed. The task used `--retry-policy off` and has no durable `RetryEligible` decision. Because current policy/evidence therefore does not prove a fresh-task replay safe, the explicit rerun condition was not met. The initial trajectory is preserved unchanged.

### Stage 4 — interruption/resume: SKIPPED

Conditional on Stage 3 PASS, which did not occur. No task, interruption, inspect, or resume was created. Recovery, effect replay, and uncertain-delivery behavior remain untested in v5.

## External Groq documentation (checked 2026-10-05)

- Groq's [OpenAI compatibility documentation](https://console.groq.com/docs/openai) documents use of the `https://api.groq.com/openai/v1` base URL with OpenAI client libraries.
- Groq's [supported models documentation](https://console.groq.com/docs/models) lists `openai/gpt-oss-120b` and describes the `/models` endpoint. The one authorized authenticated account check returned HTTP 200 and confirmed the exact model ID.
- Groq's [rate limits documentation](https://console.groq.com/docs/rate-limits) describes rate-limit dimensions and response headers, including request/token counters and resets. Published plan values and account limits are mutable external information, not Runstead invariants. V5 records only the typed allowlisted observations above; it does not treat the observed values as current account limits or infer a limit dimension.

## Secret hygiene

- The key was loaded from the operator-owned external env file into the authorized request process; only boolean presence and sanitized source (`external_env_file`) were recorded.
- No credential value was emitted, logged, committed, or persisted in report, SQLite, or repository artifacts.
- Raw provider bodies, private transcripts, prompts, and arbitrary response headers are not retained. Stage 1 kept only request count, method/path, HTTP status, and exact-model-present boolean; Stage 3 kept only the typed fields enumerated in the report.
- Stage 2 and Stage 3 retained-state secret scans were clean. No exact credential value is retained for scanning.

## Offline validation

Final required gates were rerun against the canary source HEAD `1f581c88b2578f9327e6b945dc502d9e472b5661` with the final report content present in the working tree. The report-only commit does not change executable source. The required CI gate inventory is taken from `.github/workflows/ci.yml`; v5 offline harness checks are additional where present.

| Gate | Result | Evidence |
|---|---|---|
| `test -z "$(gofmt -l .)"` | PASS | exit 0 on final source HEAD |
| `go test ./...` | PASS | exit 0; CLI package completed in 314.058 s |
| `go vet ./...` | PASS | exit 0 |
| `go build ./cmd/runstead` | PASS | exit 0; generated binary removed |
| `go test -race ./...` | PASS | exit 0; CLI package completed in 451.337 s |
| `bash experiments/protocol/test.sh` | PASS | `PASS: protocol parser and offline experiment checks` |
| provider-abstraction Go test/vet/build | PASS | all three commands exit 0 |
| sidecar install, tests, lint, compile | PASS | temporary Python 3.11 environment; 36 tests passed; Ruff and compileall passed |
| cookie-encryption residue scan | PASS | exit 0 |
| standalone browser-substrate checks | PASS | all deterministic checks passed |
| protocol golden-corpus gate | PASS | `go test -count=1 ...` exit 0 |
| quality tool build, self-tests, vet, growth, errcheck, live-convention | PASS | all commands exit 0 |
| v5 no-dispatch resolver | PASS | exact config; `adapter_constructed=false`; `provider_dispatches=0` |
| v5 model-control guard and offline harness/audit tests | PASS | missing explicit dispatch flag yields 0 requests; 12 Python unit tests pass with mocks only |
| Stage 3 audit/typed-observation offline tests | PASS | five tests passed before the single governed Stage 3 dispatch |
| external secret-reference and exact model control | PASS | sanitized booleans confirmed key loaded from external env file; exactly one authenticated `GET /models`, HTTP 200, exact model present; no secret emitted |
| `git diff --check` | PASS | rerun after final report edit; exit 0 |

## Limitations and next step

**Gate A = NOT SATISFIED.** Stages 1 and 2 passed; Stage 3 failed with a provider-reported HTTP 429 classified as `rate_or_capacity`; Stage 4 was skipped. The persisted provider observations establish HTTP status, Retry-After, and counters/resets as listed, but do not identify a specific rate-limit dimension or prove a capacity subtype. The one controlled rerun allowance remains unused because current policy/evidence does not prove replay safety for a completed, upstream-reached delivery. No Runstead defect was observed. Secret hygiene passed. #123 remains blocked; no merge, dogfood, or follow-on issue was started.
