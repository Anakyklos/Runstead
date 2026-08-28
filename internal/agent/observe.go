package agent

import (
	"context"
	"errors"

	"github.com/RenyEnnos/Runstead/internal/governor"
)

// ErrAttemptObservation marks a control-plane failure while observing one
// governed attempt for conservative learning (issue #93). Observation runs
// AFTER a physical dispatch and BEFORE any retry decision: when the
// observer cannot safely persist what it learned, the attempt loop must
// stop conservatively (no further dispatch, no retry) rather than continue
// with durable state it cannot account for.
var ErrAttemptObservation = errors.New("attempt observation failed")

// AttemptObserver receives the provider-neutral outcome of every ADMITTED
// governed attempt, exactly once, after the governor's durable finish
// (TX 1/TX 2) and before any retry decision. It is the ONLY seam through
// which execution outcomes can become conservative profile evidence
// (issue #93).
//
// Contracts:
//
//   - ObserveAttempt is called at most once per admitted physical attempt
//     (retries are separate attempts and each is observed).
//   - Admission denials are never observed (no physical attempt happened).
//   - The observer has NO execution authority: it cannot issue requests,
//     retry, rotate or fan out; it only turns sanitized evidence into
//     durable profile updates.
//   - A returned error is a conservative stop: the executor returns it as
//     the attempt result and never issues another physical attempt.
//   - Implementations must be idempotent-safe: the same attempt may be
//     replayed by a survivor/restart path and the profile boundary is
//     monotonic against already-learned state.
type AttemptObserver interface {
	// ObserveAttempt observes one governed physical attempt outcome.
	// request is the attempt request as admitted (including its task id and
	// client request id); result is the completed governor result.
	ObserveAttempt(ctx context.Context, request governor.AttemptRequest, result governor.ExecutionResult) error
}
