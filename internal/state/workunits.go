package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Work unit lifecycle (issue #106, M9 Stage A).
//
//	created -> ready          dependency satisfying (driver/scheduler)
//	ready   -> running        driver start (deps satisfied + envelope valid)
//	running -> completed      ONLY after a passed verification decision for
//	                          this work unit (evidence-backed; model text can
//	                          never complete a unit)
//	running -> failed         typed terminal failure
//	running -> blocked        policy / approval / human review
//	running -> uncertain      uncertain effect; blocks continuation
//	blocked -> ready          approval resolved (driver-initiated)
//	running -> ready          recovery-only reset of an interrupted unit
//
// Every other transition is rejected fail-closed. Each transition journals a
// `workunit_transition` event in the same transaction as the projection
// change.
var workUnitTransitions = map[TransitionKey]bool{
	{From: "created", To: "ready"}:     true,
	{From: "ready", To: "running"}:     true,
	{From: "running", To: "completed"}: true,
	// Recovery-only: an 'uncertain' unit whose effect records were all
	// reconciled returns to 'ready' (issue #106 review). No arbitrary reason
	// bypasses this: only ReconcileUncertainWorkUnits performs it.
	{From: "uncertain", To: "ready"}:   true,
	{From: "running", To: "failed"}:    true,
	{From: "running", To: "blocked"}:   true,
	{From: "running", To: "uncertain"}: true,
	{From: "blocked", To: "ready"}:     true,
	{From: "running", To: "ready"}:     true, // recovery-only reset
}

// TransitionKey identifies one lifecycle transition edge.
type TransitionKey struct {
	From string
	To   string
}

// WorkUnitState is the closed set of persisted work unit statuses.
var WorkUnitStates = map[string]bool{
	"created": true, "ready": true, "running": true, "completed": true,
	"failed": true, "blocked": true, "uncertain": true,
}

// ErrInvalidWorkUnitTransition rejects an impossible lifecycle edge.
var ErrInvalidWorkUnitTransition = errors.New("invalid work unit lifecycle transition")

// ErrWorkUnitNotFound reports an unknown work unit for the task.
var ErrWorkUnitNotFound = errors.New("work unit not found")

// ErrWorkUnitCycle reports a dependency cycle detected at create/schedule.
var ErrWorkUnitCycle = errors.New("work unit dependency cycle")

// ErrWorkUnitMissingDependency reports a dependency that does not exist or
// belongs to another task.
var ErrWorkUnitMissingDependency = errors.New("work unit dependency missing")

// ErrWorkUnitEnvelope reports a capability/tool envelope outside the parent
// task contract.
var ErrWorkUnitEnvelope = errors.New("work unit capability envelope exceeds parent task contract")

// WorkUnitCreate is the operator-declared definition of one work unit.
// Everything here is sanitized, non-secret, control-plane material; the model
// can never create or mutate work units.
type WorkUnitCreate struct {
	TaskID           string
	WorkUnitID       string
	ParentWorkUnitID string
	Objective        string
	Dependencies     []string
	Tools            []string
	WorkspaceScope   string
	AcceptancePlan   []byte
	AcceptanceDigest string
	ContextBudget    int
	ProviderBudget   int
	StepBudget       int
	// Version is the persisted contract version; the recovery loader refuses
	// anything but supportedWorkUnitVersion before reconstruction (issue #106).
	Version int
}

// WorkUnit is one persisted Runstead-owned subtask.
type WorkUnit struct {
	WorkUnitID         string
	TaskID             string
	ParentWorkUnitID   string
	Objective          string
	Status             string
	Tools              []string
	WorkspaceScope     string
	AcceptancePlanSpec string
	AcceptanceDigest   string
	ContextBudget      int
	ProviderBudget     int
	StepBudget         int
	EvidenceRefs       []string
	FailureReason      string
	BlockingReason     string
	Version            int
	CreatedAt          string
	UpdatedAt          string
	Dependencies       []string
}

// RecoveryWorkUnit is the authoritative persisted work unit state one resume
// needs for the #51 context compiler and the serial scheduler.
type RecoveryWorkUnit struct {
	WorkUnitID       string
	TaskID           string
	ParentWorkUnitID string
	Objective        string
	Status           string
	Dependencies     []string
	Tools            []string
	WorkspaceScope   string
	AcceptanceDigest string
	FailureReason    string
	BlockingReason   string
	ContextBudget    int
	ProviderBudget   int
	StepBudget       int
	// Version is the persisted contract version; the recovery loader refuses
	// anything but supportedWorkUnitVersion before reconstruction (issue #106).
	Version int
}

const maxWorkUnitObjectiveBytes = 4096

// supportedWorkUnitVersion is the ONLY persisted Work Unit contract version
// this runtime understands (issue #106): code must never interpret state
// whose semantics it does not understand.
const supportedWorkUnitVersion = 1

// WorkUnitConcurrencyKey is the tasks.config_json key holding the effective
// scheduler concurrency of a task that ran Work Units (issue #109). It is a
// versioned durable contract: resume reads it to adopt the SAME concurrency
// the task started under and rejects an explicitly different operator value
// fail-closed. No new table/migration is needed: the task configuration
// snapshot is the existing authoritative durable structure resume already
// validates for provider/model/policy continuity.
const WorkUnitConcurrencyKey = "workunit_concurrency"

// WorkUnitConcurrencyFromConfigJSON reads the persisted effective scheduler
// concurrency from the task configuration snapshot. ok=false when the key is
// absent (a Stage A task: serial contract, DefaultConcurrency) or when the
// persisted value is not an integer (the caller must fail closed, never
// adopt a guessed value).
func WorkUnitConcurrencyFromConfigJSON(configJSON string) (value int, ok bool) {
	if strings.TrimSpace(configJSON) == "" {
		return 0, false
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(configJSON), &snapshot); err != nil {
		return 0, false
	}
	raw, present := snapshot[WorkUnitConcurrencyKey]
	if !present {
		return 0, false
	}
	number, ok := raw.(float64)
	if !ok || number != float64(int(number)) {
		// Absent, non-numeric or non-integral persisted values are NEVER
		// adopted: the caller fails closed on an incompatible contract.
		return 0, false
	}
	return int(number), true
}

// ErrUnsupportedWorkUnitVersion is returned by the authoritative load
// boundaries (GetWorkUnit, ListWorkUnits, ReadyWorkUnits and the recovery
// snapshot loader) when a persisted Work Unit carries a version the runtime
// does not implement. It fails closed BEFORE any provider dispatch or
// effect; a future-version row is never executed as v1.
var ErrUnsupportedWorkUnitVersion = errors.New("unsupported work unit version")

// Validate checks the static shape of a create record: identity, bounded
// objective, closed status vocabulary and bounded dependency/tool lists.
func (s *Store) ValidateWorkUnitCreate(record WorkUnitCreate) error {
	if strings.TrimSpace(record.TaskID) == "" {
		return errors.New("work unit requires a task id")
	}
	if strings.TrimSpace(record.WorkUnitID) == "" {
		return errors.New("work unit requires a stable id")
	}
	if strings.TrimSpace(record.Objective) == "" {
		return errors.New("work unit requires a bounded objective")
	}
	if len(record.Objective) > maxWorkUnitObjectiveBytes {
		return fmt.Errorf("work unit objective exceeds %d bytes", maxWorkUnitObjectiveBytes)
	}
	for _, tool := range record.Tools {
		if strings.TrimSpace(tool) == "" {
			return errors.New("work unit tool envelope contains an empty tool id")
		}
	}
	if len(record.Dependencies) > 64 || len(record.Tools) > 64 {
		return errors.New("work unit dependency/tool lists exceed the bounded maximum")
	}
	if record.ContextBudget < 0 || record.ProviderBudget < 0 || record.StepBudget < 0 {
		return errors.New("work unit budgets must not be negative")
	}
	if len(record.AcceptancePlan) > 0 {
		if err := validateWorkUnitPlanShape(record.AcceptancePlan); err != nil {
			return err
		}
		if strings.TrimSpace(record.AcceptanceDigest) == "" {
			return errors.New("work unit acceptance plan requires its digest (driver supplies verifier.Plan.Digest)")
		}
	}
	return nil
}

// validateWorkUnitPlanShape is the store-side structural sanity check of a
// work unit acceptance plan. It deliberately performs NO semantic parsing:
// the authoritative plan parse/validation lives in the driver through
// verifier.ParsePlan (verifier imports state, so state cannot import it).
// This check only rejects material that cannot be a versioned typed plan.
func validateWorkUnitPlanShape(spec []byte) error {
	var raw struct {
		Version int `json:"version"`
		Checks  []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(spec, &raw); err != nil {
		return fmt.Errorf("work unit acceptance plan: %w", err)
	}
	if raw.Version != 1 {
		return fmt.Errorf("work unit acceptance plan: unsupported version %d", raw.Version)
	}
	if len(raw.Checks) == 0 {
		return errors.New("work unit acceptance plan: no checks")
	}
	for _, check := range raw.Checks {
		if check.ID == "" || check.Type == "" {
			return errors.New("work unit acceptance plan: check requires id and type")
		}
	}
	return nil
}

// CreateWorkUnit persists one operator-declared work unit with its dependency
// edges, rejecting missing/foreign dependencies and cycles before execution.
// The whole insert + journal event commits atomically; a unit with an
// existing id is REJECTED (explicit, no silent mutation; re-supplying the
// same operator file is handled by the driver, which skips existing ids).
func (s *Store) CreateWorkUnit(ctx context.Context, record WorkUnitCreate) (string, error) {
	if err := s.ValidateWorkUnitCreate(record); err != nil {
		return "", err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin work unit create: %w", err)
	}
	defer tx.Rollback()

	// The owning task must exist (authoritative parent).
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM tasks WHERE task_id = ?`, record.TaskID).Scan(&exists); err != nil {
		return "", fmt.Errorf("check task: %w", err)
	}
	if exists != 1 {
		return "", ErrWorkUnitNotFound
	}

	// Parent (when present) must exist under the same task.
	if record.ParentWorkUnitID != "" {
		var parent int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM work_units WHERE work_unit_id = ? AND task_id = ?`,
			record.ParentWorkUnitID, record.TaskID).Scan(&parent); err != nil {
			return "", fmt.Errorf("check parent work unit: %w", err)
		}
		if parent != 1 {
			return "", fmt.Errorf("%w: parent %q", ErrWorkUnitNotFound, record.ParentWorkUnitID)
		}
	}

	// Dependencies must exist under the same task.
	for _, dependency := range record.Dependencies {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM work_units WHERE work_unit_id = ? AND task_id = ?`,
			dependency, record.TaskID).Scan(&count); err != nil {
			return "", fmt.Errorf("check dependency %q: %w", dependency, err)
		}
		if count != 1 {
			return "", fmt.Errorf("%w: %q", ErrWorkUnitMissingDependency, dependency)
		}
	}

	// Cycle detection: deterministic iterative DFS over the whole task graph
	// (existing edges + the new unit's edges). A cycle fails creation.
	edges, err := s.loadDependencyEdges(ctx, tx, record.TaskID)
	if err != nil {
		return "", err
	}
	edges[record.WorkUnitID] = append(append([]string(nil), record.Dependencies...), record.ParentWorkUnitID)
	if cycle := findWorkUnitCycle(edges); cycle != "" {
		return "", fmt.Errorf("%w: %s", ErrWorkUnitCycle, cycle)
	}

	toolsJSON, err := json.Marshal(record.Tools)
	if err != nil {
		return "", fmt.Errorf("encode tool envelope: %w", err)
	}
	digest := record.AcceptanceDigest
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO work_units (work_unit_id, task_id, parent_work_unit_id, objective, status,
		     tools_json, workspace_scope, acceptance_plan, acceptance_digest, context_budget,
		     provider_budget, step_budget, evidence_refs, failure_reason, blocking_reason,
		     version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'created', ?, ?, ?, ?, ?, ?, ?, '[]', '', '', 1, ?, ?)`,
		record.WorkUnitID, record.TaskID, Redact(record.ParentWorkUnitID), Redact(record.Objective),
		string(RedactJSON(toolsJSON)), Redact(record.WorkspaceScope),
		string(RedactJSON(record.AcceptancePlan)), Redact(digest),
		record.ContextBudget, record.ProviderBudget, record.StepBudget, now, now); err != nil {
		return "", fmt.Errorf("insert work unit: %w", err)
	}
	for _, dependency := range record.Dependencies {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO work_unit_dependencies (work_unit_id, depends_on_work_unit_id) VALUES (?, ?)`,
			record.WorkUnitID, dependency); err != nil {
			return "", fmt.Errorf("insert work unit dependency: %w", err)
		}
	}
	if err := appendEvent(ctx, tx, record.TaskID, "workunit_created", map[string]any{
		"work_unit_id": record.WorkUnitID,
		"parent":       record.ParentWorkUnitID,
		"dependencies": record.Dependencies,
		"tools":        record.Tools,
	}, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit work unit create: %w", err)
	}
	return record.WorkUnitID, nil
}

// GetWorkUnit loads one work unit with its dependency set.
func (s *Store) GetWorkUnit(ctx context.Context, taskID, workUnitID string) (*WorkUnit, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT work_unit_id, task_id, parent_work_unit_id, objective, status, tools_json,
		        workspace_scope, acceptance_plan, acceptance_digest, context_budget,
		        provider_budget, step_budget, evidence_refs, failure_reason, blocking_reason,
		        version, created_at, updated_at
		 FROM work_units WHERE work_unit_id = ? AND task_id = ?`, workUnitID, taskID)
	unit, err := scanWorkUnit(row)
	if err != nil {
		if errors.Is(err, ErrUnsupportedWorkUnitVersion) {
			return nil, err
		}
		return nil, ErrWorkUnitNotFound
	}
	dependencies, err := s.workUnitDependencies(ctx, taskID, workUnitID)
	if err != nil {
		return nil, err
	}
	unit.Dependencies = dependencies
	return unit, nil
}

