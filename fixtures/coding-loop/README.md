# fixtures/coding-loop

Deterministic sample repository for the #12 inspect-edit-test-fix coding loop
(M4 — verifiable coding agent).

This fixture is deliberately small but not trivial: it is a real Go module
whose test suite genuinely fails until the implementation is corrected. A
Runstead task against this workspace must walk the real loop — inspection,
scoped writes, recipe execution, real failure evidence, diagnosis, corrective
write, passing rerun, Git observation, independent verification — before the
task can reach `completed`.

## Layout

```text
fixtures/coding-loop/
  README.md         this document
  go.mod            fixture scenario module (keeps the fixture out of the Runstead module)
  acceptance.json   operator acceptance plan (recipe_exit_zero for the test recipe)
  recipes.json      operator-declared recipe catalog (test = `go test ./...` in app/)
  app/
    go.mod          module runstead.fixture.calc (no external dependencies)
    calc.go         the intentionally buggy implementation (no whitespace trimming)
    calc_test.go    the real test suite; fails against the initial implementation
  fixes/
    calc-wrong.go   the deterministic FIRST (insufficient) fix: trims only in SumValues
    calc-correct.go the deterministic CORRECTIVE fix: trims inside ParseValues
```

## The task

"Fix the calculator so the test suite passes." The initial implementation
contains one real bug: `ParseValues` does not trim whitespace, so
`ParseValues("1, 2 , 3")` fails with a parse error.

The deterministic trajectory (produced by the scripted provider in the E2E
tests) requires:

1. inspection of multiple files (`README.md`, `app/calc.go`, `app/calc_test.go`);
2. a first scoped write that fixes only `SumValues` (trimming there) — the
   test suite STILL fails, because `TestParseValuesTrimsWhitespace` requires
   the fix inside `ParseValues`;
3. a real failing recipe execution (`go test ./...` exits non-zero) with
   bounded stdout/stderr evidence;
4. diagnosis from the real process evidence;
5. a second, corrective write (trim inside `ParseValues`);
6. a passing rerun of the same recipe (exit 0);
7. final Git observation and acceptance verification.

So the fixture genuinely requires at least two write effects and at least one
real failing test execution, and a premature completion proposal is refused
by the verifier (`recipe_exit_zero` fails while the suite is red).

## How the E2E tests use it

The tests copy `fixtures/coding-loop` into a fresh temp directory, initialize
a real Git repository there (`git init` + commit), and run the real
`runstead run`/`resume` CLI composition with:

- `--scripted` responses (the deterministic model trajectory, built with the
  real file hashes at test time);
- `--recipes fixtures/coding-loop/recipes.json` and
  `--recipe-policy test=allow`;
- `--write-policy write_file=allow`;
- an acceptance plan (the committed `acceptance.json` plus, in the full
  scenario, a `file_hash` check on the corrected `app/calc.go`).

The recipe runs the real `go` tool. No network, no Docker and no user secret
is required; the fixture module has no external dependencies, so the tests
work offline in CI.

## Seams for #13 (fault injection)

The fixture is structured so #13 can reuse it without rewriting the scenario:

- the scenario is data-driven: the scripted responses, recipes, acceptance
  plan and workspace are all parameters of the test helpers;
- interruption: the runtime crash seam (`state.SetCrashPoint` via the
  subprocess helpers in `cmd/runstead`) already targets every persistence
  boundary (task start, provider TX 1/TX 2, tool TX 1/TX 2, verification
  recorded, recovery transitions), so #13 can kill the loop at any point of
  this trajectory and assert resume behavior;
- provider failure: the scripted provider can be replaced by any
  `provider.Client` (the fake supports fail injection);
- process failure: the recipe catalog is operator input; changing
  `recipes.json` changes the definition digest and is rejected as catalog
  drift on resume (fail-closed), while process failures are produced by the
  real `go test` exit status;
- write uncertainty: a prepared write attempt (crash between tool TX 1 and
  TX 2) is reconciled from observable filesystem state; #13 can crash inside
  that window with the existing seam.

The acceptance plan check ids (`tests-pass`, and the scenario's `fix-hash`)
are stable identifiers that the #13 assertions can rely on.
