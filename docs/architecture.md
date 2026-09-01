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

Resolution fails closed on unknown provider ID, unknown protocol family, incomplete configuration, missing mandatory model, invalid endpoint, missing required capability, incompatible RouteSafety and required-but-unconfigured authentication.

Issue #14 adds the minimal composition surface over this contract:
`internal/provider/compat` selects the adapter for the configured protocol
family (the only place a family branch exists) and `internal/config` loads the
operator provider-declaration document (`--providers`). `runstead run` and
`runstead resume` select exactly one configured provider per execution; the
agent loop still depends only on `provider.Client` through the governor-owned
executor. A sanitized provider-neutral execution identity (provider ID,
protocol family, exact model, sanitized config identity, adapter version) is
persisted with the task configuration and per attempt, so execution through a
configured endpoint is inspectable without ever carrying wire types or
credentials across the boundary (see
[`provider-compatibility.md`](provider-compatibility.md)). Capability is proven per endpoint; it is never inferred from the vendor name or the declared family. Credentials are external secret material named only by a non-secret reference; they never enter SQLite state, metadata, traces, contract hashes, fixtures or model context.

Concrete HTTP adapters for each family are separate issues. The OpenAI-compatible adapter (#87) is implemented in `internal/provider/openaicompat`, the Anthropic-compatible adapter (#88) in `internal/provider/anthropiccompat` and the Google/Gemini-compatible adapter (#89) in `internal/provider/googlecompat`: standard-library HTTP only, the minimal family wire subset (exact configured model, rendered prompt as a single user message, streaming disabled), fail-closed response parsing, no redirect following, no retries, authentication resolved at dispatch time through a non-persisted secret resolver seam, and delivery evidence derived from observable transport facts (`httptrace`) rather than absence of error. The anthropiccompat adapter transports the Messages-style generation limit and versioned header semantics through the validated non-secret protocol options propagated by `provider.Resolved` (#88 extension to #79); the googlecompat adapter carries the exact model through the URL resource path (`models/{model}:generateContent`) and keeps an EMPTY protocol-option vocabulary, because the minimal generateContent wire needs nothing beyond `provider.Request`. The existing OmniRoute adapter remains a fail-closed scaffold behind its own pinned receipt lane until a compatible producer exists; it holds no special architectural status beyond being the first historical adapter.

## Composition layer (M10, issue #54)

`internal/composition` is an optional, metadata-only layer above the existing
execution spine. It makes the runtime operator-composable without making the
trusted kernel replaceable:

```text
operator Profile JSON
        ↓ strict parser
built-in CapabilityPackage registry
        ↓ deterministic resolver
effective existing tool registry + frozen execution contract
        ↓
normal protocol → policy → durable effect → evidence → verifier path
```

The declarative `Profile` selects exact versions of compiled-in
`CapabilityPackage` metadata and may identify one configured provider and
recipe requirements. The initial built-ins only reference existing seams:
`repo.read`, `repo.write` and `process.recipes`. Packages contain no callbacks,
commands, credentials or approval/verifier/governor replacement hooks. A write
package describes the existing write boundary; it never grants approval.

The resolver validates strict JSON, package versions, dependencies/conflicts,
runtime compatibility, existing tool names and recipe identities. It sorts all
order-insensitive material, canonicalizes the effective contract and computes a
SHA-256 over non-secret material. `run --profile FILE` persists the exact
contract with the task before the first model/provider attempt. `inspect`
renders only stable sanitized identities. A resumed M10 task requires the
original Profile and an exact byte/hash match before recovery starts; profile,
package, provider, recipe or tool-schema drift is rejected rather than
silently migrated.

The composition layer cannot replace the governor, policy/approval boundary,
durable task/effect truth, evidence provenance, recovery, or completion
verifier. Work Units are contained by the frozen effective tool set and still
use the existing scheduler and agent loop. There is no Go plugin, dynamic code
loading, second execution engine or model authority in this layer. See
[`composition.md`](composition.md) for the operator contract and drift rules.

## Improvement proposal layer (M11, issue #55)

`internal/improvement` + the `improvement_*` tables are a NON-AUTHORITATIVE
control-plane boundary: evidence-backed `ImprovementProposal`s reviewed by
the operator, versioned when applied and measured later through objective
validation records. They are deliberately separate from authoritative
task/effect/evidence state; no execution path reads them and no protocol
tool exists for them (the model can never approve, apply, validate or roll
back its own proposal).

The initial implementation has one concrete kind: `composition`, whose
proposed change is a strict declarative Profile (the M10 typed format).
Applying an approved proposal produces a versioned revision of a
`profiles/<name>` target the operator may point a NEW task at through the
existing explicit `--profile` path; a task already started stays frozen
under its original `FrozenExecutionContract`. Evidence refs are validated
against the real `tool_results` rows at propose and validate time, targets
are structurally unable to name trusted-kernel identities, and workspace
content can never promote itself to policy or active configuration. See
[`improvements.md`](improvements.md) for the lifecycle, authority boundary
and threat model.

## Architectural style

Runstead starts as a **modular monolith** distributed as one CLI executable.

```text
cmd/runstead
    ↓
optional composition/profile layer
    ↓
agent loop
    ├── protocol
    ├── provider
    │   ├── (contract: family/config/capability/route-safety)
    │   ├── omniroute      # historical fail-closed adapter scaffold
    │   ├── openaicompat   # OpenAI-compatible protocol adapter (#87)
    │   ├── anthropiccompat # Anthropic-compatible protocol adapter (#88)
    │   └── googlecompat   # Google/Gemini-compatible protocol adapter (#89)
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

On recovery, Runstead reconstructs model context through the
evidence-preserving context compiler (issue #51, below) and may create a new
upstream conversation, use the same provider with a new session or resume
through another compatible adapter. Reconstruction never means re-executing
historical calls: the run counters, grounding set and repeat guard continue
seeded from persisted state.

## Context compiler (issue #51)

The model-facing context is a deterministic, bounded projection of
authoritative durable task state, not an ever-growing transcript.

`internal/context` compiles the persisted `state.RecoverySnapshot` (plus
typed pending approvals and the workspace signature known at compile time)
into a typed `Compiled` projection with:

- **Authority model** — authoritative facts (objective, lifecycle, consumed
  constraints, actions and attempts, evidence ids and bounded content,
  failures, uncertain effects, pending approvals, remaining acceptance
  checks, latest verification decision, workspace signatures) are separated
  structurally from non-authoritative notes (model summaries/inferences),
  which are explicitly marked and can never satisfy verification.
- **Provenance** — every authoritative fact carries an origin (evidence,
  execution, action id, plan digest, approval row, snapshot task) tracing it
  to persisted state or environment evidence.
- **Boundedness** — mandatory/pinned content (including every evidence id
  required by pending checks) is byte-accounted before rendering; if it does
  not fit the budget the compile fails with `ErrBudgetExhausted` before any
  provider dispatch. Degradable detail (observation content, per-item detail
  lines) is selected in fixed order until the budget is exhausted; every
  skip is recorded in the diagnostics. No silent truncation exists.
- **Determinism** — equal input + equal budget + equal compiler version
  produce byte-identical output; explicit sort keys (evidence id, execution
  id, action id, approval order) and no map-order dependence.
- **Staleness** — workspace-derived facts carry their recorded signature and
  a classification (current / needs-refresh / unverified-current) derived
  from the current signature; classification is presentation only and never
  alters verifier authority.
- **Diagnostics** — `Compiled.RenderDiagnostics()` exposes sanitized
  metadata (version, budget bytes, per-kind counts, omission count) through
  the recovery trace (`recovery_context` line); full context and item values
  are never dumped.

`internal/recovery.BuildContext` is the adapter over the compiler: it keeps
the `Context{Text, EvidenceIDs, Chars}` seed contract consumed by the agent
loop (`transcript.recovery(...)`), adds `Diagnostics`, and propagates budget
exhaustion as a fail-closed resume error. The compiler introduces no
persistence, no migration, no SQL and no new dependency.

Limitations: acceptance per-check status is derived from the latest
verification attempt; an unparseable plan renders as explicitly unavailable
("acceptance plan unavailable"), never as all-passed. The current workspace
signature is supplied by the pipeline when known; otherwise workspace facts
render as unverified-current. Context compaction of long conversations
remains deferred to the plugin/composable-provider tracks.

## Work Units (issues #106/#109)

Work Units are operator-defined, durable subtasks of one task, executed by
the same governed agent loop under a **bounded shared/exclusive scheduler**
(Stage B1, issue #109) on top of the Stage A serial contract (issue #106).
Stage A behavior is exactly the `--workunit-concurrency 1` case; general
multi-agent orchestration, parallel writes and concurrent `run_recipe` remain
out of scope.

`runstead run --workunits FILE [--workunit-concurrency N]` (and
`runstead resume --workunits FILE`) loads a strict JSON definition file
(`work_unit_id`, `objective`, `dependencies`, optional tool/workspace/budget
limits, per-unit acceptance plan). `internal/workunit.Driver` owns the unit
lifecycle and the scheduler:

- **Shared/exclusive policy** — a unit is eligible for the concurrent
  (shared) lane ONLY when its tool envelope is explicit and contains
  exclusively the closed observational set (`read_file`, `list_files`,
  `search_text`, `git_status`, `git_diff`); an explicitly empty envelope is
  also read-only. An OMITTED envelope (task default surface), any effectful
  tool (`write_file`, `apply_patch`, `run_recipe`) and any
  future/unknown tool are EXCLUSIVE: they never overlap another unit, and
  unknown capabilities fail closed instead of becoming concurrent because
  the scheduler does not recognize their effects. The classification is
  derived deterministically from the persisted `tools_json`; it is never
  stored separately and never supplied by a model. `workspace_scope` does
  NOT authorize parallel writers: even apparently disjoint write scopes
  remain exclusive.
- **Bounded scheduling** — shared/read-only units run concurrently up to the
  operator bound (`--workunit-concurrency N`: default 1, minimum 1, initial
  hard ceiling 4); the bound is never exceeded and is never inferred from
  provider/model identity or observed success. An exclusive unit starts only
  when no other unit is active, and while one is active nothing else starts.
  A ready exclusive unit blocks NEW shared dispatch until it runs, so a
  later read-only unit can never starve it. Ready selection continues to
  come exclusively from persisted state (`ReadyWorkUnits`, deterministic
  creation order) and a unit never starts before every required dependency
  is durably `completed`. The scheduler is a narrow extension of the Stage A
  driver: one goroutine per dispatched unit (bounded by `concurrency`), no
  worker framework, daemon or external concurrency dependency.
- **Capability containment** — `Driver.ValidateEnvelope` validates the
  declaration against the parent envelope (a unit can never grant more than
  its parent owns), AND the runtime enforces it: each unit loop runs inside a
  restricted registry view (`tools.Registry.Restricted`) whose tool surface,
  workspace root and system prompt are limited to the unit's declaration.
  A unit declared with only `read_file` cannot propose or execute `write_file`,
  `apply_patch` or `run_recipe`: the protocol parser rejects the proposal
  deterministically before any action record, policy decision or effect, and
  the registry refuses to execute it as a second line of defense. Capability
  contract: OMITTED tools mean the task default surface; EXPLICITLY EMPTY
  tools (`[]`) are a fail-closed no-tools envelope; a workspace scope without
  an explicit tool list never grants the parent surface implicitly. The
  workspace scope is workspace-RELATIVE (canonical single coordinate system,
  the same resolver every tool uses); absolute paths and `..` traversal are
  rejected before any effect.
- **Dependency order** — `work_unit_dependencies` edges are validated
  acyclically at creation (deterministic DFS) and re-supplied definitions are
  created in dependency order regardless of the JSON file ordering; material
  drift on a re-supplied id fails closed instead of being silently skipped.
  `ReadyWorkUnits` exposes only units whose dependencies are all
  `completed` (a dependency that failed/blocked/uncertain keeps its dependents
  from becoming ready and the parent gate open).
- **Durable lifecycle** — `created -> ready -> running -> completed | failed
  | blocked | uncertain`, with `blocked -> ready` after operator decision and
  `running -> ready` as the interrupted-run recovery transition. `created ->
  ready -> running` is persisted BEFORE a unit is dispatched (a `running`
  row is durable before its loop starts), and every outcome transition is
  journaled. The unit row, its evidence refs and its provenance-tagged
  actions/attempts live in SQLite (`work_units`, `work_unit_dependencies`,
  `work_unit_id` columns on `actions`, `tool_attempts`, `provider_attempts`,
  `verification_attempts`).
- **Verification-gated completion** — a unit completes only when its own
  acceptance plan passes (the driver reads the latest verification attempt
  for the unit). A model summary is never enough and a sibling's evidence
  never satisfies another unit's acceptance: completion is
  evidence-backed per unit, and the parent loop only proceeds after the
  chain gate is closed. Under concurrency the uncertain-effect gate of a
  unit's completion verification is scoped to the unit's OWN tool attempts
  (per-unit work_unit_id): a sibling's attempt can legitimately be in
  flight at that exact moment, and the batch-settle semantics allow a
  sibling to complete while another unit is durably uncertain. The parent
  gate still fails closed on every open/uncertain unit, and durable
  evidence remains task-wide and citable.
- **Authoritative resolution of paused units** — an approval-blocked unit
  (blocked reason `approval`) returns to `ready` only when every pending
  approval of the unit's own actions is resolved (operator `decide` + resume;
  the transition is tied to `WorkUnitPendingApprovalCount == 0`, never to an
  arbitrary reason). A unit left `uncertain` by a conservative terminal
  outcome returns to `ready` after the recovery pipeline reconciled all of
  its effect records (`ReconcileUncertainWorkUnits`); an unreconcilable or
  unresolved effect keeps it blocking (`human_review_required`).
- **Failure/cancellation semantics** — a failed/blocked/uncertain outcome in
  one unit stops NEW scheduling (no new batches), lets the already-dispatched
  bounded batch settle to durable states (no artificial sibling cancellation
  that would manufacture uncertain effects), then returns the typed
  open-units gate error with the parent gate closed. Real operator/context
  cancellation propagates to every active unit, starts no new unit, leaves
  interrupted units durably `running` for recovery, and the scheduler drains
  every worker goroutine before returning (no leaks).
- **Authoritative parent gate** — parent completion is impossible while any
  persisted work unit is not completed. The gate lives at TWO boundaries:
  `state.Store.FinalizeTask` refuses to persist `completed` while open units
  exist (so omitting `--workunits` on resume can never finalize the parent),
  and `resume` without `--workunits` on a task with open units exits gated
  before the recovery pipeline or any parent dispatch. Resumed units carry
  the FULL recovery seed (the #51-reconstructed context, persisted evidence,
  repeat/streak guards and counters; per-unit turn/attempt counters continue
  from the unit's own history), and a unit `context_budget` recompiles the
  model-facing context under that budget, failing closed if it does not fit.
- **Unit-scoped execution** — each unit runs through `agent.NewLoop` with
  its own objective and budgets but the same durable task row. Unit loops
  carry `SkipTaskFinalize`: they never finalize the shared task; the parent
  loop owns task finalization. Client request ids are namespaced per unit
  (`task-workunit-turn`) so the governor's duplicate-request protection
  never conflates unit attempts. Evidence ids stay unique across concurrent
  units through the registry's shared atomic evidence sequence.
- **Governor authority unchanged** — every provider attempt of every
  concurrent unit flows through the SAME account-scoped governor-owned
  executor; the scheduler adds no second admission path and cannot bypass
  `MaxInFlight`/pacing/circuit authority. One governed physical attempt
  remains one accounted attempt, with the existing delivery-state and retry
  semantics. The scheduler never rotates providers/models/keys/accounts to
  force parallelism.
- **Recovery without replay** — interruption leaves the interrupted units
  `running` (possibly several, under concurrency); `recovery.Resume` resets
  ALL interrupted units to `ready` before building the context, then
  RELOADS the authoritative snapshot so the compiled context can never
  project `running` that SQLite already persisted as `ready`. The resumed
  conversation re-runs only the interrupted units: each unit's
  loop counters continue from ITS OWN persisted attempt history (so
  request ids never collide with the interrupted session and no provider
  request is blindly re-issued) and the evidence seed grounds finals against
  completed historical observations without re-executing them. Completed
  units, their rows and their effects are never replayed; a provider attempt
  is never assumed NOT to have reached the provider merely because its
  worker disappeared.

Work Unit tasks bootstrap through the SAME path as a normal task
(`agent.BootstrapTask` + `agent.ConfigSnapshot`): the durable row carries the
full authoritative execution configuration (provider identity,
protocol/config identity, exact model, policies, recipe catalog digest,
acceptance digest, limits, git baseline AND the effective Work Unit
scheduler bound under the durable `workunit_concurrency` key in
`tasks.config_json`), so resume validates provider/model/config continuity
and rejects drift exactly like a normal task. `resume` REJECTS an explicitly
supplied `--workunit-concurrency` that differs from the persisted value
(fail-closed before the recovery pipeline); omitting the flag adopts the
persisted contract, so a task can never silently change its scheduler
configuration across restart. A present-but-corrupted persisted value (invalid type, non-integral or outside the operator contract) is refused as corrupted state in the resume pre-flight, BEFORE the recovery pipeline journals anything; only a genuinely absent key maps to the Stage A serial contract.

`runstead inspect` renders a "Work Units:" section (unit id, status, scope)
derived from the durable rows, and the effective concurrency appears in the
Configuration section (from the same durable `config_json`), without
secrets.

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

## Architectural borrowing doctrine (issue #49)

Runstead studies strong external runtimes, coding agents, harnesses and
research systems and actively mines their strongest ideas. External projects
are REFERENCES and EVIDENCE sources, never implicit roadmap authority. The
doctrine is: **mine the gold, preserve Runstead** — identify the strongest
ideas, re-express only the compatible parts in Runstead-native abstractions,
and import the useful mechanism without the surrounding architecture. Issue
#35 established the first local precedent: adopt an external concept without
copying the external implementation.

The inverse rule is equally binding: **a maintainer preference is not a
Runstead invariant merely because it was written down first.** A restriction
becomes durable project doctrine only when it follows from the established
trust model, an explicit project/maintainer decision, or evidence recorded in
an ADR/issue. An unstated prudential preference must be written as a
hypothesis, trade-off, experiment or ADR to validate, never as a prohibition.

Every externally inspired architectural proposal answers these questions
before implementation:

1. **What Runstead problem does this solve?** A feature is never accepted
   only because another respected project has it.
2. **Which Runstead invariants does it strengthen?** Reliability, boundedness,
   recoverability, auditability, evidence, policy control, provider
   replaceability or maintainability must improve materially.
3. **Which foreign assumptions are incompatible?** Record what is rejected,
   not only what is copied, and tie every rejection to a real Runstead
   invariant or evidence rather than maintainer taste.
4. **Can the concept be expressed through existing boundaries?** Prefer
   `state`, `agent`, `protocol`, `tools`, `governor`, `verifier`, explicit
   capabilities and durable entities over importing a foreign framework or
   runtime.
5. **What new trust or failure boundary would it create?** Model-controlled
   execution, dynamic code loading, hidden retries, opaque state, automatic
   policy mutation and unreconciled side effects require exceptional
   justification.
6. **What evidence would justify keeping it?** Define acceptance checks and
   measurable benefit before broadening the architecture.
7. **What is the smallest compatible slice?** Adopt the useful mechanism
   without importing surrounding complexity.
8. **Are we rejecting the idea because it conflicts with Runstead, or because
   the maintainer personally dislikes the technique?** In the latter case,
   convert the rejection into an experiment/ADR question instead of project
   law.

### The Runstead trust model is not importable

No imported idea may permit:

- model control of policy or approvals;
- model execution of arbitrary code outside the authorized boundaries;
- retries or fallbacks escaping governor accounting;
- durable truth replaced by session or model state;
- evidence or the verifier replaced by claims;
- recovery replaying historical effects blindly;
- secrets promoted to ordinary state;
- the trusted kernel becoming a configurable plugin.

### Prefer the smallest compatible slice

Adopt the mechanism without the framework. No architectural rewrite for
aesthetic purity, no new dependency without a demonstrable benefit, no
preventive daemon/plugin/runtime, and no abstraction for a requirement that
does not exist yet. If an adopted idea cannot demonstrate its expected
benefit, simplify or remove it.

### External provenance for materially inspired proposals

An issue materially inspired by an external project:

```text
Source project/paper:
Observed concept:
Runstead problem addressed:
What we adopt:
What we explicitly reject:
Why each rejection is a Runstead constraint rather than preference:
Runstead invariants preserved/strengthened:
Evidence/exit gate:
```

This structure is already in active use, for example issue #55 (the M11
improvement lifecycle) records its Prime Agent provenance with explicit
adopt/reject fields. Trivial references need no bureaucracy; the requirement
applies to architectural proposals materially derived from an external
source. Code or protocol copied from another source still requires the normal
license/provenance review. Architectural inspiration alone is never a reason
to add a dependency.

### Historical correction — the Camoufox lesson

Runstead once converted prudential maintainer preferences about
stealth/fingerprint/automation concealment and browser automation into
architectural prohibitions, propagating them through earlier browser-work
issues as if they were established invariants. That was architecturally
incorrect and was corrected. Runstead's real invariants concern authority,
accounting, recoverability, evidence and provider replaceability, not whether
a transport exposes or conceals automation characteristics. Such properties
are transport properties to be evaluated on measurements rather than banned
by assumption; they must never become a hidden second policy layer, bypass
governor accounting, or rotate accounts around explicit quotas, and required
login/MFA/CAPTCHA/provider challenges remain user-interactive. The lesson
applies to Runstead's own ideas too: keep the useful invariant, discard the
accidental dogma. This section records the correction; browser work itself
remains deferred and is not reopened here.

### Governance rules

- Do not weaken an active milestone's exit criteria to land an attractive
  external idea; move useful non-critical concepts to a later capability gate.
- No architecture migration for aesthetic purity or trend alignment; prefer a
  measured experiment or ADR before replacing working infrastructure.
- A foreign system's benchmark is not proof that the same mechanism helps
  Runstead under its trust model; a maintainer's preference is not proof that
  the opposite mechanism is incompatible with Runstead.
- Document rejected foreign assumptions with their Runstead rationale so they
  are not re-proposed without new evidence; if a rejection has no
  invariant/evidence rationale, rewrite it as a hypothesis, trade-off or
  experiment rather than a permanent prohibition.
- If an existing architectural rule is discovered to lack proper project
  basis, correct the source issues instead of preserving the inconsistency
  for historical continuity.
