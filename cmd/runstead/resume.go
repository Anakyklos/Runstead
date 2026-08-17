package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/trace"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// Resume-specific exit codes. The generic codes (0 success, 1 not found,
// 2 usage, 3 state unavailable) and the agent outcome codes (20+) are shared.
const (
	exitNotResumable    = 4 // task is already terminal (completed/failed/canceled)
	exitHumanReview     = 5 // automatic continuation stopped; human review required
	exitCorrupt         = 6 // corrupted or incompatible persisted state
	exitGovernorBlocked = 7 // continuation blocked by restored account protection
)

// resumeCommand implements `runstead resume <task-id>`. It reconstructs the
// interrupted task from durable state (issue #9), reconciles interrupted
// attempts, reconstructs a bounded model context, restores the persisted
// governor protection state and continues through the normal governed agent
// loop. The provider conversation is never assumed to survive: the provider
// input (scripted responses) is supplied again at resume time and the model
// continues from the reconstructed context.
func resumeCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if hasHelp(args) {
		printResumeHelp(out)
		return exitSuccess
	}

	taskID := ""
	stateDir := ""
	scripted := ""
	logLevel := ""
	minStartInterval := ""
	intervalSet := false
	writePolicy := ""
	recipesFile := ""
	recipePolicy := ""
	acceptanceFile := ""
	// Parse manually so flags may appear before or after the task id (the flag
	// package stops at the first positional argument).
	for index := 0; index < len(args); index++ {
		arg := args[index]
		value := func(name string) (string, bool) {
			if index+1 >= len(args) {
				fmt.Fprintf(errOut, "resume: %s requires a value\n", name)
				return "", false
			}
			index++
			return args[index], true
		}
		switch {
		case arg == "--state-dir":
			if next, ok := value("--state-dir"); ok {
				stateDir = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--state-dir="):
			stateDir = strings.TrimPrefix(arg, "--state-dir=")
		case arg == "--scripted":
			if next, ok := value("--scripted"); ok {
				scripted = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--scripted="):
			scripted = strings.TrimPrefix(arg, "--scripted=")
		case arg == "--log-level":
			if next, ok := value("--log-level"); ok {
				logLevel = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--log-level="):
			logLevel = strings.TrimPrefix(arg, "--log-level=")
		case arg == "--write-policy":
			if next, ok := value("--write-policy"); ok {
				writePolicy = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--write-policy="):
			writePolicy = strings.TrimPrefix(arg, "--write-policy=")
		case arg == "--recipes":
			if next, ok := value("--recipes"); ok {
				recipesFile = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--recipes="):
			recipesFile = strings.TrimPrefix(arg, "--recipes=")
		case arg == "--recipe-policy":
			if next, ok := value("--recipe-policy"); ok {
				recipePolicy = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--recipe-policy="):
			recipePolicy = strings.TrimPrefix(arg, "--recipe-policy=")
		case arg == "--acceptance":
			if next, ok := value("--acceptance"); ok {
				acceptanceFile = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--acceptance="):
			acceptanceFile = strings.TrimPrefix(arg, "--acceptance=")
		case arg == "--min-start-interval":
			if next, ok := value("--min-start-interval"); ok {
				minStartInterval = next
				intervalSet = true
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--min-start-interval="):
			minStartInterval = strings.TrimPrefix(arg, "--min-start-interval=")
			intervalSet = true
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(errOut, "resume: unknown flag %q\n", arg)
			printResumeHelp(errOut)
			return exitUsage
		default:
			if taskID != "" {
				fmt.Fprintln(errOut, "resume: exactly one task id is required")
				printResumeHelp(errOut)
				return exitUsage
			}
			taskID = arg
		}
	}
	if taskID == "" {
		fmt.Fprintln(errOut, "resume: exactly one task id is required")
		printResumeHelp(errOut)
		return exitUsage
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(errOut, "resume: canceled\n")
		return agent.OutcomeCanceled.ExitCode()
	}

	dir, err := resolveStateDir(stateDir, stateDir != "")
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUsage
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()

	// The persisted governor protection state is authoritative across restart:
	// restore it so account usage, cooldown, circuit and receipts survive.
	var restored *governor.PersistedState
	if snapshot, ok, loadErr := store.GovernorState(ctx); loadErr != nil {
		fmt.Fprintf(errOut, "resume: cannot restore account protection state: %v\n", loadErr)
		return exitUnavailable
	} else if ok {
		restored = &snapshot
	}

	accountConfig, err := resolveResumeGovernorConfig(restored, minStartInterval, intervalSet)
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUsage
	}

	level, err := trace.ParseLevel(resolveResumeLogLevel(logLevel))
	if err != nil {
		fmt.Fprintf(errOut, "resume: invalid configuration: %v\n", err)
		return exitUsage
	}
	logger := trace.NewLogger(errOut, level)
	traceSink := cliTraceSink(errOut)

	// The account governor is rebuilt from the persisted protection projection
	// BEFORE the recovery pipeline so resume can report account-protection
	// blocks without starting the loop, and so the resumed loop runs under the
	// same restored protection as the interrupted run.
	accountGovernor, err := governor.New(accountConfig, governor.Options{
		Events:      trace.NewPolicySink(logger),
		Persistence: store,
		Restore:     restored,
	})
	if err != nil {
		fmt.Fprintf(errOut, "resume: invalid account policy: %v\n", err)
		return exitUsage
	}

	// Pre-flight validation BEFORE the recovery pipeline: a resume invocation
	// that cannot possibly execute (missing workspace, undecodable persisted
	// configuration, no provider input) must fail without journaling recovery
	// events or inflating the resume count. The snapshot load also carries the
	// task-level errors (not found, already terminal) with their typed codes.
	preload, err := store.LoadRecoverySnapshot(ctx, taskID)
	if errors.Is(err, state.ErrTaskNotFound) {
		fmt.Fprintf(errOut, "resume: task %q not found in %s\n", taskID, dir)
		return exitNotFound
	}
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitCorrupt
	}
	switch preload.Task.Status {
	case "human_review_required":
		fmt.Fprintf(errOut, "resume: task %q has an unresolved human review requirement\n", taskID)
		return exitHumanReview
	case "planned", "running":
		// resumable
	default:
		fmt.Fprintf(errOut, "resume: task %q is not resumable: %s\n", taskID, preload.Task.Status)
		return exitNotResumable
	}

	// The task workspace is part of the durable task identity: resume always
	// operates on the persisted workspace. There is deliberately no
	// --workspace override, because continuing the same task in a different
	// directory would let a final ground claims on evidence produced in the
	// original workspace while executing tools elsewhere.
	workspacePath := preload.Task.Workspace
	if _, err := tools.NewRegistry(tools.Options{Workspace: workspacePath}); err != nil {
		fmt.Fprintf(errOut, "resume: workspace unavailable: %v\n", err)
		return exitUnavailable
	}
	if _, err := limitsFromConfig(preload.Task.ConfigJSON); err != nil {
		fmt.Fprintf(errOut, "resume: invalid persisted configuration: %v\n", err)
		return exitCorrupt
	}
	// The effective write policy is part of the authoritative task
	// configuration: resume uses the persisted policy by default, and a
	// --write-policy override that diverges from it is rejected fail-closed
	// (no external authority could justify silently widening the policy).
	// This check runs in the pre-flight section, before any recovery or
	// execution side effect.
	resumePolicy, err := resolveResumeWritePolicy(preload.Task.ConfigJSON, writePolicy, writePolicy != "")
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUsage
	}
	// The effective recipe policy is part of the authoritative task
	// configuration, exactly like the write policy: resume uses the persisted
	// recipe policy by default, and a divergent --recipe-policy override is
	// rejected fail-closed before any recovery or execution side effect. The
	// recipe catalog must be re-supplied at resume time (like --scripted) and
	// must match the effective catalog the task started with; a missing or
	// drifted catalog is rejected below before any recovery or execution side
	// effect.
	resumeRecipes, err := resolveRecipeCatalog(recipesFile, recipesFile != "")
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUsage
	}
	// The recipe catalog is part of the authoritative task configuration: the
	// task started under one effective catalog, its digest was persisted with
	// the task, and resume rejects any drift fail-closed. A different catalog
	// (changed executable/argv/capabilities/env/timeout, added or removed
	// recipes) can never silently continue a task under a different recipe
	// set (issue #26 review).
	if err := resolveResumeRecipeCatalog(preload.Task.ConfigJSON, resumeRecipes); err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUsage
	}
	resumeRecipePolicy, err := resolveResumeRecipePolicy(preload.Task.ConfigJSON, recipePolicy, recipePolicy != "", resumeRecipes)
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUsage
	}
	// The acceptance plan is part of the authoritative task configuration: it
	// is persisted with the task at run start and loaded from state on resume.
	// A divergent --acceptance override is rejected fail-closed (issue #11).
	// A task that started WITHOUT a plan can never complete (the verifier
	// refuses completion blocked, issue #11 review), so the operator may
	// explicitly attach a plan at resume; that operator-owned act is persisted
	// with the task before any recovery or execution side effect.
	resumeAcceptance, resumeAcceptanceDigest, attachedPlan, err := resolveResumeAcceptancePlan(ctx, store, taskID, acceptanceFile, acceptanceFile != "")
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitUsage
	}
	// The provider input is supplied again at resume time: the original remote
	// conversation is disposable metadata, never an authority over task state.
	scriptedPath, scriptedSet := resolveScriptedFlag(scripted)
	if !scriptedSet {
		fmt.Fprintln(errOut, "resume: no provider configured. Use --scripted FILE for a deterministic offline continuation.")
		return exitUnavailable
	}
	responses, loadErr := loadScriptedResponses(scriptedPath)
	if loadErr != nil {
		fmt.Fprintf(errOut, "resume: %v\n", loadErr)
		return exitUsage
	}
	// Persist an operator-attached acceptance plan before the recovery pipeline
	// so the task is durably resumable under the SAME plan from here on.
	if attachedPlan {
		spec, marshalErr := json.Marshal(resumeAcceptance)
		if marshalErr != nil {
			fmt.Fprintf(errOut, "resume: cannot encode acceptance plan: %v\n", marshalErr)
			return exitCorrupt
		}
		if err := store.SaveAcceptancePlan(ctx, taskID, spec, resumeAcceptanceDigest); err != nil {
			fmt.Fprintf(errOut, "resume: cannot attach acceptance plan: %v\n", err)
			return exitCorrupt
		}
	}

	// The recovery pipeline: load persisted history, classify and reconcile
	// interrupted attempts, decide whether automatic continuation is safe, and
	// reconstruct the bounded model context. All transitions are journaled.
	// The block check reads the governor protection projection FRESH from the
	// store: reconciliation may have applied a conservative debit (receipt-aware
	// attempts) that the pre-pipeline restored governor cannot see.
	plan, err := recovery.Resume(ctx, store, recovery.Options{
		TaskID: taskID,
		Trace:  traceSink,
		Blocked: func() (bool, string) {
			snapshot, ok, loadErr := store.GovernorState(ctx)
			if loadErr != nil || !ok {
				return false, ""
			}
			fresh, newErr := governor.New(accountConfig, governor.Options{Restore: &snapshot})
			if newErr != nil {
				return false, ""
			}
			return recovery.GovernorBlocks(fresh, taskID)
		},
	})
	if err != nil {
		fmt.Fprintf(errOut, "resume: %v\n", err)
		return exitCorrupt
	}
	if plan.Decision == recovery.DecisionHumanReview {
		fmt.Fprintf(errOut, "resume: %s\n", plan.Reason)
		return exitHumanReview
	}
	if plan.Decision == recovery.DecisionBlocked {
		fmt.Fprintf(errOut, "resume: continuation blocked: %s\n", plan.Reason)
		return exitGovernorBlocked
	}

	// Wire the resumed task with the same control boundaries as a normal run:
	// the restored account governor, the read-only registry, the protocol
	// parser and the persistence boundary.
	fakeClient := provider.NewFake(responses...)
	client := fakeClient
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		fmt.Fprintf(errOut, "resume: executor unavailable: %v\n", err)
		return exitUnavailable
	}
	registry, err := tools.NewRegistry(tools.Options{
		Workspace:            workspacePath,
		Recipes:              resumeRecipes,
		NextEvidenceSequence: plan.NextEvidenceSequence,
	})
	if err != nil {
		fmt.Fprintf(errOut, "resume: workspace unavailable: %v\n", err)
		return exitUnavailable
	}
	limits, err := limitsFromConfig(plan.Task.ConfigJSON)
	if err != nil {
		fmt.Fprintf(errOut, "resume: invalid persisted configuration: %v\n", err)
		return exitCorrupt
	}
	policyConfig := resumePolicy
	policyConfig.RecipeModes = resumeRecipePolicy.RecipeModes
	loop, err := agent.NewLoop(agent.Config{
		Runner:               executor,
		Registry:             registry,
		Limits:               limits,
		Model:                plan.Task.Model,
		Trace:                traceSink,
		State:                store,
		Policy:               policy.NewStatic(policyConfig, storeApprovals(store)),
		WritePolicy:          resumePolicy.Spec(),
		RecipePolicy:         resumeRecipePolicy.RecipeSpec(recipeIDs(resumeRecipes)),
		RecipeCatalogDigest:  resumeRecipes.Digest(),
		Verifier:             verifier.New(registry, resumeAcceptance),
		AcceptancePlanDigest: resumeAcceptanceDigest,
		Recovery:             plan.Seed,
	})
	if err != nil {
		fmt.Fprintf(errOut, "resume: loop unavailable: %v\n", err)
		return exitUnavailable
	}

	logger.InfoContext(ctx, "resume continued", "task_id", taskID, "provider", "scripted", "workspace", workspacePath)
	fmt.Fprintf(errOut, "resume: task %s continuing\n", taskID)
	result := loop.Run(ctx, agent.Task{ID: taskID, Prompt: ""})
	if err := printFinalRuntimeResult(ctx, out, errOut, store, taskID, result, "resume"); err != nil {
		return exitUnavailable
	}
	return result.Outcome.ExitCode()
}

