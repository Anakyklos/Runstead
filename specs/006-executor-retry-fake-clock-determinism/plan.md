# Implementation Plan: Deterministic Retry Fake-Clock Synchronization

**Branch:** `fix/115-deterministic-retry-fake-clock`

**Issue:** #115

**Spec:** `specs/006-executor-retry-fake-clock-determinism/spec.md`

## Scope

Test-only determinism fix: observable timer-registration event in the fake
clock + deterministic await in the tests that order `waitForCalls` →
(backoff timer registration) → `Advance`. No production semantics change
unless an independent production defect is demonstrated.

## Sequence

1. Reproduce/analyze the interleaving hazard (`waitForCalls` vs `NewTimer`).
2. Add the registration event + `awaitTimerRegistered` to `fakeClock`.
3. Insert the await in every test that provably registers a backoff timer;
   leave the observer-error/TX2-failure tests untouched (no timer exists).
4. Run the mandatory repetitions (200x plain, 100x race, affected suites).
5. Run the full repository validation and CI quality gates.
6. Register the SpecKit slice and open one PR against `main`.

## Validation matrix

| Requirement | Evidence |
| --- | --- |
| Deterministic ordering (timer armed before Advance) | `awaitTimerRegistered` + fireAt invariant assertion in the observer test |
| No sleep/polling as synchronization | registration channel is the only synchronization; wall-clock select is a failure guard |
| Production retry semantics unchanged | `executor.go` untouched; every retry test still proves new admission/accounting/eligibility/cancellation |
| High repetition stability | 200x plain + 100x race on the main test; 100x plain + 50x race on the affected suite |
| Full suite green | `go test ./...`, `go test -race ./...`, vet, build, protocol, quality gates |

## Out of scope proof

Final diff contains no `time.Sleep` synchronization, no timeout increases, no
retry-policy changes, no governor changes, no production code changes unless
independently demonstrated.