package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ErrTaskNotFound is returned by RenderInspect when the task row is absent.
var ErrTaskNotFound = errors.New("task not found")

type inspectTask struct {
	TaskID     string
	Objective  string
	Status     string
	Outcome    string
	StopReason string
	Workspace  string
	Model      string
	ConfigJSON string
	Summary    string
	CreatedAt  string
	StartedAt  string
	FinishedAt string
}

type inspectAction struct {
	ActionID      string
	Tool          string
	ArgumentsJSON string
	Fingerprint   string
	Status        string
	CreatedAt     string
}

type inspectToolAttempt struct {
	ExecutionID    string
	ActionID       string
	Tool           string
	Status         string
	Classification string
	RecoveryClass  int
	EvidenceID     string
	DurationNanos  int64
	CreatedAt      string
	PreparedAt     string
	CompletedAt    string
}

type inspectProviderAttempt struct {
	ExecutionID     string
	ClientRequestID string
	Provider        string
	Model           string
	Status          string
	Outcome         string
	UpstreamReached bool
	Uncertain       bool
	AttemptDebited  int
	SelectedBackoff int64
	ErrorClass      string
	ReceiptCount    int
	CreatedAt       string
	PreparedAt      string
	CompletedAt     string
}

type inspectReceipt struct {
	ReceiptAttemptID string
	ExecutionID      string
	Sequence         int
	Provider         string
	Model            string
	StartedAt        string
	CompletedAt      string
	Outcome          string
	Trigger          string
	UpstreamReached  bool
}

