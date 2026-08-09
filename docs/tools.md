# Tool registry

The registry in `internal/tools` is the concrete implementation of the
existing `protocol.ToolCatalog` seam. It started as strictly observational
(issue #6); issue #10 adds two policy-gated write tools (`write_file`,
`apply_patch`). There is still no shell, network access, plugin loading or
model-controlled command arguments. See `docs/writes.md` for the full write
safety model; this page covers the registry surface.

## Tools and arguments

The registry accepts exactly these JSON argument objects:

```json
{"path":"relative/file.txt"}
```

for `read_file`, and:

```json
{"path":"relative/directory"}
```

for `list_files`.

```json
{"query":"text to find","path":"relative/path"}
```

for `search_text`.

```json
{}
```

for `git_status` and `git_diff`.

```json
{"path":"relative/file.txt","content":"new content","expected_before_hash":"<sha256 or \"absent\">"}
```

for `write_file`.

```json
{"path":"relative/file.txt","patch":"--- ...\n+++ ...\n@@ -S,C +S,C @@\n...","expected_before_hash":"<sha256>"}
```

for `apply_patch`.

Unknown tools, unknown fields, missing fields, wrong JSON types, empty paths
and empty queries are rejected before execution. Validation performs no tool
execution. `Execute` validates again so a direct caller cannot bypass the
registry boundary.

`read_file` returns the file content (bounded) plus a `sha256` of the
COMPLETE file content, which is the stale-state precondition source for the
write tools. `list_files` returns one directory level, sorted by normalized
relative path. Entries are labeled as regular files, directories, symlinks or
other targets; symlinks are listed but never followed by the listing
operation.

## Workspace boundary

The configured workspace is made absolute and canonical once. Tool paths must
be relative, cannot contain `..` components or absolute volume names, and must
exist. The resolver uses `filepath.EvalSymlinks` and a `filepath.Rel` directory
boundary check; textual prefix checks are not used. A symlink that resolves
outside the workspace fails with `symlink_escape`. Internal symlinks are
allowed for file reads and searches, while directory listings preserve their
symlink type without following them.

Write targets use the same canonical security model plus fail-closed rules
for symlinks: a write never follows or replaces a symlink, even an internal
one. Missing parent directories and non-regular targets are typed failures.
The effect boundary revalidates canonical containment and the before-state
immediately before the rename (a revalidation, not a compare-and-swap; see
`docs/writes.md` for the honest residual limitation).

Observations return normalized slash-separated paths relative to the workspace.

## Limits and observations

Limits are configured through `tools.Limits` and default to:

| Output or operation | Default |
| --- | ---: |
| File bytes returned | 64 KiB |
| Directory entries returned | 256 |
| Search matches returned | 256 |
| Search output bytes | 128 KiB |
| Git stdout bytes | 64 KiB |
| Git stderr bytes | 16 KiB |
| Write content bytes | 256 KiB |
| Patch argument bytes | 128 KiB |
| Patch target file bytes | 4 MiB |
| Diff evidence bytes | 8 KiB |
| Search timeout | 2 seconds |
| Git timeout | 2 seconds |

Every execution gets an opaque runtime-generated ID such as
`obs-000001`. The model cannot provide or choose it. An observation includes
the normalized arguments, success flag, structured data or typed failure,
truncation flag, returned/original counts where known, source/backend metadata,
and an `untrusted` marker for file, search and Git data.

Truncation is deterministic and never silent. The observation keeps the valid
prefix or first sorted results, sets `truncated: true`, and reports original
and returned byte, entry or match counts. Truncation is a successful partial
observation, not permission to claim that the complete result was returned.

File reads accept only regular files and valid UTF-8 text without NUL bytes.
Binary and invalid UTF-8 files fail with distinct typed codes. A byte limit is
applied without returning an incomplete UTF-8 suffix.

## Search and Git execution

`search_text` uses `rg` when it is found. It invokes the executable directly
with a fixed argv, `--no-config`, fixed-string matching, JSON output, and `--`
before model-controlled query/path values. If `rg` is unavailable, a portable
Go fallback uses `filepath.WalkDir`, does not follow symlinks, sorts files, and
returns the same `SearchMatch` shape. The fallback explicitly counts binary
and invalid-UTF-8 files that it skips.

`git_status` and `git_diff` invoke the real `git` executable directly from the
canonical workspace with fixed argv, no pager/colors, no external diff or
textconv, disabled optional locks, and a workspace pathspec. They never accept
Git arguments from the model and never run staging, commit, checkout, reset,
clean or push operations. Exit codes and signals are retained; exit code 128
is reported as `not_git_repository`, and other non-zero exits are typed Git
failures. Git stdout and stderr are separately bounded.

All file contents, search matches and Git output are environment observations,
not trusted control data. The registry does not log their contents or copy
them into failure messages.

## Write tools

`write_file` and `apply_patch` are policy-gated write tools: the agent loop
consults the control-plane policy before any execution decision, and approval
comes only from persisted operator records (`runstead decide`), never from
model output. Both tools require `expected_before_hash` (the sha256 reported
by `read_file`, or `absent` for a new file) and refuse to execute when the
current state no longer matches. Effects are temp-file-plus-rename, executed
outside any SQLite transaction, and every successful write returns structured
`WriteEvidence` (before/after hashes, byte count, change kind, bounded diff).
See `docs/writes.md` for the complete safety, approval, evidence and
reconciliation contract.

## Process recipes

`run_recipe` is the policy-gated process-recipe tool: the model selects a
recipe ID from the operator-controlled catalog and never supplies a command,
argv or shell string. The recipe declares its executable, fixed argv,
working directory (inside the workspace), timeout, output limits,
capabilities and an environment allowlist. The runner executes argv directly
(no `sh -c`), starts the process in its own process group and terminates the
full group on timeout or cancellation. The child environment is built from an
explicit allowlist; provider credential-shaped names are never inherited.
stdout and stderr are bounded independently with explicit truncation, and the
observed result becomes structured `recipe.Evidence` (real exit code, signal,
duration, bounded output, truncation flags, declared capabilities, policy
decision and `network_isolation = "unenforced"`). `run_recipe` is gated by
`--recipe-policy` (default `approval_required`), shares the approval pause and
durable policy semantics of the write tools, and its attempts are recovery
class 4 (a crashed prepared process attempt stops with
`human_review_required`, never blindly re-run). See
`docs/process-runner.md` for the full contract and native limitations.
