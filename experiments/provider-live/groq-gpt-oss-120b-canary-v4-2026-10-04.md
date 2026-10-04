# Groq GPT-OSS-120B Gate A canary v4

Status: Gate A NOT SATISFIED. No raw provider body or transcript was retained.

## Identity and candidate

- Issue: #149
- Branch: `issue-149-groq-gpt-oss-120b-canary-v4`
- Base: `7306bc46fdd430e1c83b3d0ba386145b83ca67a4`
- HEAD used to build and resolve Runstead for the canary:
  `7306bc46fdd430e1c83b3d0ba386145b83ca67a4`.
- Candidate: `groq-gpt-oss-120b-canary-v4`
- Protocol family: `openai_compatible`
- Base URL: `https://api.groq.com/openai/v1`
- Model: `openai/gpt-oss-120b`
- Authentication: `reference_required`, `GROQ_API_KEY`
- Historical trajectories: v1/v2/v3 were not opened, resumed, or reused as tasks.

## Acceptance preflight — Stage 1 (registered before any provider request)

- Objective: build this exact `main`, resolve the exact configured candidate
  through `openai_compatible` without constructing an adapter or dispatching,
  then prove the exact model is available with at most one authenticated
  `GET /models`.
- Required observable evidence: exact base and candidate commit; build exit
  status; resolver output for provider ID, family, base URL, model, auth
  reference and sanitized identity; static proof no Groq-specific adapter is
  involved; zero dispatches from resolution; secret presence only; if the
  credential is available, one bounded authenticated request with explicit
  User-Agent, HTTP 200 and exact model ID present.
- Acceptance checks: build passes on the expected base; the declaration
  matches every candidate field; resolution chooses `openai_compatible`,
  constructs no adapter and dispatches zero requests; no Groq-specific
  adapter exists; the sole allowed request is `GET /models`; the exact model
  is present. Any mismatch stops before the request.
- Allowed effects: create local experiment metadata; build Runstead; run the
  no-network resolver; check whether the exact auth reference is available
  without printing it; make zero or one authenticated `GET /models` request.
- Prohibited effects: chat/completion dispatch; another request, retry,
  redirect, model sweep, quota probe, key/account rotation, fallback, or
  alternate provider/model; retaining request/response bodies or headers;
  persisting or printing the secret.
- Preflight status: registered before dispatch. The exact candidate resolved;
  build, static adapter audit, harness checks, and private credential-presence
  check passed before the single permitted request.

### Stage 1 result

- `go build ./cmd/runstead`: PASS on the expected `main` commit.
- The resolution helper returned the exact provider ID, family, endpoint,
  model, auth requirement and reference; `adapter_constructed=false` and
  `provider_dispatches=0`.
- Static scan of `internal/provider`: no Groq-specific adapter or provider
  code found.
- The exact `GROQ_API_KEY` auth reference was present in the ignored
  `.env.local`; its value was loaded only into the calling process and never
  printed or written.
- Authenticated discovery: exactly one `GET /models`, User-Agent
  `Runstead-GateA-Canary/1.0`, HTTP 200, exact model present. No response body
  or headers were retained.
- Stage 1: **PASS**.

## Acceptance preflight — Stage 2 (registered before task dispatch)

- Objective persisted by the task: `Read app/calc.go using the available
  repository tool. Complete only after citing the actual read_file observation
  for app/calc.go. Do not modify files and do not run recipes or processes.`
  The sentences, order and punctuation match the requested objective; the
  harness passed them as one line with spaces between sentences.
- Required observable evidence: new fixture workspace and state directory;
  frozen pre-task SHA-256 of `app/calc.go`; exact objective and frozen provider
  contract; persisted `read_file(app/calc.go)` action, successful tool attempt
  and observation; final citation ID matching that observation; zero write and
  recipe/process attempts; acceptance saved before provider dispatch and
  unchanged afterward; independent verifier PASS; task terminal `completed`;
  all physical attempts reconciled to admissions/debits and separately logged
  outcome, delivery, provider failure class and receipt error; bounded key scan
  of retained state.
- Runtime acceptance: frozen `file_hash` check for the unchanged fixture file.
  The built-in verifier also requires a real in-run evidence citation. The
  independent SQLite audit adds the objective-specific `read_file` path and
  citation match, which the typed acceptance-plan schema cannot express as a
  check. Together these checks cover both the task objective and its unchanged
  workspace state; no acceptance mutation is allowed after dispatch.
