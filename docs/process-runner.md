# Bounded process runner

Issue #26 introduces the first controlled local process execution into
Runstead: operator-declared **recipes** that the model can select by ID through
`run_recipe`. There is **no generic shell** and no model-controlled command
string. The runner executes argv directly, terminates the full process tree on
timeout or cancellation, builds the child environment from an explicit
allowlist, bounds stdout/stderr independently with visible truncation, and
persists structured process evidence under the durable-execution contract.

This document is intentionally honest about limitations: native execution does
not provide kernel-level sandboxing, network isolation is **not enforced** by
the native runner, and Docker (when it exists, #15) is not automatically an
execution sandbox.

## The recipe model

A recipe is an operator-declared, typed description of one allowed local
process:

```json
{
  "id": "test",
  "executable": "go",
  "argv": ["test", "./..."],
  "working_directory": "",
  "timeout_nanos": 120000000000,
  "output_limits": {"max_stdout_bytes": 262144, "max_stderr_bytes": 262144},
  "capabilities": ["execute_repository_code", "temporary_files", "network"],
  "allowed_environment": ["GOCACHE", "GOMODCACHE"]
}
```

- `id` is the stable identifier the model uses (`run_recipe`
  `{"recipe":"test"}`). The model never supplies `executable`, `argv`,
  capabilities or environment.
- `executable` is the program to run. It may be a PATH name or an absolute
  path; `..` traversal is refused. It is never model-controlled.
- `argv` is the fixed operator-declared argument vector. Arguments are passed
  literally: shell metacharacters (`;`, `&&`, `|`, `$()`, backticks, `<`,
  `>`) have no shell semantics because no shell is involved.
- `working_directory` is relative to the selected workspace; empty means the
  workspace root. Absolute paths, `..` components and symlink escapes are
  rejected.
- `timeout_nanos` bounds the whole process run (including children); the
  default is 60s.
- `output_limits` bounds stdout and stderr capture **independently** (default
  256 KiB each, hard cap 4 MiB per stream). Retention is always bounded in
  memory; the total observed byte counts are recorded even when truncated.
- `capabilities` is the declared effect set. It is a description, never an
  authorization; the control-plane policy decides. An environment allowlist
  is only honored when `inherit_environment` is among the declared
  capabilities; a catalog that lists `allowed_environment` without it is
  refused fail-closed.
- `allowed_environment` is the allowlist of parent environment variable
  **names** the recipe may inherit. The complete parent environment is never
  inherited.

The catalog source is the **control plane**: `--recipes FILE` (or
`RUNSTEAD_RECIPES`), a JSON array read once at startup. The catalog is
**strictly decoded**: unknown fields and duplicate keys are rejected, so a typo
can never silently change a recipe definition (consistent with the main
protocol parser). Files inside the workspace that the agent could modify are
never treated as automatic recipe authorization. Without a catalog,
`run_recipe` fails closed.

### Effective definition digest

Every recipe has a stable SHA-256 **definition digest** over the effective
normalized definition: executable, argv, working directory, declared
capabilities, environment allowlist, timeout and output limits. The recipe id
is the selector; the digest binds the definition. This digest is the basis of
three fail-closed guarantees:

1. **Approval identity**: the approval fingerprint of a `run_recipe` proposal
   is `hash(run_recipe + recipe_id + definition_digest)`. An operator approval
   for one definition of `test` can never authorize a different definition of
   the same id; changing argv, capabilities, environment or limits invalidates
   every prior approval.
2. **Catalog digest**: the digest of the whole effective catalog (sorted
   `id=definitionDigest` pairs) is persisted with the task configuration
   (`config_json.recipe_catalog_digest`). Resume compares the re-supplied
   catalog against it and rejects any drift fail-closed, before any recovery
   or execution side effect. A task that started without a catalog cannot
   silently gain one at resume, and a task that started with a catalog cannot
   resume without it.
3. **Process intent**: the digest is persisted with the TX 1 process intent so
   the attempted definition is auditable even when the catalog changed later.

### Capabilities

Recognized capabilities (strictly validated at catalog load):

| Capability | Meaning |
| --- | --- |
| `read_workspace` | reads files inside the workspace |
| `write_workspace` | writes files inside the workspace |
| `temporary_files` | creates/reads temporary or cache files outside the workspace |
| `execute_repository_code` | executes code from the repository |
| `network` | performs network access |
| `git_metadata` | reads or writes Git metadata of the workspace repository |
| `inherit_environment` | inherits the listed environment variables |

Capability is a declaration of effect, not an aesthetic description. The
policy decides whether the declared set is allowed, denied or requires
operator approval. Unknown capabilities are rejected at catalog load.

## Policy integration

`run_recipe` is a policy-gated effect exactly like the #10 write tools,
sharing the same `internal/policy` seam:

- `--recipe-policy RECIPE=MODE,...` (or `RUNSTEAD_RECIPE_POLICY`) configures
  per-recipe modes `allow`, `deny`, `approval_required`. The default for
  every recipe is `approval_required` (fail closed).
- A recipe with no configured mode defaults to `approval_required`; a recipe
  not in the catalog is denied (`unknown_recipe`) without starting anything.
- Decisions are persisted (`write_policy_decisions` rows with `tool
  = 'run_recipe'` plus `recipe_policy_decision` journal events) with typed
  reasons before any execution decision.
- Approvals come exclusively from the operator control plane
  (`runstead decide <task-id> <action-id> approved|rejected`), keyed by the
  **digest-bound proposal fingerprint** (recipe id + effective definition
  digest). Model prose, reasoning, repository content and tool output can
  never approve a recipe. Capabilities participate in the authorization
  boundary because the fingerprint binds the full effective definition: a
  capability change is a definition change and requires a fresh approval.

### `approval_required` is a real pause

When a recipe requires approval, the run pauses with the typed outcome
`approval_required`:

- the recipe does **not** start;
- no correction budget is consumed;
- no further provider attempt is made to wait for the operator;
- the task is **not** finalized: it stays durably resumable (status
  `running`);
- the CLI reports the task + pending action for `runstead decide`;
- `runstead inspect` lists the pending recipe approval.

After `approved`, a normal `runstead resume` re-proposes the recipe and it
executes. After `rejected`, the rejection persists and the recipe never
executes. A task with a pending recipe approval can never be finalized as
completed.

### The effective recipe policy is durable

The effective `recipe=mode` specification is persisted with the task
configuration (`config_json.recipe_policy`, sanitized, visible in inspect).
Resume always continues under the persisted recipe policy; a divergent
`--recipe-policy` override is rejected fail-closed in the pre-flight. The
recipe catalog must be re-supplied at resume time (like `--scripted`) and must
match the effective catalog the task started with: its digest is persisted
with the task and any drift is rejected before recovery or execution.

## Process execution

The runner (`internal/recipe`) executes the argv directly with
`os/exec.Command(executable, argv...)` and no shell. The child runs in its
own **process group** (`Setpgid`). On timeout or cancellation the whole group
is terminated: SIGTERM, then SIGKILL after a short grace period on Unix.
Termination is a **synchronous barrier**: `recipe.Run` does not return until
the termination routine completed, so no SIGTERM-ignoring child can outlive
the attempt's TX 2. A deliberately daemonized child that escapes its process
group is a documented native limitation, not a silent guarantee.

The child environment is built by `recipe.BuildEnvironment`:

- `PATH` is always passed (so the executable can be resolved);
- `RUNSTEAD_RECIPE_ID=<id>` is always set;
- only names in `allowed_environment` are copied from the parent, and only
  when the recipe declares the `inherit_environment` capability (without it,
  nothing is inherited even when an allowlist is present);
- **credential-shaped names are never inherited**, even if listed
  (case-insensitive markers such as `API_KEY`, `TOKEN`, `SECRET`, `PASSWORD`,
  `COOKIE`, `AUTHORIZATION`, `CHATGPT`, `OMNIROUTE`, `SESSION`). Provider
  credentials can therefore never reach a child process, and recipe catalog
  validation refuses to declare a credential name in the allowlist.

Output is captured per stream with a bounded buffer: retention is capped at
`max_stdout_bytes` / `max_stderr_bytes` while the total observed bytes are
counted. Truncation is explicit evidence (`stdout_truncated`,
`stderr_truncated`, observed and retained byte counts); a truncated output is
never presented as complete.

## Workspace boundary

The recipe working directory is resolved with the same canonical security
model as the read/write tools: relative only, no `..`, no absolute paths, and
`EvalSymlinks` containment inside the configured workspace. A path or parent
component swapped to a symlink pointing outside fails `symlink_escape`
without starting the process. Setting `cwd` inside the workspace does **not**
mean the kernel forbade the process from touching the rest of the host; that
is a documented native limitation, not a sandbox claim.

## Durable execution and recovery

Process execution is an external effect and follows the ADR two-transaction
ordering:

```text
TX 1: persist action + attempt intent ('prepared'), recovery class 4, and the
      bounded process intent (recipe, argv, capabilities, policy decision,
      definition digest);
      append event; COMMIT
      → start/run the process OUTSIDE SQLite; terminate the full process tree
      SYNCHRONOUSLY on timeout/cancellation before TX 2
TX 2: persist the observed result (status + citable process evidence);
      append completion/failure event; COMMIT
```

- No SQLite transaction is ever held open while the process runs.
- A crash after TX 1 leaves a `prepared` class-4 process attempt: the effect
  may have started and is not generically reconcilable, so recovery stops
  automatic continuation with `human_review_required`. It is **never**
  blindly re-executed.
- A completed attempt (including a non-zero exit, a signal, a timeout or a
  cancellation) is terminal verified progress with citable evidence.
- `exit code 0` is **not** conflated with task completion; #11 decides task
  completion from the evidence.

## Evidence

`recipe.Evidence` is the persisted, structured process evidence consumed by
the verifier (#11):

- recipe id, executable, argv and normalized working directory;
- declared capabilities;
- the control-plane policy decision **as actually decided** (for example
  `allowed` / `approved_by_operator`), never a hardcoded placeholder;
- duration, real exit code and terminating signal (negative exit code when
  killed);
- bounded stdout and stderr with observed/retained byte counts and truncation
  flags;
- `timed_out` / `canceled` flags;
- execution and evidence identifiers, annotated before TX 2: `action_id`,
  `execution_id` and `evidence_id` are always filled on persisted evidence;
- `network_isolation = "unenforced"` (the native runner does not enforce
  network isolation; a recipe that declares `network` is simply allowed by the
  operator to touch the network);

Process output is untrusted data. It never grants permissions, changes policy
or concludes task completion.

## Repeat semantics

Running the same recipe twice is legitimate when the workspace changed (for
example `test` fails, a write fixes a file, `test` runs again and passes).
The repeat guard remains a workspace-aware loop-protection mechanism, not a
universal cache: it rejects an identical proposal only while the workspace
signature is unchanged. Execution identity is always the concrete
`execution_id`; a fingerprint is repeat/approval evidence only.

## Configuration

| Flag / env | Meaning |
| --- | --- |
| `--recipes FILE` / `RUNSTEAD_RECIPES` | operator-controlled recipe catalog (JSON array); `run_recipe` fails closed without it |
| `--recipe-policy SPEC` / `RUNSTEAD_RECIPE_POLICY` | `recipe=mode` list, e.g. `test=allow,vet=deny`; default `approval_required` for every recipe |

## Honest limitations

- The native runner provides no kernel-level sandbox. Setting `cwd` to the
  workspace does not restrict the process from accessing other host paths.
- Network isolation is **not enforced** natively; evidence always records
  `network_isolation = "unenforced"`. A recipe that must not touch the
  network is an operator policy decision, not a runtime enforcement.
- Process-group termination covers children in the same group; a process that
  deliberately daemonizes and escapes its group is not guaranteed to die.
- Docker (the #15 development container) is a development boundary, not
  automatically an execution sandbox.
- There is no generic shell, no arbitrary subprocess tool and no
  model-controlled argv. Those remain out of scope; a separately reviewed
  policy boundary would be required before any generic shell exists.
