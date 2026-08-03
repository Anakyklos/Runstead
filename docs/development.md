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

The native bootstrap has no external Go dependencies. Build and test it with:

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

`run` currently validates configuration and then fails explicitly because the
full agent loop is deferred. `inspect` and `resume` are
explicit placeholders because durable state and recovery are not part of this
bootstrap.

Configuration precedence is deterministic: command-line flags, then
environment, then conservative defaults. Workspace/logging use
`RUNSTEAD_WORKSPACE` and `RUNSTEAD_LOG_LEVEL`. The optional OmniRoute config
uses `OMNIROUTE_BASE_URL`, `OMNIROUTE_API_KEY`, `OMNIROUTE_MODEL` and
`OMNIROUTE_CHAT_ENDPOINT`, with optional management URL and timeout variables;
the `run` command exposes matching flags. API keys are never logged or
included in errors, snapshots, telemetry or URLs.
The explicit route declaration variables
`OMNIROUTE_SINGLE_ATTEMPT_GUARANTEED`,
`OMNIROUTE_INTERNAL_RETRIES_DISABLED`,
`OMNIROUTE_COOLDOWN_REPLAY_DISABLED`,
`OMNIROUTE_ACCOUNT_POOLING_DISABLED` and
`OMNIROUTE_AUTOMATIC_FALLBACK_DISABLED`, or the equivalent
`--omniroute-safe-route` declaration, remain configuration inputs for the
governor contract. They do not authorize OmniRoute model execution: the
adapter remains unknown until #29/#30 provide and verify authoritative
attempt receipts.

Implemented package responsibilities are deliberately narrow:

- `cmd/runstead`: signal-aware process entrypoint, exit codes and CLI help;
- `internal/config`: flag/environment/default resolution;
- `internal/agent`: governor-owned executor seam with no loop or retry;
- `internal/protocol`: the adopted `runstead.protocol.v1` identifier;
- `internal/provider`: provider-neutral request/response types and a
  deterministic fake;
- `internal/provider/omniroute`: stdlib-only, non-streaming OmniRoute transport,
  fail-closed observable-settings checks, sanitized typed errors, classifier
  and optional telemetry source. It is a scaffold: the proposed
  `singleAttemptContract` and management snapshots never authorize a model
  POST. Protected execution remains disabled until #29/#30 provide
  authoritative attempt receipts and per-attempt governor accounting;
- `internal/trace`: JSON `log/slog` construction and level parsing.

The planned `tools`, `state` and `verifier` packages are intentionally absent
until they contain real behavior. The one-call/one-attempt provider contract
forbids adapter-owned retries, fallback selection, account rotation, queue
scheduling and quota policy; those decisions belong to the #21 governor above
the adapter. Construct the OmniRoute client, call `Preflight` for diagnostics,
then route each turn through `agent.Executor`/`governor.Execute` with
`omniroute.Classify`; the governor must reject the current unknown route.
Transport and response parsing are exercised through a package-local test
seam. The production `Complete` path returns `ErrUnsafeRoute` without a model
POST. The adapter's live safety check is opt-in:
`RUNSTEAD_LIVE_OMNIROUTE=1 go test ./internal/provider/omniroute -run Live`;
it must stop before any model request because authoritative attempt receipts
are unavailable. Docker support remains optional and pending #15; native
commands are authoritative.

## Issue #21 account protection

The account-scoped governor is process-local M1 policy above every provider
adapter. It owns admission, one-account serialization, start-to-start pacing,
rolling budgets, manual reserve, cooldowns, retry eligibility and circuit
state. `provider.Client.Complete` remains one attempt and never gains retry,
fallback, account rotation or quota behavior. See
[`docs/account-protection.md`](account-protection.md) for the SLO, explicit
Instant/reasoning profiles, single-attempt route safety, telemetry seam and
the limitations deferred to #7 and #8.

## Issue #5 protocol parser

`internal/protocol.Parse` is a stateless parser for the adopted
`runstead.protocol.v1` contract. It accepts exactly one strict
`<runstead_action>...</runstead_action>` or
`<runstead_final>...</runstead_final>` envelope. Short prose before or after a
single valid envelope is allowed and sets `MixedProse`; prose inside the tagged
JSON block, multiple/nested/mismatched envelopes, unclosed tags, trailing JSON
values and unknown JSON fields are rejected without repair. Responses larger
than 1 MiB or JSON nested beyond 128 levels are rejected as malformed; the
parser never truncates input and attempts a partial parse.

Actions contain only `version`, `tool` and `arguments`. The injected
`protocol.ToolCatalog` seam first identifies a registered tool and then
validates its typed `protocol.Arguments` object, keeping the future #6 registry
out of this package. A schema-valid action can still be rejected as
`unknown_tool` or `invalid_arguments`; only an accepted action is executable.
Final responses contain only `version`, `status` (`complete` or `incomplete`),
`summary` and non-empty string `evidence`. An accepted final response is not a
tool execution and does not by itself establish task completion.

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
registration/execution remains #6 work, and correction budgets plus the agent
loop remain #7 work.

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