// resolveResumeGovernorConfig reconstructs the account governor policy for a
// resumed task from the persisted protection state plus the optional
// min-start-interval override. The allowance policy is reconstructed from the
// persisted profile/kind so a resumed unlimited-text or unknown allowance
// does not silently become a published-quota policy (or vice versa). It fails
// closed when the persisted ceilings disagree with the reconstructed policy,
// so restored protection is never silently checked against different budgets.
func resolveResumeGovernorConfig(restored *governor.PersistedState, minStartInterval string, intervalSet bool) (governor.Config, error) {
	providerID := "scripted"
	model := "scripted"
	modelPool := "instant"
	policyID := "runstead-cli"
	if restored != nil {
		if restored.ProviderID != "" {
			providerID = restored.ProviderID
		}
		if restored.Model != "" {
			model = restored.Model
		}
		if restored.ModelPool != "" {
			modelPool = restored.ModelPool
		}
		if restored.AccountPolicyID != "" {
			policyID = restored.AccountPolicyID
		}
	}
	var accountConfig governor.Config
	if restored != nil {
		switch governor.AllowanceKindForProfile(restored.AllowanceProfile) {
		case governor.AllowanceKindUnlimitedText:
			accountConfig = governor.DefaultLunaUnlimitedTextConfig(policyID, providerID, modelPool, provider.SafeRouteSafety())
		case governor.AllowanceKindUnknown:
			// The upstream allowance is unknown, so the conservative local
			// layer is the durable policy the operator configured: rebuild it
			// from the persisted explicit ceilings instead of inventing
			// values. A projection without explicit local ceilings is
			// malformed and fails closed (#21 contract, #58 review).
			accountConfig = governor.DefaultUnknownConfig(policyID, providerID, modelPool, provider.SafeRouteSafety())
			if restored.Ceilings.Rolling3h == 0 || restored.Ceilings.Rolling1h == 0 || restored.Ceilings.Rolling10m == 0 ||
				restored.Ceilings.TaskBudget == 0 || restored.Ceilings.RetryBudget == 0 {
				return governor.Config{}, fmt.Errorf("incompatible persisted account policy: unknown allowance projection carries no explicit local ceilings")
			}
			accountConfig.Rolling3h = restored.Ceilings.Rolling3h
			accountConfig.Rolling1h = restored.Ceilings.Rolling1h
			accountConfig.Rolling10m = restored.Ceilings.Rolling10m
			accountConfig.TaskBudget = restored.Ceilings.TaskBudget
			accountConfig.RetryBudget = restored.Ceilings.RetryBudget
			if restored.Ceilings.ManualReserve != 0 {
				accountConfig.ManualReserve = restored.Ceilings.ManualReserve
			}
		default:
			accountConfig = governor.DefaultInstantConfig(policyID, providerID, modelPool, provider.SafeRouteSafety())
		}
	} else {
		accountConfig = governor.DefaultInstantConfig(policyID, providerID, modelPool, provider.SafeRouteSafety())
	}
	accountConfig.Model = model
	if intervalSet {
		interval, err := time.ParseDuration(strings.TrimSpace(minStartInterval))
		if err != nil || interval <= 0 {
			return governor.Config{}, fmt.Errorf("invalid min-start-interval %q", minStartInterval)
		}
		accountConfig.MinimumStartInterval = interval
	} else if value, ok := os.LookupEnv(config.EnvMinStartInterval); ok {
		interval, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || interval <= 0 {
			return governor.Config{}, fmt.Errorf("invalid %s %q", config.EnvMinStartInterval, value)
		}
		accountConfig.MinimumStartInterval = interval
	}
	if err := accountConfig.Validate(); err != nil {
		return governor.Config{}, err
	}
	if restored != nil {
		switch governor.AllowanceKindForProfile(restored.AllowanceProfile) {
		case governor.AllowanceKindPublishedQuota:
			if restored.Ceilings.Rolling3h != 0 && restored.Ceilings.Rolling3h != accountConfig.Rolling3h ||
				restored.Ceilings.Rolling1h != 0 && restored.Ceilings.Rolling1h != accountConfig.Rolling1h ||
				restored.Ceilings.Rolling10m != 0 && restored.Ceilings.Rolling10m != accountConfig.Rolling10m ||
				restored.Ceilings.TaskBudget != 0 && restored.Ceilings.TaskBudget != accountConfig.TaskBudget ||
				restored.Ceilings.RetryBudget != 0 && restored.Ceilings.RetryBudget != accountConfig.RetryBudget ||
				restored.Ceilings.ManualReserve != 0 && restored.Ceilings.ManualReserve != accountConfig.ManualReserve {
				return governor.Config{}, fmt.Errorf("incompatible persisted account policy: restored ceilings disagree with the reconstructed policy")
			}
		case governor.AllowanceKindUnlimitedText:
			// An unlimited-text allowance must not carry fabricated persisted
			// rolling ceilings. Legacy rows predating #58 stored zeros for
			// these profiles; any nonzero value means the restored policy is
			// not the policy that produced the projection, so resume fails
			// closed instead of silently checking against different budgets.
			if restored.Ceilings.Rolling3h != 0 || restored.Ceilings.Rolling1h != 0 || restored.Ceilings.Rolling10m != 0 {
				return governor.Config{}, fmt.Errorf("incompatible persisted account policy: restored rolling ceilings on an unlimited-text allowance")
			}
			if restored.Ceilings.TaskBudget != 0 && restored.Ceilings.TaskBudget != accountConfig.TaskBudget ||
				restored.Ceilings.RetryBudget != 0 && restored.Ceilings.RetryBudget != accountConfig.RetryBudget ||
				restored.Ceilings.ManualReserve != 0 && restored.Ceilings.ManualReserve != accountConfig.ManualReserve {
				return governor.Config{}, fmt.Errorf("incompatible persisted account policy: restored ceilings disagree with the reconstructed policy")
			}
		case governor.AllowanceKindUnknown:
			// The policy was reconstructed from the persisted explicit local
			// ceilings above; the durable projection is authoritative, so
			// there is nothing to re-check.
		}
	}
	return accountConfig, nil
}

