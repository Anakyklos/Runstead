# Initial Architecture

## Purpose

Runstead exists to make the ChatGPT Web model exposed by OmniRoute behave as a dependable local agent. It does this by owning the execution contract around the model rather than trusting the provider session or the model's claims.

## System boundary

### OmniRoute owns

- provider authentication and session access;
- exposure of ChatGPT Web through an API-compatible endpoint;
- request and response transport;
- provider-specific compatibility work;
- upstream retries or routing that belong to the gateway.

### Runstead owns

- task lifecycle;
- prompts and the action contract;
- action parsing and validation;
- local tools;
- permissions and safety policy;
- event history and checkpoints;
- failure recovery;
- verification of observable effects;
- final evidence and auditability.

Runstead must not depend on a remote conversation as its source of truth.

## Architectural style

Runstead starts as a **modular monolith** distributed as one CLI executable.

```text
cmd/runstead
    ↓
agent loop
    ├── protocol
    ├── provider/omniroute
    ├── tools
    ├── executor
    ├── policy
    ├── verifier
    ├── state
    └── trace
```

Packages separate responsibilities, but they do not become services. Internal interfaces should be introduced where real substitution or test isolation is required, not to imitate enterprise architecture.

## Main execution loop

```text
load task state
    ↓
build bounded context
    ↓
request next model decision
    ↓
parse Runstead action
    ↓
validate schema and policy
    ↓
execute local tool
    ↓
verify observable result
    ↓
persist event and checkpoint
    ↓
return observation to model
    ↓
continue or finish with evidence
```

The model never executes a tool directly. It proposes an action. Runstead remains responsible for whether that action is valid, permitted, executed and proven.

## Action protocol

Runstead will use a protocol it controls instead of requiring OmniRoute's emulated native tool calling to work perfectly.

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

Native `tool_calls` may be accepted later as an additional input format, but the project must retain an independent fallback.

## Provider adapter

The first adapter targets OmniRoute and ChatGPT Web only.

Initial behavior:

- use a non-streaming request path first;
- configure base URL, model and credentials explicitly;
- apply request timeouts and cancellation;
- preserve useful upstream identifiers in the trace;
- classify transport, authentication, timeout, empty-response and malformed-response failures;
- avoid coupling task persistence to OmniRoute session persistence.

Provider interfaces should remain minimal. Broad provider abstraction is not a v0.1 deliverable.

## Local tools

### Read-only stage

- `read_file`
- `list_files`
- `search_text`
- restricted `shell`
- `git_status`
- `git_diff`

### Write stage

- `write_file`
- `apply_patch`

Every tool returns structured observations including success, failure, exit status, stdout/stderr where appropriate and evidence needed by the verifier.

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

SQLite is the authoritative task store.

Initial entities:

- `tasks`
- `events`
- `messages`
- `actions`
- `tool_results`
- `checkpoints`

The event history should be append-oriented. Derived task status may be updated for convenience, but it must remain possible to reconstruct what happened from persisted events.

## Recovery model

A remote session can fail without invalidating the task.

On recovery, Runstead should reconstruct a bounded context from:

- original objective;
- current plan or working summary;
- relevant files and hashes;
- completed actions;
- latest verified observations;
- unresolved errors;
- remaining constraints.

It may create a new upstream conversation and continue from the local checkpoint.

## Verification

Model statements are not evidence.

Examples:

- file creation is proven by filesystem inspection;
- file modification is proven by hash or diff;
- command completion is proven by process exit status;
- tests passing is proven by actual test output and exit status;
- repository changes are proven by `git diff`;
- final completion requires satisfied acceptance checks.

The verifier should remain separate from the model-facing narrative so that false claims cannot silently become state.

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
- loop exhaustion.

## Dependency policy

The first milestone should keep external Go dependencies to the practical minimum. A dependency is accepted only when it removes meaningful risk or maintenance burden that the standard library cannot reasonably cover.

Explicitly excluded from the initial architecture:

- agent frameworks;
- ORMs;
- dependency-injection frameworks;
- internal event buses;
- queues and brokers;
- distributed services;
- UI frameworks;
- vector databases.
