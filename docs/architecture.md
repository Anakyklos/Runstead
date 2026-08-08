# Initial Architecture

## Purpose

Runstead exists to make ChatGPT Web behave as a dependable local agent by owning the execution contract around the model rather than trusting a provider session, a transport adapter or the model's claims.

The project deliberately separates the **agent runtime** from the **provider transport**. The runtime must become reliable first through OmniRoute. A first-party ChatGPT Web connector is introduced only after the runtime baseline is proven.

## System boundary

### Runstead core owns

- task lifecycle;
- prompts and the action contract;
- action parsing and validation;
- local tools;
- permissions and safety policy;
- event history and checkpoints;
- failure recovery;
- verification of observable effects;
- final evidence and auditability.

### Provider adapters own

- authentication material required by the provider path;
- request and response transport;
- provider-specific request construction;
- streaming or response decoding;
- provider error classification;
- sanitized transport diagnostics.

Provider adapters must not own task truth, acceptance decisions or local side effects.

## Provider transition strategy

### Stage 1 — OmniRoute baseline

OmniRoute is the first supported adapter because it already exposes ChatGPT Web and lets Runstead validate the higher-risk agent-runtime assumptions without first rebuilding the unofficial web transport.

The OmniRoute adapter owns:

- configurable base URL, API key and model;
- non-streaming requests initially;
- fail-closed OmniRoute scaffold; management snapshots are sanity evidence
  only. The Runstead consumer side of the #29 attempt-receipt contract is
  merged (PR #33); protected model execution remains disabled until a
  compatible OmniRoute producer emits authoritative receipts and #30
  activates the live path;
- timeouts and cancellation;
- capture of useful upstream identifiers;
- classification of transport, authentication, timeout, rate/capacity,
  account-safety, empty-response and malformed-response failures;
- optional sanitized rate-limit/resilience telemetry.

Runstead does not depend on OmniRoute's emulated native tool calling. Model output is treated as text and interpreted through a Runstead-owned action protocol.

### Stage 2 — First-party ChatGPT Web connector

After the OmniRoute-backed v0.1 is reliable, Runstead will add `provider/chatgptweb`.

The first-party connector may need to own:

- explicit local credential import without collecting account passwords;
- secure credential storage or operating-system keyring integration;
- session-cookie rotation and access-token exchange;
- ChatGPT Web request requirements and proof material;
- browser-compatible HTTP/TLS behavior;
- model-slug discovery or mapping;
- request construction for the internal conversation endpoint;
- SSE decoding and final-answer extraction;
- sanitized raw-response capture for diagnosis;
- precise failure categories and bounded recovery.

This connector is intentionally narrow. It will not expose a public OpenAI-compatible API, manage many accounts, implement quota routing or reproduce OmniRoute's provider catalog.

### Reverse-engineering input (2026-08-07)

A static audit of OmniRoute's `ChatGptWebExecutor` (release/v3.8.50,
SHA `976d670ff3a7712df0c695f13095c43eace5e29b`) informs both adapter stages.
Full findings and evidence: [`research/omniroute-chatgpt-web-executor.md`](research/omniroute-chatgpt-web-executor.md).

Key consequences for this architecture:

- **Conversation-per-request.** OmniRoute starts a fresh Temporary Chat per
  turn and folds history into the system message. Runstead must not assume
  conversation continuity through the provider.
- **Cumulative SSE with echo suppression.** Deltas are diffs of cumulative
  text; echoes of prior turns are suppressed until the current turn is
  `in_progress`. This is the reference pattern for Runstead's future stream
  reconciliation and stale-return prevention.