- Acceptance checks: profile selects only `repo.read@1.0.0` (read tools and
  Git metadata; no writes or process recipe capability); the persisted plan
  is byte-for-byte the preflight file; its file hash matches the fresh copy;
  runtime verifier PASS is supplemented by the fail-closed history audit
  above. Provider ID/model/config are frozen to the exact Stage 1 candidate.
- Allowed effects: one fresh copy of `fixtures/coding-loop`; its local Git
  baseline; one new state directory/task; read-only repository tools; normal
  governed turns for that task (maximum 12 physical attempts, retry policy
  off); runtime verification; read-only SQLite audit and bounded exact-key
  scan.
- Prohibited effects: workspace writes; recipe/process execution; changing
  objective, profile, acceptance, provider/model/key or bounds after dispatch;
  second task or controlled rerun; automatic adapter retry/fallback; retaining
  raw provider bodies, transcripts or error text.
- Preflight status: objective and combined runtime/history acceptance are
  aligned; exact fixture hash is
  `b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94`;
  profile exposes read-only tools only. No Stage 2 provider request has been
  made.

## Stage results

### Stage 2 result

- Fresh task: `cli-1791148984761603981`; fresh workspace baseline commit
  `ad81bdd77a4ac5afbbe5934d11c715f7d1af5f6b`; `app/calc.go` hash matched
  before and after; Git diff remained clean.
- Terminal: status/outcome `completed`; Runstead exit code 0; independent
  verifier sequence 1 `passed`; acceptance digest
  `3c1206c11be99bbd41f780830b899902ba3bb17c807d67081a30bfdd9e932af1` was
  saved before dispatch and never changed.
- Provider attempts/admissions/debits: 2 / 2 / 2; retry count 0. `exec-000001`
  and `exec-000004` each completed with outcome `success`, delivery
  `completed`, empty `provider_failure_class`, empty `receipt_error`, and one
  debit. No provider failure occurred.
- Actions and evidence: one action and one tool attempt, `exec-000003`
  `read_file`, completed successfully for `app/calc.go`; result path and
  observation path both matched. Evidence `obs-000001` was cited by the final
  and by the verifier as `read_file`; the verifier confirmed it existed and
  the claimed tool matched.
- Effects: zero writes; zero recipes/processes. The workspace file hash stayed
  `b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94`.
- The bounded exact-key scan of retained Stage 2 state returned `clean`.
  The sanitized audit reported `PASS` with no failed checks. No controlled
  rerun was used.
- Stage 2: **PASS**.

## Acceptance preflight — Stage 3 (registered before task dispatch)

- Objective, verbatim: `Fix the whitespace handling bug in app/calc.go so the
  test suite passes. Inspect the relevant code and tests, make the minimum
  correct scoped change, run the declared test recipe, and finish only after
  Runstead's independent acceptance verification passes.`
- Required observable evidence: a fresh Git workspace from the committed
  fixture; task ID and exact objective; frozen provider/model/config;
  persisted read observations for `app/calc.go` and relevant tests; a real
  stale-state-protected write with before/after hashes; changed paths limited
  to `app/calc.go`; final content matching the correct whitespace fix;
  declared `test` recipe actually executed and exiting 0; admissions/debits
  reconciled; verifier PASS; terminal `completed`; exact-key scan of retained
  state; each provider attempt separately reported by outcome, delivery,
  provider failure class and receipt error.
- Acceptance checks: preserve the fixture's declared `test` recipe and its
  `tests-pass` recipe check, and add a pre-dispatch `file_hash` check requiring
  `app/calc.go` to match the fixture's reviewed `fixes/calc-correct.go` hash.
  The independent Stage 3 audit will additionally require code/test
  inspection, stale-state evidence and no unrelated file changes. The combined
  predicate matches the objective and was fixed before dispatch.
- Allowed effects: create a second fresh fixture workspace and state
  directory; one task with the exact Stage 1 candidate; read tools; scoped
  `write_file`/`apply_patch`; the fixture's declared `test` recipe only; normal
  governed turns (maximum 24 physical attempts, retry policy off); independent
  verifier; read-only state audit and bounded exact-key scan.
- Prohibited effects: alternate provider, model, credential or endpoint;
  writes outside `app/calc.go`; undeclared recipes or generic processes;
  changing task, acceptance, recipe catalog or bounds after dispatch; a second
  task or controlled rerun; adapter retry/fallback; raw provider body,
  transcript or error text retention.
