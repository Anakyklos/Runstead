# Initial Architecture

## Purpose

Runstead exists to make model access behave as a dependable local agent by owning the execution contract around the model rather than trusting a provider session, a transport adapter or the model's claims.

The project deliberately separates the **agent runtime** from the **provider layer**. The runtime depends on a small provider-neutral contract, and concrete transports are configurable endpoints implementing one of three compatibility protocol families (`openai_compatible`, `anthropic_compatible`, `google_compatible`). Official OpenAI, Anthropic and Google services are only examples of those families: they are valid implementations, not privileged architectural dependencies (#86). ChatGPT Web/OmniRoute work is deferred to future plugin/composable-provider tracks.

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

### Provider layer owns

- provider identity (operator-configured `provider_id`) and protocol family as distinct concepts;
- configuration resolution that fails closed before any dispatch;
- authentication material required by the provider path (referenced externally, never persisted);
- request and response transport;
- protocol-family-specific request construction;
- streaming or response decoding;
- provider error classification;
- sanitized transport diagnostics;
- declared/proven capability profiles and route-safety declarations.

Provider adapters must not own task truth, acceptance decisions or local side effects. The agent loop never branches on a vendor name or a protocol family; family dispatch belongs exclusively to the provider layer.

## Provider architecture

```text
Runstead runtime
      |
provider-neutral contract (internal/provider)
      |
protocol adapter (one per supported family)
  |        |        |
openai_   anthropic_ google_
compatible compatible compatible
      |
configured provider endpoint
```

The provider-neutral contract (#79) consists of:

- `provider.Client`: one logical completion through governed admission;
- `provider.RouteSafety`: the executable declaration of attempt/amplification behavior; the single source of truth for attempt safety;
- delivery evidence (`DeliveryState`, attempt receipts): transport-level proof of what physically happened upstream;
- `ProtocolFamily`: which compatibility wire contract the endpoint speaks (`openai_compatible`, `anthropic_compatible`, `google_compatible`);
- `Config` / `Registry.Resolve`: operator-declared endpoint identity, base URL, exact model, non-secret auth reference, non-secret options and config version, resolved fail-closed before any provider code runs;
- `CapabilityProfile`: an explicit, versioned capability profile (text turn, Runstead structured protocol, native tools only when separately proven, streaming, cancellation, size bounds) plus the endpoint's RouteSafety declaration.

Resolution fails closed on unknown provider ID, unknown protocol family, incomplete configuration, missing mandatory model, invalid endpoint, missing required capability, incompatible RouteSafety and required-but-unconfigured authentication. Capability is proven per endpoint; it is never inferred from the vendor name or the declared family. Credentials are external secret material named only by a non-secret reference; they never enter SQLite state, metadata, traces, contract hashes, fixtures or model context.

Concrete HTTP adapters for each family are separate issues (#89 Google/Gemini-compatible remains open). The OpenAI-compatible adapter (#87) is implemented in `internal/provider/openaicompat` and the Anthropic-compatible adapter (#88) in `internal/provider/anthropiccompat`: standard-library HTTP only, the minimal family wire subset (exact configured model, rendered prompt as a single user message, streaming disabled), fail-closed response parsing, no redirect following, no retries, authentication resolved at dispatch time through a non-persisted secret resolver seam, and delivery evidence derived from observable transport facts (`httptrace`) rather than absence of error. The anthropiccompat adapter transports the Messages-style generation limit and versioned header semantics through the validated non-secret protocol options propagated by `provider.Resolved` (#88 extension to #79). The existing OmniRoute adapter remains a fail-closed scaffold behind its own pinned receipt lane until a compatible producer exists; it holds no special architectural status beyond being the first historical adapter.

## Architectural style

Runstead starts as a **modular monolith** distributed as one CLI executable.

```text
cmd/runstead
    ↓
agent loop
    ├── protocol
    ├── provider
    │   ├── (contract: family/config/capability/route-safety)
    │   ├── omniroute      # historical fail-closed adapter scaffold
    │   ├── openaicompat   # OpenAI-compatible protocol adapter (#87)
    │   └── anthropiccompat # Anthropic-compatible protocol adapter (#88)
    ├── tools
    ├── executor
    ├── governor
    ├── verifier
    ├── state
    └── trace
```

The `governor` package is the account-protection boundary above every provider
adapter. It is account-scoped rather than a generic remote-service router: one
FIFO lane admits at most one in-flight provider completion for an account. On a
legacy single-attempt route, the permit start charges the rolling and task
ledgers. On a receipt-aware route, start reserves the logical request and
finish validates and reconciles one debit per authoritative upstream-attempt
receipt. M1 accepts one receipt per protected completion; observed amplification
is still fully accounted, then marks the lane unsafe and blocks later
admission. Missing or structurally invalid accounting produces a conservative
uncertain debit and also fails closed. Provider safety metadata and management
snapshots are diagnostic inputs, not proof of actual attempt count. The
Runstead consumer side is merged in PR #33; protected live rollout through any
adapter remains disabled until that adapter produces authoritative receipts.
See [`account-protection.md`](account-protection.md) and
[`architecture/attempt-receipts.md`](architecture/attempt-receipts.md).

The governor keeps upstream allowance semantics (#58) separate from
Runstead-local workload controls. A typed `AllowanceKind` distinguishes
`published_quota` (numeric rolling ceilings and a profile-specific manual
reserve), `unlimited_text` (explicitly configured; no fabricated quota, no
reserve) and `unknown` (no evidence; explicit conservative local ceilings and
a local manual-use reserve stay mandatory from the #21 contract, and success
never upgrades the semantic). Local controls, cooldown, circuit breakers,
fail-closed security and receipt accounting are identical across kinds, and
changing the allowance kind never resets the durable projection.

Packages separate responsibilities, but they do not become services. Internal interfaces are introduced only where real substitution or test isolation is required.

The provider interface should remain small enough to support deterministic fake providers in tests and one real adapter per supported protocol family without becoming a generic routing framework.

## Main execution loop

Issue #7 implements the bounded agent loop in `internal/agent`; issue #10 adds
policy-gated write tools and issue #26 adds policy-gated process recipes to it.

```text
task
    ↓
build deterministic system contract (protocol version + registered tools + recipes)
    ↓
governed model turn (account-scoped attempt executor)
    ↓
parse exactly one envelope (runstead.protocol.v1)
    ↓
action → policy-gated effects (writes, run_recipe):
         control-plane policy gate (allow/deny/approval_required)
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
(`write_file`, `apply_patch`, `run_recipe`) are gated by the control-plane
policy before any execution decision; approval comes only from persisted
operator records, never from model output. An unapproved effect pauses the run
with the typed `approval_required` outcome (a control-plane dependency, not a
protocol correction): the task stays durably resumable, and `runstead decide`
+ `runstead resume` continues it under the same persisted policy. Process
recipes (`run_recipe`) select only operator-declared recipes from the
configured catalog; the model never supplies commands or argv. The recipe
runner executes argv directly, terminates the full process tree on
timeout/cancellation, builds an allowlisted child environment (credential
names never inherited), bounds output per stream and persists structured
process evidence consumed by the #11 verifier (see
[`process-runner.md`](process-runner.md) and
[`verification.md`](verification.md)). A `status="complete"` final is only a
proposal: the independent verifier (`internal/verifier`) observes persisted
evidence, the real filesystem, real git state and the operator acceptance
plan, rejects cited evidence whose declared tool does not match the persisted
row, refuses completion `blocked` when no acceptance plan exists (fail
closed), and the state layer refuses `completed` without a passed verification
attempt. The loop enforces bounded steps, corrections, repeated
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

Issue #12 completes the inspect-edit-test-fix coding loop on top of these
boundaries (see [`coding-loop.md`](coding-loop.md)): a recipe observation
whose real exit code is non-zero is recoverable evidence returned to the next
model turn with the recipe id, real exit status, signal, bounded output,
truncation flags and evidence ID; a corrective write followed by a rerun is
allowed because the workspace signature changed; a premature completion
proposal is refused by the verifier and returns to execution; and two new
loop guards stop unproductive repetition with typed outcomes
(`consecutive_failures_exhausted`, `verification_failures_exhausted`), with
counters that survive resume through the recovery seed. The verifier's
`writes_reconciled` check reconciles the LATEST persisted write of every
target path against the current filesystem and records earlier writes to the
same path as superseded, so the multi-write corrective trajectory is provable
without pretending intermediate states still exist. The deterministic sample
repository (`fixtures/coding-loop/`) requires real inspection, two writes, a
real failing test recipe, diagnosis from bounded process evidence, a
corrective write, a passing rerun, real Git attribution and a passed
verification before `completed`.

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

A future transport adapter may require a dedicated image or transport helper because unusual TLS or browser-compatible behavior can introduce native dependencies. That work must not contaminate the core runtime image before such an adapter milestone exists.

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
