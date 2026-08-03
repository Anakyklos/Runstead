# Delivery Roadmap

Runstead follows a staged provider strategy:

1. prove the agent runtime through OmniRoute;
2. harden an OmniRoute-backed v0.1;
3. implement a narrow first-party ChatGPT Web connector;
4. migrate only after a controlled comparison demonstrates a real advantage.

Dates are intentionally omitted until the protocol experiment produces evidence. Milestones represent capability gates, not calendar promises.

## Milestone 0 — OmniRoute protocol proof

**Goal:** determine whether ChatGPT Web can follow a strict, recoverable Runstead action contract with useful consistency when reached through OmniRoute.

Deliverables:

- reproducible Docker-assisted or host-native `curl`/shell experiment against OmniRoute;
- candidate action envelope and final-response envelope;
- prompt contract for available tools and observations;
- corpus of valid, malformed and mixed responses;
- sanitized raw-response capture;
- measurements for parse success, correction success, protocol refusal and repeated-action behavior;
- written decision on the protocol accepted for implementation.

Exit criteria:

- the model completes a multi-step read-only loop repeatedly;
- malformed actions can be corrected within a bounded retry policy;
- failures can be classified from captured evidence;
- the result is reproducible enough to justify implementing the runtime.

## Milestone 1 — OmniRoute-backed read-only agent loop

**Goal:** build a small Go CLI that can inspect a repository through ChatGPT Web using OmniRoute as the baseline transport.

Deliverables:

- Go module and modular-monolith skeleton;
- reproducible Docker development environment that remains optional;
- configuration through flags and environment variables;
- minimal provider interface and deterministic fake provider;
- account-scoped ChatGPT Web request governor with rolling budgets and circuit protection;
- OmniRoute baseline provider adapter;
- action parser and validator;
- read-only tool registry;
- bounded agent loop;
- human-readable trace output;
- unit and integration tests for the protocol boundary.

Exit criteria:

- `runstead run` can inspect an unfamiliar repository and answer a grounded question using actual tool observations;
- no write operations are available;
- all executed actions are visible in the trace;
- the same test suite runs inside Docker and through native Go commands.

## Milestone 2 — Durable state and recovery

**Goal:** make task progress survive process and remote-session failure while remaining independent of OmniRoute session state.

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
- the execution history remains inspectable;
- provider-specific identifiers remain disposable metadata.

## Milestone 3 — Safe repository modification

**Goal:** allow ChatGPT Web to modify code while Runstead retains control over side effects.

Deliverables:

- `write_file` and `apply_patch`;
- workspace boundary enforcement;
- command allow/deny policy;
- configurable approval gates;
- file hashes and before/after evidence;
- Git status and diff verification;
- protection against repeated writes and path traversal;
- explicit Docker workspace-mount policy for write-capable runs.

Exit criteria:

- Runstead can make a scoped code change without touching files outside the selected workspace;
- every modification is represented by evidence and traceable to a validated action;
- denied actions fail closed;
- Docker-based runs do not receive broader host access than the selected workspace.

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

## Milestone 5 — OmniRoute-backed v0.1 hardening

**Goal:** demonstrate that the runtime remains dependable under expected failures before adding direct ChatGPT Web transport complexity.

Deliverables:

- chaos and interruption test suite;
- stale-session recovery;
- empty, truncated and malformed-response fixtures;
- stuck-command termination;
- duplicate-request protection;
- installation and OmniRoute configuration documentation;
- Docker development workflow documentation;
- stable CLI behavior for the v0.1 surface;
- OmniRoute-backed end-to-end acceptance scenario.

Exit criteria:

- a task with at least 20 meaningful steps survives interruption and one recoverable upstream failure;
- all side effects are accounted for;
- final output includes actual diff, test evidence and task history;
- known limitations are documented;
- the runtime baseline is stable enough that later transport failures can be isolated from agent-runtime failures.

## Milestone 6 — First-party ChatGPT Web connector

**Goal:** implement a narrow direct ChatGPT Web provider adapter without turning Runstead into a general-purpose router.

Deliverables:

- documented credential-import flow that never collects account passwords;
- secure local credential-storage decision;
- session and token lifecycle implementation;
- browser-compatible transport strategy;
- ChatGPT Web request construction and SSE decoding;
- sanitized raw transport traces;
- precise authentication, challenge, timeout, protocol and upstream error classes;
- direct-connector integration tests;
- dedicated container image or helper only if native transport dependencies require it.

Exit criteria:

- the direct adapter can run the same read-only protocol suite used by OmniRoute;
- credentials are not written to repositories, images, task databases or logs;
- a failed direct session can be replaced without losing local task state;
- the direct connector does not introduce a public compatibility API or unrelated provider features.

## Milestone 7 — Provider bake-off and migration decision

**Goal:** compare OmniRoute and the first-party connector under equivalent conditions and choose the default based on evidence.

Deliverables:

- shared benchmark and failure corpus;
- repeated independent sessions using the same account, model, prompt contract and tasks;
- comparison of valid-action rate, correction rate, refusal rate, malformed responses, recoverability, latency and diagnostic precision;
- maintenance and security review;
- written architecture decision record;
- migration plan if the direct connector wins;
- retained OmniRoute compatibility plan or removal rationale.

Exit criteria:

- the decision is based on repeated measurements rather than isolated demos;
- the direct connector becomes default only with a material and documented advantage;
- migration does not weaken task recovery, evidence, credential safety or testability;
- if the direct connector does not win, OmniRoute remains the default without treating the experiment as failure.

## Deferred until after the provider decision

- additional web providers such as Qwen or GLM;
- native tool-calling optimization;
- MCP;
- multi-agent orchestration;
- automatic model routing;
- graphical interfaces;
- distributed workers;
- semantic or vector memory;
- unattended long-running autonomy;
- a general-purpose OpenAI-compatible gateway.

## Milestone governance

Each GitHub milestone should have:

- one capability-focused objective;
- explicit exit criteria;
- only issues necessary to reach that capability;
- no speculative features;
- a closing review documenting evidence and unresolved limitations.

An issue should move to a later milestone rather than weakening the current milestone's exit criteria.
