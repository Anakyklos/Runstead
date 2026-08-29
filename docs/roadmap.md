# Delivery Roadmap

Runstead follows a staged provider strategy (decision #86, implemented from #79):

1. define the provider-neutral contract: provider identity, protocol family (`openai_compatible`, `anthropic_compatible`, `google_compatible`), configuration and versioned capability profiles with fail-closed resolution (#79);
2. implement one protocol adapter per supported family (#87 OpenAI-compatible — done in `internal/provider/openaicompat`; #88 Anthropic-compatible — done in `internal/provider/anthropiccompat`; #89 Google/Gemini-compatible — done in `internal/provider/googlecompat`);
3. harden the runtime against configured compatible endpoints, including a shared deterministic compatibility suite — done in #14 (`internal/provider/compat` composition surface, `--providers`/`--provider-id` operator surface, shared matrix, cross-family inspect-edit-test-fix E2E with interruption/resume; see [`provider-compatibility.md`](provider-compatibility.md));
4. run opt-in live smoke tests against representative configured providers — procedure implemented (`experiments/provider-live/run.sh`, opt-in only); no family is live-proven yet in the gate environment (all three are recorded as operationally unproven).

Provider identity and protocol family are distinct concepts; official vendors are examples, not privileged dependencies. ChatGPT Web/OmniRoute work is deferred to future plugin/composable-provider tracks and is not on the v0.1 critical path.

Dates are intentionally omitted until the protocol experiment produces evidence. Milestones represent capability gates, not calendar promises.

## Milestone P0 — Provider contract (issue #79)

**Goal:** represent and validate configurable providers across the three supported protocol families without touching the agent loop.

Deliverables:

- stable operator-configured provider IDs distinct from protocol family;
- the three-family enum with fail-closed parsing (`internal/provider`);
- provider configuration: base URL, exact model, non-secret auth reference, strictly necessary non-secret options, config version identity;
- explicit versioned capability profiles gating execution (text turn, Runstead structured protocol, native tools only when separately proven, streaming, cancellation);
- RouteSafety as the single executable attempt-safety truth, integrated with configuration resolution;
- pre-dispatch resolution failing closed on unknown provider/family, incomplete config, missing model/endpoint/capability/auth reference or incompatible route safety;
- secrets kept outside metadata, errors, traces, contract identity and fixtures.

Exit criteria:

- two provider IDs on the same family resolve through the same contract with no agent-loop branching;
- all three families are representable through the same boundary;
- every invalid configuration case fails before dispatch;
- no hidden retry/fallback/pooling can be declared as safe configuration;
- existing fake-provider and deterministic runtime tests stay green.

## Milestone 0 — Protocol proof

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

## Milestone 1 — Read-only agent loop (historical: OmniRoute-backed)

**Goal (historical wording):** build a small Go CLI that can inspect a repository through ChatGPT Web using OmniRoute as the then-baseline transport. The implemented runtime is provider-neutral; the capabilities below are the delivered outcome.

Deliverables:

- Go module and modular-monolith skeleton;
- reproducible Docker development environment that remains optional;
- configuration through flags and environment variables;
- minimal provider interface and deterministic fake provider;
- account-scoped provider request governor with rolling budgets and circuit protection;
- fail-closed baseline provider adapter;
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

**Goal:** make task progress survive process and remote-session failure while remaining independent of any provider session state.

Deliverables:

- durable-execution contract ([`adr/0001-durable-execution.md`](adr/0001-durable-execution.md), #25);
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

**Goal:** allow the configured model to modify code while Runstead retains control over side effects.

Deliverables:

- `write_file` and `apply_patch` (implemented in #10, see
  [`writes.md`](writes.md));
- workspace boundary enforcement (implemented: shared resolver, traversal,
  absolute-path and symlink-escape fail-closed);
- command allow/deny policy (implemented for write tools: `--write-policy`);
- configurable approval gates (implemented: `runstead decide` operator
  approvals, default `approval_required`);
- file hashes and before/after evidence (implemented: sha256 preconditions,
  `WriteEvidence`);
- Git status and diff verification (implemented for read observations; the
  independent verifier milestone #11 is implemented: real git status/diff
  observation with pre-existing vs during-task attribution);
- protection against repeated writes and path traversal (implemented:
  stale-state preconditions plus the repeat guard);
- explicit Docker workspace-mount policy for write-capable runs (pending #15).

Exit criteria:

- Runstead can make a scoped code change without touching files outside the selected workspace;
- every modification is represented by evidence and traceable to a validated action;
- denied actions fail closed;
- Docker-based runs do not receive broader host access than the selected workspace.

## Milestone 4 — Verifiable coding agent

**Goal:** complete real inspect/edit/test/fix cycles and reject unsupported completion claims.

Deliverables:

- test execution with timeout and captured exit status (implemented in #26:
  operator-declared recipes with fixed argv, capability policy, bounded
  output, process-tree termination and structured process evidence);
- explicit acceptance checks (implemented in #11: the operator acceptance
  plan, typed checks, and the independent completion verifier; the
  inspect/edit/test/fix loop is #12);
- completion verifier (implemented in #11: runtime-decided completion with a
  persisted verification report and a state-layer completion gate);
- loop and repetition detection (implemented in #7/#9: bounded steps,
  corrections, repeats, provider attempts, time; #12 adds the consecutive
  tool/process failure and repeated verification failure guards);
- malformed-action correction policy (implemented in #5/#7);
- failure classification and retry limits (implemented in #12: recoverable
  recipe/verification/write failures continue the loop; terminal and
  human-review conditions stop or pause with typed outcomes; no hidden retry
  loop);
- final evidence report (implemented in #11/#12: verified summary from the
  acceptance checks, persisted verification report, process evidence,
  write hashes and the real Git observation with change attribution rendered
  by `runstead inspect`).

Exit criteria:

- a real repository task is completed through multiple tool cycles
  (implemented in #12: the deterministic scenario in
  `fixtures/coding-loop/` runs the full inspect -> edit -> failing test ->
  diagnose -> corrective edit -> passing rerun -> verifier -> completed
  trajectory with a real `go test` recipe and real Git observation);
- at least one test failure is diagnosed and corrected (implemented in #12);
- the model cannot mark the task complete when acceptance checks fail
  (implemented in #11/#12).

The live ChatGPT Web scenario of #12 remains blocked by #29 (producer-side
receipts) -> #30 (protected live activation) -> #4 (OmniRoute live provider);
the deterministic offline core is implemented and the live gate fails closed
(see [coding-loop.md](coding-loop.md)).

> **Historical milestones.** Milestones 0 through 4 above were planned under
> the original ChatGPT Web/OmniRoute bootstrap strategy; Milestone 5 below was
> reframed for configured compatible endpoints. The provider strategy
> was rebased on compatibility protocol families (#86/#79): these milestones
> remain as
> historical planning provenance, and the deterministic runtime capabilities
> they describe stay valid, but the OmniRoute/ChatGPT Web transport path is no
> longer the v0.1 baseline. Active provider work proceeds through Milestone P0
> (#79) and the family adapters #87/#88/#89; ChatGPT Web/OmniRoute is deferred
> to future plugin/composable-provider tracks.

## Milestone 5 — Runtime hardening on configured providers

**Goal:** demonstrate that the runtime remains dependable under expected failures before adding more transport complexity.

Deliverables:

- chaos and interruption test suite;
- stale-session recovery;
- empty, truncated and malformed-response fixtures;
- stuck-command termination;
- duplicate-request protection;
- installation and provider configuration documentation;
- Docker development workflow documentation;
- stable CLI behavior for the v0.1 surface;
- end-to-end acceptance scenario against a configured compatible endpoint.

Exit criteria:

- a task with at least 20 meaningful steps survives interruption and one recoverable upstream failure;
- all side effects are accounted for;
- final output includes actual diff, test evidence and task history;
- known limitations are documented;
- the runtime baseline is stable enough that later transport failures can be isolated from agent-runtime failures.

## Deferred to plugin/composable-provider tracks

- additional web providers such as Qwen or GLM;
- native tool-calling optimization;
- MCP;
- multi-agent orchestration;
- automatic model routing;
- graphical interfaces;
- distributed workers;
- semantic or vector memory;
- unattended long-running autonomy;
- context compaction for long conversations (the deterministic
  evidence-preserving context reconstruction foundation required by it now
  exists as issue #51 — see `docs/architecture.md`; compaction itself stays
  deferred and, when introduced, must preserve tool-call/result pairs, never
  discard evidence silently, treat model summaries as non-authoritative, and
  keep required evidence identifiers verifiable);
- a general-purpose OpenAI-compatible gateway;
- the first-party ChatGPT Web connector and any OmniRoute-specific live path
  (historical milestones 6 and 7 document that research; see also
  [`research/`](research/) for preserved provenance).

## Historical record — ChatGPT Web / OmniRoute milestones (superseded)

The following two milestones were part of the original OmniRoute bootstrap
strategy. They are superseded by the #86 protocol-family decision and are kept
here as historical planning evidence only.

**Milestone 6 (historical) — First-party ChatGPT Web connector.** A narrow
direct ChatGPT Web provider adapter: credential-import flow without account
passwords, secure local credential storage, session/token lifecycle,
browser-compatible transport, request construction with SSE decoding,
sanitized transport traces, precise failure classes and integration tests.
Exit criteria required running the shared read-only protocol suite, keeping
credentials out of repositories/images/databases/logs and replacing failed
sessions without losing local state. Related preserved research:
[`research/first-party-chatgpt-web-spike.md`](research/first-party-chatgpt-web-spike.md),
[`research/first-party-chatgpt-web-standalone-spike.md`](research/first-party-chatgpt-web-standalone-spike.md).

**Milestone 7 (historical) — Provider bake-off and migration decision.**
Compare OmniRoute and the first-party connector under equivalent conditions
and choose a default based on repeated measurements, with a written
architecture decision record and a migration plan only on material advantage.
This bake-off is no longer scheduled: any future ChatGPT Web or OmniRoute use
arrives as an ordinary endpoint behind one of the supported protocol families
or as plugin work, never as a privileged core adapter.

## Milestone governance

Each GitHub milestone should have:

- one capability-focused objective;
- explicit exit criteria;
- only issues necessary to reach that capability;
- no speculative features;
- a closing review documenting evidence and unresolved limitations.

An issue should move to a later milestone rather than weakening the current milestone's exit criteria.
