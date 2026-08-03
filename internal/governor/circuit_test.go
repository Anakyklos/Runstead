package governor

import (
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type circuitTestClock struct{ now time.Time }

func (c *circuitTestClock) Now() time.Time { return c.now }
func (c *circuitTestClock) NewTimer(time.Duration) Timer {
	return circuitTestTimer{c: make(chan time.Time)}
}

type circuitTestTimer struct{ c chan time.Time }

func (t circuitTestTimer) C() <-chan time.Time { return t.c }
func (t circuitTestTimer) Stop() bool          { return true }

func TestOutcomeRateResponseBeforePreviousResetOpensCircuit(t *testing.T) {
	clock := &circuitTestClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
	config := DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	governor, err := New(config, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	reset := clock.Now().Add(time.Minute)
	governor.mu.Lock()
	first := &Permit{governor: governor, request: AttemptRequest{TaskID: "task"}, started: true, attemptSequence: 1}
	governor.recordOutcomeLocked(clock.Now(), first, Outcome{Class: OutcomeRateCapacity, ResetAt: reset})
	clock.now = clock.now.Add(time.Second)
	second := &Permit{governor: governor, request: AttemptRequest{TaskID: "task"}, started: true, attemptSequence: 2}
	governor.recordOutcomeLocked(clock.Now(), second, Outcome{Class: OutcomeRateCapacity, ResetAt: reset})
	snapshot := governor.circuitSnapshotLocked()
	governor.mu.Unlock()

	if snapshot.State != CircuitOpenUntil {
		t.Fatalf("circuit state = %q, want %q", snapshot.State, CircuitOpenUntil)
	}
	if want := reset.Add(config.ResetSafetyMargin); !snapshot.OpenUntil.Equal(want) {
		t.Fatalf("circuit open_until = %s, want %s", snapshot.OpenUntil, want)
	}
}
