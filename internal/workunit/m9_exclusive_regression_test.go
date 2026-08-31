package workunit

// Deterministic exclusive-isolation regression for the M9 corpus: a shared
// (read-only) unit must NEVER be dispatched while an exclusive unit is
// RUNNING. The test is causally synchronized -- no absence window, no
// time.After, no timing bound participates in the verdict:
//
// Store-ordering argument:
//
//   - A shared unit is dispatched only via the scheduler's shared fill, which
//     (fixed scheduler) requires the exclusive's settle event, which is sent
//     only after the exclusive's run has ended and its row transitioned to
//     'completed'. Therefore, in the fixed code, EVERY shared entry reads the
//     exclusive row as 'completed': the trap below always passes, causally.
//   - In a reintroduced buggy scheduler, a shared dispatch can land while the
//     exclusive's row is still 'running'. The shared unit's trap reads the
//     DURABLE row at its own entry: if the exclusive is still mid-run the row
//     says 'running' and the trap fires (the exclusive cannot reach
//     'completed' until its run ends, and the shared's entry precedes its own
//     completion). The only dispatch that escapes the trap is one that lands
//     after the exclusive durably completed, which is not an observable
//     overlap. There is no timing window anywhere in this argument.
//
// The exclusive unit additionally performs a deterministic store-work window
// (real SQLite roundtrips) so a reintroduced buggy dispatch commonly lands
// while its row is still 'running', and a secondary trap at its run end
// reports any other unit still 'running'. The chain error and the violations
// channel are drained at the end; both are checked after the chain settles.

import (
	"context"
	"fmt"
	"testing"

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
			// Capacity covers every unit: each mis-dispatched shared unit and
			// the exclusive's own secondary trap may each report once
			// without ever blocking a worker (the chain must always settle).
			violations := make(chan string, concurrency+2)
			run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
				if unit.WorkUnitID == "wu-x" {
					exclusiveEntered <- struct{}{}
					waitFor(t, releaseExclusive, "exclusive unit release")
					// Deterministic store-work window: while it runs, any
					// shared dispatch that overlaps the exclusive lands with
					// the exclusive's durable row still 'running'.
					for i := 0; i < 40; i++ {
						if _, err := store.ListWorkUnits(ctx, unit.TaskID); err != nil {
							return RunResult{}, err
						}
					}
					// Secondary trap: no other unit may be in flight when the
					// exclusive run ends.
					others, err := store.ListWorkUnits(ctx, unit.TaskID)
					if err != nil {
						return RunResult{}, err
					}
					for _, other := range others {
						if other.WorkUnitID != "wu-x" && other.Status == "running" {
							return m9Violation("exclusive run ended while "+other.WorkUnitID+" was running", violations)
						}
					}
					return completeRun(t, store, unit)
				}
				// Primary trap, store-authoritative: the exclusive row must
				// already be durably completed. In the fixed scheduler this is
				// causally guaranteed (shared dispatch requires the
				// exclusive's settle after its completed transition); a
				// reintroduced bug that dispatches while the exclusive is
				// still running makes this read 'running' and fires.
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
			close(releaseExclusive)
			// Let the chain settle to durable states before asserting, so the
			// verdict never interrupts an active worker goroutine.
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