- Preflight status: Stage 2 passed. Fresh Stage 3 workspace commit is
  `6f19318218fd23016e91e7ae6680fab7564590b8`; initial
  `app/calc.go` SHA-256 is
  `b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94`; the
  reviewed correct-file SHA-256 is
  `1c5aa56c1715d93c8d72f6d62b4695c123047977d81528e12d7be5776a883b03`.
  The fixture's code, tests and recipe are unchanged. Acceptance and recipe
  hashes are fixed before dispatch. No Stage 3 provider request has been made.

### Stage 3 initial task result

- Fresh task: `cli-1791149781504182558`; workspace baseline commit
  `6f19318218fd23016e91e7ae6680fab7564590b8`; terminal status `failed`,
  outcome `provider_failure`, stop reason `provider failure: rate_or_capacity`.
- Six physical provider attempts, six admissions and six debits; automatic
  retry count 0. Sequences 1–5 (`exec-000001`, `exec-000004`, `exec-000007`,
  `exec-000010`, `exec-000013`) completed successfully with delivery
  `completed`, empty provider failure class and empty receipt error.
- Sequence 6 (`exec-000016`) recorded outcome `rate_or_capacity`, delivery
  `completed`, `provider_failure_class=rate_or_capacity`,
  `receipt_error=empty`, `upstream_reached=1`, `uncertain=0`, debit 1. No
  `sent_unconfirmed` delivery occurred. The persisted failure class is
  diagnostic and does not alter the governor outcome or accounting.
- Actions/tools: two successful `list_files` reads and two successful
  `read_file` observations (`obs-000001` for `.`, `obs-000002` for `app`,
  `obs-000003` for `app/calc.go`, `obs-000004` for `app/calc_test.go`), then one
  `apply_patch` attempt failed with typed `invalid_patch`; no result/effect was
  persisted for that attempt. Zero completed writes; zero recipe/process
  attempts; source remained at its original hash and Git diff was empty.
- No completion proposal or verification attempt was persisted; verifier PASS
  and terminal `completed` were not reached. Bounded exact-key scan of the
  retained state was `clean`. The sanitized audit reported
  `NOT_QUALIFIED`; Stage 3 did not pass.
- The first offline audit invocation used an incorrect expected-fix path and
  returned `FileNotFoundError`; the audit was rerun with the correct local
  fixture path. This was audit-only and made no provider request.

## Controlled rerun preflight — Stage 3 (registered before rerun dispatch)

- Objective: the exact four-line Stage 3 objective above, unchanged.
- Required evidence and acceptance: unchanged from the Stage 3 preflight;
  same `test` recipe, same `stage3-acceptance.json` bytes/digest, same expected
  corrected source hash, fresh task/workspace, and the complete independent
  Stage 3 audit. The acceptance file is not changed for this rerun.
- Allowed effects: exactly one fresh Stage 3 workspace and task; reuse the
  existing state directory solely to preserve governor cooldown, circuit,
  and rolling accounting; same provider/model/auth reference; retry policy
  off; maximum 24 physical attempts; exact-key scan after completion.
- Prohibited effects: any additional rerun; changing provider, model, key,
  endpoint, objective, acceptance, recipe or bound; replaying an uncertain
  delivery; using the rerun merely to seek a green result; Stage 4 unless this
  Stage 3 task passes; raw provider bodies, transcripts or error text.
- Safety basis: this was one concrete typed `rate_or_capacity` failure with
  `delivery_state=completed` and `uncertain=0`, matching the separate provider
  failure class; no write or process effect completed. The durable governor
  showed a closed circuit, no refresh requirement and a cooldown that expired
  at `2026-10-04T21:37:07.590148583Z`; the current clock was
  `2026-10-04 21:42:45 UTC`. The task had six attempts, zero retries, and the
  existing retry policy classifies `rate_or_capacity` as recoverable only
  when delivery is safely completed. Reusing the same state directory keeps
  that governor history authoritative. This is the canary's sole controlled
  rerun; if it fails, the canary stops.
- Preflight status: acceptance/objective remain aligned; fresh rerun workspace
  commit `53c463084ea6a40acee7706b19bf4cdfaa922a60` has the original
  `app/calc.go` hash; no further Stage 3 request has been made.

### Stage 3 sole controlled rerun result

- Fresh task: `cli-1791150335547312794`; fresh workspace baseline commit
  `53c463084ea6a40acee7706b19bf4cdfaa922a60`; the original Stage 3 task and
  its state remain preserved. The same durable state directory retained the
  governor cooldown/circuit/rolling ledger.
