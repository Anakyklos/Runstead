# Delivery Roadmap

The roadmap is organized around proving the riskiest assumption first: ChatGPT Web must reliably participate in a Runstead-owned action loop through OmniRoute.

Dates are intentionally omitted until the protocol experiment produces evidence. Milestones represent capability gates, not calendar promises.

## Milestone 0 — Protocol proof

**Goal:** determine whether ChatGPT Web can follow a strict, recoverable Runstead action contract with useful consistency.

Deliverables:

- reproducible `curl`/shell experiment against OmniRoute;
- candidate action envelope and final-response envelope;
- prompt contract for available tools and observations;
- corpus of valid, malformed and mixed responses;
- measurements for parse success, correction success and repeated-action behavior;
- written decision on the protocol accepted for implementation.

Exit criteria:

- the model completes a multi-step read-only loop repeatedly;
- malformed actions can be corrected within a bounded retry policy;
- the result is reproducible enough to justify implementing the runtime.

## Milestone 1 — Read-only agent loop

**Goal:** build a small Go CLI that can inspect a repository through ChatGPT Web.

Deliverables:

- Go module and modular-monolith skeleton;
- configuration through flags and environment variables;
- OmniRoute provider adapter;
- action parser and validator;
- read-only tool registry;
- bounded agent loop;
- human-readable trace output;
- unit and integration tests for the protocol boundary.

Exit criteria:

- `runstead run` can inspect an unfamiliar repository and answer a grounded question using actual tool observations;
- no write operations are available;
- all executed actions are visible in the trace.

## Milestone 2 — Durable state and recovery

**Goal:** make task progress survive process and remote-session failure.

Deliverables:

- SQLite schema and explicit migrations;
- append-oriented event history;
- persisted actions and tool results;
- checkpoints;
- `runstead inspect` and `runstead resume`;
- request idempotency strategy;
- bounded context reconstruction;
- recovery tests that terminate and restart the process.

Exit criteria:

- an interrupted task resumes without replaying completed side effects;
- a dead upstream conversation can be replaced using local state;
- the execution history remains inspectable.

## Milestone 3 — Safe repository modification

**Goal:** allow ChatGPT Web to modify code while Runstead retains control over side effects.

Deliverables:

- `write_file` and `apply_patch`;
- workspace boundary enforcement;
- command allow/deny policy;
- configurable approval gates;
- file hashes and before/after evidence;
- Git status and diff verification;
- protection against repeated writes and path traversal.

Exit criteria:

- Runstead can make a scoped code change without touching files outside the workspace;
- every modification is represented by evidence and traceable to a validated action;
- denied actions fail closed.

## Milestone 4 — Verifiable coding agent

**Goal:** complete real inspect/edit/test/fix cycles and reject unsupported completion claims.

Deliverables:

- test execution with timeout and captured exit status;
- explicit acceptance checks;
- completion verifier;
- loop and repetition detection;
- malformed-action correction policy;
- failure classification and retry limits;
- final evidence report.

Exit criteria:

- a real repository task is completed through multiple tool cycles;
- at least one test failure is diagnosed and corrected;
- the model cannot mark the task complete when acceptance checks fail.

## Milestone 5 — v0.1 hardening

**Goal:** demonstrate that the runtime remains dependable under expected failures.

Deliverables:

- chaos and interruption test suite;
- stale-session recovery;
- empty, truncated and malformed-response fixtures;
- stuck-command termination;
- duplicate-request protection;
- installation and configuration documentation;
- stable CLI behavior for the v0.1 surface;
- end-to-end acceptance scenario.

Exit criteria:

- a task with at least 20 meaningful steps survives interruption and one recoverable upstream failure;
- all side effects are accounted for;
- final output includes actual diff, test evidence and task history;
- known limitations are documented.

## Deferred until after v0.1

- additional providers;
- native tool-calling optimization;
- MCP;
- multi-agent orchestration;
- automatic model routing;
- graphical interfaces;
- distributed workers;
- semantic or vector memory;
- unattended long-running autonomy.

## Milestone governance

Each GitHub milestone should have:

- one capability-focused objective;
- explicit exit criteria;
- only issues necessary to reach that capability;
- no speculative features;
- a closing review documenting evidence and unresolved limitations.

An issue should move to a later milestone rather than weakening the current milestone's exit criteria.
