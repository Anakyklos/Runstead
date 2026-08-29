// Package workunit implements the M9 Stage A serial Work Unit driver (issue
// #106): operator-defined durable subtasks of one task, executed at most one
// at a time through the EXISTING agent loop path. The durable object is the
// work; the model/provider session is a disposable executor. This package
// adds no second agent engine, no worker pool and no concurrency: it selects
// the next ready unit from persisted state and delegates each bounded run to
// a caller-provided RunFunc (the composition root wires agent.Loop there; the
// driver never calls provider.Client or tools directly).
package workunit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// Definition is one operator-declared Work Unit from the --workunits file.
// Control-plane input only: the model can never create or modify units.
type Definition struct {
	WorkUnitID       string          `json:"work_unit_id"`
	Objective        string          `json:"objective"`
	ParentWorkUnitID string          `json:"parent_work_unit_id,omitempty"`
	Dependencies     []string        `json:"dependencies,omitempty"`
	Tools            []string        `json:"tools,omitempty"`
	WorkspaceScope   string          `json:"workspace_scope,omitempty"`
	AcceptancePlan   json.RawMessage `json:"acceptance_plan,omitempty"`
	ContextBudget    int             `json:"context_budget,omitempty"`
	ProviderBudget   int             `json:"provider_budget,omitempty"`
	StepBudget       int             `json:"step_budget,omitempty"`
}

// RunResult is the bounded outcome of one Work Unit loop run.
type RunResult struct {
	// Outcome mirrors the agent terminal outcome classes used by the driver:
	// "completed", "failed", "blocked", "uncertain", "canceled". Only the
	// composition root can produce it (from the real loop result); the model
	// never supplies it.
	Outcome string
	// Reason is the bounded operator-facing reason for non-success outcomes.
	Reason string
}

// RunFunc executes one Work Unit through the existing loop path. The driver
// guarantees at most one invocation at a time and only for ready units whose
// capability envelope is contained.
type RunFunc func(ctx context.Context, unit state.WorkUnit) (RunResult, error)

// ErrCapabilityEscalation reports a Work Unit envelope outside the parent
// task contract. It fails before any effect.
var ErrCapabilityEscalation = errors.New("work unit capability envelope exceeds parent task contract")

// ErrWorkUnitBlockedChain reports that the serial chain stopped early with a
// unit in a non-completed state; the parent completion gate remains open.
var ErrWorkUnitBlockedChain = errors.New("work unit chain stopped with open units")

// ErrParentCompletionGated reports that the parent task cannot finalize while
// required Work Units are unresolved (open units listed in Reason).
var ErrParentCompletionGated = errors.New("parent completion gated by open work units")

// ErrWorkUnitDefinitionDrift reports that a re-supplied Work Unit id carries
// a materially different definition than the persisted one. Re-supply is
// idempotent ONLY for identical definitions; drift fails closed so a changed
// operator intent can never silently apply to an already-persisted unit.
var ErrWorkUnitDefinitionDrift = errors.New("work unit definition drift")

// Driver executes the serial Stage A chain for one task. It is a pure
// coordinator over the durable store; all execution is delegated to RunFunc.
type Driver struct {
	Store  *state.Store
	TaskID string
	// AllowedTools is the parent task's tool envelope (registry tool ids).
	AllowedTools []string
	// TaskWorkspace is the parent task's workspace root (ownership scope).
	TaskWorkspace string
}

// ValidateEnvelope checks the containment rule
// workunit capabilities ⊆ parent task capabilities, fail-before-effect.
func (d *Driver) ValidateEnvelope(toolList []string, workspaceScope string) error {
	allowed := make(map[string]bool, len(d.AllowedTools))
	for _, tool := range d.AllowedTools {
		allowed[tool] = true
	}
	for _, tool := range toolList {
		if !allowed[tool] {
			return fmt.Errorf("%w: tool %q is not in the parent task envelope", ErrCapabilityEscalation, tool)
		}
	}
	// The workspace scope is validated unconditionally: an empty tool list
	// is a valid fail-closed envelope (no tools) and must never short-circuit
	// scope containment (issue #106 review). The canonical representation is
	// WORKSPACE-RELATIVE (matching tools.NormalizeWorkspacePath): absolute
	// paths and ".." traversal are rejected here, and every tool execution
	// resolves through the same resolver so a scope can never escape the
	// parent workspace.
	if strings.TrimSpace(workspaceScope) != "" {
		if _, failure := tools.NormalizeWorkspacePath(workspaceScope); failure != nil {
			return fmt.Errorf("%w: workspace scope %q must be a valid relative path inside the task workspace (%v)", ErrCapabilityEscalation, workspaceScope, failure)
		}
	}
	return nil
}

