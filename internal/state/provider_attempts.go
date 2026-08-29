package state

import (
	"context"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// providerAttemptStatus maps a classified governor outcome to the persisted
// provider attempt status. An uncertain outcome stays uncertain and is never
// reinterpreted as success or failure on restart.
func providerAttemptStatus(outcome governor.OutcomeClass, uncertain bool) string {
	if uncertain || outcome == governor.OutcomeUncertainReached {
		return "uncertain"
	}
	if outcome == governor.OutcomeSuccess {
		return "completed"
	}
	return "failed"
}

// persistedDeliveryState keeps invalid/zero observations unobserved on disk.
// Runtime classification may use a conservative effective state, but the
// durable projection must retain only the raw transport evidence.
func persistedDeliveryState(state provider.DeliveryState) string {
	if !state.Valid() {
		return ""
	}
	return state.String()
}

func parsePersistedDeliveryState(raw string) (provider.DeliveryState, error) {
	switch raw {
	case "":
		return 0, nil
	case "not_sent":
		return provider.DeliveryNotSent, nil
	case "sent_confirmed":
		return provider.DeliverySentConfirmed, nil
	case "sent_unconfirmed":
		return provider.DeliverySentUnconfirmed, nil
	case "response_started":
		return provider.DeliveryResponseStarted, nil
	case "completed":
		return provider.DeliveryCompleted, nil
	default:
		return 0, fmt.Errorf("invalid persisted provider delivery state %q", raw)
	}
}

// RecordProviderPrepared implements governor.Persistence (TX 1): the provider
// attempt intent, the post-start governor protection projection and the
// provider_attempt_prepared event commit atomically BEFORE the provider call.
func (s *Store) RecordProviderPrepared(ctx context.Context, record governor.ProviderPrepared) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider attempt preparation: %w", err)
	}
	defer tx.Rollback()
	executionID, err := nextIdentity(tx, "exec")
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO provider_attempts
			 (execution_id, task_id, work_unit_id, client_request_id, provider, model_pool, model, attempt_sequence, receipt_aware, protocol_family, config_identity, delivery_state, status, created_at, prepared_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 'prepared', ?, ?)`,
		executionID, record.TaskID, Redact(record.WorkUnitID), record.ClientRequestID, record.ProviderID, record.ModelPool, record.Model,
		record.AttemptSequence, boolInt(record.ReceiptAware), string(record.ProtocolFamily), Redact(record.ConfigIdentity),
		now, formatTime(record.StartedAt)); err != nil {
		return fmt.Errorf("insert provider attempt: %w", err)
	}
	if err := s.saveGovernorStateInTx(ctx, tx, record.State, record.TaskID, now); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, record.TaskID, "provider_attempt_prepared", map[string]any{
		"execution_id":      executionID,
		"client_request_id": record.ClientRequestID,
		"provider":          record.ProviderID,
		"model":             record.Model,
		"model_pool":        record.ModelPool,
		"attempt_sequence":  record.AttemptSequence,
		"receipt_aware":     record.ReceiptAware,
		"protocol_family":   string(record.ProtocolFamily),
		"config_identity":   Redact(record.ConfigIdentity),
		"governor":          governorEventPayload(record.State),
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider attempt preparation: %w", err)
	}
	hitCrashPoint("provider_tx1_after")
	return nil
}

// RecordProviderFinished implements governor.Persistence (TX 2): the
// classified outcome, receipt evidence, the post-finish governor projection
// and the outcome event commit atomically AFTER the provider call.
func (s *Store) RecordProviderFinished(ctx context.Context, record governor.ProviderFinished) error {
	// The provider call already returned; this crash point simulates death
	// before TX 2 commits, leaving the attempt 'prepared'.
	hitCrashPoint("provider_tx2_before")
	now := s.now()
	status := providerAttemptStatus(record.Outcome, record.Uncertain)
	receiptError := record.ReceiptErrorCode
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider attempt finish: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE provider_attempts
			 SET status = ?, outcome = ?, upstream_reached = ?, uncertain = ?, attempt_debited = ?,
			     selected_backoff_ns = ?, error_class = ?, delivery_state = ?, request_id = ?, completed_at = ?
			 WHERE task_id = ? AND client_request_id = ? AND status = 'prepared'`,
		status, record.Outcome, boolInt(record.UpstreamReached), boolInt(record.Uncertain),
		record.AttemptDebited, int64(record.SelectedBackoff), receiptError, persistedDeliveryState(record.DeliveryState),
		Redact(record.RequestID), now,
		record.TaskID, record.ClientRequestID); err != nil {
		return fmt.Errorf("finish provider attempt: %w", err)
	}
	for _, receipt := range record.Receipts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO provider_attempt_receipts
			 (receipt_attempt_id, task_id, execution_id, schema_version, client_request_id, sequence, provider, model, account_lane_hash, started_at, completed_at, outcome, trigger, upstream_reached)
			 VALUES (?, ?, (SELECT execution_id FROM provider_attempts WHERE task_id = ? AND client_request_id = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			receipt.AttemptID, record.TaskID, record.TaskID, record.ClientRequestID,
			receipt.SchemaVersion, receipt.ClientRequestID, receipt.Sequence, receipt.Provider,
			receipt.Model, receipt.AccountLaneHash, formatTime(receipt.StartedAt), formatTime(receipt.CompletedAt),
			receipt.Outcome, receipt.Trigger, boolInt(receipt.UpstreamReached)); err != nil {
			return fmt.Errorf("insert provider attempt receipt: %w", err)
		}
	}
	if err := s.saveGovernorStateInTx(ctx, tx, record.State, record.TaskID, now); err != nil {
		return err
	}
	eventKind := "provider_attempt_completed"
	if status == "failed" {
		eventKind = "provider_attempt_failed"
	} else if status == "uncertain" {
		eventKind = "provider_attempt_uncertain"
	}
	if err := appendEvent(ctx, tx, record.TaskID, eventKind, map[string]any{
		"client_request_id": record.ClientRequestID,
		"status":            status,
		"outcome":           record.Outcome,
		"upstream_reached":  record.UpstreamReached,
		"uncertain":         record.Uncertain,
		"delivery_state":    record.DeliveryState.String(),
		"attempt_debited":   record.AttemptDebited,
		"selected_backoff":  int64(record.SelectedBackoff),
		"protocol_family":   string(record.ProtocolFamily),
		"config_identity":   Redact(record.ConfigIdentity),
		"request_id":        Redact(record.RequestID),
		"receipts":          len(record.Receipts),
		"receipt_error":     receiptError,
		"circuit":           record.Circuit.State,
		"governor":          governorEventPayload(record.State),
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider attempt finish: %w", err)
	}
	hitCrashPoint("provider_tx2_after")
	return nil
}
