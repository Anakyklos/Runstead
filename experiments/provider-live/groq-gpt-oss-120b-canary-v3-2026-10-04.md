# Groq GPT-OSS-120B Gate A canary v3

Status: complete; Gate A NOT SATISFIED; report-only. This report records only
sanitized evidence; raw provider bodies and transcripts are not retained.

## Identity

- Issue: #145
- Branch: `issue-145-groq-gpt-oss-120b-canary-v3`
- Base: `c78cd99e7625796376c6aa4375a75539759510aa`
- Candidate: `groq-gpt-oss-120b-canary-v3`
- Family: `openai_compatible`
- Endpoint: `https://api.groq.com/openai/v1`
- Model: `openai/gpt-oss-120b`
- Auth declaration: `reference_required`, reference `GROQ_API_KEY`
- Secret hygiene: the key value is not written to the repository, SQLite,
  traces, evidence, fixtures, prompts, issue/PR, or this report. Only
  presence/configuration status may be recorded.

## Stage 1 — authentication and exact model

### Acceptance preflight (recorded before Stage 1 dispatch)

- Objective: build corrected `main`; resolve exactly the v3 declaration through
  the `openai_compatible` family before constructing any adapter; then verify
  authentication and presence of the exact model with at most one `/models`
  request.
- Required observable evidence: build exit status; resolver output for the
  exact provider/family/endpoint/model/auth reference; static evidence that no
  Groq-specific adapter exists; secret presence only (never the value); and,
  if present, one authenticated `GET /models` with explicit User-Agent,
  HTTP success, and an exact match for `openai/gpt-oss-120b`.
- Acceptance checks: declaration matches the issue contract; registry
  resolution succeeds with `openai_compatible`; resolution constructs no
  adapter and makes zero provider requests; no provider-specific Groq code is
  involved; the sole permitted HTTP request is GET `/models`; the exact model
  ID is present. Any mismatch stops before that request.
- Allowed effects: create local experiment metadata; build the CLI; run the
  no-network declaration resolver; check secret presence; load the reference
  into the request process; make at most one authenticated GET `/models`.
- Prohibited effects: chat/completion requests; a second HTTP request,
  redirects, retries, quota probes, model sweeps, alternate credentials,
  accounts, endpoints or models; secret output or persistence.
- Preflight status: registered before Stage 1 dispatch; no provider request
  has yet been made.

### Results

- Corrected `main` build: PASS using `/tmp/runstead-groq-v3-bin`.
- Exact provider declaration resolution: PASS. The no-network resolver
  confirmed provider ID, `openai_compatible`, exact endpoint/model, required
  authentication reference, and sanitized configuration identity. It
  constructed no adapter and reported zero provider dispatches.
- Groq-specific adapter audit: none found under `internal/provider`.
- `.env.local` ignore rule: confirmed before commit. Secret presence check:
  present in the ignored environment file; the value was not output or stored.
- Authenticated model-list request: PASS. Exactly one `GET /models` was made
  with User-Agent `Runstead-GateA-Canary/1.0`; HTTP 200; exact model present.
  The key was loaded from the ignored `.env.local` into the request process and
  was not printed or persisted. No response body or headers were retained.

## Stage 2 — real read-only agent loop

### Acceptance preflight (recorded before Stage 2 dispatch)

- Objective: on a fresh copy of `fixtures/coding-loop`, run one new v3 task
  with the exact requested objective: `Read app/calc.go using the available
  repository tool. Complete only after citing the actual read_file observation
  for app/calc.go. Do not modify files and do not run recipes or processes.`
- Required observable evidence: fresh workspace and state directory; frozen
  SHA-256 for `app/calc.go`; task ID; provider configuration identity; complete
  task history including admissions, physical requests/debits, verification
  attempts, actions, tool attempts/results and evidence IDs; `read_file` result
  for `app/calc.go`; nonempty final citation matching that persisted result;
  zero write or recipe/process effects; independent verifier PASS; terminal
  `completed`.
- Acceptance checks: runtime acceptance plan uses the verifier-supported
  `file_hash` check for the unchanged `app/calc.go`; supplemental audit checks
  every history predicate above, including that the citation exists and its
  persisted tool is `read_file`. The hash alone cannot qualify Stage 2.
  Each physical model request must have its own governor admission and exactly
  one debit. Normal turns in this task are not reruns. The run will use
  `--retry-policy off` and explicit `--max-verification-retries 3` (the current
  runtime default), with adapter retry/fallback disabled. A fabricated or
  nonexistent citation may receive normal verifier feedback and continue in
  this same task, but a terminal failure or exhausted bound stops the canary.
- Allowed effects: create one fresh fixture copy and state directory; create
  and run one task using the declared v3 provider; repository read tools only;
  normal governed model turns; independent verification; read-only inspection
  of the task's SQLite state for audit.
- Prohibited effects: any second task/restart/rerun; workspace writes; recipe
  or process execution; changing provider, model, key/account, endpoint,
  acceptance, or retry bounds after dispatch; adapter retry/fallback; retaining
  raw provider transcripts/bodies; Gate A decisions by workers.
- Preflight status: PASS; profile contains only the built-in `repo.read@1.0.0`
  package; acceptance hash is frozen before task dispatch.

### Results

- Fresh workspace: `/tmp/runstead-issue145-v3-stage2-one4hmd3/workspace`;
  fresh state directory: `/tmp/runstead-issue145-v3-stage2-one4hmd3/state`.
  The workspace `app/calc.go` hash matched the frozen acceptance hash before
  and after the run.
- Task: `cli-1791122946929726868`, exact requested objective persisted,
  Runstead exit code `28`, terminal status `failed`, outcome `provider_failure`,
  stop reason `uncertain_reached`.
- Provider chronology: one prepared physical attempt (`exec-000001`) for
  `groq-gpt-oss-120b-canary-v3` / `openai/gpt-oss-120b`; one governor task
  attempt and one ledger entry; `attempt_debited=1`; retries `0`. The attempt
  ended `uncertain`, `upstream_reached=1`, delivery `sent_unconfirmed`.
- No action, tool attempt, or tool result was persisted. In particular, there
  was no `read_file(app/calc.go)` observation and no final citation. There
  were zero writes and zero recipe/process tool effects.
- Verifier chronology: acceptance plan saved at task start; no verification
  attempt ran because no completion proposal or usable provider response was
  received. Runtime verifier PASS and terminal `completed` were not achieved.
- Failure detail: durable `error_class` and HTTP status are unavailable. The
  captured CLI output was discarded when the process exited, so the concrete
  transient cause cannot be established. The one allowed controlled rerun is
  therefore not admissible; the failed trajectory is preserved and no second
  task was started. The request ID also failed the sanitized-ID pattern check,
  so it was not used as diagnostic evidence.
- Secret hygiene: no key value or raw provider response/body was persisted in
  the report or repository. Only sanitized attempt metadata was retained.

## Stage 3 — bounded coding fixture

Not dispatched; requires Stage 2 PASS and its own recorded preflight.

## Stage 4 — interruption and resume

Not dispatched; requires Stage 3 PASS and its own recorded preflight.

## Gate A decision

**NOT SATISFIED.** Stage 1 passed; Stage 2 failed terminally without enough
evidence to qualify for the single controlled rerun; Stages 3 and 4 were not
dispatched. Issue #123 remains blocked on Gate A. Historical canaries #138 and
#141 are context only; no task, database, attempt, branch, or trajectory was
reused. Normal turns inside one task do not count as reruns.
