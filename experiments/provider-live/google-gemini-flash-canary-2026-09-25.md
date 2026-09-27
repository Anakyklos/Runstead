# Google Gemini Flash live canary report

- Issue: #131
- Execution/report timestamp (UTC): 2026-09-25T19:54:09Z
- Runstead commit tested: `79ac8baaa2abcd428822fb39124b8490b64dfc21`
- Branch: `issue-131-google-gemini-live-canary`
- Provider ID: `google-gemini-canary`
- Protocol family: `google_compatible`
- Endpoint: `https://generativelanguage.googleapis.com/v1beta`
- Exact model: `gemini-3.8-flash`
- Auth reference: `GEMINI_API_KEY`
- Adapter: existing `internal/provider/googlecompat`, `compatible-provider-v0.1`
- Config version: `google-gemini-canary-2026-09-25-gemini-3.8-flash`

## Safety and deterministic baseline

- `test -n "${GEMINI_API_KEY:-}"`: passed before any live traffic. The key value was not printed or persisted; only its environment variable name was used in the declaration.
- `go build -o /tmp/runstead-gemini-canary ./cmd/runstead`: passed.
- The candidate passed `Registry.Resolve` with the required capabilities and safe RouteSafety declaration; a deliberately missing temporary acceptance file stopped that no-dispatch CLI check before `compat.New`/adapter construction. The real Stage 2 invocation subsequently constructed and dispatched through the existing `google_compatible` adapter.
- Candidate used `reference_required`, `auth_ref: GEMINI_API_KEY`, empty options, required `text_turn`/`runstead_protocol` capabilities, and the existing single-attempt `RouteSafety` declaration. No adapter or trust behavior was changed.
- Committed fixture contracts inspected: `fixtures/coding-loop/acceptance.json` and `fixtures/coding-loop/recipes.json`.
- Live canary stayed explicit opt-in. `experiments/provider-live/run.sh` still requires `RUNSTEAD_LIVE_SMOKE=1`; no normal CI workflow references it.

## Stage 1 — authenticated preflight: PASS

One authenticated `GET https://generativelanguage.googleapis.com/v1beta/models` returned HTTP 200. In-memory parsing found exact model ID `models/gemini-3.8-flash`; `supportedGenerationMethods` was present and included `generateContent`. Only status and boolean results were retained; the model-list response was not saved. No model sweep or second control-plane request was made.

The Models response did not expose project Free Tier eligibility/status. Free Tier status is therefore unverified and is not claimed as a Runstead guarantee.

## Stage 2 — governed live protocol task: FAIL

