// Package workunit implements the M9 Stage A/B1 Work Unit driver (issues
// #106/#109): operator-defined durable subtasks of one task, executed through
// the EXISTING agent loop path under a bounded shared/exclusive scheduler.
// The durable object is the work; the model/provider session is a disposable
// executor. This package adds no second agent engine, no worker pool
// framework and no external concurrency dependency: it selects the next ready
// units from persisted state and delegates each bounded run to a
// caller-provided RunFunc (the composition root wires agent.Loop there; the
// driver never calls provider.Client or tools directly). Stage A behavior is
// the concurrency=1 case of the scheduler.
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

// ErrWorkUnitConcurrency reports an out-of-range scheduler bound (issue
// #109): values below MinConcurrency or above MaxConcurrency fail before any
// Work Unit executes. The bound is operator configuration, never inferred
// from provider/model identity or observed success.
var ErrWorkUnitConcurrency = errors.New("invalid work unit concurrency")

// Driver executes the Work Unit chain for one task under the bounded
// shared/exclusive scheduler. It is a pure coordinator over the durable
// store; all execution is delegated to RunFunc. Concurrency is the Stage B1
// operator bound (issue #109): shared (provably read-only) units run in
// parallel up to Concurrency; exclusive units (omitted/effectful/unknown
// envelopes) never overlap any other unit. Zero means DefaultConcurrency
// (1): the Stage A serial contract.
type Driver struct {
	Store  *state.Store
	TaskID string
	// AllowedTools is the parent task's tool envelope (registry tool ids).
	AllowedTools []string
	// TaskWorkspace is the parent task's workspace root (ownership scope).
	TaskWorkspace string
	// Concurrency is the operator-selected scheduler bound for this task's
	// Work Units. Zero = 1 (Stage A behavior); values outside
	// [MinConcurrency, MaxConcurrency] fail before any unit runs.
	Concurrency int
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
	if strings.TrimSpace(existing.ParentWorkUnitID) != strings.TrimSpace(def.ParentWorkUnitID) {
		return fmt.Errorf("%w: parent work unit changed", ErrWorkUnitDefinitionDrift)
	}
	if strings.TrimSpace(existing.WorkspaceScope) != strings.TrimSpace(def.WorkspaceScope) {
		return fmt.Errorf("%w: workspace scope changed", ErrWorkUnitDefinitionDrift)
	}
	if existing.ContextBudget != def.ContextBudget || existing.ProviderBudget != def.ProviderBudget || existing.StepBudget != def.StepBudget {
		return fmt.Errorf("%w: budgets changed", ErrWorkUnitDefinitionDrift)
	}
	// The nil-vs-explicit-empty distinction is SECURITY-SIGNIFICANT (issue
	// #106 review): omitted tools = task default surface, explicit [] =
	// no tools. A persisted omitted envelope re-supplied as [] is material
	// narrowing and must fail, never be treated as identical.
	if !toolEnvelopeIdentical(existing.Tools, def.Tools) {
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

// toolEnvelopeIdentical preserves the nil (omitted = task default) versus
// explicitly-empty ([]) identity of the tool envelope: both the nil-ness and
// the unordered set must match.
func toolEnvelopeIdentical(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return stringSetsEqual(a, b)
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
			// A dependency outside this batch is a PRE-EXISTING persisted
			// unit (partial re-supply); its actual existence is validated by
			// CreateWorkUnit against the store. Only batch-internal edges
			// constrain ordering.
			if _, ok := byID[dep]; !ok {
				continue
			}
			outgoing[dep] = append(outgoing[dep], def.WorkUnitID)
			indegree[def.WorkUnitID]++
		}
		// Parent relationships are graph edges of the same durable contract
		// (CreateWorkUnit requires the parent to exist), so ordering and
		// cycle validation cover them identically to dependencies. An
		// out-of-batch parent is assumed pre-existing; the store validates it.
		if strings.TrimSpace(def.ParentWorkUnitID) != "" {
			parent := def.ParentWorkUnitID
			if _, ok := byID[parent]; !ok {
				continue
			}
			outgoing[parent] = append(outgoing[parent], def.WorkUnitID)
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

// RunAll executes the Work Unit chain under the bounded shared/exclusive
// scheduler (issue #109). With Concurrency == 1 (the default / Stage A
// contract) behavior is exactly the serial Stage A chain: repeatedly select
// the NEXT ready unit in deterministic order, validate its envelope again,
// transition it to running (persisted before dispatch), delegate the bounded
// run to RunFunc, then transition on the loop outcome AND the unit's own
// verification decision. Concurrency > 1 additionally overlaps INDEPENDENT,
// provably read-only (shared-lane) units up to the configured bound, while
// exclusive units never overlap any other unit. Completed units are never
// selected again; the chain stops (fail closed) on the first non-completed
// terminal unit (after the current bounded batch settles), leaving the
// parent gate open.
func (d *Driver) RunAll(ctx context.Context, run RunFunc) error {
	concurrency := d.Concurrency
	if concurrency == 0 {
		concurrency = DefaultConcurrency
	}
	if concurrency < MinConcurrency || concurrency > MaxConcurrency {
		return fmt.Errorf("%w: %d (allowed %d..%d)", ErrWorkUnitConcurrency, concurrency, MinConcurrency, MaxConcurrency)
	}
	scheduler := &boundedScheduler{
		driver:      d,
		runFunc:     run,
		ctx:         ctx,
		concurrency: concurrency,
		settleCh:    make(chan settleEvent, MaxConcurrency),
	}
	return scheduler.schedule()
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
