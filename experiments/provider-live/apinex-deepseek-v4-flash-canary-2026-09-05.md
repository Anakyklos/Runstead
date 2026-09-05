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
fresh disposable workspaces. A separate Stage 3 attempt used an over-constrained
additional temporary file-hash check; the provider-written implementation
passed the real recipe but failed that extra check and then encountered an
upstream failure. This was classified as `executor_acceptance_setup_error`, not
as proof of provider incompatibility. The final accepted task used the
committed fixture acceptance plan unchanged.

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

An initial fresh coding task, `cli-1788637609849099719`, made four successful
provider-backed inspection turns and then timed out before a write. The final
fresh accepted task was `cli-1788637879214590998`.

Durable Stage 3 evidence for the accepted task:

- final status/outcome: `completed` / `completed`;
- final verifier: `verif-000024`, **passed**;
- provider identity remained the exact configured provider, family, endpoint and
  model on every attempt;
- 9 admitted provider attempts were recorded, all upstream-reached successes,
  each debited exactly once; no fallback or rotation occurred;
- evidence `obs-000001` and `obs-000002`: repository inspection via `list_files`;
- evidence `obs-000003` and `obs-000004`: source/test inspection via `read_file`;
- evidence `obs-000005`: one completed scoped `apply_patch` write, with
  `effect_after_hash=d520060356cb9acbb665b0cf2ebd5c9fba4a123198b2f192d2fab96e9ae4693c`;
- evidence `obs-000006`: declared recipe `test`, `go test ./...`, exit 0,
  stdout/stderr untruncated;
- acceptance `tests-pass`: **passed**;
- Git observation: no pre-existing changes and only `app/calc.go` changed during
  the task;
- before/after hashes: baseline
  `b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94`, after
  write `d520060356cb9acbb665b0cf2ebd5c9fba4a123198b2f192d2fab96e9ae4693c`.

The task encountered two normal operator approval pauses for the model's
`apply_patch` proposal. Both approvals were recorded through `runstead decide`
outside model prose. Recovery re-proposed the approved action and executed one
write effect; the historical planned proposals were not executed as duplicate
effects. A separate independent `go test ./...` also passed on the final
workspace.

**Stage 3: PASS.**

## Stage 4: interruption and resume

The first Stage 4 task, `cli-1788638350661868905`, made three durable inspection
observations and then ended in a `sent_confirmed` timeout before an effect. It
was preserved and not resumed.

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

**Stage 4: PASS.**

## Compatibility documentation

`docs/provider-compatibility.md` was updated narrowly to state operational
positive evidence only for:

- endpoint `https://api.apinex.bond/v1`;
- path `openai_compatible`;
- model `free/deepseek-v4-flash-0731`.

The report and documentation do not generalize to other Apinex endpoints,
other Apinex models, the full Apinex API surface or other protocol families.

## Final safety statement

No adapter-specific Apinex code, router, fallback, model rotation, account
rotation, daemon, MCP integration or unrelated issue work was introduced. The
live canary remains opt-in and is not part of normal CI.
