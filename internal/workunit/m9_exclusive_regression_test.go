package workunit

// Deterministic exclusive-isolation regression for the M9 corpus: a shared
// (read-only) unit must NEVER be dispatched while an exclusive unit is
// RUNNING. The check is store-authoritative: the exclusive unit blocks on a
// release channel; any shared unit dispatched during that window verifies the
// exclusive row is still durably 'running' and reports a violation. The test
// waits for the violation with a detection bound (deadlock-detector role,
// never an assertion), then releases the exclusive. Before the scheduler
// honored this isolation the violation fired within milliseconds (the shared
// fill issued while the exclusive was active); after the fix no shared unit
// is ever dispatched during the window and the test completes after the
// detection bound.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/state"
)

func TestM9EvidenceExclusiveIsolationRegression(t *testing.T) {
	for _, concurrency := range []int{2, 4} {
		concurrency := concurrency
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			taskID := fmt.Sprintf("m9-excl-isolation-%d", concurrency)
			driver, store := newSchedulerDriver(t, taskID, concurrency)
			ctx := context.Background()
			definitions := append([]Definition{
				{WorkUnitID: "wu-x", Objective: "exclusive unit", Tools: []string{"write_file"}},
			}, readOnlyDefs("wu", concurrency)...)
			if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
				t.Fatal(err)
			}
			exclusiveEntered := make(chan struct{}, 1)
			releaseExclusive := make(chan struct{})
			violations := make(chan string, 1)
			run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
				if unit.WorkUnitID == "wu-x" {
					exclusiveEntered <- struct{}{}
					waitFor(t, releaseExclusive, "exclusive unit release")
					return completeRun(t, store, unit)
				}
				// Store-authoritative isolation check: the exclusive row must
				// already be durably completed. A shared unit dispatched while
				// the exclusive is still running is an overlap violation.
				exclusiveUnit, err := store.GetWorkUnit(ctx, unit.TaskID, "wu-x")
				if err != nil {
					return RunResult{}, err
				}
				if exclusiveUnit.Status != "completed" {
					return m9Violation("shared unit dispatched while exclusive unit is "+exclusiveUnit.Status, violations)
				}
				return completeRun(t, store, unit)
			}
			errCh := runChain(t, driver, ctx, run)
			waitFor(t, exclusiveEntered, "exclusive unit to enter")
			// Detection window: a buggy scheduler dispatches a shared unit
			// within milliseconds while the exclusive is blocked; a correct
			// scheduler dispatches nothing until the exclusive settles. The
			// 1s bound is a detector for the buggy case, never a timing
			// assertion (the release of the exclusive is the synchronization).
			select {
			case reason := <-violations:
				t.Fatalf("exclusive isolation violation: %s", reason)
			case <-time.After(time.Second):
				close(releaseExclusive)
			}
			if err := waitChain(t, errCh); err != nil {
				t.Fatalf("RunAll(): %v", err)
			}
			select {
			case reason := <-violations:
				t.Fatalf("exclusive isolation violation: %s", reason)
			default:
			}
			for i := 1; i <= concurrency; i++ {
				unit, err := store.GetWorkUnit(ctx, taskID, fmt.Sprintf("wu-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				if unit.Status != "completed" {
					t.Fatalf("wu-%d = %s, want completed", i, unit.Status)
				}
			}
		})
	}
}
