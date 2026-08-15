# Runstead quality gates

Deterministic, offline CI quality gates for the Runstead repository.
This is development tooling only: it is a separate Go module, never
imported by the Runstead binary, and adds no runtime dependency.

Every gate is hermetic: it needs no credentials, no provider, no Docker and
makes no network calls. The `errcheck` gate shells out to the local Go
toolchain (`go list -export` with `GOPROXY=off`): it reads only local package
source and build-cache export data, compiles export data on demand from the
local module cache and GOROOT (a cold build cache works), and fails fast if a
module dependency is missing from the local module cache instead of fetching
it. The CI job runs `go test ./...` before the gates, which populates the
module cache.

## Usage

```sh
go build -o /tmp/quality-gates .
/tmp/quality-gates growth --root /path/to/runstead
/tmp/quality-gates errcheck --root /path/to/runstead
/tmp/quality-gates live-convention --root /path/to/runstead
```

`--root` defaults to the current directory. Exit codes: 0 = pass,
1 = violations, 2 = usage or tooling error.

## Gates

### growth

Bounded-growth guards for the Go tree:

- max source (non-test) file length in lines;
- max test file length in lines (test files may grow larger than source
  files; the current tree justifies the distinction);
- max number of non-test `.go` files per package;
- max total `.go` files per package.

Limits live in `limits.json` (embedded at build time). They start green
on the main tree with generous headroom: the largest source file is
`internal/agent/loop.go` (1346 lines, limit 1800), the largest test file
is `internal/governor/governor_test.go` (1497 lines, limit 2400), and the
largest packages are `internal/state` (15 source / 31 total files, limits
40 / 60). Raising a limit is an explicit, reviewable change to
`limits.json` in the PR; the failure message always shows the
file/package, the observed value and the limit.

Directories named `.git`, `vendor`, `fixtures`, `experiments`, `scratch`,
`testdata`, `.omx` and `.superpowers` are excluded. The tree currently
has no generated Go files; if generated files are introduced, exclude
them explicitly in `limits.json` as part of that change.

### errcheck

Type-accurate swallowed-error detection, equivalent to `errcheck`'s
coverage. Using `go/types`, the gate flags every discarded value in a
non-test Go file whose static type implements `error`:

- blank identifiers bound to an error-typed value: `_ = f()`,
  `x, _ := f()` / `x, _ = f()` where the discarded value is an error,
  and `_, _ = f()` for multi-error returns;
- bare call statements whose call has at least one error-typed result:
  `os.Remove(path)`, and multi-value calls such as `fmt.Println(...)`
  whose error component is discarded;
- `defer f()` and `go f()` where the call has at least one error-typed
  result. Policy: `defer` and `go` discard their results exactly like a
  bare call, so they are swallowed errors; there is no silent exclusion
  for "best-effort cleanup". Such sites are reviewed through the
  explicit allowlist, one entry per site.

Because resolution is type-based, type assertions (`x, _ := v.(T)`), map
lookups and channel receives (which discard `bool`, not `error`) are
never reported, and concrete custom types implementing `error` are
reported. Channel receives and select cases are out of scope even when
the received element implements `error`: the policy covers function and
method call results, not value consumption through channels. Test files
are out of scope: the test suite deliberately discards errors in
scaffolding (mock HTTP writers, setup/teardown).

A small, explicit excluded set mirrors `errcheck`'s documented defaults
and is never reported: in-memory buffer writes (`bytes.Buffer`,
`strings.Builder`), `fmt` output printing (`Print*`, `Fprint*`), `io.Copy*`,
`math/rand.Read` and `os.Stdout/Stderr.Write`. The set is a reviewed
policy list in `errcheck.go`, not a heuristic: any other call whose
identity resolves to a statically known function is reported. Adding or
removing an entry is a deliberate gate change.

Findings fail the gate unless they are listed in `errcheck.allowlist`
(embedded at build time) with a justification. The allowlist is
line-anchored: an entry only covers the exact file, line and source text
it records. The current baseline is 85 reviewed sites: 20 blank
identifiers and 65 cleanup sites surfaced by the widened bare-call /
`defer` / `go` coverage.

Process:

- Fix a swallowed error: update the code and DELETE the allowlist entry.
- Allow a new one: add the finding line (copied from the gate output) in
  the same PR with a concrete justification.
- An allowlist entry that no longer matches a current finding is a
  "stale entry" and fails the gate, so exceptions cannot silently go
  dead.

### live-convention

Per-function opt-in live test convention. A function in a `_test.go` file
that reads a `RUNSTEAD_LIVE_*` environment variable (via `os.Getenv` or
`os.LookupEnv`) must call a skip method (`Skip`, `Skipf` or `SkipNow`) on
its own testing object (`*testing.T`, `*testing.B` or `*testing.F`)
within its own body, so the default `go test ./...` path stays hermetic
and live provider tests are skipped unless the operator explicitly opts
in:

```go
func TestLiveOmniRoute(t *testing.T) {
    if os.Getenv("RUNSTEAD_LIVE_OMNIROUTE") != "1" {
        t.Skip("set RUNSTEAD_LIVE_OMNIROUTE=1 to enable the live check")
    }
    // ...
}
```

The association is structural and per function: a `t.Skip` in another
test or helper in the same file does not protect the function that reads
the variable, and a `Skip` method on an object that is not the function's
testing object does not count. A read at package scope (which cannot be
guarded by any test skip) and a read in a function without a testing
object both fail closed. The skip is attributed by receiver name, so a
nested closure (for example a `t.Run` subtest) that shadows the
parameter name is a documented approximation: the gate is structural,
not a formal flow analysis, and does not prove that the skip is reachable
before every live read. The existing opt-in check in
`internal/provider/omniroute/live_test.go` follows this convention.

## Self-tests

```sh
go test -count=1 ./...
go vet ./...
```

The suite uses temporary synthetic fixtures (never modifies the working
tree), proves the baseline passes on the current tree, proves negative
cases fail (oversized file, oversized package, representative swallowed
error, missing `t.Skip`), proves the allowlist suppresses and detects
stale entries, and asserts non-zero exit codes by executing the built
binary.
