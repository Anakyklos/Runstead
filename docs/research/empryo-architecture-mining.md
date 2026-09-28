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

Evaluate typed compound operations that group several predictable local steps
behind one model decision.

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

This is one of the most useful practices observed in Empryo.

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

This fits Runstead's existing improvement-proposal model:

experiment evidence -> non-authoritative proposal -> operator decision ->
versioned change -> later validation.

### 8. Context reconstruction and compaction

Borrow the problem framing, not advertised compression ratios.

Prefer deterministic context reconstruction from frozen task contracts,
durable Work Units, accepted effects, unresolved failures, relevant
observations and current workspace evidence.

Summaries may be useful but must not replace durable records. Evaluate loss of
constraints, pending approvals, unresolved failures and repository freshness.

### 9. Model roles and multi-agent routing

Treat separate scout/worker/reviewer roles as later optimization mechanisms,
not product identity.

Runstead already has Work Units and bounded concurrency. Add role/model routing
only when measured task classes show that it improves completed work per
cost/provider attempt.

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
