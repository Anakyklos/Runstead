# Apinex DeepSeek V4 Flash live canary report

- Issue: #129
- Execution timestamp (UTC): 2026-09-05T20:04:53Z
- Runstead commit tested: `cc86945a733a7f86ebf49f235bce7319e97f1447`
- Provider ID: `apinex-canary-openaiprotocol`
- Protocol family: `openai_compatible`
- Endpoint: `https://api.apinex.bond/v1`
- Exact model: `free/deepseek-v4-flash-0731`
- Auth reference: `APINEX_API_KEY`
- Adapter: `compatible-provider-v0.1`
- Config version: `apinex-canary-2026-09-05-deepseek-v4-flash-0731`
- Audit correction: 2026-09-21; no additional live canary traffic was generated.

## Credential and setup handling

The supplied credential was assigned only to the environment of the live
processes and was unset from each controlling shell after use. Only the
non-secret reference `APINEX_API_KEY` was used in the provider declaration and
Runstead durable state. No credential value, authorization header, raw private
prompt or raw private response was written to the repository, SQLite, evidence,
logs, report, issue or PR.

The CLI was built successfully with `go build ./cmd/runstead`. The recipe
executor PATH was explicitly checked: `go` resolved to the installed Go 1.22.12
binary and `git` was available.

Two executor-only setup errors occurred before live task creation and are not
provider failures: an initial smoke invocation omitted `RUNSTEAD_BIN` (exit
127), and one shell invocation had malformed quoting. Both were corrected on
fresh disposable workspaces.

The Stage 3 audit found a separate executor acceptance-plan error that must be
counted in the live chronology. The first two Stage 3 trajectories persisted the
same temporary acceptance digest, `fc7194da72328fa942ec260f76cac5ba6623795e65478caeb4aa7861a3ced9c1`.
On the second trajectory that plan exposed an extra `fix-hash` check that was
not part of the committed fixture acceptance contract. The model-written
implementation passed the real `test` recipe but failed only that extra check,
then the next provider request ended in `upstream_server_failure`. This is
`executor_acceptance_setup_error` plus a real upstream failure; it is not proof
of provider incompatibility. Because the trajectory reached the provider and
produced effects, it still counts against #129's bounded live-attempt history.

## Stage 1: authenticated preflight

- Runstead build: **PASS**.
- One authenticated `GET https://api.apinex.bond/v1/models`: **HTTP 200**.
- Returned model IDs: 17.
- Exact model `free/deepseek-v4-flash-0731`: **present**.
- Chat Completions diagnostic: not needed because `/models` succeeded.
- Model sweep, alternate model, fallback and rotation: **not used**.

## Stage 2: live protocol task

The first fresh task, `cli-1788637167114776708`, reached the real provider
through the configured adapter. It recorded six successful upstream provider
attempts followed by one `sent_confirmed` timeout. The task ended
`provider_failure`; no verifier result was claimed. This was preserved as
transient provider evidence and not treated as a Runstead defect.

The one controlled fresh rerun, `cli-1788637477563110805`, completed positively:

- status/outcome: `completed` / `completed`;
- provider attempts: 2 admitted physical attempts, both `success`,
  `upstream_reached=true`, and `attempt_debited=1`;
- tool evidence: `obs-000001` from `read_file`;
- verifier: `verif-000005`, **passed**;
- acceptance: `readme-present`, **passed**;
- no fallback, rotation, hidden retry or model change.

**Stage 2: PASS.**

## Stage 3: coding task

The durable state contains three live Stage 3 trajectories. They must be read
as one chronology; a later green trajectory does not erase earlier attempts.

### Trajectory 1 — initial attempt

Task `cli-1788637609849099719` used acceptance digest
`fc7194da72328fa942ec260f76cac5ba6623795e65478caeb4aa7861a3ced9c1`.
It ended `failed` / `provider_failure` with stop reason `provider failure:
timeout`.

