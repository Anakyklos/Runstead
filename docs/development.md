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
provider adapter and full loop are deferred. `inspect` and `resume` are
explicit placeholders because durable state and recovery are not part of this
bootstrap.

Configuration precedence is deterministic: command-line flags, then
`RUNSTEAD_WORKSPACE`/`RUNSTEAD_LOG_LEVEL`, then conservative defaults (`.` and
`info`). Credentials and complete environment contents are never logged.

Implemented package responsibilities are deliberately narrow:

- `cmd/runstead`: signal-aware process entrypoint, exit codes and CLI help;
- `internal/config`: flag/environment/default resolution;
- `internal/agent`: one provider request seam with no loop or retry;
- `internal/protocol`: the adopted `runstead.protocol.v1` identifier;
- `internal/provider`: provider-neutral request/response types and a
  deterministic fake;
- `internal/trace`: JSON `log/slog` construction and level parsing.

The planned `tools`, `policy`, `state` and `verifier` packages are intentionally
absent until they contain real behavior. The one-call/one-attempt provider
contract forbids adapter-owned retries, fallback selection, account rotation,
queue scheduling and quota policy; those belong above the adapter and will be
designed with #21. The OmniRoute adapter is #4, the complete parser is #5 and
the agent loop is #7. Docker support remains optional and pending #15; native
commands are authoritative.

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
