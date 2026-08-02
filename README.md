# Runstead

Runstead is a local agent runtime that turns the ChatGPT Web model exposed by OmniRoute into a tool-using, stateful and verifiable worker.

The project does **not** reimplement OmniRoute. OmniRoute remains the transport and provider gateway. Runstead owns the agent loop, local tools, durable state, policies, recovery and verification.

> Models may fail, sessions may disappear, and providers may change. The work must remain recoverable and inspectable.

## Initial product focus

The first supported path is deliberately narrow:

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

The first release must make ChatGPT Web perform real local development work through OmniRoute without trusting emulated native tool calling or unsupported claims made by the model.

## Core rules

1. **OmniRoute is transport; Runstead is the runtime.**
2. **Remote sessions are disposable; local state is authoritative.**
3. **The model proposes actions; Runstead validates and executes them.**
4. **No action is complete without evidence from the environment.**
5. **A failed conversation must not destroy an unfinished task.**
6. **Prefer explicit mechanisms over agent-framework magic.**
7. **Generalize only after the ChatGPT Web path is demonstrably reliable.**

## Technology direction

Runstead will begin as a modular monolith:

- **Go** for the CLI and runtime;
- **SQLite** for tasks, events and checkpoints;
- Go standard library for HTTP, JSON, processes, cancellation, logging and tests;
- the real `git` executable for repository operations;
- `rg` when available for text search, with a portable fallback;
- SQL and migrations written explicitly;
- a single distributable executable.

The initial project will not use agent frameworks, an ORM, Redis, message brokers, microservices, Kubernetes, a web UI or a vector database.

## First-release capabilities

Runstead v0.1 should be able to:

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
- `shell` with policy restrictions
- `git_status`
- `git_diff`

Write tools are introduced only after the read-only loop is stable:

- `write_file`
- `apply_patch`

## Explicit non-goals for v0.1

- multi-agent orchestration;
- model marketplaces or automatic model routing;
- MCP support;
- vector memory or RAG;
- graphical interfaces;
- distributed execution;
- broad provider compatibility;
- unattended long-running autonomy;
- replacing OmniRoute.

## Definition of success

A hello-world tool call is not enough. The v0.1 acceptance scenario must complete a real repository task with multiple inspect/edit/test cycles, include at least one recoverable failure, survive process interruption, resume correctly and provide verifiable final evidence.

## Project status

**Planning and protocol research.** The backlog is being organized around a sequence of milestones from protocol validation to a reliable v0.1 agent runtime.

See [`docs/architecture.md`](docs/architecture.md) and [`docs/roadmap.md`](docs/roadmap.md) for the current technical direction and delivery sequence.
