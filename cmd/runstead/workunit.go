package main

// Work Unit wiring (issues #106/#109): the operator supplies a --workunits
// JSON file; the driver persists the definitions, executes them through the
// bounded shared/exclusive scheduler by building one bounded agent.Loop per
// unit (the EXISTING loop, never a second engine), each with the unit
// acceptance plan as its verifier and the unit budgets in its limits, and
// keeps the parent completion gate closed while any unit is unresolved.
// Every provider attempt still flows through the governor-owned executor
// constructed by run/resume; the scheduler bound is operator configuration
// persisted with the task and enforced fail-closed on resume.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
	"github.com/RenyEnnos/Runstead/internal/workunit"
)

// exitWorkUnitCanceled mirrors the agent canceled outcome exit code for the
// work unit chain (context canceled before a terminal unit outcome).
const exitWorkUnitCanceled = 130

// exitWorkUnitGated is the CLI exit code for a parent completion gate refusal
// (required Work Units unresolved). It mirrors the control-plane blocked
// semantics of OutcomeVerificationBlocked (33) and leaves the task durable
// and resumable.
const exitWorkUnitGated = 33

// unitLoopPieces carries the composition-root objects the unit loop must
// reuse: governor-owned executor, registry, policy surfaces and budgets.
type unitLoopPieces struct {
	runner              agent.AttemptRunner
	registry            *tools.Registry
	model               string
	providerIdentity    provider.Identity
	trace               agent.TraceSink
	store               *state.Store
	policy              policy.Policy
	writePolicy         string
	recipePolicy        string
	recipeCatalogDigest string
	limits              agent.Limits
	// recovery grounds evidence + repeat guard for resumed unit runs (nil =
	// fresh run). Counters stay per-unit; task-level accounting stays
	// authoritative through the governor.
	recovery *agent.RecoverySeed
}

// loadWorkUnitFile parses the operator-defined Work Unit file. Unknown fields
// are rejected (strict decoder) so typos fail closed.
func loadWorkUnitFile(path string) ([]workunit.Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read work units file: %w", err)
	}
	var definitions []workunit.Definition
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&definitions); err != nil {
		return nil, fmt.Errorf("parse work units file: %w", err)
	}
	if len(definitions) == 0 {
		return nil, errors.New("work units file contains no definitions")
	}
	return definitions, nil
}

// registryToolIDs extracts the parent task's tool envelope (registry tools).
func registryToolIDs(registry *tools.Registry) []string {
	specs := registry.Describe()
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		ids = append(ids, spec.Name)
	}
	return ids
}

// bootstrapTaskForWorkUnits persists the task root and operator acceptance
// plan BEFORE the chain in work unit mode: the chain runs before the parent
// loop (which normally bootstraps the task), so the durable task row
// must exist for CreateWorkUnit. Unit and parent loops receive a non-nil
// (possibly empty) Recovery seed to skip the loop-internal re-bootstrap.
// It reuses agent.BootstrapTask + agent.ConfigSnapshot, the SAME bootstrap
// the normal loop uses, so a Work Unit task persists the full authoritative
// execution configuration (provider identity, protocol/config identity,
// exact model, policies, recipe catalog digest, acceptance digest, limits)
// PLUS the effective Work Unit scheduler bound (issue #109) as a durable,
// inspectable, resume-safe configuration.
func bootstrapTaskForWorkUnits(ctx context.Context, store *state.Store, taskID, objective, workspace, model string, plan *verifier.Plan, identity provider.Identity, writePolicy, recipePolicy, recipeCatalogDigest, acceptanceDigest string, limits agent.Limits, registry *tools.Registry, workUnitConcurrency int, frozen ...state.ExecutionContractRecord) error {
	snapshot := agent.ConfigSnapshot(registry, model, identity, writePolicy, recipePolicy, recipeCatalogDigest, acceptanceDigest, limits)
	merged, err := withWorkUnitConcurrency(snapshot, workUnitConcurrency)
	if err != nil {
		return err
	}
	snapshot = merged
	record := state.TaskRecord{
		TaskID:     taskID,
		Objective:  objective,
		Workspace:  workspace,
		Model:      model,
		ConfigJSON: snapshot,
	}
	if len(frozen) > 1 {
		return fmt.Errorf("work unit bootstrap received more than one execution contract")
	}
	if len(frozen) == 1 {
		record.ExecutionContractJSON = append([]byte(nil), frozen[0].JSON...)
		record.ExecutionContractHash = frozen[0].Hash
	}
	return agent.BootstrapTask(ctx, store, record, plan, registry)
}

