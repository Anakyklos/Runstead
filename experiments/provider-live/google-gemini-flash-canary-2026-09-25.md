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

## Attempt totals and limitations

- Authenticated control-plane requests: 1 Models GET, HTTP 200; not a governed model-effect attempt.
- Governed model-effect requests: 1 physical request / 1 admitted and durably accounted provider attempt / 1 debit.
- Total observed outbound HTTP requests from the canary: 2 (one preflight GET plus one governed generation request). No hidden retry or fallback was used.
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

### Revalidation and controlled Stage 2 rerun

- Revalidated the PR branch HEAD as `be4bc6a8098016694caf90441935bef7f4104c82`, matching the maintainer-reviewed HEAD. PR #132 remains open with `CHANGES_REQUESTED`; Issue #131 remains open.
- The one required pre-use check, `test -n "${GEMINI_API_KEY:-}"`, failed (exit 1). No credential was available in this execution environment. No provider request or Stage 2 rerun was started; there is therefore no second task ID, outcome, provider attempt, admission, debit, delivery state, retry, fallback, or rotation to claim.
- The original Stage 2 trajectory above remains unchanged and is still the only live model-effect trajectory. Its one admitted/physical/debited request remains classified `provider_failure / upstream_server_failure`; verifier was not reached.
- The bounded rerun is **not executed** because the required secret reference was unavailable before use. This is not a provider result and does not exhaust the allowed rerun contract. Stage 3 and Stage 4 remain unrun because Stage 2 rerun PASS was not established. No further live attempt was made.
- Provider, protocol family, endpoint, model, auth reference, profile, RouteSafety, capabilities/policies, limits/timeouts, and trust model were not changed. No key/account/model rotation, retry, fallback, or additional model request occurred.

### Busy-timeout investigation

- PR HEAD `be4bc6a8098016694caf90441935bef7f4104c82`: `timeout 60s go test -count=1 -v ./internal/state -run '^TestBusyTimeoutBindsContendedWriter$'` passed; test duration 0.23s.
- Clean detached checkout of fetched `origin/main` at `79ac8baaa2abcd428822fb39124b8490b64dfc21`: the same bounded command passed; test duration 0.23s.
- The previous 10-minute block was not reproduced on either tree. These runs do not establish its cause; no test or SQLite implementation was modified.

- Continuation attempt totals: 0 additional authenticated control-plane requests; 0 additional governed model-effect requests; 0 additional admitted attempts/debits. No content was sent during this continuation. The previously documented Stage 1 and initial Stage 2 evidence remain as recorded.
- The environment lacked `GEMINI_API_KEY`; no secret value was displayed or persisted. Only previously documented public/synthetic fixture content was sent in the initial trajectory; no private repository or confidential content was sent.
- Positive compatibility claim remains inadmissible because Stage 2 rerun, Stage 3, and Stage 4 have not all passed. `docs/provider-compatibility.md` remains unchanged.

### Final validation on continuation content

The following gates were run after the continuation report update on the PR branch content based on `be4bc6a8098016694caf90441935bef7f4104c82`:

- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: PASS.
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS.
- `bash experiments/protocol/test.sh`: PASS.
- `git diff --check`: PASS.

- GitHub Actions on reviewed HEAD `be4bc6a8098016694caf90441935bef7f4104c82`: Go CI run `36182515959` completed successfully. Continuation HEAD `45551102095c63f237284034dea76a2b7f7b09b3`: Go CI run `36321857970` completed successfully (12m55s).
### Credential availability recheck

After the operator reported that the key had been supplied, `test -n "${GEMINI_API_KEY:-}"` was run again in the execution environment and exited 1. The key is still unavailable to this process; no provider request or controlled rerun was started. No key content was read, printed, logged, or persisted. The single authorized Stage 2 rerun remains unexecuted; Stage 3/4 remain gated.
