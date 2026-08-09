# Development Environment

## Decision

Runstead provides a Docker-based development environment to keep compilers, command-line tools, caches and later transport-specific native dependencies out of the host system.

Docker remains optional. The project must continue to support normal native Go commands and release a standalone executable for end users.

## What Docker solves

- reproducible Go and utility versions;
- no host installation of Go, `jq`, SQLite tools or test dependencies;
- isolated module and build caches;
- easier CI parity;
- a controlled place for future ChatGPT Web transport dependencies;
- disposable protocol experiments.

## What Docker does not solve

Docker is not a complete security sandbox for an agent that edits host code.

A bind-mounted writable repository can be modified or deleted by processes inside the container. The mount is the intentionally granted boundary. Docker reduces dependency pollution and limits unmounted host access; it does not make mounted data safe from the agent.

## Required development layout

The initial environment should contain one primary development service with:

- a pinned Go toolchain image or project-owned development image;
- the Runstead repository mounted at `/workspace`;
- an explicit working directory of `/workspace`;
- a named volume for `GOMODCACHE`;
- a named volume for `GOCACHE`;
- a named volume or explicit path for disposable development state;
- `git`, `curl`, `jq`, `ripgrep`, SQLite CLI and CA certificates;
- a non-root runtime user;
- no privileged mode;
- no Docker socket mount;
- no host PID or network namespace;
- `no-new-privileges` and dropped capabilities where supported.

Example conceptual layout:

```text
host Runstead source  ──bind mount──> /workspace
Go module cache       ─named volume─> /go/pkg/mod
Go build cache        ─named volume─> /home/runstead/.cache/go-build
Runstead dev data     ─named volume─> /home/runstead/.local/share/runstead
selected target repo  ──bind mount──> /target
```

Only the target repository selected for a run should be mounted. Do not mount the user's home directory, filesystem root or broad parent folders for convenience.

## Credentials

Credentials must never be:

- copied into an image layer;
- committed to the repository;
- stored in the task SQLite database;
- printed in logs;
- included in fixtures;
- persisted in shell history through documented examples.

For the OmniRoute stage, inject the API key at runtime through an environment variable or a local secret file excluded from version control.

For the later direct ChatGPT Web connector, prefer operating-system keyring storage for native execution. Container-based direct-connector testing should use a narrowly mounted, read-only secret or ephemeral environment injection. Password-based login automation is out of scope.

## File ownership

The development container should run with the host user's UID and GID when practical so files created in bind mounts remain editable on the host.

UID/GID behavior must be tested on the primary Linux development environment. Rootless Docker is preferred when available, but it does not remove the need to verify ownership on bind-mounted files.

## Network policy

The OmniRoute client requires outbound access to the configured OmniRoute endpoint. The development container should use normal bridge networking and should not use host networking unless a concrete local-endpoint requirement cannot be solved through an explicit host gateway configuration.

Later direct ChatGPT Web work may require outbound access to ChatGPT endpoints and additional transport dependencies. Keep that work in a dedicated layer or image so the core development environment remains understandable.

## Issue #3 Go foundation

