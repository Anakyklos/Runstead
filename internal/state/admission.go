package state

import (
	"context"
	"errors"
	"fmt"
)

var ErrAdmissionUnsafe = errors.New("durable state contains an unreconciled provider attempt")

// CheckSIWCAdmissionSafety blocks a fresh task admission while a prior
// provider attempt may still have reached upstream or requires human review.
// Reconciled uncertain history remains durable and debited; it is not treated
// as an active attempt by this check.
func (s *Store) CheckSIWCAdmissionSafety(ctx context.Context) error {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM provider_attempts
		WHERE status IN ('prepared', 'running', 'uncertain', 'human_review_required')`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check SIWC admission safety: %w", err)
	}
	if count != 0 {
		return ErrAdmissionUnsafe
	}
	return nil
}
