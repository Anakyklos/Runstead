# Runstead

Runstead is a local agent runtime that turns model access into durable, controlled and verifiable work.

Runstead is provider-neutral. It supports configurable model providers that implement one of three compatibility protocol families:

- `openai_compatible`;
- `anthropic_compatible`;
- `google_compatible`.

Provider identity and protocol family are separate concepts. `provider_id` names one operator-configured endpoint; `protocol_family` names the wire contract it speaks. Official OpenAI, Anthropic and Google endpoints are only examples of those families: they are valid implementations, not privileged architectural dependencies. A new endpoint that sufficiently implements a supported family normally requires configuration plus compatibility evidence, not changes to the agent loop.

> Models may fail, sessions may disappear, providers may change, and transports may be replaced. The work must remain recoverable and inspectable.

## Provider strategy

```text
Runstead
    ├── agent loop
    ├── action protocol
    ├── local tools
    ├── policy enforcement
    ├── durable state
    ├── verification
    └── recovery
        ↓
provider-neutral contract (internal/provider)
        ↓
┌─────────────────────┬───────────────────────────┬─────────────────────┐
│ openai_compatible   │ anthropic_compatible      │ google_compatible   │
└─────────────────────┴───────────────────────────┴─────────────────────┘
        ↓
configured provider endpoint (official vendor, gateway, local service
or third-party inference provider)
```