The Go foundation has exactly one external runtime dependency:
`modernc.org/sqlite` (pure Go, no CGO), added by issue #8 for durable state.
Build and test it with:

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build ./cmd/runstead
go test -race ./...
```

The current CLI is intentionally small:

```text
runstead --help
runstead run --help
runstead inspect --help
runstead resume --help
```

`run` executes one bounded task with the issue #7 agent loop, the policy-gated
write tools from issue #10, the policy-gated process recipes from issue #26 and
durable SQLite state (issue #8). When a policy-gated effect requires operator
approval the run pauses with the typed `approval_required` outcome (a
control-plane dependency, not a protocol correction): no correction budget is
consumed, no further provider attempt is made, and the task stays durably
resumable. `inspect <task-id>` renders the persisted task, attempts, journal,
policy decisions, pending approvals, approvals, process evidence and governor
state after the run process exits (see [`persistence.md`](persistence.md)).
`resume` reconciles interrupted attempts from durable state (issues #9/#10/#26)
and continues under the task's persisted write and recipe policies; divergent
`--write-policy` / `--recipe-policy` overrides are rejected fail-closed, and a
re-supplied recipe catalog that drifts from the effective catalog the task
started with is rejected before any recovery side effect (the catalog digest
is persisted with the task).
`decide <task-id> <action-id> approved|rejected` is the operator control plane
that records approvals for actions actually pending approval (writes and
recipes); model output can never approve an effect.

Write-tool policy is configured with `--write-policy tool=mode,...` (or
`RUNSTEAD_WRITE_POLICY`); the default is `approval_required` for every write
tool. The effective policy is persisted with the task configuration and is
authoritative across restart. Process recipes are configured with
`--recipes FILE` (or `RUNSTEAD_RECIPES`) and gated by `--recipe-policy
recipe=mode,...` (or `RUNSTEAD_RECIPE_POLICY`); the default is
`approval_required` for every recipe, and the effective recipe policy is
persisted and authoritative across restart exactly like the write policy. See
[`writes.md`](writes.md) for the safe-writes contract and
[`process-runner.md`](process-runner.md) for the process-runner contract,
including the honest native limitations (no kernel sandbox, network isolation
unenforced).

Configuration precedence is deterministic: command-line flags, then
environment, then conservative defaults. Workspace/logging use
`RUNSTEAD_WORKSPACE` and `RUNSTEAD_LOG_LEVEL`; the durable state directory
uses `RUNSTEAD_STATE_DIR` (default `$XDG_DATA_HOME/runstead` or
`~/.local/share/runstead`). The optional OmniRoute config
uses `OMNIROUTE_BASE_URL`, `OMNIROUTE_API_KEY`, `OMNIROUTE_MODEL` and
`OMNIROUTE_CHAT_ENDPOINT`, with optional management URL and timeout variables;
the `run` command exposes matching flags. API keys are never logged or
included in errors, snapshots, telemetry or URLs.

The legacy single-attempt route declaration uses
`OMNIROUTE_SINGLE_ATTEMPT_GUARANTEED`,
`OMNIROUTE_INTERNAL_RETRIES_DISABLED`,
`OMNIROUTE_COOLDOWN_REPLAY_DISABLED`,
`OMNIROUTE_ACCOUNT_POOLING_DISABLED`,
`OMNIROUTE_AUTOMATIC_FALLBACK_DISABLED` and
`OMNIROUTE_COMBO_ROUTING_DISABLED`, or the equivalent
`--omniroute-safe-route` declaration. These values are configuration evidence
for the governor; management snapshots and client-authored declarations never
prove the number of upstream attempts atomically.

PR #33 added the Runstead consumer side of the authoritative receipt contract:
provider-neutral receipt types, strict validation, receipt-aware OmniRoute
transport parsing and per-attempt governor reconciliation. The normal CLI
configuration does not yet expose receipt-aware activation. Protected live use
therefore remains fail-closed until a compatible OmniRoute producer emits the
versioned receipts and issue #30 wires the production configuration and opt-in
live path. `Preflight` remains diagnostic and does not authorize model
execution.

Implemented package responsibilities are deliberately narrow:

- `cmd/runstead`: signal-aware process entrypoint, exit codes and CLI help;
- `internal/config`: flag/environment/default resolution;
- `internal/agent`: the governor-owned executor seam plus the issue #7 bounded
  loop: typed terminal outcomes, deterministic system contract, untrusted
  observation framing, evidence grounding, workspace-aware repeat guard,
  control-plane write policy enforcement (issue #10), sanitized lifecycle
  trace and one-shot context cancellation;
- `internal/protocol`: strict `runstead.protocol.v1` action/final parser,
  typed schema validation, deterministic correction messages, canonical action
  fingerprints and caller-owned repeat guard;
- `internal/provider`: provider-neutral requests/responses, route-safety and
  authoritative attempt-receipt types, validation contracts and deterministic
  fakes;
- `internal/provider/omniroute`: stdlib-only, non-streaming OmniRoute
  transport, fail-closed management checks, sanitized typed errors,
  classifier, optional telemetry, receipt request/response headers and strict
  mapping into provider-neutral receipt metadata;
- `internal/tools`: the issue #6 registry with workspace boundary, typed
  observations, deterministic truncation and evidence identifiers, plus the
  issue #10 policy-gated `write_file`/`apply_patch` effects with stale-state
  protection and structured write evidence, and the issue #26 policy-gated
  `run_recipe` effect;
- `internal/recipe`: the issue #26 operator-controlled recipe model and
  process runner: catalog parsing/validation, capability declarations,
  environment allowlist with credential denylist, bounded per-stream output
  capture, process-group execution with full-tree termination on
  timeout/cancellation, and structured process evidence;
- `internal/policy`: the control-plane policy seam (allow, deny,
  approval_required) shared by write tools (#10) and process recipes (#26),
  with operator-approval lookup; model output is never an input to a
  decision;
- `internal/state`: SQLite persistence for tasks, actions, attempts, evidence,
  journal, write-policy decisions, approvals and governor protection;
- `internal/recovery`: the issue #9 resume/reconciliation pipeline, extended
  by #10 to reconcile interrupted writes from observable filesystem state;
- `internal/trace`: JSON `log/slog` construction and level parsing.

The `internal/verifier` package implements the #11 control-plane completion
boundary (see [verification.md](verification.md)): a `status="complete"` final
is a proposal, the verifier independently observes persisted evidence, the
real filesystem, real git state and the operator acceptance plan, and the
state-layer completion gate refuses `completed` without a passed verification
attempt. The provider boundary represents one logical completion,
not an unaccounted retry loop. Legacy single-attempt clients must explicitly
declare amplification disabled. Receipt-aware clients return one authoritative
receipt per real upstream attempt, and the governor reconciles every receipt.
Adapter-owned retry policy, fallback selection, account rotation, queue
scheduling and quota policy remain forbidden; those decisions belong to the
#21 governor above the adapter.

Constructing a `Client` with receipt-aware configuration and a compatible
producer permits its package-level `Complete` path to make one model POST and
validate the returned receipt set. Missing, malformed, duplicated,
out-of-order or mismatched receipts fail closed. The default CLI path does not
yet construct that configuration, while `Preflight` always remains a
non-authorizing diagnostic. Production activation, compatible contract/version
documentation and the opt-in live success test belong to #30. Docker support
remains optional and pending #15; native commands are authoritative.

## Issue #21 account protection

The account-scoped governor is process-local M1 policy above every provider
adapter. It owns admission, one-account serialization, start-to-start pacing,
rolling budgets, manual reserve, cooldowns, retry eligibility and circuit
state. A provider call is one logical completion. On a legacy route, the permit
start is the single-attempt debit point. On a receipt-aware route, the permit
reserves the logical request and finish validates and reconciles one debit per
authoritative upstream-attempt receipt. Missing or structurally invalid
accounting produces a conservative uncertain debit, marks telemetry unsafe and
blocks later admission. The governor never runs an autonomous retry loop; each
executor retry must re-enter admission. See
[`docs/account-protection.md`](account-protection.md) and
[`architecture/attempt-receipts.md`](architecture/attempt-receipts.md).

## Issue #5 protocol parser

`internal/protocol.Parse` is a stateless parser for the adopted
`runstead.protocol.v1` contract. It accepts exactly one strict
`<runstead_action>...</runstead_action>` or
`<runstead_final>...</runstead_final>` envelope. Short prose before or after a
single valid envelope is allowed and sets `MixedProse`; prose inside the tagged
JSON block, multiple/nested/mismatched envelopes, unclosed tags, trailing JSON
values and unknown JSON fields are rejected without repair. Responses larger
than 1 MiB or JSON nested beyond 128 levels are rejected as malformed; the
parser never truncates input or attempts a partial parse.

Actions contain only `version`, `tool` and `arguments`. The injected
`protocol.ToolCatalog` seam first identifies a registered tool and then
validates its typed `protocol.Arguments` object. The issue #6/#10 registry
lives in `internal/tools` and stays out of this package. A schema-valid action
can still be rejected as `unknown_tool` or `invalid_arguments`; only an
accepted action is executable. Final responses
contain only `version`, `status` (`complete` or `incomplete`), `summary` and
non-empty `evidence`, where every evidence entry is a typed citation
`{"evidence_id": "...", "tool": "..."}` declaring the tool that produced the
cited observation. An accepted final response is not a tool execution and does
not by itself establish task completion: the cited IDs must exist in the
task's persisted evidence AND match their declared tool, or the verifier
rejects the final (issue #11).

Failures expose stable codes: `missing_envelope`, `protocol_refusal`,
`unsupported_execution_claim`, `multiple_envelopes`, `unclosed_envelope`,
`malformed_json`, `invalid_action_schema`, `invalid_final_schema`,
`unsupported_protocol_version`, `unknown_tool`, `invalid_arguments` and
`repeated_action`. Each failure says whether a bounded structured correction is
reasonable; `GenerateCorrectionMessage` only emits the deterministic M0
correction JSON and never owns retries or a correction budget.

The no-envelope refusal/claim classifier is only a deterministic aid for known
phrases with concrete local-work context; generic protocol commentary remains
`missing_envelope`. It is not natural-language understanding, and the
disposable M0 shell implementation remains unchanged.

`ActionFingerprint` hashes the tool name and canonical JSON arguments with
SHA-256. `RepeatGuard` is caller-owned session state; its `Check` method returns
the typed `repeated_action` failure and the parser itself keeps no history, so
repeated actions can be rejected before execution. Actual tool
registration/execution is implemented by the issue #6/#10 registry, and the
issue #7 loop owns the correction budget, the workspace-aware repeat guard and
the control-plane write-policy gate for `write_file`/`apply_patch` (issue
#10). See
[`tools.md`](tools.md) for the strict tool contracts and workspace boundary,
and [`writes.md`](writes.md) for the safe-writes model.

## Issue #7 bounded agent loop

`internal/agent` implements the bounded loop. The composition root is
`cmd/runstead run`, which wires one task, one workspace, the account-scoped
governor, the tool registry (read-only tools plus policy-gated writes) and the
loop.

### Running a task

The deterministic offline mode runs the real loop without any network access:
the model responses are replayed from a JSONL file (`{"text":"..."}` per line)
through the real governor, parser and tools.

```bash
runstead run \
  --task "What does this repository's README describe?" \
  --workspace /path/to/repository \
  --scripted /path/to/responses.jsonl