// resolveResumeLogLevel resolves the log level from the flag or environment,
// defaulting to info like `run`.
func resolveResumeLogLevel(flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	if value, ok := os.LookupEnv(config.EnvLogLevel); ok {
		return value
	}
	return "info"
}

// resolveScriptedFlag resolves the scripted responses path from the flag or
// RUNSTEAD_SCRIPTED_RESPONSES, mirroring `run`.
func resolveScriptedFlag(scripted string) (string, bool) {
	if strings.TrimSpace(scripted) != "" {
		return scripted, true
	}
	if value, ok := os.LookupEnv(config.EnvScriptedResponses); ok {
		value = strings.TrimSpace(value)
		return value, value != ""
	}
	return "", false
}

// writePolicyFromConfig reconstructs the effective write policy persisted with
// the task configuration snapshot. A legacy task without a persisted
// write_policy falls back to the fail-closed default (approval_required for
// every write tool), never to a permissive gap.
func writePolicyFromConfig(configJSON string) (policy.Config, error) {
	if strings.TrimSpace(configJSON) == "" || strings.TrimSpace(configJSON) == "{}" {
		return policy.DefaultConfig(), nil
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(configJSON), &snapshot); err != nil {
		return policy.Config{}, fmt.Errorf("decode persisted configuration snapshot: %w", err)
	}
	raw, ok := snapshot["write_policy"]
	if !ok {
		return policy.DefaultConfig(), nil
	}
	spec, ok := raw.(string)
	if !ok || strings.TrimSpace(spec) == "" {
		return policy.Config{}, fmt.Errorf("invalid persisted write policy")
	}
	config, err := policy.ParseConfig(spec)
	if err != nil {
		return policy.Config{}, fmt.Errorf("invalid persisted write policy: %w", err)
	}
	return config, nil
}