The runtime depends only on the small provider-neutral contract (`provider.Client`, `RouteSafety`, delivery evidence) and on declared/proven capabilities. Protocol adapters are replaceable infrastructure added per family (#87 OpenAI-compatible, #88 Anthropic-compatible, #89 Google/Gemini-compatible). The agent loop never branches on a vendor name or a protocol family.

### ChatGPT Web / OmniRoute status

ChatGPT Web and OmniRoute are **out of the v0.1 critical path** and explicitly deferred to future plugin/composable-provider work. OmniRoute may later be used as an ordinary compatible endpoint if it satisfies one of the supported protocol contracts, but it holds no special core status. The historical browser/web research under [`docs/research/`](docs/research/) remains preserved as provenance/reference material only.

## Core rules

1. **Runstead owns the agent runtime, regardless of transport.**
2. **Providers are replaceable infrastructure behind a neutral contract; no vendor is privileged.**
3. **Remote sessions are disposable; local state is authoritative.**
4. **The model proposes actions; Runstead validates and executes them.**
5. **No action is complete without evidence from the environment.**
6. **A failed conversation or provider must not destroy an unfinished task.**
7. **Provider identity and protocol family are distinct; capability is proven, not assumed from naming.**
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

The current tool registry contract is documented in
[`docs/tools.md`](docs/tools.md); safe writes, approval and crash behavior in
[`docs/writes.md`](docs/writes.md).

## Durable state

Runstead persists task state, execution attempts, the event journal,
write-policy decisions, approvals and account-protection state to a local
SQLite database (issues #8/#10): every accepted action, every concrete
tool/provider attempt, every observation evidence id and every meaningful
transition is durably recorded, and a task can be inspected after the original
process exits with:

```text
runstead run --task "inspect the repo" --workspace /path --scripted responses.jsonl
task: cli-...
runstead inspect cli-... --state-dir ~/.local/share/runstead
```

An interrupted task can be resumed from that durable state (issues #9/#10)
with a new provider conversation, without replaying completed effects, and
write approvals are recorded by the operator control plane:

```text
runstead resume cli-... --state-dir ~/.local/share/runstead --scripted responses.jsonl
runstead decide cli-... action-000005 approved --state-dir ~/.local/share/runstead
```

The persistence architecture, SQLite driver decision, pragmas, database
location, migrations, `runstead inspect`, `runstead resume`, governor
durability, security/redaction and the recovery contract
(`docs/persistence.md`, `docs/adr/0001-durable-execution.md`) document the
implemented behavior.

The deterministic chaos and interruption suite (issue #13) and its auditable
failure matrix are documented in
[`docs/chaos-hardening.md`](docs/chaos-hardening.md).

## Development environment

An optional Docker-based development environment keeps Go, `jq`, test utilities, caches and later transport-specific native dependencies out of the host system.

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

The provider-neutral v0.1 should be able to:

- accept a development task;
- inspect a repository;
- ask the configured model provider for structured actions through a Runstead-owned protocol;
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

Write tools (`write_file`, `apply_patch`) are implemented (issue #10) with
workspace containment, stale-state protection, durable intent/effect/result
ordering, structured evidence and control-plane approval. Bounded process
recipes (`run_recipe`, issue #26) let the model select operator-declared
recipes (test, build, vet, ...) by ID with no generic shell: fixed argv,
allowlisted environment, bounded output, full process-tree termination on
timeout/cancellation and structured process evidence.

## Explicit non-goals for the provider-neutral v0.1

- multi-agent orchestration;
- model marketplaces or automatic model routing;
- MCP support;
- vector memory or RAG;
- graphical interfaces;
- distributed execution;
- a universal AI gateway or generic OpenAI-compatible proxy/server;
- unattended long-running autonomy;
- automatic provider/model/key/account fallback, rotation or pooling;
- ChatGPT Web transport in the critical path (deferred plugin work).

## Definition of success

A hello-world tool call is not enough. The v0.1 acceptance scenario must complete a real repository task with multiple inspect/edit/test cycles, include at least one recoverable failure, survive process interruption, resume correctly and provide verifiable final evidence through a configured provider endpoint implementing one of the supported protocol families.

## Project status

The repository now contains the native Go module, the small CLI surface, the
strict `runstead.protocol.v1` parser (#5), the tool registry with workspace
boundary and evidence identifiers (#6), the account-scoped request governor
with attempt receipts (#21, PR #33), the fail-closed OmniRoute adapter
scaffold (#28), the bounded agent loop (#7), durable SQLite state (#8),
resume/recovery (#9), the policy-gated safe write tools (#10) and the bounded
process runner (#26). The provider strategy has been rebased on compatibility
protocol families (#79/#86): provider identity, protocol family
(`openai_compatible`, `anthropic_compatible`, `google_compatible`),
configuration, versioned capability profiles and fail-closed pre-dispatch
resolution now exist in `internal/provider`; the three concrete family adapters
live in `internal/provider/openaicompat` (#87, OpenAI-compatible),
`internal/provider/anthropiccompat` (#88, Anthropic-compatible) and
`internal/provider/googlecompat` (#89, Google/Gemini-compatible), each with one
physical request per governed completion and no retry/fallback/redirect
following. `runstead run` executes a deterministic task end to end:
every model turn is admitted by the account governor, actions are validated
and executed by the registry (read-only tools plus policy-gated
`write_file`/`apply_patch` with stale-state protection and operator-declared
`run_recipe` processes with no generic shell), observations return as
untrusted data, and a final answer is accepted only when grounded in evidence
IDs produced during the run. The deterministic offline mode replays scripted
model responses through the real governor and tools (`--scripted FILE`);
live execution through a configured endpoint uses the provider-neutral
surface (`--providers FILE --provider-id ID`), resolved through the #79
contract before any dispatch, with one governed physical request per model
turn and no retry/fallback/rotation.

See [`docs/architecture.md`](docs/architecture.md), [`docs/roadmap.md`](docs/roadmap.md) and [`docs/development.md`](docs/development.md).

The M1 account-scoped request governor and its internal Account Protection SLO
are documented in [`docs/account-protection.md`](docs/account-protection.md).
Bounded governor-owned retry execution is documented in [`docs/retry.md`](docs/retry.md).
Passive conservative operational envelope learning from typed provider
evidence is documented in [`docs/learning.md`](docs/learning.md). Sanitized
per-attempt request telemetry (adapter version, transport, session
fingerprint, first-token latency and the protected-lane zero fields) is
documented in [`docs/telemetry.md`](docs/telemetry.md).
