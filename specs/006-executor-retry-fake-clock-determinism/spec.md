# Deterministic Retry Fake-Clock Synchronization

**Feature branch:** `fix/115-deterministic-retry-fake-clock`

**Issue:** #115

**Status:** Proposed implementation slice

## Goal

Remove the synchronization race in the retry/observer tests that use
`waitForCalls(...)` followed by `clock.Advance(...)`. The tests must know, from
an observable harness event, that the retry backoff timer was registered BEFORE
the fake clock advances, so they are deterministic even under `-race` and
unfavorable scheduling. Production retry semantics must not change.

## Root cause

`scriptedClient.Complete` increments the call counter, and `waitForCalls`
observes that counter. The executor, however, registers the backoff timer in
`retryBackoff` AFTER the governor returns and the call counter was already
visible. `waitForCalls` therefore does not order the timer registration against
`clock.Advance`: the test may advance the clock before `fakeClock.NewTimer`
computes `fireAt`, arming the timer relative to an already-advanced time, after
which no further advance fires it and `<-done` blocks forever (observed as a
10-minute CI timeout).

## Design

The fake clock gains an observable registration event:

- `fakeClock.NewTimer` signals every registration on a buffered channel
  (non-blocking sender: the executor never waits on the signal).
- `fakeClock.awaitTimerRegistered(t)` blocks the test until a registration
  event is received and returns the registered timer. A wall-clock select is a
  FAILURE GUARD ONLY, never synchronization.
- Ordering guarantee in this harness: the first timer registered after the
  first physical call is the executor's retry-backoff timer; governor
  admission pacing timers can only appear at the RETRY admission, which
  happens after the backoff registration.

Tests that provably register a timer (retry/backoff paths) insert
`clock.awaitTimerRegistered(t)` between `waitForCalls` and `clock.Advance`
(and before `cancel()` in the cancellation tests). Tests where the loop stops
before any timer exists (observer failure, TX2 persistence failure) are safe by
construction: `Advance` is a no-op and `Execute` returns without any timer
dependency.

## Non-goals

No changes to `internal/agent/executor.go` unless an independent production
defect is demonstrated. No governor/retry semantic changes, no new retry
policy, no fallback, no sleep/polling synchronization, no timeout increases.

## Acceptance evidence

- The fake clock cannot be advanced prematurely in the affected tests.
- The synchronization is a real observable harness event (timer registration).
- `TestExecutorObserverCalledExactlyOncePerAdmittedAttempt` still proves: 2
  physical calls, 2 observations, `Retry` false/true, distinct request ids,
  `-r1` suffix, final success.
- Cancellation and boundedness tests keep their invariants.
- High repetitions pass without race and under `-race` (see tasks.md).
- Full repository validation green.