// RenderInspect writes a stable, human-readable reconstruction of one task.
// It never dumps raw SQLite rows: the output is fixed-section, ordered and
// sanitized, and every identifier shown comes from the persisted projection.
func (s *Store) RenderInspect(ctx context.Context, out io.Writer, taskID string) error {
	task, err := s.loadInspectTask(ctx, taskID)
	if err != nil {
		return err
	}
	events, err := s.loadEvents(ctx, taskID)
	if err != nil {
		return err
	}
	actions, err := s.loadInspectActions(ctx, taskID)
	if err != nil {
		return err
	}
	toolAttempts, err := s.loadInspectToolAttempts(ctx, taskID)
	if err != nil {
		return err
	}
	providerAttempts, err := s.loadInspectProviderAttempts(ctx, taskID)
	if err != nil {
		return err
	}
	receipts, err := s.loadInspectReceipts(ctx, taskID)
	if err != nil {
		return err
	}
	governor, err := s.loadInspectGovernor(ctx, taskID)
	if err != nil {
		return err
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "Task: %s\n", task.TaskID)
	fmt.Fprintf(&builder, "Objective: %s\n", task.Objective)
	fmt.Fprintf(&builder, "Status: %s\n", task.Status)
	if task.Outcome != "" {
		fmt.Fprintf(&builder, "Outcome: %s\n", task.Outcome)
	}
	if task.StopReason != "" {
		fmt.Fprintf(&builder, "Stop reason: %s\n", task.StopReason)
	}
	fmt.Fprintf(&builder, "Workspace: %s\n", task.Workspace)
	if task.Model != "" {
		fmt.Fprintf(&builder, "Model: %s\n", task.Model)
	}
	fmt.Fprintf(&builder, "Created: %s\n", task.CreatedAt)
	if task.StartedAt != "" {
		fmt.Fprintf(&builder, "Started: %s\n", task.StartedAt)
	}
	if task.FinishedAt != "" {
		fmt.Fprintf(&builder, "Finished: %s\n", task.FinishedAt)
	}
	if task.Summary != "" {
		fmt.Fprintf(&builder, "Summary: %s\n", task.Summary)
	}

	builder.WriteString("\nConfiguration:\n")
	if config, ok := renderConfig(task.ConfigJSON); ok {
		for _, line := range config {
			fmt.Fprintf(&builder, "  %s\n", line)
		}
	} else {
		builder.WriteString("  (none recorded)\n")
	}

	builder.WriteString("\nEvents:\n")
	if len(events) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, event := range events {
		fmt.Fprintf(&builder, "  %d. %s at %s\n", event.Sequence, event.Kind, event.CreatedAt)
		if payload := compactEventPayload(event.Payload); payload != "" {
			fmt.Fprintf(&builder, "     %s\n", payload)
		}
	}

	builder.WriteString("\nActions:\n")
	if len(actions) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, action := range actions {
		fmt.Fprintf(&builder, "  %s %s status=%s\n", action.ActionID, action.Tool, action.Status)
		if fingerprint := action.Fingerprint; fingerprint != "" {
			fmt.Fprintf(&builder, "    fingerprint=%s\n", shortID(fingerprint))
		}
	}

	builder.WriteString("\nTool attempts:\n")
	if len(toolAttempts) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, attempt := range toolAttempts {
		fmt.Fprintf(&builder, "  %s tool=%s action=%s status=%s\n", attempt.ExecutionID, attempt.Tool, attempt.ActionID, attempt.Status)
		if attempt.Classification != "" {
			fmt.Fprintf(&builder, "    classification=%s\n", attempt.Classification)
		}
		if attempt.EvidenceID != "" {
			fmt.Fprintf(&builder, "    evidence=%s\n", attempt.EvidenceID)
		}
		if attempt.Status == "prepared" {
			fmt.Fprintf(&builder, "    uncertain=prepared: the effect may have started; reconcile before re-execution\n")
		}
		if attempt.DurationNanos > 0 {
			fmt.Fprintf(&builder, "    duration=%dns\n", attempt.DurationNanos)
		}
	}

	builder.WriteString("\nProvider attempts:\n")
	if len(providerAttempts) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, attempt := range providerAttempts {
		fmt.Fprintf(&builder, "  %s request=%s provider=%s model=%s status=%s\n", attempt.ExecutionID, attempt.ClientRequestID, attempt.Provider, attempt.Model, attempt.Status)
		if attempt.Outcome != "" {
			fmt.Fprintf(&builder, "    outcome=%s upstream_reached=%t\n", attempt.Outcome, attempt.UpstreamReached)
		}
		if attempt.Uncertain || attempt.Status == "uncertain" {
			fmt.Fprintf(&builder, "    uncertain=yes: the upstream may have been reached; never auto-retry\n")
		}
		if attempt.AttemptDebited > 0 {
			fmt.Fprintf(&builder, "    debited=%d\n", attempt.AttemptDebited)
		}
		if attempt.ErrorClass != "" {
			fmt.Fprintf(&builder, "    receipt_error=%s\n", attempt.ErrorClass)
		}
		if attempt.Status == "prepared" {
			fmt.Fprintf(&builder, "    uncertain=prepared: the upstream may have been reached; reconcile before re-execution\n")
		}
	}

	builder.WriteString("\nProvider attempt receipts:\n")
	if len(receipts) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, receipt := range receipts {
		fmt.Fprintf(&builder, "  receipt=%s execution=%s sequence=%d outcome=%s trigger=%s upstream=%t\n",
			receipt.ReceiptAttemptID, receipt.ExecutionID, receipt.Sequence, receipt.Outcome, receipt.Trigger, receipt.UpstreamReached)
	}

	builder.WriteString("\nGovernor state:\n")
	if governor == nil {
		builder.WriteString("  (none recorded)\n")
	} else {
		fmt.Fprintf(&builder, "  account=%s provider=%s model_pool=%s\n", governor.AccountPolicyID, governor.ProviderID, governor.ModelPool)
		fmt.Fprintf(&builder, "  rolling usage: 10m=%d/%d 1h=%d/%d 3h=%d/%d\n",
			governor.Rolling10m, governor.Rolling10mCeiling, governor.Rolling1h, governor.Rolling1hCeiling, governor.Rolling3h, governor.Rolling3hCeiling)
		fmt.Fprintf(&builder, "  task attempts: %d/%d\n", governor.TaskUsed, governor.TaskCeiling)
		if governor.CooldownUntil != "" {
			fmt.Fprintf(&builder, "  cooldown until: %s\n", governor.CooldownUntil)
		} else {
			builder.WriteString("  cooldown until: none\n")
		}
		fmt.Fprintf(&builder, "  circuit: %s\n", governor.CircuitState)
		if governor.CircuitReason != "" {
			fmt.Fprintf(&builder, "  circuit reason: %s\n", governor.CircuitReason)
		}
		if governor.TelemetryUnsafe {
			builder.WriteString("  telemetry: unsafe (conservative accounting is active)\n")
		}
	}

	if _, err := io.WriteString(out, builder.String()); err != nil {
		return fmt.Errorf("write inspect output: %w", err)
	}
	return nil
}