// ListWorkUnits lists the task's work units in deterministic creation order
// (created_at, then work_unit_id).
func (s *Store) ListWorkUnits(ctx context.Context, taskID string) ([]WorkUnit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT work_unit_id, task_id, parent_work_unit_id, objective, status, tools_json,
		        workspace_scope, acceptance_plan, acceptance_digest, context_budget,
		        provider_budget, step_budget, evidence_refs, failure_reason, blocking_reason,
		        version, created_at, updated_at
		 FROM work_units WHERE task_id = ? ORDER BY created_at, work_unit_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list work units: %w", err)
	}
	defer rows.Close()
	var units []WorkUnit
	for rows.Next() {
		unit, err := scanWorkUnit(rows)
		if err != nil {
			return nil, err
		}
		units = append(units, *unit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range units {
		dependencies, err := s.workUnitDependencies(ctx, taskID, units[index].WorkUnitID)
		if err != nil {
			return nil, err
		}
		units[index].Dependencies = dependencies
	}
	return units, nil
}

// TransitionWorkUnit applies one deterministic lifecycle transition, journals
// the event in the same transaction, and on transition to `completed`
// snapshots the durable evidence references derived from rows tagged with the
// unit (never narrative).
func (s *Store) TransitionWorkUnit(ctx context.Context, taskID, workUnitID, from, to, reason string) error {
	if !WorkUnitStates[from] || !WorkUnitStates[to] {
		return ErrInvalidWorkUnitTransition
	}
	if !workUnitTransitions[TransitionKey{From: from, To: to}] {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidWorkUnitTransition, from, to)
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin work unit transition: %w", err)
	}
	defer tx.Rollback()

	// Update only when the current persisted status equals `from`; a
	// concurrent or stale transition changes zero rows and fails closed.
	// evidence_refs is refreshed only when reaching `completed`.
	if to == "completed" {
		evidence, refErr := s.workUnitEvidenceRefsTx(ctx, tx, taskID, workUnitID)
		if refErr != nil {
			return refErr
		}
		encoded, encErr := json.Marshal(evidence)
		if encErr != nil {
			return fmt.Errorf("encode evidence refs: %w", encErr)
		}
		result, updErr := tx.ExecContext(ctx,
			`UPDATE work_units SET status = ?, failure_reason = ?, blocking_reason = ?,
			        evidence_refs = ?, updated_at = ?
			 WHERE work_unit_id = ? AND task_id = ? AND status = ?`,
			to, reasonFor(to, reason, "failure"), reasonFor(to, reason, "blocking"),
			string(encoded), now, workUnitID, taskID, from)
		if updErr != nil {
			return fmt.Errorf("transition work unit (completed): %w", updErr)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: %s -> %s (current status differs)", ErrInvalidWorkUnitTransition, from, to)
		}
	} else {
		result, updErr := tx.ExecContext(ctx,
			`UPDATE work_units SET status = ?, failure_reason = ?, blocking_reason = ?, updated_at = ?
			 WHERE work_unit_id = ? AND task_id = ? AND status = ?`,
			to, reasonFor(to, reason, "failure"), reasonFor(to, reason, "blocking"),
			now, workUnitID, taskID, from)
		if updErr != nil {
			return fmt.Errorf("transition work unit: %w", updErr)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: %s -> %s (current status differs)", ErrInvalidWorkUnitTransition, from, to)
		}
	}
	if err := appendEvent(ctx, tx, taskID, "workunit_transition", map[string]any{
		"work_unit_id": workUnitID,
		"from":         from,
		"to":           to,
		"reason":       reason,
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func reasonFor(to, reason, column string) string {
	if reason == "" {
		return ""
	}
	switch column {
	case "failure":
		if to == "failed" {
			return Redact(reason)
		}
	case "blocking":
		if to == "blocked" || to == "uncertain" {
			return Redact(reason)
		}
	}
	return ""
}

// ReadyWorkUnits returns units in (created, ready) whose dependencies are all
// completed, in deterministic creation order. Dependency satisfaction is
// established exclusively from persisted status, never model claims.
func (s *Store) ReadyWorkUnits(ctx context.Context, taskID string) ([]WorkUnit, error) {
	units, err := s.ListWorkUnits(ctx, taskID)
	if err != nil {
		return nil, err
	}
	statusByID := make(map[string]string, len(units))
	for _, unit := range units {
		statusByID[unit.WorkUnitID] = unit.Status
	}
	completed := func(id string) bool { return statusByID[id] == "completed" }
	var ready []WorkUnit
	for _, unit := range units {
		if unit.Status != "created" && unit.Status != "ready" {
			continue
		}
		allDone := true
		for _, dependency := range unit.Dependencies {
			if !completed(dependency) {
				allDone = false
				break
			}
		}
		if unit.ParentWorkUnitID != "" && !completed(unit.ParentWorkUnitID) {
			allDone = false
		}
		if allDone {
			ready = append(ready, unit)
		}
	}
	return ready, nil
}

// HasOpenWorkUnits reports whether any required unit is not completed: parent
// finalization must fail closed while this is true.
func (s *Store) HasOpenWorkUnits(ctx context.Context, taskID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM work_units WHERE task_id = ? AND status != 'completed'`, taskID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check open work units: %w", err)
	}
	return count > 0, nil
}

// ResetInterruptedWorkUnits is the recovery-only transition: a unit left in
// `running` by a process interruption moves back to `ready` after the
// existing pipeline reconciled its attempt records. Prior completed units and
// effects are never touched.
func (s *Store) ResetInterruptedWorkUnits(ctx context.Context, taskID, reason string) (int, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin interrupted reset: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT work_unit_id FROM work_units WHERE task_id = ? AND status = 'running'`, taskID)
	if err != nil {
		return 0, fmt.Errorf("list interrupted work units: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_units SET status = 'ready', updated_at = ? WHERE work_unit_id = ? AND task_id = ? AND status = 'running'`,
			now, id, taskID); err != nil {
			return 0, fmt.Errorf("reset interrupted work unit %s: %w", id, err)
		}
		if err := appendEvent(ctx, tx, taskID, "workunit_transition", map[string]any{
			"work_unit_id": id, "from": "running", "to": "ready", "reason": reason,
		}, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit interrupted reset: %w", err)
	}
	return len(ids), nil
}

// WorkUnitPendingApprovalCount returns how many control-plane approvals of
// THIS work unit's actions are still awaiting an operator decision. It is
// the authoritative resolution state for unblocking an approval-blocked
// work unit (issue #106 review): the unit moves blocked -> ready only when
// this reaches zero.
func (s *Store) WorkUnitPendingApprovalCount(ctx context.Context, taskID, workUnitID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM write_policy_decisions d
		 JOIN actions a ON a.task_id = d.task_id AND a.action_id = d.action_id
		 WHERE d.task_id = ? AND a.work_unit_id = ? AND d.decision = 'approval_required'
		   AND NOT EXISTS (
		       SELECT 1 FROM approvals ap
		       WHERE ap.task_id = d.task_id AND ap.fingerprint = `+effectiveFingerprintExpr+`
		   )`, taskID, workUnitID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count pending approvals for work unit %s: %w", workUnitID, err)
	}
	return count, nil
}

// ReconcileUncertainWorkUnits is the recovery-only transition for units left
// in `uncertain` by a terminal-but-conservative loop outcome: once the
// recovery pipeline has reconciled every effect record of the unit (no tool
// attempt remains prepared/running/human-review), the unit moves back to
// `ready` WITHOUT replay. A unit with an unreconcilable or still-unresolved
// attempt stays uncertain (blocking). Completed effects are never re-run:
// the resumption seed carries the repeat guard.
func (s *Store) ReconcileUncertainWorkUnits(ctx context.Context, taskID, reason string) (int, error) {
	units, err := s.ListWorkUnits(ctx, taskID)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, unit := range units {
		if unit.Status != "uncertain" {
			continue
		}
		var unresolved int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*)
			 FROM tool_attempts
			 WHERE task_id = ? AND work_unit_id = ?
			   AND status NOT IN ('completed', 'failed', 'reconciled', 'canceled')`,
			taskID, unit.WorkUnitID).Scan(&unresolved); err != nil {
			return changed, fmt.Errorf("check uncertain work unit %s attempts: %w", unit.WorkUnitID, err)
		}
		if unresolved > 0 {
			continue // effect not (yet) reconciled: stays blocking
		}
		if err := s.TransitionWorkUnit(ctx, taskID, unit.WorkUnitID, "uncertain", "ready", reason); err != nil {
			return changed, fmt.Errorf("reconcile uncertain work unit %s: %w", unit.WorkUnitID, err)
		}
		changed++
	}
	return changed, nil
}

// loadDependencyEdges loads every (unit -> dependency) edge of a task for
// cycle detection.
func (s *Store) loadDependencyEdges(ctx context.Context, tx *sql.Tx, taskID string) (map[string][]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT w.work_unit_id, COALESCE(d.depends_on_work_unit_id, '')
		 FROM work_units w
		 LEFT JOIN work_unit_dependencies d ON d.work_unit_id = w.work_unit_id
		 WHERE w.task_id = ?`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load dependency edges: %w", err)
	}
	defer rows.Close()
	edges := make(map[string][]string)
	for rows.Next() {
		var unit, dependency string
		if err := rows.Scan(&unit, &dependency); err != nil {
			return nil, err
		}
		if dependency != "" {
			edges[unit] = append(edges[unit], dependency)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return edges, nil
}

// findWorkUnitCycle runs a deterministic iterative DFS over the dependency
// graph and returns a cycle path (or "" when acyclic).
func findWorkUnitCycle(edges map[string][]string) string {
	units := make([]string, 0, len(edges))
	for unit := range edges {
		units = append(units, unit)
	}
	sort.Strings(units)
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(units))
	var stack []string
	for _, start := range units {
		if color[start] != white {
			continue
		}
		stack = append(stack, start)
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			if color[current] == gray {
				color[current] = black
				stack = stack[:len(stack)-1]
				continue
			}
			if color[current] == black {
				continue
			}
			color[current] = gray
			deps := append([]string(nil), edges[current]...)
			sort.Strings(deps)
			pushed := false
			for _, dependency := range deps {
				switch color[dependency] {
				case gray:
					return current + " -> " + dependency
				case white:
					stack = append(stack, dependency)
					pushed = true
				}
			}
			if !pushed {
				color[current] = black
				stack = stack[:len(stack)-1]
			}
		}
	}
	return ""
}

func (s *Store) workUnitDependencies(ctx context.Context, taskID, workUnitID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT depends_on_work_unit_id FROM work_unit_dependencies
		 WHERE work_unit_id = ? ORDER BY depends_on_work_unit_id`, workUnitID)
	if err != nil {
		return nil, fmt.Errorf("load work unit dependencies: %w", err)
	}
	defer rows.Close()
	var dependencies []string
	for rows.Next() {
		var dependency string
		if err := rows.Scan(&dependency); err != nil {
			return nil, err
		}
		dependencies = append(dependencies, dependency)
	}
	return dependencies, rows.Err()
}