// withWorkUnitConcurrency merges the effective scheduler bound into the
// task configuration snapshot under the durable key (issue #109). It is the
// ONLY place the bound is persisted; resume reads it back from the same
// key and rejects an explicitly different operator value fail-closed.
func withWorkUnitConcurrency(snapshot []byte, concurrency int) ([]byte, error) {
	var values map[string]any
	if err := json.Unmarshal(snapshot, &values); err != nil {
		// The snapshot is produced by agent.ConfigSnapshot: it is always
		// valid JSON. A corrupt render must FAIL, never silently drop the
		// scheduler contract (issue #109 review).
		return nil, fmt.Errorf("encode work unit scheduler configuration: %w", err)
	}
	values[state.WorkUnitConcurrencyKey] = concurrency
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode work unit scheduler configuration: %w", err)
	}
	return encoded, nil
}

// unitChainTraceSink wraps the CLI trace sink so concurrent unit loops of
// one chain can emit lifecycle lines safely (issue #109): under concurrency
// several loop goroutines share the sink, and io.Writers such as the test
// strings.Builder are not safe for concurrent writes. The mutex serializes
// emission only; it changes no trace content.
func unitChainTraceSink(errOut io.Writer) agent.TraceSink {
	var mu sync.Mutex
	base := cliTraceSink(errOut)
	return func(line agent.TraceLine) {
		mu.Lock()
		defer mu.Unlock()
		base(line)
	}
}

// runUnitLoop executes ONE Work Unit through the existing agent loop with the
// unit objective, its acceptance plan as the verifier and its budgets applied
// to the loop limits. The loop persists every action/attempt/verification row
// tagged with the unit (work_unit_id provenance).
func runUnitLoop(ctx context.Context, pieces unitLoopPieces, taskID string, unit state.WorkUnit) (workunit.RunResult, error) {
	limits := pieces.limits
	if unit.ProviderBudget > 0 {
		limits.ProviderBudget = unit.ProviderBudget
	}
	if unit.StepBudget > 0 {
		limits.MaxSteps = unit.StepBudget
	}
	// Capability/workspace enforcement (issue #106 review): the unit runs
	// inside a RESTRICTED registry view. Driver.ValidateEnvelope validates
	// the declaration; this is the runtime boundary. Tools outside the
	// unit's declared envelope are not registered for the unit's session:
	// the protocol parser rejects them deterministically (unknown_tool
	// correction) BEFORE any action record, policy decision or effect, and
	// the registry refuses to execute them as a second line of defense.
	// WorkspaceScope bounds every tool to a sub-root of the task workspace.
	//
	// Tool-list semantics (single intentional contract, aligned with
	// spec.md/docs/tests): OMITTED tools (nil) mean the task default surface
	// (no restriction); EXPLICITLY EMPTY tools ([]) mean a fail-closed
	// no-tools envelope. A scope restriction without an explicit tool list
	// is fail-closed as well: declaring a boundary but no tools never grants
	// the full parent surface implicitly.
	registry := pieces.registry
	if unit.Tools != nil || strings.TrimSpace(unit.WorkspaceScope) != "" {
		restricted, err := pieces.registry.Restricted(unit.Tools, unit.WorkspaceScope)
		if err != nil {
			return workunit.RunResult{}, fmt.Errorf("work unit %s capability envelope: %w", unit.WorkUnitID, err)
		}
		registry = restricted
	}
	var plan *verifier.Plan
	if strings.TrimSpace(unit.AcceptancePlanSpec) != "" {
		parsed, err := verifier.ParsePlan([]byte(unit.AcceptancePlanSpec))
		if err != nil {
			return workunit.RunResult{}, fmt.Errorf("work unit %s acceptance plan: %w", unit.WorkUnitID, err)
		}
		plan = parsed
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner:               pieces.runner,
		Registry:             registry,
		Limits:               limits,
		Model:                pieces.model,
		ProviderIdentity:     pieces.providerIdentity,
		Trace:                pieces.trace,
		State:                pieces.store,
		Policy:               pieces.policy,
		WritePolicy:          pieces.writePolicy,
		RecipePolicy:         pieces.recipePolicy,
		RecipeCatalogDigest:  pieces.recipeCatalogDigest,
		Verifier:             verifier.New(registry, plan),
		AcceptancePlanDigest: unit.AcceptanceDigest,
		Recovery:             pieces.recovery,
	})
	if err != nil {
		return workunit.RunResult{}, fmt.Errorf("work unit %s loop: %w", unit.WorkUnitID, err)
	}
	result := loop.Run(ctx, agent.Task{ID: taskID, Prompt: unit.Objective, WorkUnitID: unit.WorkUnitID, SkipTaskFinalize: true})
	return mapUnitLoopResult(result)
}