func (s *Store) loadInspectTask(ctx context.Context, taskID string) (inspectTask, error) {
	var task inspectTask
	err := s.db.QueryRowContext(ctx,
		`SELECT task_id, objective, status, outcome, stop_reason, workspace, model, config_json, summary, created_at, started_at, finished_at
		 FROM tasks WHERE task_id = ?`, taskID).Scan(
		&task.TaskID, &task.Objective, &task.Status, &task.Outcome, &task.StopReason,
		&task.Workspace, &task.Model, &task.ConfigJSON, &task.Summary, &task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	if err == sql.ErrNoRows {
		return inspectTask{}, ErrTaskNotFound
	}
	if err != nil {
		return inspectTask{}, fmt.Errorf("load task: %w", err)
	}
	return task, nil
}

func (s *Store) loadInspectActions(ctx context.Context, taskID string) ([]inspectAction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_id, tool, arguments_json, fingerprint, status, created_at
		 FROM actions WHERE task_id = ? ORDER BY action_sequence`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load actions: %w", err)
	}
	defer rows.Close()
	var actions []inspectAction
	for rows.Next() {
		var action inspectAction
		if err := rows.Scan(&action.ActionID, &action.Tool, &action.ArgumentsJSON, &action.Fingerprint, &action.Status, &action.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

func (s *Store) loadInspectToolAttempts(ctx context.Context, taskID string) ([]inspectToolAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT execution_id, action_id, tool, status, classification, recovery_class, evidence_id, duration_ns, created_at, prepared_at, completed_at
		 FROM tool_attempts WHERE task_id = ? ORDER BY created_at, execution_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load tool attempts: %w", err)
	}
	defer rows.Close()
	var attempts []inspectToolAttempt
	for rows.Next() {
		var attempt inspectToolAttempt
		if err := rows.Scan(&attempt.ExecutionID, &attempt.ActionID, &attempt.Tool, &attempt.Status, &attempt.Classification,
			&attempt.RecoveryClass, &attempt.EvidenceID, &attempt.DurationNanos, &attempt.CreatedAt, &attempt.PreparedAt, &attempt.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan tool attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) loadInspectProviderAttempts(ctx context.Context, taskID string) ([]inspectProviderAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.execution_id, p.client_request_id, p.provider, p.model, p.status, p.outcome, p.upstream_reached,
		        p.uncertain, p.attempt_debited, p.selected_backoff_ns, p.error_class,
		        (SELECT COUNT(*) FROM provider_attempt_receipts r WHERE r.execution_id = p.execution_id),
		        p.created_at, p.prepared_at, p.completed_at
		 FROM provider_attempts p WHERE p.task_id = ? ORDER BY p.created_at, p.execution_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load provider attempts: %w", err)
	}
	defer rows.Close()
	var attempts []inspectProviderAttempt
	for rows.Next() {
		var attempt inspectProviderAttempt
		if err := rows.Scan(&attempt.ExecutionID, &attempt.ClientRequestID, &attempt.Provider, &attempt.Model, &attempt.Status,
			&attempt.Outcome, &attempt.UpstreamReached, &attempt.Uncertain, &attempt.AttemptDebited,
			&attempt.SelectedBackoff, &attempt.ErrorClass, &attempt.ReceiptCount, &attempt.CreatedAt,
			&attempt.PreparedAt, &attempt.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan provider attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) loadInspectReceipts(ctx context.Context, taskID string) ([]inspectReceipt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT receipt_attempt_id, execution_id, sequence, provider, model, started_at, completed_at, outcome, trigger, upstream_reached
		 FROM provider_attempt_receipts WHERE task_id = ? ORDER BY execution_id, sequence`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load receipts: %w", err)
	}
	defer rows.Close()
	var receipts []inspectReceipt
	for rows.Next() {
		var receipt inspectReceipt
		if err := rows.Scan(&receipt.ReceiptAttemptID, &receipt.ExecutionID, &receipt.Sequence, &receipt.Provider,
			&receipt.Model, &receipt.StartedAt, &receipt.CompletedAt, &receipt.Outcome, &receipt.Trigger,
			&receipt.UpstreamReached); err != nil {
			return nil, fmt.Errorf("scan receipt: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

type inspectGovernor struct {
	AccountPolicyID   string
	ProviderID        string
	ModelPool         string
	Rolling3h         int
	Rolling1h         int
	Rolling10m        int
	Rolling3hCeiling  int
	Rolling1hCeiling  int
	Rolling10mCeiling int
	TaskUsed          int
	TaskCeiling       int
	CooldownUntil     string
	CircuitState      string
	CircuitReason     string
	TelemetryUnsafe   bool
}

func (s *Store) loadInspectGovernor(ctx context.Context, taskID string) (*inspectGovernor, error) {
	var governor inspectGovernor
	var cooldown string
	err := s.db.QueryRowContext(ctx,
		`SELECT account_policy_id, provider_id, model_pool, cooldown_until, circuit_state, circuit_reason, telemetry_unsafe,
		        rolling_3h_ceiling, rolling_1h_ceiling, rolling_10m_ceiling, task_budget_ceiling
		 FROM governor_state WHERE id = 1`).Scan(
		&governor.AccountPolicyID, &governor.ProviderID, &governor.ModelPool, &cooldown,
		&governor.CircuitState, &governor.CircuitReason, &governor.TelemetryUnsafe,
		&governor.Rolling3hCeiling, &governor.Rolling1hCeiling, &governor.Rolling10mCeiling, &governor.TaskCeiling)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load governor state: %w", err)
	}
	governor.CooldownUntil = cooldown
	now := s.clock.Now()
	var ledgerRaw sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT group_concat(at, '|') FROM governor_ledger`).Scan(&ledgerRaw); err != nil {
		return nil, fmt.Errorf("load governor ledger: %w", err)
	}
	for _, raw := range strings.Split(ledgerRaw.String, "|") {
		at := parseTime(raw)
		if at.IsZero() {
			continue
		}
		if now.Sub(at) <= 3*time.Hour {
			governor.Rolling3h++
		}
		if now.Sub(at) <= time.Hour {
			governor.Rolling1h++
		}
		if now.Sub(at) <= 10*time.Minute {
			governor.Rolling10m++
		}
	}
	// The inspected task's own attempt usage, not the number of tasks the
	// governor has retained.
	if err := s.db.QueryRowContext(ctx,
		`SELECT attempts FROM governor_task_states WHERE task_id = ?`, taskID).Scan(&governor.TaskUsed); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("load governor task usage: %w", err)
	}
	return &governor, nil
}

// renderConfig parses the sanitized configuration snapshot into stable lines.
func renderConfig(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil, false
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, false
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s: %v", key, values[key]))
	}
	return lines, true
}

// compactEventPayload renders one journal payload as a single stable line.
func compactEventPayload(payload string) string {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "{}" {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(payload), &values); err != nil {
		return payload
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString("{")
	for index, key := range keys {
		if index > 0 {
			builder.WriteString(" ")
		}
		fmt.Fprintf(&builder, "%s=%v", key, values[key])
	}
	builder.WriteString("}")
	return builder.String()
}

func shortID(value string) string {
	if len(value) <= 16 {
		return value
	}
	return value[:16] + "..."
}
