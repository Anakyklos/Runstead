# Runstead

Runstead is a local agent runtime that turns model access into durable, controlled and verifiable work.

The first implementation path uses ChatGPT Web through OmniRoute. This is an intentional bootstrap decision: Runstead must first prove its agent loop, action protocol, tools, persistence and verification before assuming ownership of the unstable ChatGPT Web transport layer.

After the OmniRoute-backed runtime is reliable, Runstead will develop a first-party ChatGPT Web connector, compare both paths under the same workload and migrate only if the direct connector demonstrates a material reliability or observability advantage.

> Models may fail, sessions may disappear, providers may change, and transports may be replaced. The work must remain recoverable and inspectable.

## Delivery strategy

### Phase 1 — OmniRoute baseline

```text
User / CLI
    ↓
Runstead
    ├── agent loop
    ├── action protocol
    ├── local tools
    ├── policy enforcement
    ├── durable state
    ├── verification
    └── recovery
    ↓
OmniRoute
    ↓
ChatGPT Web
```

OmniRoute is the initial transport and compatibility layer. Runstead does not trust OmniRoute's emulated native tool calling; it owns a text action protocol and independently verifies every side effect.

### Phase 2 — First-party ChatGPT Web connector

```text
Runstead Core
├── provider/omniroute
└── provider/chatgptweb
```

The direct connector will own only the ChatGPT Web concerns required by Runstead: credential import, session exchange, browser-compatible transport, request construction, SSE parsing, error classification and sanitized diagnostics.

It will not become a general-purpose router or attempt to reproduce OmniRoute's full provider catalog.

### Phase 3 — Evidence-based migration

The direct connector becomes the default only after an A/B evaluation using the same account, model, protocol, tasks and acceptance criteria. Until then, OmniRoute remains the baseline, compatibility adapter and fallback for development.

## Core rules

1. **Runstead owns the agent runtime, regardless of transport.**
2. **OmniRoute is the first transport, not a permanent architectural dependency.**
3. **Remote sessions are disposable; local state is authoritative.**
4. **The model proposes actions; Runstead validates and executes them.**
5. **No action is complete without evidence from the environment.**
6. **A failed conversation or provider must not destroy an unfinished task.**
7. **Generalize only after the ChatGPT Web path is demonstrably reliable.**
8. **Do not replace working infrastructure without measured evidence.**

## Technology direction

Runstead begins as a modular monolith:

- **Go** for the CLI and runtime;
- **SQLite** for tasks, events and checkpoints;
- Go standard library for HTTP, JSON, processes, cancellation, logging and tests;
- the real `git` executable for repository operations;
- `rg` when available for text search, with a portable fallback;
- SQL and migrations written explicitly;
- a single native executable as the primary distribution artifact.

The initial project will not use agent frameworks, an ORM, Redis, message brokers, microservices, Kubernetes, a web UI or a vector database.

The current read-only registry contract is documented in
[`docs/tools.md`](docs/tools.md).

## Development environment

A Docker-based development environment is recommended to keep Go, `jq`, test utilities, caches and later transport-specific native dependencies out of the host system.

Docker is an optional development and test boundary, not the product architecture and not a complete sandbox. Source repositories mounted with write access remain modifiable by the container.

The development container must follow these rules:

- run as a non-root user;
- mount only the Runstead source tree and explicitly selected test workspaces;
- never mount the Docker socket;
- never use `--privileged`;
- keep credentials outside the image and repository;
- use named volumes for Go module and build caches;
- preserve a native host build path for the final CLI;
- avoid requiring Docker for end users.

See [`docs/development.md`](docs/development.md).

## First-release capabilities

The OmniRoute-backed v0.1 should be able to:

- accept a development task;
- inspect a repository;
- ask ChatGPT Web for structured actions through a Runstead-owned protocol;
- validate and execute approved local tools;
- edit files safely;
- run tests and inspect exit codes;
- verify changes through files, hashes and Git diffs;
- persist every meaningful event;
- survive interruption and resume from a checkpoint;
- detect malformed actions, repeated actions and false completion claims;
- finish with evidence, not merely a model-generated summary.

## Initial tools

Read-only tools come first:

- `read_file`
- `list_files`
- `search_text`
- `git_status`
- `git_diff`

Write tools are introduced only after the read-only loop is stable:

- `write_file`
- `apply_patch`

## Explicit non-goals for the OmniRoute-backed v0.1

- multi-agent orchestration;
- model marketplaces or automatic model routing;
- MCP support;
- vector memory or RAG;
- graphical interfaces;
- distributed execution;
- broad provider compatibility;
- unattended long-running autonomy;
- a general-purpose replacement for OmniRoute;
- direct ChatGPT Web credential handling before the runtime baseline is proven.

## Definition of success

A hello-world tool call is not enough. The v0.1 acceptance scenario must complete a real repository task with multiple inspect/edit/test cycles, include at least one recoverable failure, survive process interruption, resume correctly and provide verifiable final evidence through the OmniRoute baseline.

The later direct connector is successful only if it matches this capability and provides measurable improvement without weakening security or maintainability.

## Project status

**M1 in progress.** The repository now contains the native Go module, the
small CLI surface, the strict `runstead.protocol.v1` parser (#5), the
read-only tool registry with workspace boundary and evidence identifiers
(#6), the account-scoped request governor with attempt receipts (#21, PR
#33), the fail-closed OmniRoute adapter scaffold (#28) and the bounded
read-only agent loop (#7). `runstead run` now executes a deterministic
read-only task end to end: every model turn is admitted by the account
governor, actions are validated and executed by the read-only registry,
observations return as untrusted data, and a final answer is accepted only
when grounded in evidence IDs produced during the run. The deterministic
offline mode replays scripted model responses through the real governor and
tools (`--scripted FILE`); live OmniRoute execution remains disabled until a
compatible attempt-receipt producer exists and #30 activates it. Durable
SQLite state (#8) and recovery (#9) remain later milestone work.

See [`docs/architecture.md`](docs/architecture.md), [`docs/roadmap.md`](docs/roadmap.md) and [`docs/development.md`](docs/development.md).

The M1 account-scoped request governor and its internal Account Protection SLO
are documented in [`docs/account-protection.md`](docs/account-protection.md).