// EnsureDefinitions idempotently creates the missing operator Work Units. It
// never mutates existing units; any invalid definition (missing dependency,
// cycle, capability escalation, malformed acceptance plan, definition drift)
// fails the whole call before any unit is executed. A valid DAG is created in
// dependency order, so the JSON file's own ordering is irrelevant.
func (d *Driver) EnsureDefinitions(ctx context.Context, definitions []Definition) (created, skipped int, err error) {
	ordered, err := topologicalDefinitions(definitions)
	if err != nil {
		return 0, 0, err
	}
	// Validate every definition (id, envelope, acceptance plan) BEFORE any
	// unit is persisted: one invalid definition fails the whole call without
	// leaving partially created units behind.
	type inspected struct {
		def    Definition
		digest string
	}
	inspectedDefs := make([]inspected, 0, len(ordered))
	for _, def := range ordered {
		if strings.TrimSpace(def.WorkUnitID) == "" {
			return 0, 0, errors.New("work unit definition requires work_unit_id")
		}
		if err := d.ValidateEnvelope(def.Tools, def.WorkspaceScope); err != nil {
			return 0, 0, err
		}
		digest := ""
		if len(def.AcceptancePlan) > 0 {
			plan, planErr := verifier.ParsePlan(def.AcceptancePlan)
			if planErr != nil {
				return 0, 0, fmt.Errorf("work unit %s acceptance plan: %w", def.WorkUnitID, planErr)
			}
			digest = plan.Digest()
		}
		inspectedDefs = append(inspectedDefs, inspected{def: def, digest: digest})
	}
	for _, item := range inspectedDefs {
		def := item.def
		existing, getErr := d.Store.GetWorkUnit(ctx, d.TaskID, def.WorkUnitID)
		if getErr == nil && existing != nil {
			if err := ensureIdenticalDefinition(*existing, def, item.digest); err != nil {
				return 0, 0, fmt.Errorf("work unit %s: %w", def.WorkUnitID, err)
			}
			skipped++ // idempotent re-supply of the SAME definition
			continue
		}
		if getErr != nil && !errors.Is(getErr, state.ErrWorkUnitNotFound) {
			return 0, 0, getErr
		}
		if _, createErr := d.Store.CreateWorkUnit(ctx, state.WorkUnitCreate{
			TaskID:           d.TaskID,
			WorkUnitID:       def.WorkUnitID,
			ParentWorkUnitID: def.ParentWorkUnitID,
			Objective:        def.Objective,
			Dependencies:     def.Dependencies,
			Tools:            def.Tools,
			WorkspaceScope:   def.WorkspaceScope,
			AcceptancePlan:   def.AcceptancePlan,
			AcceptanceDigest: item.digest,
			ContextBudget:    def.ContextBudget,
			ProviderBudget:   def.ProviderBudget,
			StepBudget:       def.StepBudget,
		}); createErr != nil {
			return created, skipped, fmt.Errorf("work unit %s: %w", def.WorkUnitID, createErr)
		}
		created++
	}
	return created, skipped, nil
}

// ensureIdenticalDefinition compares a persisted unit against its re-supplied
// definition. Any material divergence (objective, tool envelope, workspace
// scope, dependencies, budgets, acceptance digest) fails closed: replay never
// mutates a persisted unit, and never silently accepts a changed contract.
func ensureIdenticalDefinition(existing state.WorkUnit, def Definition, digest string) error {
	if strings.TrimSpace(existing.Objective) != strings.TrimSpace(def.Objective) {
		return fmt.Errorf("%w: objective changed", ErrWorkUnitDefinitionDrift)
	}
	if strings.TrimSpace(existing.WorkspaceScope) != strings.TrimSpace(def.WorkspaceScope) {
		return fmt.Errorf("%w: workspace scope changed", ErrWorkUnitDefinitionDrift)
	}
	if existing.ContextBudget != def.ContextBudget || existing.ProviderBudget != def.ProviderBudget || existing.StepBudget != def.StepBudget {
		return fmt.Errorf("%w: budgets changed", ErrWorkUnitDefinitionDrift)
	}
	if !stringSetsEqual(existing.Tools, def.Tools) {
		return fmt.Errorf("%w: tool envelope changed", ErrWorkUnitDefinitionDrift)
	}
	if !stringSetsEqual(existing.Dependencies, def.Dependencies) {
		return fmt.Errorf("%w: dependency set changed", ErrWorkUnitDefinitionDrift)
	}
	if existing.AcceptanceDigest != digest {
		return fmt.Errorf("%w: acceptance plan changed", ErrWorkUnitDefinitionDrift)
	}
	return nil
}

