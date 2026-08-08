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
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/trace"
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
	workspace := ""
	logLevel := ""
	minStartInterval := ""
	intervalSet := false
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
		case arg == "--workspace":
			if next, ok := value("--workspace"); ok {
				workspace = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--workspace="):
			workspace = strings.TrimPrefix(arg, "--workspace=")
		case arg == "--log-level":
			if next, ok := value("--log-level"); ok {
				logLevel = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--log-level="):
			logLevel = strings.TrimPrefix(arg, "--log-level=")
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

	// The recovery pipeline: load persisted history, classify and reconcile
	// interrupted attempts, decide whether automatic continuation is safe, and
	// reconstruct the bounded model context. All transitions are journaled.
	// Task validation errors (not found, already terminal) take precedence over
	// provider configuration, so the provider is resolved only when the task
	// can actually continue.
	plan, err := recovery.Resume(ctx, store, recovery.Options{
		TaskID: taskID,
		Trace:  traceSink,
		Blocked: func() (bool, string) {
			return recovery.GovernorBlocks(accountGovernor, taskID)
		},
	})
	if errors.Is(err, state.ErrTaskNotFound) {
		fmt.Fprintf(errOut, "resume: task %q not found in %s\n", taskID, dir)
		return exitNotFound
	}
	if errors.Is(err, state.ErrNotResumable) {
		// A task already stopped for human review reports the unresolved
		// requirement with the human-review exit code rather than the generic
		// not-resumable code.
		if status, statusErr := store.TaskStatus(ctx, taskID); statusErr == nil && status == "human_review_required" {
			fmt.Fprintf(errOut, "resume: task %q has an unresolved human review requirement\n", taskID)
			return exitHumanReview
		}
		fmt.Fprintf(errOut, "resume: task %q is not resumable: %v\n", taskID, err)
		return exitNotResumable
	}
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

	// Wire the resumed task with the same control boundaries as a normal run:
	// the restored account governor, the read-only registry, the protocol
	// parser and the persistence boundary.
	client := provider.NewFake(responses...)
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		fmt.Fprintf(errOut, "resume: executor unavailable: %v\n", err)
		return exitUnavailable
	}
	workspacePath := plan.Task.Workspace
	if workspace != "" {
		workspacePath = workspace
	}
	registry, err := tools.NewRegistry(tools.Options{
		Workspace:            workspacePath,
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
	loop, err := agent.NewLoop(agent.Config{
		Runner:   executor,
		Registry: registry,
		Limits:   limits,
		Model:    plan.Task.Model,
		Trace:    traceSink,
		State:    store,
		Recovery: plan.Seed,
	})
	if err != nil {
		fmt.Fprintf(errOut, "resume: loop unavailable: %v\n", err)
		return exitUnavailable
	}

	logger.InfoContext(ctx, "resume continued", "task_id", taskID, "provider", "scripted", "workspace", workspacePath)
	fmt.Fprintf(errOut, "resume: task %s continuing\n", taskID)
	result := loop.Run(ctx, agent.Task{ID: taskID, Prompt: ""})
	printResult(out, errOut, result)
	return result.Outcome.ExitCode()
}

// resolveResumeGovernorConfig reconstructs the account governor policy for a
// resumed task from the persisted protection state plus the optional
// min-start-interval override. It fails closed when the persisted ceilings
// disagree with the reconstructed policy, so restored protection is never
// silently checked against different budgets.
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
	accountConfig := governor.DefaultInstantConfig(policyID, providerID, modelPool, provider.SafeRouteSafety())
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
		switch {
		case restored.Ceilings.Rolling3h != 0 && restored.Ceilings.Rolling3h != accountConfig.Rolling3h,
			restored.Ceilings.Rolling1h != 0 && restored.Ceilings.Rolling1h != accountConfig.Rolling1h,
			restored.Ceilings.Rolling10m != 0 && restored.Ceilings.Rolling10m != accountConfig.Rolling10m,
			restored.Ceilings.TaskBudget != 0 && restored.Ceilings.TaskBudget != accountConfig.TaskBudget,
			restored.Ceilings.RetryBudget != 0 && restored.Ceilings.RetryBudget != accountConfig.RetryBudget:
			return governor.Config{}, fmt.Errorf("incompatible persisted account policy: restored ceilings disagree with the reconstructed policy")
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
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Flags:")
	fmt.Fprintln(out, "  --scripted FILE           scripted responses for a deterministic offline continuation (RUNSTEAD_SCRIPTED_RESPONSES)")
	fmt.Fprintln(out, "  --workspace PATH          workspace override (default: the persisted task workspace)")
	fmt.Fprintln(out, "  --state-dir PATH          durable state directory (RUNSTEAD_STATE_DIR)")
	fmt.Fprintln(out, "  --log-level LEVEL         debug, info, warn or error (RUNSTEAD_LOG_LEVEL, default info)")
	fmt.Fprintln(out, "  --min-start-interval DURATION  account governor pacing override (RUNSTEAD_MIN_START_INTERVAL)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Exit codes: 0 resumed and finished, 1 task not found, 2 usage, 3 state database unavailable,")
	fmt.Fprintln(out, "4 task not resumable (already terminal), 5 human review required, 6 corrupted/incompatible state,")
	fmt.Fprintln(out, "7 continuation blocked by restored account protection.")
}