// resolveResumeWritePolicy reconstructs the effective write policy of a
// resumed task. The policy persisted with the task configuration is
// authoritative: resume uses it by default, and a --write-policy override
// that diverges from the persisted policy is rejected fail-closed (there is
// no external authority that could justify silently widening the policy a
// task started under).
func resolveResumeWritePolicy(configJSON, flagValue string, flagSet bool) (policy.Config, error) {
	persisted, err := writePolicyFromConfig(configJSON)
	if err != nil {
		return policy.Config{}, err
	}
	if flagSet {
		requested, err := policy.ParseConfig(strings.TrimSpace(flagValue))
		if err != nil {
			return policy.Config{}, err
		}
		if !policy.Equal(requested, persisted) {
			return policy.Config{}, fmt.Errorf("--write-policy diverges from the task's persisted policy %q; resume always continues under the policy the task started with", persisted.Spec())
		}
	}
	return persisted, nil
}

// recipeCatalogDigestFromConfig reads the digest of the effective recipe
// catalog persisted with the task configuration snapshot. Empty when the task
// started without a recipe catalog.
func recipeCatalogDigestFromConfig(configJSON string) string {
	if strings.TrimSpace(configJSON) == "" || strings.TrimSpace(configJSON) == "{}" {
		return ""
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(configJSON), &snapshot); err != nil {
		return ""
	}
	raw, ok := snapshot["recipe_catalog_digest"]
	if !ok {
		return ""
	}
	value, _ := raw.(string)
	return strings.TrimSpace(value)
}