- provider attempts: 5 total; attempts 1-4 completed successfully, attempt 5
  ended `sent_confirmed` / `timeout` with `upstream_reached=true`;
- all 5 attempts were governor-accounted and debited exactly once;
- evidence: `obs-000001` `list_files`, `obs-000002` `read_file`,
  `obs-000003` `list_files`, `obs-000004` `read_file`;
- no write, process/recipe result or verification attempt was reached;
- no fallback, provider/model rotation or alternate route was used.

This was the initial Stage 3 failure.

### Trajectory 2 — first controlled rerun

Task `cli-1788637741663589436` used the same temporary acceptance digest and
ended `failed` / `provider_failure` with stop reason `provider failure:
upstream_server_failure`.

- provider attempts: 6 total; attempts 1-5 completed successfully and attempt 6
  ended `delivery_state=completed`, `outcome=upstream_server_failure`,
  `upstream_reached=true`;
- all 6 attempts were governor-accounted and debited exactly once;
- evidence `obs-000001` and `obs-000002`: `read_file` inspections;
- evidence `obs-000003`: completed scoped `write_file`, with
  `effect_after_hash=d520060356cb9acbb665b0cf2ebd5c9fba4a123198b2f192d2fab96e9ae4693c`;
- evidence `obs-000004`: declared recipe `test`, exit 0, stdout/stderr
  untruncated;
- verification `verif-000014` was **failed** only because the temporary
  `fix-hash` check expected prefix `1c5aa56c1715` while the observed file hash
  prefix was `d520060356cb`; structural checks and `tests-pass` passed;
- after that verifier refusal, the next provider request produced the terminal
  upstream server failure;
- Git observed no pre-existing changes and only `app/calc.go` modified.

This trajectory is the one explicit controlled rerun permitted by #129. The
extra `fix-hash` requirement was an executor acceptance setup error, but the
live provider attempts and effects remain part of the audit trail.

### Trajectory 3 — later successful but out-of-budget rerun

Task `cli-1788637879214590998` corrected the acceptance plan to digest
`cf4a8b3c63848a9350cf6beff5a8409145dc0cf6ad98f59f7f2f112a83797eb0`
and completed positively:

- final status/outcome: `completed` / `completed`;
- final verifier: `verif-000024`, **passed**;
- 9 admitted provider attempts, all upstream-reached successes and each debited
  exactly once;
- evidence `obs-000001` and `obs-000002`: repository inspection via
  `list_files`;
- evidence `obs-000003` and `obs-000004`: source/test inspection via
  `read_file`;
- evidence `obs-000005`: one completed scoped `apply_patch` write, with
  `effect_after_hash=d520060356cb9acbb665b0cf2ebd5c9fba4a123198b2f192d2fab96e9ae4693c`;
- evidence `obs-000006`: declared recipe `test`, `go test ./...`, exit 0,
  stdout/stderr untruncated;
- acceptance `tests-pass`: **passed**;
- Git observation: no pre-existing changes and only `app/calc.go` changed;
- the two `apply_patch` approval pauses were resolved through `runstead decide`,
  and only one write effect executed.

This is valid positive runtime evidence for that exact trajectory, but it was a
second rerun after the initial attempt and the already-used controlled rerun.
Issue #129 authorizes only one explicit controlled rerun for a Stage 3 failure.
Therefore this third trajectory cannot be selected retroactively as the Stage 3
acceptance result.

Across all Stage 3 trajectories there are 20 durable provider-attempt records
(5 + 6 + 9), each separately governor-accounted and debited once. No hidden
fallback, provider/model/key/account rotation or unaccounted live task remains
in the Stage 3 chronology.

**Stage 3: NOT ACCEPTED.** The runtime/provider evidence is useful, but the
canary exceeded #129's bounded rerun allowance before the green trajectory.
No new live run was performed to repair this documentation defect.

## Stage 4: interruption and resume

