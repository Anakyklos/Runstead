# Empryo architectural mining for Runstead

**Date:** 2026-09-28  
**Status:** Research reference; non-normative  
**Doctrine:** issue #49 — mine the gold, preserve Runstead

This note records ideas observed in Empryo that may be worth testing in
Runstead. It is not an implementation plan and does not change the roadmap.

## External provenance

Source project: Empryo (https://empryo.com/) and its public repository
(https://github.com/proxysoul/Empryo).

Observed direction: reduce fragile language-model work by moving repository
understanding, editing, validation and repeated mechanical operations into
structured local tooling.

Runstead problem addressed: Runstead already owns durability, policy, effects,
evidence, recovery and verification. The opportunity is to make repository
understanding and modification more precise and efficient without changing
those authority boundaries.

No Empryo source code is copied by this research note.

## Maintainer conclusion

The strongest transferable principle is:

> A coding agent can improve by making the model responsible for fewer
> operations that deterministic tools can perform more reliably.

Empryo is therefore a useful source of mechanisms and experiments, not a
target architecture.

## Current Runstead status (2026-10-07)

This is historical provenance research, not an implementation plan, roadmap,
or proposal to reopen completed milestones. M8–M11 are complete on `main`.
The current implementation and its limits are described by the canonical
architecture, persistence, composition, improvement, and roadmap documents.

**Implemented in Runstead:**

- M8 provider compatibility hardening and the configured endpoint contract;
- M9 durable Work Units, serial execution by default, and opt-in bounded
  shared execution for explicitly read-only units (maximum concurrency 4;
  parallel writers remain disabled);
- M10 static built-in capability metadata, strict operator Profiles,
  deterministic composition, and a persisted frozen execution contract;
- M11 non-authoritative, evidence-backed Improvement Proposals with explicit
  operator review, versioned apply, later validation, and deterministic
  rollback;
- deterministic, bounded reconstruction of model context from durable task
  state and evidence, with provenance, authority separation, and fail-closed
  budget handling.

**Partially available:**

- Existing recipes group local execution, and Work Units organize durable
  subtasks, but Runstead has no general compound-tool framework. Composition
  selects existing built-in surfaces; it does not load executable packages.
- M11 records operator-attested outcome classifications against durable
  evidence. It is not a statistical benchmark framework and does not
  automatically change runtime behavior.

**Still hypotheses or future experiments:**

- long-conversation context compaction is deferred; it is distinct from the
  implemented recovery-context reconstruction described above;
- repository intelligence indexing, semantic editing, an optional semantic
  reviewer, and provenance-aware retrieval are not established Runstead
  capabilities;
- additional role/model routing and broader orchestration have no promoted
  milestone. Any future experiment needs a concrete Runstead problem, a
  baseline, and its own evidence and maintainer decision;
- Empryo's benchmark and performance claims remain external hypotheses and
  must be reproduced on Runstead workloads before they can support adoption.

The authoritative details are in [`architecture.md`](../architecture.md),
[`persistence.md`](../persistence.md), [`composition.md`](../composition.md),
[`improvements.md`](../improvements.md), and [`roadmap.md`](../roadmap.md).
SQLite, governor, policy, recovery, and verifier remain authoritative;
indexes, summaries, reviewers, and memory derived from them do not.

Implementation and test entry points: [M8 compatibility contract](../../internal/provider/compat/compat.go)
and [matrix tests](../../internal/provider/compat/matrix_test.go); [M9 Work Unit driver](../../internal/workunit/driver.go),
[scheduler](../../internal/workunit/scheduler.go), and [governed concurrency E2E](../../cmd/runstead/workunit_m9_evidence_e2e_test.go);
[M10 resolver](../../internal/composition/resolve.go), [composition tests](../../internal/composition/composition_test.go),
and [CLI E2E](../../cmd/runstead/composition_e2e_test.go); [M11 proposal contract](../../internal/improvement/contract.go)
and [lifecycle E2E](../../cmd/runstead/improvement_e2e_test.go); [context compiler](../../internal/context/compiler.go),
[recovery adapter](../../internal/recovery/context.go), and [authority tests](../../internal/context/authority_test.go).

## Candidate mechanisms to evaluate

### 1. Derived repository intelligence map

Evaluate a rebuildable repository index containing selected structural facts:
files, symbols, references, dependency edges, test relationships and measured
Git history signals.

Runstead constraints:

- the index is derived data, never authoritative task state;
- every index has a repository/revision identity;
- stale data must be visible;
- predictions about impacted files are hints, not completion evidence;
- normal Runstead tools, policy and verifier remain authoritative.

Evidence gate: compare needed-file recall, tool calls, model turns, token use,
wall time and end-to-end task success against the current baseline.

### 2. Structured editing before raw text editing

Evaluate semantic edit tools backed by language tooling where available, for
operations such as symbol rename, declaration move, import updates and
AST-constrained changes.

Runstead constraints:

- semantic tools extend the existing write path rather than replacing it;
- each materialized file change still records before/after evidence;
- ambiguous or unsupported targets fail explicitly;
- tests and the independent verifier still decide acceptance;
- raw patching remains available when semantic tooling is not suitable.

Evidence gate: compare structured editing with current patch editing on real
refactors and migrations, measuring retries, malformed edits, cost and final
correctness.

### 3. Compound deterministic tools

Runstead recipes and Work Units already provide bounded ways to group
deterministic local work. M10 composition selects existing built-in surfaces;
it does not add a general compound-tool framework or load executable
capability packages. A broader typed compound operation remains an experiment.

A compound tool is acceptable only when its internal work remains bounded,
policy-visible, cancelable, auditable and decomposable into durable evidence.

The goal is fewer model/provider round trips, not hidden execution.

Evidence gate: lower provider-turn count with equal or better correctness and
no loss of effect provenance or recovery behavior.

### 4. Deterministic checks before model review

Runstead already has the stronger trust rule: a model completion claim is not
evidence.

Preserve that rule and, if a separate semantic reviewer is added later, order
the pipeline as:

1. deterministic acceptance checks;
2. environment evidence and verifier;
3. optional model review only for questions not already settled by those
   checks.

A reviewer may provide critique. It may not override deterministic failure or
become completion authority.

### 5. Isolated reviewer context and bounded repair

A future review pattern may use an isolated reviewer that receives the task
contract plus raw evidence, returns a typed defect packet, and triggers a
small bounded number of repair Work Units.

The durable Runstead state machine owns the loop. Every repair remains a
separate attempt with cause, scope, budget, evidence and a terminal condition.

Reject open-ended self-critique loops and repeated retries without new state.

### 6. Provenance-aware project memory

Runstead already has the more important primitive: durable local task history.

If project memory is introduced, build it as a derived retrieval view over
inspectable sources such as accepted architectural decisions, verified prior
failures, build/test conventions and operator-approved notes.

Every recalled item should retain source, scope and freshness information.
Semantic retrieval may index durable records later, but must not replace them
as the source of truth.

### 7. Benchmark-driven feature admission

This remains a useful research practice observed in Empryo. M11's implemented
ImprovementProposal lifecycle supplies a non-authoritative, evidence-linked
operator workflow, but it is not a statistical benchmark harness or an
automatic feature-admission mechanism.

Runstead should benchmark agent mechanisms, not only models/providers. Each new
mechanism should start with a falsifiable question and a baseline. Examples:

- does repository-map retrieval improve needed-file recall?
- does semantic editing reduce corrective turns?
- does a compound tool reduce model calls without hiding effects?
- does a separate reviewer catch defects that deterministic checks miss?
- does project memory improve completion without introducing stale
  assumptions?

Record negative results. Do not ship a mechanism merely because one headline
metric improved if end-to-end work regressed.

When experiment evidence exists, the existing improvement-proposal model can
carry it through an explicit operator decision:

experiment evidence -> non-authoritative proposal -> operator decision ->
versioned change -> later validation.

### 8. Context reconstruction and compaction

Deterministic, bounded recovery-context reconstruction from durable task state
and evidence is already implemented (issue #51). It preserves authoritative
facts and provenance, separates non-authoritative notes, and fails closed when
mandatory context exceeds its budget. See the current architecture and
persistence documentation for its contract.

Long-conversation compaction is a separate, deferred capability. Any future
compaction experiment must preserve tool/action-result relationships and make
loss explicit; summaries cannot replace SQLite records, pending approvals,
unresolved failures, or current workspace evidence. Empryo's compression
claims do not establish a Runstead benefit.

### 9. Model roles and multi-agent routing

Treat separate scout/worker/reviewer roles and model routing as unpromoted
future optimization mechanisms, not as delivered Runstead capabilities or
product identity.

Runstead already has durable Work Units and an opt-in bounded shared/exclusive
scheduler for explicitly read-only units. This is M9 implementation, not a
general multi-agent runtime. Additional role/model routing would require
measured benefit in completed work per cost/provider attempt.

Do not assume that additional agents or parallel exploration are improvements
by default.

### 10. Generated declarative interfaces

Empryo's generated interface/workflow surfaces are interesting but are not a
current Runstead requirement.

If a future operator UI is needed, the Runstead-compatible form is a
schema-validated presentation/control surface over existing operator APIs. It
must not become a second execution or policy system.

## Explicit non-goals

Do not import the following merely because they exist in Empryo:

- product branding as architectural vocabulary;
- first-party benchmark multipliers as universal claims;
- private implementation details as architectural proof;
- opaque memory as authoritative truth;
- multi-agent orchestration without measured benefit;
- model review as a substitute for environment evidence;
- generated capability changes that bypass Runstead's normal operator,
  policy, evidence and versioning boundaries.

## Evidence quality

Empryo is more useful than a typical agent project because it publishes a
substantial public code lineage, a benchmark harness and negative experiments.
That makes it a credible source of hypotheses.

Important limits remain:

- current Empryo v3 is not fully public;
- performance measurements are first-party;
- published benchmarks do not cover enough languages, repositories and task
  classes to prove a general ranking;
- documentation and product claims can be broader than the measurements.

Therefore Runstead should reproduce relevant benefits on its own workloads
before adopting a mechanism.

## Candidate research sequence

This sequence is advisory only and does not override active issue dependencies.

1. establish a benchmark harness for agent mechanisms;
2. test compound read/query operations;
3. test a derived repository intelligence index;
4. test one semantic-editing path on one language/tooling stack;
5. test an optional isolated semantic reviewer;
6. test provenance-aware project memory;
7. consider role/model routing only after the earlier measurements exist.

The ordering intentionally starts with measurement and low-authority
read/query improvements before introducing more complex editing or
orchestration behavior.

## What Runstead already does better

Any borrowed mechanism must preserve the parts of Runstead that are already
stronger than the external design:

- provider-neutral runtime contract;
- local durable state independent of provider sessions;
- frozen execution contracts;
- governed provider attempts;
- typed effect provenance;
- operator-owned approvals;
- independent completion verification;
- durable recovery;
- non-authoritative improvement proposals;
- bounded Work Unit concurrency;
- issue #49's architectural borrowing doctrine.

An Empryo-inspired feature that weakens one of these properties is a
regression even if it improves a benchmark.

## Recorded decision

Mine Empryo for mechanisms that reduce fragile model work: structured
repository understanding, semantic editing, compound deterministic tools,
bounded review/repair, provenance-aware memory and empirical feature
evaluation.

Do not import Empryo wholesale. Do not treat its benchmark numbers as
Runstead evidence. Do not let derived maps, summaries or reviewers replace
durable Runstead truth.

The first implementation derived from this note should be a narrow experiment
or issue with its own baseline and evidence gate, not a broad "implement
Empryo" epic.
