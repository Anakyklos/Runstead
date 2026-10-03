# Basinfy-derived repository intelligence — research note

**Status:** Research / non-normative  
**Date:** 2026-10-03  
**Source:** Basinfy / BasinMind — https://basinfy.com/  
**Observed package surface:** Basinfy 1.1.0 documentation as inspected on 2026-10-03  
**Governance:** issue #49, "mine the gold, preserve Runstead"

## Purpose

Record the repository-intelligence mechanisms worth mining from Basinfy while preserving Runstead's trust kernel, roadmap gates and evidence model.

Basinfy is useful here because it treats a repository as structured dependency data rather than a bag of text. The strongest Runstead opportunity is not to copy Basinfy or make it a required dependency; it is to make **repository understanding a deterministic, freshness-aware preflight feeding the existing Runstead action/policy/evidence/verifier chain.**

This document is research provenance only. It does not start a new milestone, add a dependency, authorize implementation or change the active adoption/cutover gates.

## External provenance

**Source project/paper:** Basinfy / BasinMind, https://basinfy.com/

**Observed concept:** repository import/structure index, L0-L3 briefings, exact symbol anchors and content hashes, dependency path/orbit queries, blast-radius analysis, pre-edit/pre-create checks, freshness/incremental reindex, historical commit-scoped briefings and scoped lessons.

**Runstead problem addressed:** coding tasks currently benefit from authoritative task/evidence state, but repository understanding can still require broad reads/searches and may not deterministically expose dependency impact, stale edit anchors or likely affected verification targets before an effect.

**What we adopt:** only the smallest Runstead-native mechanisms described below, gated by measurement.

**What we explicitly reject:** Basinfy's autonomous harness/agent loop, mandatory Basinfy runtime/dependency, model-controlled memory promotion, architectural warnings as execution authority, mandatory embeddings/vector infrastructure, background watcher requirements and any bypass of Runstead policy/governor/effect/evidence/verifier boundaries.

**Why each rejection is a Runstead constraint rather than preference:** Runstead's established invariants require runtime-owned authority, bounded effects, durable state, explicit policy/approval, independently observed evidence, fail-closed recovery and a non-pluggable trust kernel. A foreign indexer may inform a decision but cannot redefine those boundaries.

**Runstead invariants preserved/strengthened:** bounded context, deterministic reconstruction, stale-state detection, edit precision, recoverability, auditability, targeted verification, provider/model disposability and evidence-backed completion.

**Evidence/exit gate:** no mechanism is promoted until representative repository tasks demonstrate measurable context/precision benefit without false assurance or authority leakage.

## What Basinfy demonstrates

The documented Basinfy surface includes:

- repository parsing and import/dependency graph construction;
- token-budgeted L0-L3 context-pyramid briefings;
- `locate` returning exact file/line anchors plus a content hash;
- `path` and `orbit` for structural navigation;
- `impact` / blast-radius traversal;
- `prepare-edit` combining location, advisory architecture checks and impact;
- `prepare-create` checking placement/import health before a new file;
- incremental `update` and an explicit freshness resource;
- briefing replay at a Git commit;
- scoped lessons injected only for relevant repository regions;
- lexical and optional semantic retrieval.

The Runstead value is the **shape of the preflight and evidence**, not Basinfy's internal "basin" model.

## Candidate Runstead-native mechanisms

### 1. Repository Intelligence Layer

Future M8-compatible context compilation may consume a local structural index that can represent, where evidence supports it:

- files/packages/modules;
- declared symbols;
- imports/dependencies;
- caller/callee relationships when reliable;
- tests linked by package, reference or measured heuristics;
- repository baseline/commit identity.

The index is derived data. Git/workspace contents remain authoritative.

A missing, corrupt or stale index must degrade explicitly to ordinary Runstead inspection or block a feature that specifically requires structural proof. It must never manufacture repository truth.

### 2. `PrepareEdit` preflight

The highest-value Basinfy mechanism to translate.

Conceptually:

```text
requested target
    ↓
exact symbol/path resolution
    + content hash / anchor
    + current repository/worktree identity
    + structural neighbors/dependencies
    + impact estimate
    + candidate relevant tests
    + applicable repository rules
    ↓
EditContext
    ↓
existing Runstead proposal → policy → controlled effect chain
```

The preflight is advisory context plus stale-state protection.

It does **not** make an edit allowed. Policy/approval still decides whether the effect may occur.

### 3. Exact anchors with stale-content detection

A model should be able to receive a bounded edit target such as:

```text
path: internal/provider/foo.go
symbol: ResolveProvider
lines: 183-241
content_hash: ...
repository_snapshot: ...
```

Before applying an edit, Runstead can verify that the anchor still resolves against the expected content identity.

If stale:

```text
anchor mismatch
    → do not blindly apply historical edit
    → re-resolve / recompile context
    → continue only under current evidence
```

This complements durable interruption/resume and future concurrent Work Unit behavior.

### 4. Blast radius as verification input

A structural impact query can identify candidate downstream dependencies and tests.

Use:

```text
changed symbol/file
    ↓
direct dependents
    ↓
bounded transitive dependents
    ↓
packages / flows / candidate tests
    ↓
ImpactReport
```

The report may help select or prioritize verification recipes.

It **cannot prove absence of impact** unless the underlying index/analysis has an explicit assurance level. It must never let a model or index silently skip acceptance checks already required by the task.

The independent verifier remains authoritative.

### 5. Context pyramid inside M8

M8 already owns authoritative context and durable working state. If measured useful, repository context can be delivered progressively:

- **L0:** task objective, acceptance and repository baseline;
- **L1:** exact relevant symbols/files;
- **L2:** dependencies, impact, repository evidence/history;
- **L3:** broader architectural context only on demand.

This is a context-compiler policy, not a second agent memory.

### 6. Deterministic context budget

A future repository context compiler should make budget allocation inspectable instead of asking a model to "be concise."

Example shape:

```text
task + acceptance        fixed/reserved
exact edit targets       bounded
dependency context       bounded
evidence/history         bounded
broader source context   remainder
reserve/error metadata   fixed
```

Truncation and omission should be observable in the compiled context metadata.

### 7. Freshness tied to durable task state

Persist enough non-secret identity to detect stale repository intelligence across interruption/resume, for example:

- repository baseline/commit;
- working-tree identity where practical;
- index schema/version;
- compiler/index generation;
- relevant anchor hashes;
- structural-index freshness marker.

On resume:

```text
persisted task snapshot
    vs
current workspace/index
    ↓
compatible → reconstruct
drifted    → explicit reconcile/recompile
```

Never silently resume against stale anchors.

### 8. Incremental reindex after observed effects

Runstead does not need a mandatory filesystem watcher.

When Runstead itself observes a successful controlled repository write:

```text
persisted intent
    ↓
controlled effect
    ↓
observed filesystem result
    ↓
refresh affected structural region
    ↓
new derived index generation
    ↓
verification
```

Index refresh follows observed effects. It does not become an ungoverned daemon authority.

### 9. `PrepareCreate` advisory placement check

Before creating a new file, a future capability may inspect:

- intended path/package;
- planned imports/dependencies;
- analogous neighboring files;
- obvious cycle/layer risks;
- repository conventions already represented by evidence.

Warnings are advisory. They cannot veto or authorize effects independently of Runstead policy/acceptance.

### 10. Dependency path/orbit explanation

A structural path query is useful both to the model and operator:

- why is file A relevant to component B?
- how does a proposed change reach package C?
- which dependency chain caused a blast-radius warning?

The output should include explicit graph edges/source locations where possible rather than narrative-only explanation.

### 11. Historical repository context

A context compiler may eventually reconstruct repository intelligence at a declared commit/baseline.

Uses:

- reproduce an older task;
- explain a regression;
- compare context before/after a PR;
- reconstruct what a resumed/historical task could legitimately have seen.

This does not replace the task's durable evidence/history.

### 12. Scoped lessons under M11

Basinfy's region-scoped lessons combine well with M11 only after translation into Runstead's approved improvement lifecycle.

Runstead shape:

```text
repeated task evidence
    ↓
ImprovementProposal
    ↓
repository/module scope
    ↓
review / approval
    ↓
versioned configuration/capability rule
    ↓
validated on later tasks
```

A model, retrieval score or positive feedback signal cannot self-promote the rule.

### 13. Retrieval-quality telemetry as evidence

Useful future measurements:

- files/symbols retrieved;
- files/symbols actually edited;
- files later proven relevant;
- affected tests predicted vs actually required;
- false-positive context;
- missed dependencies;
- tokens delivered to the model;
- stale-anchor catches;
- recompile/reconcile events.

This corpus may justify later ranking changes. Runtime weights/policy must not mutate automatically from model feedback.

## Relationship to existing milestones

### M8 — Authoritative context and durable working state

Primary home for:

- repository context compilation;
- hierarchical context delivery;
- freshness/reconstruction;
- exact repository anchors;
- bounded context budgets.

Do not widen M8 merely to reach Basinfy feature parity. Promote only mechanisms required by actual Runstead dogfood evidence.

### M9 — Durable Work Units

Potential beneficiary of:

- stable/stale anchor checks;
- explicit repository snapshot identity;
- structural read-only context for scoped Work Units;
- impact information that helps reconcile independent units.

Repository intelligence must not authorize parallel writers or override the existing shared/exclusive scheduler.

### M11 — Evidence-backed harness improvement proposals

Primary home for:

- scoped lessons;
- retrieval-quality feedback;
- evidence-backed proposals to adjust context/repository capabilities.

No self-modifying retrieval/policy loop.

## Priority if evidence justifies promotion

### P0 candidates

1. exact anchor + content-hash contract;
2. `PrepareEdit` preflight;
3. blast-radius / candidate affected-test report;
4. freshness/reconstruction contract integrated with M8/resume.

### P1 candidates

5. repository dependency/path queries;
6. deterministic context-budget allocation;
7. incremental structural reindex after observed effects;
8. `PrepareCreate`;
9. historical commit-scoped context;
10. M11-scoped lessons and retrieval-quality telemetry.

## Explicit non-goals

This research does not authorize:

- replacing normal repository reads/search with a Basinfy-only protocol;
- making Basinfy a required dependency;
- importing Basinfy's "basin" abstraction as Runstead architecture;
- adding a vector DB;
- trusting embeddings/graph edges as authoritative facts without provenance/assurance;
- a resident watcher/daemon solely for indexing;
- an autonomous topology-healing loop;
- model-written architectural memory becoming policy;
- an indexer selecting providers, approvals, retries or execution authority;
- an indexer declaring task completion;
- weakening verifier/acceptance gates because blast-radius analysis predicts low impact;
- reopening active provider/adoption gates to land repository-intelligence work early.

## Evidence gates before promotion

A future experiment should use representative Runstead dogfood repositories/tasks and record at least:

- retrieval precision/recall or another declared relevance metric;
- delivered-context token reduction versus current inspection behavior;
- latency and index-update cost;
- memory/disk overhead;
- exact-anchor stale detection rate/false positives;
- dependency-impact false negatives on controlled fixtures;
- affected-test prediction quality where claimed;
- deterministic reconstruction after interruption/resume;
- behavior under dirty worktrees and branch/commit drift;
- negative tests proving no bypass of policy, governor, effect accounting, evidence or verifier.

A benchmark from Basinfy itself is evidence about Basinfy, not proof of Runstead benefit.

## Maintainer decision

Mine these mechanisms selectively:

> Basinfy repository map → Runstead derived repository intelligence  
> `prepare-edit` → bounded preflight feeding existing effect policy  
> blast radius → verifier input, never completion authority  
> exact anchor/hash → stale-edit protection and recovery evidence  
> scoped lessons → M11 reviewed improvement proposals  
> index freshness → durable context/reconstruction contract

Do not import the surrounding harness or trust model.

The first implementation candidate, when M8/dogfood evidence actually requires it, should be the smallest measurable slice around **exact anchors + pre-edit context + stale detection**, not a general Basinfy clone.