- Terminal: status `failed`, outcome `provider_failure`, stop reason
  `provider failure: rate_or_capacity`. The controlled rerun ended after six
  physical attempts; sequences 7–11 were successful, sequence 12 failed.
  Rerun attempts/admissions/debits were 6/6/6, retries 0. Across both Stage 3
  tasks totals were 12/12/12 with no automatic retries.
- Sequence 12 (`exec-000032`) recorded outcome `rate_or_capacity`, delivery
  `completed`, `provider_failure_class=rate_or_capacity`,
  `receipt_error=empty`, and debit 1. It was not `sent_unconfirmed` and was
  not replayed. Sequences 7–11 completed with outcome `success`, delivery
  `completed`, empty provider failure class and empty receipt error.
- Actions/tools: two successful `list_files`, two successful `read_file`
  observations (`obs-000001` for `.`, `obs-000002` for `app`,
  `obs-000003` for `app/calc.go`, `obs-000004` for `app/calc_test.go`), and
  one successful
  stale-state-protected `write_file` attempt `exec-000031`, evidence
  `obs-000005`. Its path was `app/calc.go`; observed before hash and
  `expected_before_hash` were the initial fixture hash
  `b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94`; its
  after/effect hash and final file hash were
  `d520060356cb9acbb665b0cf2ebd5c9fba4a123198b2f192d2fab96e9ae4693c`.
  Git showed only `app/calc.go` changed.
- No recipe or process ran; no verification attempt ran; the task did not
  reach terminal `completed`. The final hash did not match the reviewed fix
  hash, so the pre-dispatch acceptance could not pass. The auditor reported
  `NOT_QUALIFIED`; its exact-key scan across both Stage 3 task histories was
  `clean`.
- The one controlled rerun was consumed. Stage 3: **NOT SATISFIED**. Stage 4
  was not dispatched. No additional rerun or provider request is permitted
  for this canary.
## Stage 4 — interruption/resume

Not dispatched because Stage 3 did not pass. There is no Stage 4 task ID,
interruption, resume, verifier result or no-replay claim.

## Stage summary and Gate A

| Stage | Result | Evidence |
| --- | --- | --- |
| 1 auth/config/model | PASS | exact `openai_compatible` resolution; one authenticated `GET /models`; exact model present |
| 2 read-only loop | PASS | task `cli-1791148984761603981`, verifier PASS, cited `obs-000001`, zero effects |
| 3 coding fixture | NOT SATISFIED | initial task and sole controlled rerun both terminal `provider_failure`; no recipe PASS or verifier PASS |
| 4 interruption/resume | NOT RUN | gated on Stage 3 PASS |

**Gate A: NOT SATISFIED.** The candidate was authenticated and model discovery
passed, and Stage 2 completed. Stage 3 failed twice with typed
`rate_or_capacity` outcomes; the single safe controlled rerun was consumed.
No compatibility claim is added. Issues #121/#123 were not changed, and #123
was not started.

## Limitations

- Stage 3 did not execute the declared test recipe and did not reach
  independent verification. Its source edit is preserved in the controlled
  rerun workspace, but the after hash differed from the reviewed fixture fix.
- Stage 4 has no evidence because its dependency, Stage 3 PASS, was not met.
- The live evidence supports only this exact endpoint/model/candidate and
  these completed stages. It does not establish a Gate A pass or universal
  Groq support.

## Repository gates

- `test -z "$(gofmt -l .)"`: PASS
- `go test ./...`: PASS (`cmd/runstead` 316.596s)
- `go vet ./...`: PASS
- `go build ./cmd/runstead`: PASS
- `go test -race ./...`: PASS (`cmd/runstead` 466.755s)
- `bash experiments/protocol/test.sh`: PASS
- Experimental harnesses: PASS, 7 unit tests; Python bytecode compilation
  passed.
- `git diff --check`: PASS after the report and harness additions.

## Antigravity worker

Antigravity (`Northstar`, Gemini 3.1 Pro) performed a read-only review of
Stage 1 commands and the Stage 2–4 acceptance risks against the verified
`main`. It made no file changes, Runstead configuration changes, Groq requests
or credential access. Its findings were treated as suggestions and checked
against repository sources before use.

## Secret hygiene

The key value was never printed or recorded in Git, SQLite, this report,
prompts, traces or evidence. Stage 2 and Stage 3 retained-state scans loaded
the configured exact key without displaying it and returned `clean`. No raw
provider response body, transcript or raw provider error text was retained.

## Gate A decision

**NOT SATISFIED.**
