package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// appendEvent inserts one journal event with the next deterministic
// task-scoped sequence inside the caller's transaction. The projection change
// and its event therefore commit atomically.
func appendEvent(ctx context.Context, tx *sql.Tx, taskID, kind string, payload any, createdAt string) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event payload: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events (task_id, sequence, kind, payload_json, created_at)
		 VALUES (?, (SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE task_id = ?), ?, ?, ?)`,
		taskID, taskID, kind, string(RedactJSON(encoded)), createdAt)
	if err != nil {
		return fmt.Errorf("append event %s: %w", kind, err)
	}
	return nil
}

// eventRow is one journal entry used by inspect.
type eventRow struct {
	Sequence  int
	Kind      string
	Payload   string
	CreatedAt string
}

// loadEvents returns the journal for one task in deterministic order.
func (s *Store) loadEvents(ctx context.Context, taskID string) ([]eventRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sequence, kind, payload_json, created_at FROM events WHERE task_id = ? ORDER BY sequence`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	defer rows.Close()
	var events []eventRow
	for rows.Next() {
		var event eventRow
		if err := rows.Scan(&event.Sequence, &event.Kind, &event.Payload, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