Stage 4 was executed at the time because trajectory 3 above had been treated as
a Stage 3 pass. The audit correction now establishes that Stage 3 was not
admissibly satisfied under the one-rerun bound. Consequently the Stage 4 runs
below are preserved as real interruption/resume evidence, but they cannot count
as formal #129 gate evidence because their Stage 3 precondition was not met.

The first Stage 4 task, `cli-1788638350661868905`, recorded 4 provider attempts:
3 completed successes followed by one `sent_confirmed` timeout, with every
attempt debited once. It made three durable inspection observations, ended
`provider_failure`, and was preserved without resume.

The controlled fresh Stage 4 task was `cli-1788638498811755793`, using the same
provider declaration, exact model, acceptance digest, recipe catalog, recipe
policy and write policy as Stage 3.

### Pre-resume state

After substantial progress, the process received a normal `SIGINT`. The
required public `runstead inspect` projection was captured before resume:

- status: `running`;
- outcome: `persistence_paused`;
- provider attempts: 3 completed successes plus prepared attempt
  `exec-000010`;
- completed effects: `obs-000001` and `obs-000002` reads, then `obs-000003`
  `write_file`;
- write post-effect hash:
  `bb925cb1edd8a399892f1680b7470dcaf63026d4f96cf7a65f2a460f8e032223`;
- next provider attempt was prepared but had no observed completion, so it was
  conservatively uncertain and the task remained resumable;
- workspace Git status: only `app/calc.go` modified.

### Resume result

`runstead resume cli-1788638498811755793` was then run with the same provider
ID, protocol family, endpoint, exact model, provider config, acceptance,
recipes, policies and secret reference.

Recovery durably recorded:

- `exec-000010` reconciled as `upstream_may_have_been_reached`;
- conservative debit preserved: 1;
- no retry of the uncertain request;
- recovery context reconstructed with the three prior evidence IDs;
- one resume recorded (`Resumes: 1`).

The resumed task then added only the new provider attempts `-0005` and `-0006`,
executed `run_recipe` as `obs-000004` with exit 0 and untruncated output, and
reached independent verification `verif-000015`, **passed**. Final state:

- status/outcome: `completed` / `completed`;
- provider-attempt records: 6 total, each admitted and debited once;
- 5 completed upstream successes and 1 prepared/unobserved attempt reconciled
  conservatively;
- provider `exec-000010` was not re-issued;
- evidence IDs `obs-000001`, `obs-000002` and `obs-000003` remained valid and
  appeared exactly once; only the new recipe evidence `obs-000004` was added;
- the completed write effect was not repeated;
- verifier checks `no_uncertain_attempts`, `writes_reconciled`, `git_observed`
  and `tests-pass` all passed;
- final workspace still contained only the scoped `app/calc.go` change.

This is the no-replay proof: the interrupted provider attempt was reconciled,
not replayed; prior effects and evidence were preserved; only new post-recovery
work was admitted.

**Stage 4: OBSERVED, NOT ACCEPTED AS GATE EVIDENCE.** The resume/no-replay
behavior is positive runtime evidence, but #129 required a valid Stage 3 pass
before Stage 4 could count toward acceptance.

## Compatibility documentation

`docs/provider-compatibility.md` is intentionally conservative after this
audit. It records that authenticated live traffic and useful positive runtime
evidence exist for the exact Apinex endpoint/model, but it does **not** mark the
OpenAI-compatible live acceptance gate as proven because #129's Stage 3 rerun
contract was exceeded.

The evidence remains scoped to:

- endpoint `https://api.apinex.bond/v1`;
- path `openai_compatible`;
- model `free/deepseek-v4-flash-0731`.

No claim is made for other Apinex endpoints/models, the full Apinex API surface,
or other protocol families. Issue #129 remains unaccepted on this evidence and
downstream adoption gate #123 remains blocked.

## Final safety statement

No adapter-specific Apinex code, router, fallback, model rotation, account
rotation, daemon, MCP integration or unrelated issue work was introduced. The
live canary remains opt-in and is not part of normal CI.