- **No retry of the text conversation-model POST inside the web executor.**
  401/403 clears the token cache and the request fails; account fallback
  happens above the executor. (Auxiliary image/handoff recovery paths exist
  upstream but are irrelevant to Runstead's text-only path.) The delivery
  states (`not_sent` / `sent_confirmed` / `sent_unconfirmed` /
  `response_started` / `completed`) remain the correct unit for idempotency.
- **Usage from this path is estimated** (`ceil(len/4)`), not real.
- **Rejected practices:** TLS impersonation, browser-fingerprint mimicry in
  Sentinel prekeys, page-load warmup, hard-coded frontend build values without
  a drift probe, silent account fallback, and encryption fail-open (plaintext
  passthrough / plaintext fallback on cipher failure; only the `enc:v1:`
  envelope is reference for a future vault). A first-party connector must not
  reproduce these; the OmniRoute adapter stays the baseline.

### Stage 3 — Bake-off and migration

The OmniRoute and direct adapters must run the same protocol test corpus and end-to-end tasks.

The direct connector becomes the default only if it demonstrates material improvement in one or more of:

- valid action rate;
- protocol-refusal rate;
- empty or malformed response rate;
- recoverability after session failure;
- diagnostic precision;
- latency or operational simplicity;
- maintenance cost.

A direct connector that merely duplicates OmniRoute with similar reliability does not justify migration.

## Architectural style

Runstead starts as a **modular monolith** distributed as one CLI executable.

```text
cmd/runstead
    ↓
agent loop
    ├── protocol
    ├── provider
    │   ├── omniroute
    │   └── chatgptweb     # later milestone
    ├── tools
    ├── executor
    ├── governor
    ├── verifier
    ├── state
    └── trace
```

The `governor` package is the account-protection boundary above both provider
adapters. It is account-scoped rather than a generic remote-service router: one
FIFO lane admits at most one in-flight provider completion for an account. On a
legacy single-attempt route, the permit start charges the rolling and task
ledgers. On a receipt-aware route, start reserves the logical request and
finish validates and reconciles one debit per authoritative upstream-attempt
receipt. M1 accepts one receipt per protected completion; observed amplification
is still fully accounted, then marks the lane unsafe and blocks later
admission. Missing or structurally invalid accounting produces a conservative
uncertain debit and also fails closed. Provider safety metadata and management
snapshots are diagnostic inputs, not proof of actual attempt count. The
Runstead consumer side is merged in PR #33, while protected OmniRoute rollout
remains disabled until a compatible producer emits authoritative receipts and
#30 activates the live path. See
[`account-protection.md`](account-protection.md) and
[`architecture/attempt-receipts.md`](architecture/attempt-receipts.md).

Packages separate responsibilities, but they do not become services. Internal interfaces are introduced only where real substitution or test isolation is required.

The provider interface should remain small enough to support deterministic fake providers in tests and the two real provider paths without becoming a generic routing framework.

## Main execution loop

Issue #7 implements the bounded agent loop in `internal/agent`; issue #10 adds
policy-gated write tools to it.

```text
task
    ↓
build deterministic system contract (protocol version + registered tools)
    ↓
governed model turn (account-scoped attempt executor)
    ↓
parse exactly one envelope (runstead.protocol.v1)
    ↓
action → write tools: control-plane policy gate (allow/deny/approval_required)
    ↓   approval_required → PAUSE with approval_required outcome (resumable)
action → repeat guard → registry validation and execution
    ↓
observation with evidence ID returned as UNTRUSTED data
    ↓
next governed model turn
    ↓
final → pending-approval check → grounding check against evidence IDs
    ↓
terminal outcome
```

The loop owns no provider client and no `provider.Client.Complete` call: its
only provider seam is the governor-owned `agent.Executor`, so every model turn
re-enters account admission. Tool output and repository content are carried as
untrusted observations in a separate transcript role and never become system
instructions, permissions, policy or approval. Fingerprints from #5 act only as
a workspace-aware loop guard, not as permanent idempotency keys: an identical
observational action may run again after the workspace changes. Write tools
(`write_file`, `apply_patch`) are gated by the control-plane policy before any
execution decision; approval comes only from persisted operator records, never
from model output. An unapproved write pauses the run with the typed
`approval_required` outcome (a control-plane dependency, not a protocol
correction): the task stays durably resumable, and `runstead decide` +
`runstead resume` continues it under the same persisted policy. The loop enforces bounded steps, corrections, repeated
actions, elapsed task time and provider attempts, and every terminal exit maps
to a typed outcome with a stable exit code (see
[`development.md`](development.md)). Durable state (issue #8) persists the
loop's task, action, attempt, evidence, write-policy decision and approval
lifecycle through the semantic persistence boundary. Write effects follow the
ADR two-transaction ordering (intent → effect outside SQLite → observed
result), stale-state preconditions refuse overwrites, and interrupted writes
are reconciled from observable filesystem state on resume (see
[`writes.md`](writes.md)). There is still no shell, background execution or
automatic Git operation in this milestone.

The model never executes a tool directly. It proposes an action. Runstead remains responsible for whether that action is valid, permitted, executed and proven.

## Action protocol

Runstead uses a protocol it controls instead of requiring provider-native or emulated tool calling to work perfectly.

Initial candidate:

```xml
<runstead_action>
{
  "tool": "read_file",
  "arguments": {
    "path": "README.md"
  }
}
</runstead_action>
```

The protocol must support:

- deterministic extraction from mixed natural-language output;
- strict JSON validation;
- one action per turn initially;
- explicit final responses;
- correction feedback for malformed actions;
- bounded retries;
- versioning once behavior stabilizes.

Native `tool_calls` may be accepted later as an additional input format, but the independent protocol remains the compatibility path across transports.

## Local tools

### Read-only stage

- `read_file`
- `list_files`
- `search_text`
- `git_status`
- `git_diff`

The issue #6 registry implements these five tools only. Generic shell remains
out of scope until a separately reviewed policy boundary exists.

### Write stage

- `write_file`
- `apply_patch`

Every tool returns structured observations including success, failure, exit status, bounded stdout/stderr where appropriate and evidence needed by the verifier.

## Policy model

Default policy:

| Action | Initial policy |
| --- | --- |
| Read inside workspace | automatic |
| Search inside workspace | automatic |
| Non-destructive local commands | automatic with timeout |
| Write inside workspace | configurable approval |
| Network access | approval required |
| Git commit | approval required |
| Git push | approval required |
| Access outside workspace | denied |
| Privilege escalation | denied |
| Destructive commands | denied |

Policy decisions must be explicit and logged.

## Durable state

SQLite is the authoritative task store. The durable-execution contract for M2
(identities, attempt state machines, transaction ordering around external
effects, recovery classes, schema invariants, and the distinction between
state reconstruction and re-execution) is defined canonically in
[`adr/0001-durable-execution.md`](adr/0001-durable-execution.md). Issue #8
implements the schema, the transactional journal + operational projection
model, attempt records, persisted governor protection state and
`runstead inspect`; #9 implements recovery/resume from that contract. The
implemented persistence reality (driver decision and evidence, pragmas,
migrations, database location, security/redaction, backup/cleanup) is
documented in [`persistence.md`](persistence.md).

The event history is append-oriented. Each operational projection change and
its corresponding event are committed in the same SQLite transaction. Derived
task status may be updated for convenience, but reconstructing state never
means re-executing historical calls.

Provider-specific session identifiers may be recorded as disposable metadata.
They are never the source of task truth.

## Recovery model

A remote session or provider adapter can fail without invalidating the task.

On recovery, Runstead reconstructs a bounded context from:

- original objective;
- current plan or working summary;
- relevant files and hashes;
- completed actions;
- latest verified observations;
- unresolved errors;
- remaining constraints.

It may create a new upstream conversation, use the same provider with a new session or resume through another compatible adapter.

## Verification

Model statements are not evidence.

Examples:

- file creation is proven by filesystem inspection;
- file modification is proven by hash or diff;
- command completion is proven by process exit status;
- tests passing is proven by actual test output and exit status;
- repository changes are proven by `git diff`;
- final completion requires satisfied acceptance checks.

The verifier remains separate from the model-facing narrative so that false claims cannot silently become state.

## Docker development boundary

Docker is used to make development and testing reproducible without installing project dependencies directly on the host.

It is not treated as a complete security sandbox. A bind-mounted writable repository is intentionally modifiable from the container.

The development environment should:

- run the process as a non-root user matching the host UID/GID when practical;
- bind-mount only the Runstead source and explicitly selected workspaces;
- use named volumes for Go module and build caches;
- keep SQLite development data in an explicit named volume or selected host path;
- mount credentials read-only or inject them at runtime;
- avoid host networking unless a demonstrated requirement exists;
- never mount `/var/run/docker.sock`;
- never run privileged;
- drop unnecessary Linux capabilities and set `no-new-privileges` where supported;
- retain direct native build and test commands outside Docker.

The later direct ChatGPT Web connector may require a dedicated image or transport helper because browser-compatible TLS behavior can introduce native dependencies. That work must not contaminate the core runtime image before the direct connector milestone.

## Failure controls

The runtime must eventually handle:

- timeout and cancellation;
- empty or truncated responses;
- malformed action blocks;
- unknown tools;
- invalid arguments;
- repeated identical actions;
- stuck commands;
- process interruption;
- stale remote sessions;
- duplicated requests;
- claims of actions that never occurred;
- loop exhaustion;
- provider-adapter replacement during recovery.

## Dependency policy

External Go dependencies are kept to the practical minimum. A dependency is accepted only when it removes meaningful risk or maintenance burden that the standard library cannot reasonably cover.

Explicitly excluded from the initial architecture:

- agent frameworks;
- ORMs;
- dependency-injection frameworks;
- internal event buses;
- queues and brokers;
- distributed services;
- UI frameworks;
- vector databases;
- a universal provider router.