// resolveResumeRecipeCatalog rejects recipe catalog drift between run and
// resume fail-closed (issue #26 review). The catalog the task started with is
// part of the authoritative task configuration: resume requires the same
// effective catalog (its digest is compared, so any change to a recipe
// definition, an added recipe or a removed recipe is drift), and a task that
// started without a catalog cannot silently gain one at resume.
func resolveResumeRecipeCatalog(configJSON string, catalog *recipe.Catalog) error {
	persisted := recipeCatalogDigestFromConfig(configJSON)
	if persisted == "" {
		if catalog != nil {
			return errors.New("the task has no persisted recipe catalog; supplying one at resume is a policy change and is rejected")
		}
		return nil
	}
	if catalog == nil {
		return errors.New("the task started with a recipe catalog; resume requires the same catalog (--recipes)")
	}
	if catalog.Digest() != persisted {
		return errors.New("recipe catalog drift: the re-supplied catalog differs from the effective catalog the task started with; resume requires the exact same catalog")
	}
	return nil
}

// recipePolicyFromConfig reconstructs the effective recipe policy persisted
// with the task configuration snapshot. A legacy task without a persisted
// recipe_policy falls back to an empty recipe policy (every recipe defaults
// to approval_required), never to a permissive gap.
func recipePolicyFromConfig(configJSON string) (policy.Config, error) {
	if strings.TrimSpace(configJSON) == "" || strings.TrimSpace(configJSON) == "{}" {
		return policy.Config{Modes: map[string]policy.Mode{}, RecipeModes: map[string]policy.Mode{}}, nil
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(configJSON), &snapshot); err != nil {
		return policy.Config{}, fmt.Errorf("decode persisted configuration snapshot: %w", err)
	}
	raw, ok := snapshot["recipe_policy"]
	if !ok {
		return policy.Config{Modes: map[string]policy.Mode{}, RecipeModes: map[string]policy.Mode{}}, nil
	}
	spec, ok := raw.(string)
	if !ok || strings.TrimSpace(spec) == "" {
		return policy.Config{Modes: map[string]policy.Mode{}, RecipeModes: map[string]policy.Mode{}}, nil
	}
	config, err := policy.ParseRecipePolicy(spec)
	if err != nil {
		return policy.Config{}, fmt.Errorf("invalid persisted recipe policy: %w", err)
	}
	return config, nil
}

