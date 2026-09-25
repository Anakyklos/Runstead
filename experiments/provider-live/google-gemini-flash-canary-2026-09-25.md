# Google Gemini Flash live canary report

- Issue: #131
- Execution/report timestamp (UTC): 2026-09-25T19:39:07Z
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
- Candidate config resolved through the existing provider registry and required `google_compatible` adapter contract. The CLI was deliberately stopped at a missing temporary acceptance file, before task bootstrap/dispatch; it reported only that acceptance-file error.
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
