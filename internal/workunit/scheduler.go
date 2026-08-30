package workunit

// Bounded shared/exclusive scheduler (issue #109, M9 Stage B1).
//
// The scheduler is a strict extension of the Stage A serial driver: ready
// selection comes from the SAME persisted deterministic ordering
// (ReadyWorkUnits), unit loops remain the ONLY execution path (RunFunc), and
// every outcome transition reuses the Stage A store contract. What Stage B1
// adds is the dispatch policy: provably read-only (shared-lane) units may
// overlap up to the operator bound; every other unit (exclusive lane) never
// overlaps anything. The scheduler lives entirely inside this package; it
// creates no worker framework, daemon, service or external concurrency.

import (
	"context"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// settleEvent reports one dispatched unit's durable outcome to the scheduler
// main loop. Outcome values mirror RunResult.Outcome plus "interrupted" for
// an unknown outcome (unit left running for recovery reset).
type settleEvent struct {
	unitID  string
	outcome string
	err     error
}

// boundedScheduler runs one task's Work Unit chain under the shared/exclusive
// policy. Only the main loop goroutine dispatches and counts; one worker
// goroutine per dispatched unit performs the bounded run and its outcome
// transition, then sends exactly one settle event.
type boundedScheduler struct {
	driver      *Driver
	runFunc     RunFunc
	ctx         context.Context
	concurrency int

	// settleCh is buffered to at least concurrency so workers never block on
	// send while the main loop drains on a stop path.
	settleCh chan settleEvent
	// active is the number of dispatched-but-not-settled units. It never
	// exceeds concurrency.
	active int
	// stop is the first hard stop condition (error, cancellation or
	// non-completed terminal outcome). Once set, no new unit is dispatched
	// and the scheduler only drains the active batch to durable states.
	stop error
}

// run implements the main scheduling loop. Termination rules:
//
//   - ctx cancellation: stop dispatching, drain the active batch (their
//     workers settle under the canceled context), return the context error.
//   - first non-completed terminal outcome (failed/blocked/uncertain): NO
//     new batch starts; the current bounded batch settles to durable states;
//     then ErrWorkUnitBlockedChain (parent gate stays open).
//   - canceled/unknown outcomes: unit stays running (recovery reset), the
//     wrapped context.Canceled / interrupted error returns after drain.
//   - a ready EXCLUSIVE unit blocks ALL new shared dispatch until it runs:
//     a ready exclusive can never be starved by later read-only work, and an
//     exclusive never overlaps another unit (it starts only at active == 0).
func (s *boundedScheduler) schedule() error {
	for {
		if err := s.ctx.Err(); err != nil {
			if s.stop == nil {
				s.stop = err
			}
			return s.drain(s.stop)
		}
		if s.stop != nil {
			return s.drain(s.stop)
		}
		// Operator-resolved approvals unblock units that paused on
		// approval_required: the transition is tied to authoritative
		// resolution state (zero pending approvals for the unit's own
		// actions), never to an arbitrary blocked reason (issue #106).
		if err := s.driver.resolveBlockedWorkUnits(s.ctx); err != nil {
			return s.drain(err)
		}
		ready, err := s.driver.Store.ReadyWorkUnits(s.ctx, s.driver.TaskID)
		if err != nil {
			return s.drain(err)
		}
		if len(ready) == 0 {
			if s.active == 0 {
				return nil
			}
			s.settle()
			continue
		}

		// Exclusive-first dispatch: while any ready unit is exclusive, NO
		// shared unit is dispatched. The first ready exclusive starts as soon
		// as the active batch drains (deterministic: ready order). This is
		// the anti-starvation rule of issue #109.
		exclusiveIndex := -1
		for index, unit := range ready {
			if Classify(unit.Tools) == LaneExclusive {
				exclusiveIndex = index
				break
			}
		}
		if exclusiveIndex >= 0 {
			if s.active > 0 {
				s.settle()
				continue
			}
			if err := s.dispatch(ready[exclusiveIndex]); err != nil {
				return s.drain(err)
			}
			continue
		}

		// Shared fill: dispatch ready read-only units in deterministic order
		// up to the configured bound.
		dispatched := false
		for _, unit := range ready {
			if s.active >= s.concurrency {
				break
			}
			if err := s.dispatch(unit); err != nil {
				return s.drain(err)
			}
			dispatched = true
		}
		if !dispatched {
			// Bound reached or a settle is pending: block on the next
			// durable outcome instead of spinning.
			s.settle()
		}
	}
}

// dispatch validates the envelope against the live parent contract, persists
// created -> ready -> running BEFORE any dispatch (the Stage A invariant:
// a running unit is durable before its loop starts), then hands the bounded
// run to its own worker goroutine.
func (s *boundedScheduler) dispatch(unit state.WorkUnit) error {
	// Re-validate the envelope against the live parent contract before any
	// effect (escalation can never sneak in after create).
	if err := s.driver.ValidateEnvelope(unit.Tools, unit.WorkspaceScope); err != nil {
		return err
	}
	if unit.Status == "created" {
		if err := s.driver.Store.TransitionWorkUnit(s.ctx, s.driver.TaskID, unit.WorkUnitID, "created", "ready", ""); err != nil {
			return err
		}
	}
	if err := s.driver.Store.TransitionWorkUnit(s.ctx, s.driver.TaskID, unit.WorkUnitID, "ready", "running", ""); err != nil {
		return err
	}
	s.active++
	go s.worker(unit)
	return nil
}

// worker runs ONE unit through the caller-provided RunFunc (the composition
// root wires agent.Loop there; the scheduler never calls provider.Client or
// tools) and applies the Stage A outcome transition. The worker sends
// exactly one settle event and exits, so the scheduler can always drain.
func (s *boundedScheduler) worker(unit state.WorkUnit) {
	result, runErr := s.runFunc(s.ctx, unit)
	event := settleEvent{unitID: unit.WorkUnitID}
	if runErr != nil {
		// The unit stays 'running': recovery reset handles interruption; the
		// error propagates without a fabricated terminal state.
		event.err = runErr
		s.settleCh <- event
		return
	}
	switch result.Outcome {
	case "completed":
		decision, found, decisionErr := s.driver.Store.LatestWorkUnitVerification(s.ctx, s.driver.TaskID, unit.WorkUnitID)
		if decisionErr != nil {
			event.err = decisionErr
			s.settleCh <- event
			return
		}
		if !found || decision != "passed" {
			// Evidence-backed verification is mandatory: narrative alone
			// never completes a unit (issue #106).
			reason := "verification did not pass for work unit"
			if found {
				reason = "verification decision " + decision
			}
			if err := s.driver.Store.TransitionWorkUnit(s.ctx, s.driver.TaskID, unit.WorkUnitID, "running", "blocked", reason); err != nil {
				event.err = err
				s.settleCh <- event
				return
			}
			event.outcome = "blocked"
			s.settleCh <- event
			return
		}
		if err := s.driver.Store.TransitionWorkUnit(s.ctx, s.driver.TaskID, unit.WorkUnitID, "running", "completed", ""); err != nil {
			event.err = err
			s.settleCh <- event
			return
		}
		event.outcome = "completed"
	case "failed":
		reason := result.Reason
		if reason == "" {
			reason = "work unit failed"
		}
		if err := s.driver.Store.TransitionWorkUnit(s.ctx, s.driver.TaskID, unit.WorkUnitID, "running", "failed", reason); err != nil {
			event.err = err
			s.settleCh <- event
			return
		}
		event.outcome = "failed"
	case "blocked":
		reason := result.Reason
		if reason == "" {
			reason = "work unit blocked"
		}
		if err := s.driver.Store.TransitionWorkUnit(s.ctx, s.driver.TaskID, unit.WorkUnitID, "running", "blocked", reason); err != nil {
			event.err = err
			s.settleCh <- event
			return
		}
		event.outcome = "blocked"
	case "uncertain":
		reason := result.Reason
		if reason == "" {
			reason = "work unit outcome uncertain"
		}
		if err := s.driver.Store.TransitionWorkUnit(s.ctx, s.driver.TaskID, unit.WorkUnitID, "running", "uncertain", reason); err != nil {
			event.err = err
			s.settleCh <- event
			return
		}
		event.outcome = "uncertain"
	case "canceled":
		// The unit stays 'running' for recovery reset (conservative, no
		// fabricated terminal state). The cancellation signal survives via
		// the stop condition below.
		event.outcome = "canceled"
	default:
		// Unknown outcome: leave the unit 'running' for recovery reset.
		event.outcome = "interrupted"
	}
	s.settleCh <- event
}

// settle blocks on the next durable outcome of a dispatched unit, updates the
// active count and records the first hard stop condition.
func (s *boundedScheduler) settle() {
	s.handleSettle(<-s.settleCh)
}

// handleSettle processes one settle event. A first non-completed terminal
// outcome sets the stop condition: the scheduler starts no new batch but lets
// the already-dispatched batch settle (no artificial sibling cancellation,
// which would manufacture uncertain provider/effect states).
func (s *boundedScheduler) handleSettle(event settleEvent) {
	s.active--
	if s.stop != nil {
		return
	}
	if event.err != nil {
		s.stop = event.err
		return
	}
	switch event.outcome {
	case "completed":
		// Durable completion: the batch continues.
	case "canceled":
		// Wrapping context.Canceled preserves the stable 130 exit across the
		// composition boundary (issue #106 review).
		s.stop = fmt.Errorf("%w: work unit %s canceled", context.Canceled, event.unitID)
	case "failed", "blocked", "uncertain":
		s.stop = fmt.Errorf("%w: %s", ErrWorkUnitBlockedChain, event.unitID)
	default:
		s.stop = fmt.Errorf("work unit %s interrupted before a terminal outcome", event.unitID)
	}
}

// drain waits until every dispatched unit has settled (their workers already
// applied the durable outcome transitions) so RunAll never returns while
// worker goroutines are still alive, then returns the final error.
func (s *boundedScheduler) drain(final error) error {
	for s.active > 0 {
		<-s.settleCh
		s.active--
	}
	return final
}