// stringSetsEqual compares two lists as unordered sets with non-empty trimmed
// elements.
func stringSetsEqual(a, b []string) bool {
	left := make(map[string]struct{}, len(a))
	for _, item := range a {
		if item = strings.TrimSpace(item); item != "" {
			left[item] = struct{}{}
		}
	}
	right := make(map[string]struct{}, len(b))
	for _, item := range b {
		if item = strings.TrimSpace(item); item != "" {
			right[item] = struct{}{}
		}
	}
	if len(left) != len(right) {
		return false
	}
	for item := range left {
		if _, ok := right[item]; !ok {
			return false
		}
	}
	return true
}

// topologicalDefinitions orders the operator definitions so every dependency
// precedes its dependents, independent of the JSON file ordering. The order
// is deterministic (lexicographic tie-breaking) and a cycle fails the call.
func topologicalDefinitions(definitions []Definition) ([]Definition, error) {
	byID := make(map[string]Definition, len(definitions))
	for _, def := range definitions {
		if _, exists := byID[def.WorkUnitID]; exists {
			return nil, fmt.Errorf("work unit definition %q appears more than once", def.WorkUnitID)
		}
		byID[def.WorkUnitID] = def
	}
	indegree := make(map[string]int, len(definitions))
	outgoing := make(map[string][]string)
	for _, def := range definitions {
		for _, dep := range def.Dependencies {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("work unit %s depends on undefined unit %q", def.WorkUnitID, dep)
			}
			outgoing[dep] = append(outgoing[dep], def.WorkUnitID)
			indegree[def.WorkUnitID]++
		}
	}
	queue := make([]string, 0, len(definitions))
	for _, def := range definitions {
		if indegree[def.WorkUnitID] == 0 {
			queue = append(queue, def.WorkUnitID)
		}
	}
	ordered := make([]Definition, 0, len(definitions))
	for len(queue) > 0 {
		sort.Strings(queue)
		current := queue[0]
		queue = queue[1:]
		ordered = append(ordered, byID[current])
		next := append([]string(nil), outgoing[current]...)
		sort.Strings(next)
		for _, dependent := range next {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}
	if len(ordered) != len(definitions) {
		return nil, fmt.Errorf("work unit dependency cycle detected: %d definition(s) cannot be ordered", len(definitions)-len(ordered))
	}
	return ordered, nil
}

// openUnits lists the units that keep the parent completion gate open, in
// deterministic order (for diagnostics and the gate error).
func (d *Driver) openUnits(ctx context.Context) ([]state.WorkUnit, error) {
	units, err := d.Store.ListWorkUnits(ctx, d.TaskID)
	if err != nil {
		return nil, err
	}
	var open []state.WorkUnit
	for _, unit := range units {
		if unit.Status != "completed" {
			open = append(open, unit)
		}
	}
	return open, nil
}

// GateParent fails closed while any required unit is not completed.
func (d *Driver) GateParent(ctx context.Context) error {
	open, err := d.openUnits(ctx)
	if err != nil {
		return err
	}
	if len(open) == 0 {
		return nil
	}
	parts := make([]string, 0, len(open))
	for _, unit := range open {
		parts = append(parts, unit.WorkUnitID+":"+unit.Status)
	}
	return fmt.Errorf("%w: %s", ErrParentCompletionGated, strings.Join(parts, ", "))
}