- Task ID: `cli-1790365111954902482`
- Workspace: committed public/synthetic `fixtures/coding-loop`
- Objective: inspect the fixture README and report its title; no file modification was authorized.
- Result: `provider_failure`, classification `upstream_server_failure`; one turn, zero observations, no verifier attempt.
- Sanitized independent inspection: status `failed`; exact provider ID/family/model/config identity and adapter version persisted. Provider attempt `exec-000001` has request `cli-1790365111954902482-0001`, `delivery_state=completed`, `upstream_reached=true`, outcome `upstream_server_failure`, and `attempt_debited=1`.
- Admission preceded dispatch: CLI trace recorded `attempt seq=1 status=admitted`; durable event sequence is `provider_attempt_prepared` then `provider_attempt_failed` then task finalization.
- Provider-effect accounting: 1 physical model-effect request, 1 governor-admitted attempt, 1 debit. No retry, fallback, rotation, or second provider attempt.
- Failure classification: upstream server failure (the adapter's sanitized server-failure/5xx class); exact HTTP status was not retained, so no more specific status is claimed. This is upstream failure evidence, not a demonstrated Runstead defect.
- Verifier: not reached; no PASS claimed.

Durable state inspected at `/tmp/runstead-gemini-stage2-state/runstead.db` using `runstead inspect` and the `provider_attempts` table. The state contains one provider-attempt row and no tool/evidence/verification rows. Secret material and raw response content are absent from the inspected projection.

## Stage 3 — bounded fixture task: NOT RUN

Stage 2 did not pass, so the required precondition was false. No Stage 3 task, rerun, or model request was made. This preserves the committed acceptance plan and recipe catalog unchanged and avoids treating model text as success.

## Stage 4 — interruption/resume: NOT RUN

Stage 3 did not admissibly pass, so no interruption/resume trajectory was started.

## Original attempt totals — before the controlled rerun

- Authenticated control-plane requests: 1 Models GET, HTTP 200; not a governed model-effect attempt.
- Original Stage 2: 1 physical model-effect request / 1 admitted and durably accounted provider attempt / 1 debit.
- Original canary total at that point: 2 outbound HTTP requests (one preflight GET plus one governed generation request). No hidden retry or fallback was used.
- Only the API's public model catalog and the committed public/synthetic `fixtures/coding-loop` prompt/workspace content were sent. No private repository or personal/confidential content was sent.
- No API key, authentication header, raw model-list body, raw provider response, or private prompt/response was retained in Git, SQLite, logs, evidence, or this report. Persistent configuration contains only `auth_ref: GEMINI_API_KEY`.
- Compatibility docs remain unchanged: the required Stage 2 + Stage 3 + Stage 4 positive proof was not achieved. `google_compatible` is not promoted to live-proven.

## Final validation

Final validation ran against code tree commit `57f9dc111c9ef01b416feacc4d8d93d94ad18ded`; this follow-up adds only these validation results to the report, with no Go/source changes.

- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: FAIL. `internal/state.TestBusyTimeoutBindsContendedWriter` timed out after 10 minutes in `second.db.Exec` while a transaction held the competing writer lock; `Store.Close` then also blocked in `wal_checkpoint(TRUNCATE)`. The test's configured busy timeout is 200 ms. Other package results shown before the failure passed. Root cause is not established; no unrelated state/test changes were made.
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS (23 packages; one package had no tests).
- `bash experiments/protocol/test.sh`: PASS.
- `git diff --check`: PASS.

No Go source was changed in this canary. The full-suite failure remains an explicit validation blocker, not a canary PASS.
## Continuation — 2026-09-27

### First continuation check

- Revalidated PR HEAD `be4bc6a8098016694caf90441935bef7f4104c82`, matching the maintainer-reviewed HEAD. PR #132 remained open with `CHANGES_REQUESTED`; Issue #131 remained open.
- The first required pre-use check, `test -n "${GEMINI_API_KEY:-}"`, failed (exit 1). At that check no credential was available, so no provider request or controlled rerun was started.
- At that point, the original Stage 2 trajectory above remained the only live model-effect trajectory. It remained `provider_failure / upstream_server_failure`, with one admitted/physical/debited request and no verifier.
- At that point Stage 3/4 remained gated. No config, key/account/model, limit/timeout, policy, or trust setting was changed.

### Busy-timeout investigation

- PR HEAD `be4bc6a8098016694caf90441935bef7f4104c82`: `timeout 60s go test -count=1 -v ./internal/state -run '^TestBusyTimeoutBindsContendedWriter$'` passed; test duration 0.23s.
- Clean detached checkout of fetched `origin/main` at `79ac8baaa2abcd428822fb39124b8490b64dfc21`: the same bounded command passed; test duration 0.23s.
- The previous 10-minute block was not reproduced on either tree. These runs do not establish its cause; no test or SQLite implementation was modified.

- At the first continuation check: 0 additional authenticated control-plane requests, 0 additional governed model-effect requests, and 0 additional admitted attempts/debits. This point-in-time accounting predates the rerun below.
- At that check the environment lacked `GEMINI_API_KEY`; no key value was displayed or persisted. The earlier trajectory used only public/synthetic fixture content.
- The positive compatibility claim remains inadmissible because Stage 2 did not pass and Stage 3/4 were not run. `docs/provider-compatibility.md` remains unchanged.

### Final validation on continuation content

The following gates were run after the continuation report update on the PR branch content based on `be4bc6a8098016694caf90441935bef7f4104c82`:

- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: PASS.
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS.
- `bash experiments/protocol/test.sh`: PASS.
- `git diff --check`: PASS.

- GitHub Actions on reviewed HEAD `be4bc6a8098016694caf90441935bef7f4104c82`: Go CI run `36182515959` completed successfully. Continuation HEAD `45551102095c63f237284034dea76a2b7f7b09b3`: run `36321857970` passed; latest prior head `35096f5839e19ba4d01d9a43b4f27f8d0b4a1a6b`: run `36323110246` passed.

### Credential recheck chronology

After the operator first reported the key was supplied, the safe presence check still failed in this executor. A later check, immediately before the live rerun at 2026-09-27T14:01Z, passed; the key was never printed or persisted.

### One controlled Stage 2 rerun — FAIL

- Task ID: `cli-1790517701251474870`; started 2026-09-27T14:01:41Z and terminated `failed / provider_failure`, classified `upstream_server_failure`. The sole authorized controlled rerun has now been used; stop the live canary. No Stage 3 or Stage 4 task was started.
- Provider declaration matched the required identity: `google-gemini-canary`, `google_compatible`, `https://generativelanguage.googleapis.com/v1beta`, `gemini-3.8-flash`, `auth_ref=GEMINI_API_KEY`, `reference_required`, empty options, config version `google-gemini-canary-2026-09-25-gemini-3.8-flash`, profile `v1`, capabilities `text_turn` and `runstead_protocol`, and the same safe single-attempt RouteSafety. CLI limits remained defaults: max steps 24, 10-minute task budget, provider budget 80; write tools remained `approval_required`, retry policy remained its default off, and no adapter timeout was increased.
- The task objective was to inspect the fixture README and report its title, with no file changes authorized. The workspace root was `fixtures/coding-loop`. A temporary acceptance check `file_exists: README.md` was used (digest `e53f9f029bb2993402a33d9693cb13a5c8f912baa3d588a58ad3d995ed204e69`); the initial Stage 2 acceptance digest was not preserved, so exact acceptance-plan identity cannot be verified.
- Provider attempt 1: `exec-000001`, request `cli-1790517701251474870-0001`; status `completed`, outcome `success`, upstream reached, delivery `completed`, debit 1. The model proposed `read_file` action `action-000002` with path `fixtures/coding-loop/README.md`; this path mismatch was caused by my task prompt using the repo-relative path while the workspace root was already `fixtures/coding-loop`. The tool attempt `exec-000003` therefore failed `path_not_found`; no successful tool observation/evidence resulted.
- Provider attempt 2: `exec-000004`, request `cli-1790517701251474870-0002`; status `failed`, outcome `upstream_server_failure`, upstream reached, delivery `completed`, debit 1. This was the next normal task turn after the failed read, not a provider retry or replay.
- Durable totals for the controlled rerun: 2 governor admissions, 2 physical model-effect requests, 2 debits; one successful provider attempt and one upstream-server failure; one failed `read_file` tool attempt, 0 observations/evidence, verifier not reached. No SDK retry, governor retry, fallback, provider/model/key/account rotation, or additional task was used.
- Runstead inspection and `provider_attempts`/`governor_ledger` rows substantiate both admissions and debits. The runner's text record did not fully render config/outcome fields; the sanitized durable `inspect` view and SQLite rows were read directly. No raw provider response or key value was retained in the report.
- The second Stage 2 task therefore did not pass. This bounded rerun is exhausted. Stop; do not make a third Stage 2 attempt. Stage 3/4 remain NOT RUN, and no positive compatibility claim is supported.
- Combined total after the controlled rerun: 1 authenticated preflight GET plus 3 governed physical model-effect requests (1 initial + 2 in the one controlled rerun), 4 observed outbound HTTP requests total; 3 admitted attempts/debits. No retry/fallback/rotation was observed.

### Final validation after controlled rerun report

Run on the updated branch content before its report-only commit:

- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: PASS.
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS.
- `bash experiments/protocol/test.sh`: PASS.
- `git diff --check`: PASS.