// resolveResumeRecipePolicy reconstructs the effective recipe policy of a
// resumed task. The policy persisted with the task configuration is
// authoritative: resume uses it by default, and a --recipe-policy override
// that diverges from the persisted policy is rejected fail-closed. The
// override must also reference recipes that exist in the re-supplied catalog.
func resolveResumeRecipePolicy(configJSON, flagValue string, flagSet bool, catalog *recipe.Catalog) (policy.Config, error) {
	persisted, err := recipePolicyFromConfig(configJSON)
	if err != nil {
		return policy.Config{}, err
	}
	if flagSet {
		requested, err := policy.ParseRecipePolicy(strings.TrimSpace(flagValue))
		if err != nil {
			return policy.Config{}, err
		}
		for id := range requested.RecipeModes {
			if catalog == nil {
				return policy.Config{}, fmt.Errorf("--recipe-policy configures %q but no recipes are configured", id)
			}
			if _, ok := catalog.Get(id); !ok {
				return policy.Config{}, fmt.Errorf("--recipe-policy configures unknown recipe %q", id)
			}
		}
		if !policy.RecipeEqual(requested, persisted, recipeIDs(catalog)) {
			return policy.Config{}, fmt.Errorf("--recipe-policy diverges from the task's persisted recipe policy %q; resume always continues under the policy the task started with", persisted.RecipeSpec(recipeIDs(catalog)))
		}
	}
	return persisted, nil
}

