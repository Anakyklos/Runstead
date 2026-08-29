# Runstead Constitution

Spec Constitution for Runstead: a local agent runtime that turns model access into durable, controlled and verifiable work.

## Core Principles

### I. Local Durable State is Authoritative
SQLite task state, action/attempt records, evidence, approval decisions, acceptance plans and recovery projections are the only task truth. Provider conversations, transcripts and any upstream session are disposable and never become truth.

### II. Model Output is Never Authority
The model proposes; Runstead validates and executes. Nothing a model says can decide policy, approvals, task truth, acceptance or completion. Model summaries, hypotheses, inferences and narrative compression are always non-authoritative and must be visibly marked as such in any compiled context.

### III. Evidence Before Claims
No action is complete without evidence from the environment. A summary cannot transform an unsupported claim into a fact or satisfy an acceptance check. Evidence required by a pending action or acceptance check must never disappear silently.

### IV. Bounded, Deterministic, Fail-Closed Context
Any projection of state presented to a model is bounded by an explicit deterministic budget, sorted by explicit keys, and fails loudly (before provider dispatch) when mandatory content cannot fit. No silent truncation, no guessing, no model-chosen forgetting.

### V. No Hidden Amplification
Retries, fallbacks, rotation and pooling live only inside the governor's accounted boundaries. The runtime never adds hidden requests, re-issues completed effects, or replays history during recovery.

### VI. Minimal Native Architecture
Keep the modular monolith. Add the smallest necessary boundary; prefer typed contracts of `internal/state` and `internal/recovery` over SQL and over new abstractions. No vector store, RAG, persistent model REPL, multi-agent scheduling or ambient execution authority without a documented need.

## Authority Model

Authoritative: original task objective; durable task/lifecycle state; persisted policy/constraints; approvals and decisions; concrete actions and attempts; verified citable evidence; acceptance plan and checks; unresolved failures; uncertain effects; workspace facts accompanied by valid evidence and their recorded signatures.

Non-authoritative: model summaries, hypotheses, inferred plans, narrative compression. These may appear for navigation only, explicitly marked, and can never satisfy verification.

## Fail-Closed Rules

- Budget exhaustion with mandatory content pending fails the resume explicitly before the next provider dispatch.
- Missing evidence for a required check is an explicit state, never an implicit pass.
- Stale workspace facts are classified (current / needs-refresh / unverified), never silently re-presented as fresh.
- Secrets, credentials and unnecessary private content never enter compiled context, diagnostics, traces, fixtures or documentation.

## Quality Gates

- TDD for every behavior change: failing test first, implementation, verification.
- Full local gates before a PR: gofmt clean, `go test ./...`, `go vet ./...`, build, `go test -race ./...`, protocol suite, quality gates (growth/errcheck/live-convention) as prescribed by `main`.
- Integration tests with real SQLite/recovery for any recovery-shaped behavior; mocks are not proof of durable-state behavior.
- PRs must document exact verification evidence and distinguish pre-existing failures from introduced ones.

## Governance

This constitution guides all Spec Kit specs, plans and tasks in this repository. A spec that contradicts an invariant above must be amended, not implemented around. Amendments require documentation of the change, rationale, and re-validation of the affected invariants.

**Version**: 1.0.0 | **Ratified**: 2026-08-29 | **Last Amended**: 2026-08-29