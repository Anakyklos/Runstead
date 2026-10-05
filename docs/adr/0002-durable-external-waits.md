# ADR 0002 — Durable external waits and event-driven resume

**Status:** accepted
**Date:** 2026-10-05
**Implementation status:** not implemented; tracked by #161
**Scheduling:** post-adoption product candidate; no M12+ milestone is promoted
**Consumers:** #161
**Relates to:** #9 (recovery/resume), #51 (context reconstruction), #106/#109
(Work Units), #121 (adoption/cutover), #123 (dogfood)

This record fixes the product and trust-model direction for work that must pause
on an external condition without keeping a provider/model session alive. The
first consumer is GitHub Pull Request review state. It does **not** schedule
implementation, promote M12, add a daemon, or make GitHub part of task truth.

Concepts below are a **design contract for #161** unless the repository later
records them as implemented.

## 1. Decision

Runstead will support a first-class, versioned **durable external wait**
primitive.

A task or Work Unit may suspend on an explicitly registered external condition,
persist that condition locally, terminate the current provider session, and
remain dormant with **zero provider attempts** until deterministic Runstead
control-plane logic accepts an external observation that satisfies the wait.

The conceptual lifecycle is:

```text
normal governed execution
        ↓
external wait registered
        ↓
WAITING_EXTERNAL
        ↓
normalized external observation
        ↓
deterministic validation/classification
   ┌───────────────┼──────────────────┐
   ↓               ↓                  ↓
irrelevant      actionable          terminal
   │               │                  │
keep waiting    READY          finish wait/block
                   │                  │
                   ↓                  └─ no model call when reasoning is unnecessary
             normal governed
             execution/resume
```

The external source wakes work; it does not gain authority over what that work
may do.

## 2. Why this belongs in Runstead

The durable object in Runstead is local work state, not a remote model
conversation. Recovery already assumes provider sessions are disposable and
reconstructs continuation from persisted state and evidence.

External review introduces the same requirement over a longer idle boundary:
there may be minutes, hours or days between "work submitted" and "changes
requested". Keeping a provider conversation alive during that period would:

- consume or reserve resources for no useful work;
- make remote session survival incorrectly important;
- encourage polling with model turns;
- make restart/recovery weaker than the existing local-first contract.

The correct abstraction is therefore **persist → stop → observe externally →
reconstruct → continue**, not "keep the agent alive while it waits".

## 3. Durable authority model

SQLite remains authoritative for the wait lifecycle.

The implementation must persist a small typed/versioned projection equivalent
to:

```text
external_wait
  wait_id
  task_id
  optional work_unit_id
  kind + contract_version
  sanitized target identity
  wake conditions
  terminal conditions
  observer/reviewer policy reference
  status
  last accepted observation identity/version
  timestamps
```

and append-oriented observation/transition evidence sufficient for inspection,
deduplication, recovery and race diagnosis.

Exact names and schema are implementation-owned. The state must not exist only
as free-form metadata, model context or a remote webhook/session record.

External wait state is **not**:

- a provider attempt;
- a retry;
- a human approval;
- an uncertain external effect;
- a substitute for Work Unit dependency state;
- a model-authored source of authority.

The implementation may use a dedicated wait projection and link it to the
owning task/Work Unit rather than expanding the persisted Work Unit status
vocabulary. That choice is preferred if it keeps approval/recovery semantics
unambiguous.

## 4. External observations are untrusted evidence

GitHub reviews, comments, CI output, webhook payloads, repository text and any
future external-source payload are untrusted input.

They may become sanitized, provenance-carrying **observations**. They never
become:

- policy;
- approval;
- capability;
- verifier success;
- permission to broaden workspace/tool scope;
- permission to retry/fallback/rotate provider, model, key or account;
- proof that an external effect succeeded merely because prose says so.

Before any provider dispatch, Runstead code must deterministically validate the
wait identity, observation identity, condition match, freshness and configured
observer/reviewer authority. Failures remain waiting, blocked or invalid
according to the persisted contract; they never fail open.

## 5. Provider and governor behavior

While an external wait is active and unsatisfied:

- no provider conversation is required to survive;
- no provider attempt is admitted;
- no provider budget is reserved;
- no token-bearing polling is allowed;
- no hidden retry loop is created.

When an actionable observation makes work runnable again, continuation goes
through the normal Runstead path: context reconstruction, governor admission,
capability containment, policy/approvals, effects, evidence and independent
verification.

The wait mechanism cannot dispatch a model directly around those boundaries.

## 6. GitHub Pull Request v1 consumer

GitHub PR review state is the first product consumer, not the architecture of
the primitive.

For one explicitly registered PR identity, v1 semantics are:

| Observation | Default Runstead behavior |
| --- | --- |
| PR merged | Satisfy the external-review wait through a deterministic terminal path. Do not call a model merely to acknowledge merge. Overall task/Work Unit completion still requires the existing verifier/state completion gates and is never granted by GitHub. |
| Authorized review = `REQUEST_CHANGES` | Persist sanitized review evidence, revalidate target identity/freshness, then make the owning work eligible for normal governed resume. |
| Review = `APPROVED` | Persist/observe as needed; do not wake a model by default. |
| Ordinary comment | Do not wake a model by default. |
| CI/status change | Do not wake a model unless an explicit future wait contract names that condition. |
| PR closed without merge | Produce the persisted contract's typed terminal/blocked outcome; never reinterpret as success. |
| Unauthorized/unknown reviewer | Record/ignore/fail closed according to policy; never wake effectful execution. |

Reviewer authority must come from explicit operator/integration policy, not
from review prose. The implementation must also re-observe the PR identity
relevant to effectful continuation (including head/base state where required)
so a stale review cannot silently authorize work against a changed target.

PR creation itself is not granted by this ADR. If Runstead later owns PR
creation, that requires an explicitly approved GitHub write capability with
the normal effect/evidence boundary.

## 7. Wake mechanism for a local CLI

The primitive must not require a hosted Runstead control plane.

For v1, prefer an explicit, lightweight operator-started watcher (for example,
`runstead watch`; exact CLI naming belongs to #161) that:

- reads only active persisted external waits;
- performs bounded external observation without a model;
- persists normalized observations and dedupe identities;
- atomically claims/satisfies a wait before scheduling continuation;
- can stop and restart without losing state;
- applies bounded cadence/backoff;
- does not create unbounded goroutines, timers or request loops.

This watcher is control-plane infrastructure, not an autonomous agent.

A future webhook relay may feed the **same** durable normalized-observation
contract. Webhook delivery is notification evidence, not task truth, and
public webhook hosting/tunneling is not required for v1.

## 8. Deduplication, races and recovery

At-least-once external notification is assumed. Exactly-once delivery is not.

The durable contract must therefore make duplicate/out-of-order observations
safe. At minimum #161 must prove:

- restart while waiting preserves the same wait;
- an event that happened while Runstead was down is observed after restart;
- duplicate observation does not duplicate resume/provider dispatch;
- two watcher processes cannot both claim the same satisfied wait for
  continuation;
- PR state changing between observation and resume is revalidated fail-closed;
- a merged PR observed after restart can satisfy the external-review wait
  without a provider call, while any overall task/Work Unit completion still
  passes the existing verifier/state completion gates;
- canceled/invalid waits cannot later wake execution;
- unsupported/corrupt wait versions fail closed;
- external API failures do not mutate task truth into success.

Notification identity is a dedupe/reconciliation input, not an authority token.

## 9. Inspection and evidence

The operator-facing inspect surface must make an external wait explainable
without model prose or secrets.

It should expose sanitized facts equivalent to:

- wait ID/kind/version;
- owning task/Work Unit;
- sanitized external target identity;
- pending wake/terminal condition;
- last accepted observation identity and timestamp;
- accepted/rejected classification and reason;
- whether continuation was scheduled/claimed;
- final wait outcome.

Raw credentials, Authorization headers and private provider material remain
outside SQLite and evidence. External review text retained for continuation
must be bounded/redacted under the same untrusted-data discipline used by
other model-facing observations.

## 10. Relationship to adoption and roadmap

This decision is **accepted but unscheduled**.

It is not a prerequisite for:

- Gate A under #121;
- #123 shadow/Runstead-primary dogfood;
- #124 Debian-family release packaging;
- the initial JCode → Runstead cutover.

The current adoption sequence remains authoritative. #161 may be implemented
after that path is accepted, or earlier only through a separate explicit
maintainer reprioritization based on concrete product evidence.

This ADR does not create M12. M12+ remains unpromoted until the repository's
milestone governance explicitly promotes a capability gate.

## 11. Rejected alternatives

### Keep the model session alive while waiting

Rejected. It wastes resources, couples durability to remote session lifetime
and adds no trustworthy state.

### Poll GitHub by asking the model periodically

Rejected. External-state detection is deterministic control-plane work and
must consume zero provider attempts while unchanged.

### Encode the wait only as `blocked` free-form text

Rejected as the architectural contract. Free-form blocking text is
insufficient for typed conditions, dedupe, recovery, race control and
inspection. Implementation may reuse an existing lifecycle state only if a
separate typed/versioned wait projection remains authoritative.

### Treat webhook delivery as authoritative

Rejected. Delivery can duplicate, reorder, arrive stale or be forged/misrouted.
Runstead validates and reconciles the current external state before effects.

### Build a generic daemon/autonomy scheduler now

Rejected. #161 is a bounded event-driven continuation primitive with GitHub PR
as the first consumer. Cron, unattended autonomy, distributed workers and a
hosted control plane remain separate non-goals.

## 12. Implementation gate

Before coding #161, the executor must re-read then-current:

- this ADR;
- issue #161;
- `docs/architecture.md`;
- `docs/persistence.md`;
- `internal/state/workunits.go` and the current recovery contracts;
- #121/#123 status and any newer maintainer decision.

Repository reality at implementation time controls package/schema details.
This ADR controls the trust boundary and product semantics.