// resolveResumeAcceptancePlan reconstructs the effective acceptance plan of a
// resumed task (issue #11). The plan persisted with the task at run start is
// authoritative: resume loads it from state, and an --acceptance override that
// diverges from the persisted plan is rejected fail-closed. A task that
// started with a plan cannot resume without it, and an override cannot change
// it. A task that started WITHOUT a plan is refused completion by the verifier
// (blocked, issue #11 review), so the operator may explicitly attach a plan at
// resume: the returned attached flag reports that operator-owned act, and the
// caller persists the plan with the task before any recovery or execution side
// effect. The model can never attach or modify acceptance criteria.
func resolveResumeAcceptancePlan(ctx context.Context, store *state.Store, taskID, flagValue string, flagSet bool) (*verifier.Plan, string, bool, error) {
	persistedSpec, persistedDigest, persisted, err := store.AcceptancePlan(ctx, taskID)
	if err != nil {
		return nil, "", false, err
	}
	if !persisted {
		if !flagSet {
			return nil, "", false, nil
		}
		// No plan was ever persisted: the operator explicitly supplies one at
		// resume. This is the only way a task without acceptance criteria can
		// become completable; it is an operator-owned act, never a model one.
		plan, digest, err := resolveAcceptancePlan(flagValue, true)
		if err != nil {
			return nil, "", false, err
		}
		return plan, digest, true, nil
	}
	var plan *verifier.Plan
	if flagSet {
		plan, _, err = resolveAcceptancePlan(flagValue, true)
		if err != nil {
			return nil, "", false, err
		}
		if plan.Digest() != persistedDigest {
			return nil, "", false, errors.New("--acceptance diverges from the task's persisted acceptance plan; resume always continues under the plan the task started with")
		}
		return plan, persistedDigest, false, nil
	}
	plan, err = verifier.ParsePlan(persistedSpec)
	if err != nil {
		return nil, "", false, fmt.Errorf("invalid persisted acceptance plan: %w", err)
	}
	return plan, persistedDigest, false, nil
}