// mapUnitLoopResult maps the loop's terminal outcome onto the driver's
// lifecycle vocabulary. The model never supplies these; they come from the
// real governed loop.
func mapUnitLoopResult(result agent.Result) (workunit.RunResult, error) {
	switch result.Outcome {
	case agent.OutcomeCompleted:
		return workunit.RunResult{Outcome: "completed"}, nil
	case agent.OutcomeCanceled:
		return workunit.RunResult{Outcome: "canceled", Reason: result.StopReason}, nil
	case agent.OutcomeApprovalRequired, agent.OutcomeVerificationBlocked, agent.OutcomeFinalIncomplete:
		return workunit.RunResult{Outcome: "blocked", Reason: result.StopReason}, nil
	case agent.OutcomeStepsExhausted, agent.OutcomeCorrectionsExhausted, agent.OutcomeRepeatedAction,
		agent.OutcomeTimeBudgetExhausted, agent.OutcomeProviderBudgetExhausted,
		agent.OutcomeAccountDelayTimeout, agent.OutcomeAccountCircuitOpen,
		agent.OutcomeFinalNotGrounded, agent.OutcomeProviderFailure,
		agent.OutcomePersistenceFailure, agent.OutcomeConsecutiveFailuresExhausted,
		agent.OutcomeVerificationFailuresExhausted:
		return workunit.RunResult{Outcome: "failed", Reason: result.StopReason}, nil
	default:
		// Uncertain/conservative terminal states map to uncertain and block
		// the chain; they are never silently retried.
		return workunit.RunResult{Outcome: "uncertain", Reason: result.StopReason}, nil
	}
}

// runWorkUnitChain runs the bounded shared/exclusive chain for one task and
// returns the open-units gate state. The parent loop ONLY proceeds when the
// gate is closed (no error and no open units).
func runWorkUnitChain(ctx context.Context, store *state.Store, taskID, workspace string, registry *tools.Registry, definitions []workunit.Definition, concurrency int, unitRun func(context.Context, state.WorkUnit) (workunit.RunResult, error)) error {
	driver := &workunit.Driver{
		Store:         store,
		TaskID:        taskID,
		AllowedTools:  registryToolIDs(registry),
		TaskWorkspace: workspace,
		Concurrency:   concurrency,
	}
	if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
		return fmt.Errorf("work unit definitions: %w", err)
	}
	if err := driver.RunAll(ctx, unitRun); err != nil {
		return err
	}
	return driver.GateParent(ctx)
}