// workUnitEvidenceRefsTx derives the durable evidence references of a unit
// from rows tagged with its id (tool_results joined with tool_attempts), in
// deterministic order. References only; content stays in the original rows.
func (s *Store) workUnitEvidenceRefsTx(ctx context.Context, tx *sql.Tx, taskID, workUnitID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT tr.evidence_id FROM tool_results tr
		 JOIN tool_attempts ta ON ta.execution_id = tr.execution_id
		 WHERE ta.task_id = ? AND ta.work_unit_id = ?
		 ORDER BY tr.evidence_id`, taskID, workUnitID)
	if err != nil {
		return nil, fmt.Errorf("load work unit evidence refs: %w", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		refs = append(refs, id)
	}
	return refs, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkUnit(row rowScanner) (*WorkUnit, error) {
	var unit WorkUnit
	var toolsJSON, planSpec, evidenceJSON, failureReason, blockingReason string
	if err := row.Scan(&unit.WorkUnitID, &unit.TaskID, &unit.ParentWorkUnitID, &unit.Objective,
		&unit.Status, &toolsJSON, &unit.WorkspaceScope, &planSpec, &unit.AcceptanceDigest,
		&unit.ContextBudget, &unit.ProviderBudget, &unit.StepBudget, &evidenceJSON,
		&failureReason, &blockingReason, &unit.Version, &unit.CreatedAt, &unit.UpdatedAt); err != nil {
		return nil, err
	}
	if unit.Version != supportedWorkUnitVersion {
		return nil, fmt.Errorf("%w: work unit %q version %d", ErrUnsupportedWorkUnitVersion, unit.WorkUnitID, unit.Version)
	}
	unit.AcceptancePlanSpec = planSpec
	unit.FailureReason = failureReason
	unit.BlockingReason = blockingReason
	if err := json.Unmarshal([]byte(toolsJSON), &unit.Tools); err != nil {
		return nil, fmt.Errorf("decode work unit tool envelope: %w", err)
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &unit.EvidenceRefs); err != nil {
		return nil, fmt.Errorf("decode work unit evidence refs: %w", err)
	}
	return &unit, nil
}

// LatestWorkUnitVerification returns the decision of the latest verification
// attempt tagged with the work unit (” = task-level rows are excluded), or
// found=false when none exists. It is the driver's evidence-backed completion
// gate source: a unit without a passed decision can never complete.
func (s *Store) LatestWorkUnitVerification(ctx context.Context, taskID, workUnitID string) (decision string, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT decision FROM verification_attempts
		 WHERE task_id = ? AND work_unit_id = ?
		 ORDER BY sequence DESC LIMIT 1`, taskID, workUnitID).Scan(&decision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("load latest work unit verification: %w", err)
	}
	return decision, true, nil
}