// limitsFromConfig reconstructs the loop limits from the persisted sanitized
// configuration snapshot (the same snapshot `run` writes). Unknown or missing
// fields fall back to the loop defaults.
func limitsFromConfig(configJSON string) (agent.Limits, error) {
	limits := agent.DefaultLimits()
	if strings.TrimSpace(configJSON) == "" || strings.TrimSpace(configJSON) == "{}" {
		return limits, nil
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(configJSON), &snapshot); err != nil {
		return agent.Limits{}, fmt.Errorf("decode persisted configuration snapshot: %w", err)
	}
	intField := func(name string) (int, bool) {
		value, ok := snapshot[name]
		if !ok {
			return 0, false
		}
		number, ok := value.(float64)
		if !ok || number <= 0 {
			return 0, false
		}
		return int(number), true
	}
	// max_corrections and max_repeated_actions treat zero as a valid explicit
	// value that disables the allowance (matching agent.Limits semantics), so
	// they must survive the persisted snapshot.
	nonNegativeField := func(name string) (int, bool) {
		value, ok := snapshot[name]
		if !ok {
			return 0, false
		}
		number, ok := value.(float64)
		if !ok || number < 0 {
			return 0, false
		}
		return int(number), true
	}
	if value, ok := intField("max_steps"); ok {
		limits.MaxSteps = value
	}
	if value, ok := nonNegativeField("max_corrections"); ok {
		limits.MaxCorrections = value
	}
	if value, ok := nonNegativeField("max_repeated_actions"); ok {
		limits.MaxRepeatedActions = value
	}
	// The #12 failure guard limits are part of the persisted loop budgets:
	// resume continues under the same consecutive-failure and verification-
	// retry allowances the task started with.
	if value, ok := intField("max_consecutive_failures"); ok {
		limits.MaxConsecutiveFailures = value
	}
	if value, ok := intField("max_verification_retries"); ok {
		limits.MaxVerificationRetries = value
	}
	if value, ok := intField("provider_budget"); ok {
		limits.ProviderBudget = value
	}
	if value, ok := intField("time_budget_ns"); ok {
		limits.TimeBudget = time.Duration(value)
	}
	return limits, nil
}

func printResumeHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead resume <task-id> [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Resumes an interrupted read-only task from durable local state (issue #9):")
	fmt.Fprintln(out, "loads the persisted task, classifies and reconciles interrupted attempts,")
	fmt.Fprintln(out, "reconstructs a bounded model context, restores the account-governor protection")
	fmt.Fprintln(out, "state and continues through the normal governed agent loop with a new provider")
	fmt.Fprintln(out, "conversation. Historical provider calls are never replayed and completed tool")
	fmt.Fprintln(out, "actions are never executed again merely because the task resumes. If automatic")
	fmt.Fprintln(out, "continuation cannot be decided safely, the task stops with a typed")
	fmt.Fprintln(out, "human_review_required outcome and structured persisted evidence.")
	fmt.Fprintln(out, "The task workspace is part of its durable identity and is never overridden.")
	fmt.Fprintln(out, "When resume reaches a verified completed outcome, stdout also includes the")
	fmt.Fprintln(out, "bounded Verified runtime result projection; runstead inspect remains the")
	fmt.Fprintln(out, "historical detailed view and model final text remains unverified.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Flags:")
	fmt.Fprintln(out, "  --scripted FILE           scripted responses for a deterministic offline continuation (RUNSTEAD_SCRIPTED_RESPONSES)")
	fmt.Fprintln(out, "  --state-dir PATH          durable state directory (RUNSTEAD_STATE_DIR)")
	fmt.Fprintln(out, "  --log-level LEVEL         debug, info, warn or error (RUNSTEAD_LOG_LEVEL, default info)")
	fmt.Fprintln(out, "  --write-policy SPEC       write tool modes, e.g. write_file=allow (RUNSTEAD_WRITE_POLICY, default approval_required)")
	fmt.Fprintln(out, "  --recipes FILE            operator-controlled recipe catalog (RUNSTEAD_RECIPES); re-supplied at resume")
	fmt.Fprintln(out, "  --recipe-policy SPEC      recipe modes, e.g. test=allow (RUNSTEAD_RECIPE_POLICY; must match the persisted policy)")
	fmt.Fprintln(out, "  --acceptance FILE         operator acceptance plan (RUNSTEAD_ACCEPTANCE_PLAN; must match the persisted plan; loaded from state when omitted; may ATTACH a plan to a task that started without one, since completion fails closed without acceptance criteria)")
	fmt.Fprintln(out, "  --min-start-interval DURATION  account governor pacing override (RUNSTEAD_MIN_START_INTERVAL)")
	fmt.Fprintln(out, "  allowance profile is reconstructed from the persisted governor state (RUNSTEAD_ALLOWANCE_PROFILE applies to `run` only)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Exit codes: 0 resumed and finished, 1 task not found, 2 usage, 3 state database unavailable,")
	fmt.Fprintln(out, "4 task not resumable (already terminal), 5 human review required, 6 corrupted/incompatible state,")
	fmt.Fprintln(out, "7 continuation blocked by restored account protection.")
}
