# Maintainer Addendum — JCode Reverse Engineering Report

**Date:** 2026-08-06  
**Status:** Normative maintainer correction  
**Applies to:** `docs/research/jcode-reverse-engineering.md` in PR #34

This addendum records the maintainer decisions made during review of the JCode reverse-engineering report. Where this addendum conflicts with the original report, **this addendum is authoritative**.

The original report remains useful as a detailed static-analysis research artifact. This document narrows its recommendations so they remain consistent with Runstead's current architecture, milestone gates and ownership boundaries.

## 1. Governor and local-effect authorization are separate boundaries

The original report sometimes describes the account governor as the only authorization boundary. That wording is incorrect.

The Runstead governor has a deliberately narrow role:

- serialize ChatGPT Web attempts for one account;
- admit, delay or reject upstream attempts;
- account for every authoritative upstream attempt;
- enforce pacing, rolling budgets, task budgets and circuit state;
- surface sanitized account-protection telemetry.

It does **not** authorize local effects.

Local effects remain controlled by distinct runtime boundaries:

- `runstead.protocol.v1` parsing and schema validation;
- tool registration and typed argument validation;
- workspace and path policy;
- explicit permission and safety policy;
- executor-controlled side effects;
- verifier-controlled acceptance of observable results;
- persisted evidence and checkpoints in later milestones.

Normative wording:

> The governor is the sole admission and accounting boundary for protected upstream account attempts. Local effects are separately controlled by protocol, policy, tools, executor and verifier boundaries.

Any statement in the original report calling the governor the "only authorization boundary" must be read according to this correction.

## 2. Current execution order takes priority over the proposed RS backlog

The report's RS-01 through RS-08 backlog is research-derived and must **not** replace the repository's existing milestone dependencies.

The current critical path is:

```text
#29 authoritative OmniRoute upstream-attempt receipts
    ↓
#30 consume receipts and enable protected live execution
    ↓
complete #4 OmniRoute baseline provider adapter
    ↓
#7 bounded read-only agent loop
```

The JCode-derived backlog is therefore advisory only. No RS item should be created or scheduled ahead of this chain unless it independently fixes a demonstrated defect in the current milestone.

## 3. Epoch-based cancellation is conditional, not the next standalone issue

JCode's `InterruptSignal` combines an atomic flag, `tokio::Notify` and a monotonically increasing epoch because JCode reuses and resets interrupt signals inside a long-lived, multi-session server. The epoch prevents a delayed reset from erasing a newer interrupt.

Runstead currently uses one-shot cancellation through Go's `context.Context` and `signal.NotifyContext`. A canceled context is not reset and reused. Therefore the JCode epoch mechanism solves a condition that Runstead does not currently have.

Maintainer decision:

- cancellation propagation and race tests belong inside #7;
- the loop, governor executor and tools must consistently honor `context.Context`;
- tests must cover cancellation while queued, before an upstream attempt, during provider I/O and during tool execution;
- a reusable epoch/reset signal must be introduced only if a concrete design later requires resetting and reusing the same signal;
- do not create RS-02 as the next independent implementation issue.

The report may still retain the JCode mechanism as a useful pattern for future comparison, but its adoption status is **conditional/deferred**, not "adopt now".

## 4. Evidence terminology for tests

No JCode Rust tests were executed during the research because the analysis environment did not contain a Rust toolchain.

The following evidence labels apply:

- `C` — confirmed by static inspection of implementation code;
- `CT` — test coverage found in source, but not executed;
- `TE` — test executed successfully in the analyzed environment;
- `D` — declared in documentation only;
- `I` — architectural inference;
- `NV` — not verified.

Every `T` marker in the original report must be interpreted as `CT`, not as proof that the test compiles or passes. Only the Runstead `go test ./...` result qualifies as executed test evidence in this report.

## 5. SQLite durability recommendations must remain implementation-neutral

JCode's JSON snapshot and JSONL journal provide useful failure scenarios, but their repair mechanisms must not be copied directly into Runstead's future SQLite store.

The following are valid cross-implementation invariants:

