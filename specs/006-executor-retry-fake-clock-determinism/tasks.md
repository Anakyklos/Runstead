# Tasks: Deterministic Retry Fake-Clock Synchronization

**Feature:** `006-executor-retry-fake-clock-determinism`

**Issue:** #115

- [x] T001 Analyze the `waitForCalls -> Advance` interleaving hazard and prove it is harness-side (call counter visibility does not order `fakeClock.NewTimer` against `Advance`; production `retryBackoff` has no such hazard because the real clock advances independently).
- [x] T002 Add the observable timer-registration event to `fakeClock` (`NewTimer` signals a buffered channel, non-blocking sender) and `awaitTimerRegistered` (failure-guard wall-clock select only).
- [x] T003 Insert the deterministic await in every test that provably registers a backoff timer (`TestExecutorObserverCalledExactlyOncePerAdmittedAttempt`, `TestExecutorRetryUsesNewAdmissionAndAccounting`, `TestExecutorRetryBudgetExhaustion`, `TestExecutorProfileCooldownInput`, `TestExecutorTimerFiresOnceNoDuplicateDispatch`, `TestExecutorCancelDuringBackoffNoDispatchNoDebit`, `TestExecutorObserverCancelDuringBackoffObservesOnce`), including an armed-before-advance invariant assertion in the observer test.
- [x] T004 Leave the no-timer tests untouched (observer error, TX2 persistence failure): `Advance` is a no-op and `Execute` returns without any timer dependency.
- [x] T005 Mandatory repetitions: main test 200x plain + 100x race; affected suite 100x plain + 50x race. All green.
- [x] T006 Full repository validation (test/vet/build/race/protocol/quality gates) green; `executor.go` and production semantics untouched.
- [x] T007 Open one PR against `main` with `Closes #115`.

## Validation log

| Run | Command | Result |
| --- | --- | --- |
| R1 | `go test ./internal/agent -run '^TestExecutorObserverCalledExactlyOncePerAdmittedAttempt$' -count=200` | ok (1.198s) |
| R2 | `go test -race ./internal/agent -run '^TestExecutorObserverCalledExactlyOncePerAdmittedAttempt$' -count=100` | ok (1.674s) |
| R3 | affected suite `-count=100` (7 tests) | ok (4.917s) |
| R4 | affected suite `-count=50` with race (7 tests) | ok (7.399s) |
| R5 | pre-fix reproduction `-race -count=60` | ok (1.399s, window not hit locally; CI hit it under adverse scheduling) |