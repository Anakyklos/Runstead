# Read-only tool registry

Issue #6 adds a small static registry in `internal/tools`. It is the concrete
implementation of the existing `protocol.ToolCatalog` seam. The registry is
strictly observational: it has no write tools, shell, network access, plugin
loading or model-controlled command arguments.

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

Unknown tools, unknown fields, missing fields, wrong JSON types, empty paths
and empty queries are rejected before execution. Validation performs no tool
execution. `Execute` validates again so a direct caller cannot bypass the
registry boundary.

`list_files` returns one directory level, sorted by normalized relative path.
Entries are labeled as regular files, directories, symlinks or other targets;
symlinks are listed but never followed by the listing operation.

## Workspace boundary

The configured workspace is made absolute and canonical once. Tool paths must
be relative, cannot contain `..` components or absolute volume names, and must
exist. The resolver uses `filepath.EvalSymlinks` and a `filepath.Rel` directory
boundary check; textual prefix checks are not used. A symlink that resolves
outside the workspace fails with `symlink_escape`. Internal symlinks are
allowed for file reads and searches, while directory listings preserve their
symlink type without following them.

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