// RunAll executes the serial chain: repeatedly select the FIRST ready unit in
// deterministic order, validate its envelope again, transition it to running
// (persisted before dispatch), delegate the bounded run to RunFunc, then
// transition on the loop outcome AND the unit's own verification decision.
// Completed units are never selected again; the chain stops (fail closed) on
// the first non-completed terminal unit, leaving the parent gate open.
func (d *Driver) RunAll(ctx context.Context, run RunFunc) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Operator-resolved approvals unblock units that paused on
		// approval_required: the transition is tied to authoritative
		// resolution state (zero pending approvals for the unit's own
		// actions), never to an arbitrary blocked reason.
		if err := d.resolveBlockedWorkUnits(ctx); err != nil {
			return err
		}
		ready, err := d.Store.ReadyWorkUnits(ctx, d.TaskID)
		if err != nil {
			return err
		}
		if len(ready) == 0 {
			return nil
		}
		unit := ready[0] // deterministic: creation order

		// Re-validate the envelope against the live parent contract before
		// any effect (escalation can never sneak in after create).
		if err := d.ValidateEnvelope(unit.Tools, unit.WorkspaceScope); err != nil {
			return err
		}
		if unit.Status == "created" {
			if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "created", "ready", ""); err != nil {
				return err
			}
		}
		if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "ready", "running", ""); err != nil {
			return err
		}

		result, runErr := run(ctx, unit)
		if runErr != nil {
			// The unit stays 'running': recovery reset handles interruption;
			// the error propagates without a fabricated terminal state.
			return runErr
		}
		switch result.Outcome {
		case "completed":
			decision, found, decisionErr := d.Store.LatestWorkUnitVerification(ctx, d.TaskID, unit.WorkUnitID)
			if decisionErr != nil {
				return decisionErr
			}
			if !found || decision != "passed" {
				// Evidence-backed verification is mandatory: narrative alone
				// never completes a unit (issue #106).
				reason := "verification did not pass for work unit"
				if found {
					reason = "verification decision " + decision
				}
				if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "running", "blocked", reason); err != nil {
					return err
				}
				return fmt.Errorf("%w: %s", ErrWorkUnitBlockedChain, unit.WorkUnitID)
			}
			if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "running", "completed", ""); err != nil {
				return err
			}
		case "failed":
			reason := result.Reason
			if reason == "" {
				reason = "work unit failed"
			}
			if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "running", "failed", reason); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s", ErrWorkUnitBlockedChain, unit.WorkUnitID)
		case "blocked":
			reason := result.Reason
			if reason == "" {
				reason = "work unit blocked"
			}
			if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "running", "blocked", reason); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s", ErrWorkUnitBlockedChain, unit.WorkUnitID)
		case "uncertain":
			reason := result.Reason
			if reason == "" {
				reason = "work unit outcome uncertain"
			}
			if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "running", "uncertain", reason); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s", ErrWorkUnitBlockedChain, unit.WorkUnitID)
		default:
			// canceled / aborted: leave the unit 'running' for recovery reset.
			return fmt.Errorf("work unit %s interrupted before a terminal outcome", unit.WorkUnitID)
		}
	}
}

// resolveBlockedWorkUnits moves approval-blocked units back to ready after
// the operator resolved every pending approval of the unit's actions. Any
// other blocking reason stays untouched: the transition requires BOTH the
// persisted approval cause AND the authoritative zero-pending state.
func (d *Driver) resolveBlockedWorkUnits(ctx context.Context) error {
	units, err := d.Store.ListWorkUnits(ctx, d.TaskID)
	if err != nil {
		return err
	}
	for _, unit := range units {
		if unit.Status != "blocked" || !strings.Contains(unit.BlockingReason, "approval") {
			continue
		}
		pending, err := d.Store.WorkUnitPendingApprovalCount(ctx, d.TaskID, unit.WorkUnitID)
		if err != nil {
			return err
		}
		if pending == 0 {
			if err := d.Store.TransitionWorkUnit(ctx, d.TaskID, unit.WorkUnitID, "blocked", "ready", "operator approval resolved"); err != nil {
				return err
			}
		}
	}
	return nil
}

// OpenUnits returns the currently open units (diagnostics).
func (d *Driver) OpenUnits(ctx context.Context) ([]state.WorkUnit, error) {
	return d.openUnits(ctx)
}

// SortedIDs returns the deterministic sorted unit ids (used by tests/diag).
func SortedIDs(units []state.WorkUnit) []string {
	ids := make([]string, 0, len(units))
	for _, unit := range units {
		ids = append(ids, unit.WorkUnitID)
	}
	sort.Strings(ids)
	return ids
}