```

Each turn must contain exactly one `runstead.protocol.v1` envelope; the final
envelope must cite observation IDs (`obs-000001`, ...) that the run actually
produced. Scripted responses therefore reference the deterministic IDs of the
tools they execute. The command prints `outcome:`/`reason:`/`summary:`/
`evidence:` to stdout — the completed task's `summary:` is the
verifier-produced summary of the acceptance checks, and the model's own final
text appears only as `note (unverified):` — plus a sanitized lifecycle trace to
stderr, and exits with the typed outcome code.

Live OmniRoute configuration is accepted but refused before execution: protected
live use remains blocked until a compatible OmniRoute attempt-receipt producer
exists and #30 activates the live path (#29 -> #30 -> #4). The opt-in live
check in `internal/provider/omniroute/live_test.go` stays skipped by default.

### Loop budgets and defaults

| Bound | Default | Outcome when exhausted |
| --- | ---: | --- |
| `--max-steps` | 24 | `steps_exhausted` |
| `--max-corrections` | 2 | `corrections_exhausted` |
| `--max-repeated-actions` | 2 | `repeated_action` |
| `--time-budget` | 10m | `time_budget_exhausted` / `account_delay_timeout` |
| `--provider-budget` | 80 | `provider_budget_exhausted` |

Every budget can also be set through the environment variable named in the
CLI help: `RUNSTEAD_MAX_STEPS`, `RUNSTEAD_MAX_CORRECTIONS`,
`RUNSTEAD_MAX_REPEATED_ACTIONS`, `RUNSTEAD_TIME_BUDGET` and
`RUNSTEAD_PROVIDER_BUDGET`. Precedence is flags > environment > defaults,
matching the rest of the CLI.

`--max-corrections 0` and `--max-repeated-actions 0` (and the matching
environment values) are valid explicit values that disable the corresponding
allowance: the loop stops with `corrections_exhausted` or `repeated_action`
without granting a single correction or repeat. In the `agent.Limits` struct,
zero has the same meaning; only negative values for those two fields fall back
to the defaults.

The account governor below the loop enforces its own rolling 3h/1h/10m ceilings,
manual reserve, task budget, retry budget, start-to-start pacing, cooldowns and
circuit state. Every model turn, including the initial request, tool-follow-up
turns, corrections and any retry, re-enters governor admission and counts
against the task request budget; no retry can bypass the governor.

### Typed outcomes and exit codes

Every loop exit is a typed `agent.Outcome` with one stable process exit code:

| Outcome | Exit code |
| --- | ---: |
| `completed` | 0 |
| `steps_exhausted` | 20 |
| `corrections_exhausted` | 21 |
| `repeated_action` | 22 |
| `time_budget_exhausted` | 23 |
| `provider_budget_exhausted` | 24 |
| `account_delay_timeout` | 25 |
| `account_circuit_open` | 26 |
| `final_not_grounded` | 27 |
| `provider_failure` | 28 |
| `final_incomplete` | 29 |
| `canceled` | 130 |

The mapping is centralized in `agent.Outcome.ExitCode`. `canceled` follows the
shell convention; the remaining outcomes start at 20 so CLI usage errors (2) and
unavailable paths (3) stay distinct. Provider failures preserve the concrete
governor classification (`rate_or_capacity`, `authentication_denied`, ...) in
the stop reason.

### Trace

The loop emits one sanitized lifecycle line per provider attempt, action,
observation, correction, protocol deviation and terminal stop. Lines carry
sequence, kind, status, duration, evidence ID, tool, correction code and stop
reason only. Prompts, response bodies, credentials, tokens, cookies and account
identifiers are never traced. The CLI prints these lines to stderr; tests use an
in-memory sink.

### Grounding and untrusted observations

Repository content and tool output are structurally separated from the system
contract: observations are appended under a distinct `observation` transcript
role with the #6 `untrusted` marker and never become system instructions,
permissions, policy or approval. A `runstead_final` is syntax only; `completed`
is accepted only when every cited evidence ID was produced by a successful
observation in the current run, the declared tool of each citation matches the
persisted evidence row (issue #11), and the control-plane verifier passes.
Fabricated IDs produce `final_not_grounded`, and an `incomplete` final is a
grounded terminal `final_incomplete`.

### Safety invariants

- No `provider.Client` reference and no `provider.Client.Complete` call exists
  in the loop implementation; a source-level test enforces the boundary.
- No write tool, shell, arbitrary subprocess, network tool, persistence,
  resume, daemon, background execution or multi-agent behavior in this
  milestone.
- Cancellation is one-shot `context.Context` propagated through governor
  admission, provider I/O and tool execution. Cancellation before admission
  consumes no upstream attempt; cancellation after the upstream may have been
  reached stays conservatively debited by the governor.
- Concurrent tasks share one account lane: admission is serialized by the
  governor, no attempt is double-executed, and race tests assert the absence
  of goroutine leaks.


## Native path remains authoritative

These commands must remain supported outside Docker:

```bash
go test ./...
go vet ./...
go build ./cmd/runstead
```

Docker must run the same commands rather than define a separate build system.

## Initial deliverables

The first Docker issue should add:

- `Dockerfile.dev` or an equivalent clearly development-only Dockerfile;
- `compose.yaml` with the development service and named caches;
- `.dockerignore`;
- documented environment-variable handling;
- helper commands through a small `Makefile` or shell script only if they remain transparent;
- tests for file ownership and workspace mounts;
- documentation for building, testing and running the protocol experiment;
- confirmation that no secrets are copied into image history.

## Later runtime isolation

Running Runstead itself inside a container may become a supported execution mode, but it should be designed separately from the development container.

A runtime container must receive an explicit target workspace mount and should expose no broader host access. It must not be marketed as a secure sandbox until adversarial escape and policy testing exist.