- a committed event must not disappear during replay;
- replay must not duplicate a completed effect;
- an incomplete transaction must not appear committed;
- uncertain effects must remain explicitly uncertain;
- corruption or unreadable state must produce a typed diagnostic;
- recovery must never fabricate successful completion;
- derived task state must remain reconstructible from authoritative records;
- checkpointing must not silently discard unresolved evidence.

The following are **not yet accepted Runstead design decisions**:

- line-level salvage;
- glued-JSON recovery;
- checkpoint-after-corrupt-line behavior;
- forensic copies as a mandatory storage mechanism;
- assumptions about corrupt SQLite pages;
- a fast-versus-durable write API copied from JCode;
- a particular WAL, fsync or backup policy.

Those mechanics must be decided with the actual Go SQLite driver, transaction model, migrations, backup policy and failure tests used by M2.

## 6. Local-effect safety language

The report correctly rejects JCode's model-justification unlock for destructive commands. The accepted Runstead direction is stricter:

- risk classification may provide deterministic metadata;
- classification is not authorization;
- unknown or unparseable shell behavior escalates or fails closed;
- catastrophic actions remain denied;
- approval, where later supported, is external and human-controlled;
- no model-authored justification may unlock an effect;
- shell remains out of scope until a separately reviewed policy boundary exists.

A blast-radius classifier should not be prototyped merely because the JCode implementation exists. It should be introduced only when the Runstead shell-policy milestone needs it.

## 7. Correct interpretation of repository mutations

The original report says that no PR or commit was created. That statement describes the research agent's claimed analysis process, but it is inaccurate as metadata for the checked-in artifact.

The authoritative statement is:

> PR #34 introduces documentation only. It makes no functional Runstead changes and copies no JCode code, assets, models or binaries.

## 8. Accepted findings from the original report

The following findings remain accepted:

1. JCode should be treated as a source of selected concepts and test designs, not as a target architecture for Runstead.
2. Runstead must remain a Go modular monolith with a narrow provider seam.
3. Native provider tool calling must not replace `runstead.protocol.v1`.
4. Provider sessions remain disposable metadata; local state is authoritative.
5. JCode's TUI, desktop, swarm, ambient execution, vector memory, public SDK and generic provider routing remain outside current Runstead scope.
6. Explicit truncation markers, typed observations, bounded loops, schema fixtures and failure-corpus tests are useful patterns.
7. JCode's command-risk implementation is defense in depth, not a sandbox.
8. A model's final response is never evidence by itself.
9. Any closely ported MIT-licensed algorithm requires provenance and license compliance.
10. Performance claims not reproduced by the analysis remain claims, not verified measurements.

## 9. Revised recommendation order

### Current priority

1. Complete #29.
2. Complete #30.
3. Close the remaining protected-live requirements in #4.
4. Implement #7 with deterministic fake provider/clock tests, grounded observations, bounded corrections, repeat detection and `context.Context` cancellation.

### Integrate into existing milestones rather than create speculative issues

- cancellation propagation and race tests → #7;
- typed tool lifecycle and bounded correction budgets → #7;
- durable replay and uncertainty invariants → M2 storage design;
- schema golden tests → protocol and future event-schema work;
- blast-radius classification → future, separately reviewed shell-policy work;
- compaction pair preservation → future context-reconstruction milestone.

### Continue to reject for the current product direction

- multi-client daemon architecture;
- TUI or desktop product surfaces;
- vector-memory runtime;
- swarm and unattended ambient execution;
- broad provider/model routing;
- public harness API or SDK;
- model-unlockable effect gates;
- native tool calling as the authoritative action contract.

## 10. Final maintainer decision

The JCode report is accepted as a **research reference**, subject to this addendum.

It must not be used as an implementation roadmap without checking:

- the current Runstead issue dependency chain;
- the architecture and account-protection documents;
- whether the cited JCode behavior was statically inspected, covered by an unexecuted test or dynamically verified;
- whether a recommendation solves a demonstrated Runstead problem rather than importing JCode-specific complexity.

The governing conclusion is:

> Borrow JCode's useful failure cases, invariants and test ideas only where they strengthen an existing Runstead milestone. Do not import JCode's server model, provider breadth, tool-authority assumptions or speculative abstractions.