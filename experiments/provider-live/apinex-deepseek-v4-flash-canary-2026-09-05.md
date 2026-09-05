# Apinex DeepSeek V4 Flash live canary report

- Issue: #129
- Execution timestamp (UTC): 2026-09-05T18:41:52Z
- Runstead commit tested: `cc86945a733a7f86ebf49f235bce7319e97f1447`
- Provider ID: `apinex-canary-openaiprotocol`
- Protocol family: `openai_compatible`
- Endpoint: `https://api.apinex.bond/v1`
- Exact model candidate: `free/deepseek-v4-flash-0731`
- Auth reference: `APINEX_API_KEY`

## Execution status

This execution prepared a fresh branch from the current `main`, built the
Runstead CLI successfully, read issue #129 and its maintainer comment, and
verified the configured provider path and secret-reference contract.

The process environment did not contain `APINEX_API_KEY`. The designated
credential assignment in the operator request still contained the literal
placeholder `COLE_AQUI_SUA_CHAVE`. No real external credential was available
to this execution. No other credential source was used or transformed.

Because an authenticated request could not be made safely, the live gates were
stopped before provider dispatch. The public `run` boundary was nevertheless
exercised with the exact non-secret provider declaration to verify fail-closed
authentication handling. This is a credential-availability blocker, not a
provider response, protocol failure or Runstead defect.

## Stage 1: authenticated preflight

- Runstead build: **PASS** (`go build ./cmd/runstead`).
- Existing adapter/config path: declaration prepared for
  `openai_compatible`; no Apinex-specific adapter was introduced.
- Secret reference: **BLOCKED**. `APINEX_API_KEY` was absent and the supplied
  assignment was still a placeholder.
- Required authenticated `GET https://api.apinex.bond/v1/models`: **NOT
  EXECUTED**. There was no credential with which to make an authenticated
  request.
- Minimal authenticated Chat Completions diagnostic: **NOT EXECUTED**. The
  fallback diagnostic is allowed only when `/models` is unsupported, not when
  the required credential is unavailable.
- Model selection: no model was selected or swept.

The fail-closed public-run probe created task
`cli-1788633712417133027` and rendered the exact provider ID, endpoint, family,
model and sanitized configuration through `runstead inspect`. It recorded one
governor-admitted provider attempt, `exec-000001`, with
`delivery_state=not_sent`, `upstream_reached=false`,
`outcome=authentication_denied` and `attempt_debited=1`. The adapter therefore
made zero physical HTTP requests. The task ended as typed
`provider_failure`, with no tools, effects, evidence payload or verifier.

The maintainer comment records that Apinex publicly documents an OpenAI-
compatible Chat Completions surface. That public documentation does not replace
an authenticated account preflight or prove the exact model is available.

## Stage 2: smallest live protocol turn

**BLOCKED before the live protocol turn.** The fail-closed probe above did not
reach `runstead.protocol.v1`, so it is not a Stage 2 pass. There was no valid
protocol response, verifier result or positive live evidence. Its single
admitted attempt was durably marked `not_sent`; no physical provider request
was made.

## Stage 3: coding task

**BLOCKED by the Stage 1 credential gate.** No coding-loop workspace task was
started, so there are no coding tools, writes, recipe results, Stage 3 task ID,
Stage 3 evidence IDs, provider attempts or verifier result to report. The
fixture recipe was not run.
The PATH preflight requirement was reviewed and will be satisfied before any
future execution, but it was not relevant to this credential-blocked run.

## Stage 4: interruption and resume

**NOT EXECUTED.** Stage 2 and Stage 3 did not pass, so there was no eligible
running task to interrupt and resume. There is no pre-resume state projection,
resume result or no-replay proof. No terminal-task resume was used as a
substitute for Stage 4.

## Safety and limitations

- No provider/model fallback, rotation, retry loop, proxy, adapter or governor
  change was used.
- No live request was made without authentication.
- No secret value, Authorization header, private prompt, private response or
  credential-shaped value was added to this report, repository, SQLite state,
  logs, issue or PR.
- `docs/provider-compatibility.md` was not changed. This execution provides no
  live compatibility evidence for Apinex or the candidate model.
- A later run requires a real externally held `APINEX_API_KEY` reference. It
  must then repeat the single authenticated model preflight before attempting
  Stage 2.
