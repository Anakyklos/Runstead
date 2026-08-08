package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/trace"
)

const (
	exitSuccess     = 0
	exitNotFound    = 1
	exitUsage       = 2
	exitUnavailable = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || isHelp(args[0]) {
		printRootHelp(out)
		return exitSuccess
	}

	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], out, errOut)
	case "inspect":
		return inspectCommand(ctx, args[1:], out, errOut)
	case "resume":
		return resumeCommand(ctx, args[1:], out, errOut)
	case "decide":
		return decideCommand(ctx, args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "runstead: unknown command %q\n\n", args[0])
		printRootHelp(errOut)
		return exitUsage
	}
}

func runCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if hasHelp(args) {
		printRunHelp(out)
		return exitSuccess
	}

	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	workspace := ""
	logLevel := ""
	task := ""
	scripted := ""
	maxSteps := 0
	maxCorrections := 0
	maxRepeatedActions := 0
	timeBudget := ""
	providerBudget := 0
	minStartInterval := ""
	omniBaseURL := ""
	omniManagementBaseURL := ""
	omniAPIKey := ""
	omniModel := ""
	omniChatEndpoint := ""
	omniTimeout := ""
	omniSafeRoute := false
	stateDir := ""
	writePolicy := ""
	flags.StringVar(&workspace, "workspace", "", "workspace path (default: RUNSTEAD_WORKSPACE or .)")
	flags.StringVar(&logLevel, "log-level", "", "log level: debug, info, warn or error")
	flags.StringVar(&task, "task", "", "task prompt (RUNSTEAD_TASK)")
	flags.StringVar(&scripted, "scripted", "", "JSONL file of scripted model responses for a deterministic offline run (RUNSTEAD_SCRIPTED_RESPONSES)")
	flags.StringVar(&stateDir, "state-dir", "", "durable state directory (RUNSTEAD_STATE_DIR; default: $XDG_DATA_HOME/runstead or ~/.local/share/runstead)")
	flags.StringVar(&writePolicy, "write-policy", "", "write tool policy modes, e.g. write_file=allow,apply_patch=approval_required (RUNSTEAD_WRITE_POLICY; default: approval_required for every write tool)")
	flags.IntVar(&maxSteps, "max-steps", 0, "maximum model turns (RUNSTEAD_MAX_STEPS, default 24)")
	flags.IntVar(&maxCorrections, "max-corrections", 0, "protocol correction attempts (RUNSTEAD_MAX_CORRECTIONS, default 2)")
	flags.IntVar(&maxRepeatedActions, "max-repeated-actions", 0, "repeated-action corrections before stopping (RUNSTEAD_MAX_REPEATED_ACTIONS, default 2)")
	flags.StringVar(&timeBudget, "time-budget", "", "elapsed task time budget (RUNSTEAD_TIME_BUDGET, default 10m)")
	flags.IntVar(&providerBudget, "provider-budget", 0, "governed provider attempts per task (RUNSTEAD_PROVIDER_BUDGET, default 80)")
	flags.StringVar(&minStartInterval, "min-start-interval", "", "account governor start-to-start pacing (RUNSTEAD_MIN_START_INTERVAL, default 5s)")
	flags.StringVar(&omniBaseURL, "omniroute-base-url", "", "OmniRoute base URL (OMNIROUTE_BASE_URL)")
	flags.StringVar(&omniManagementBaseURL, "omniroute-management-base-url", "", "OmniRoute management URL (OMNIROUTE_MANAGEMENT_BASE_URL)")
	flags.StringVar(&omniAPIKey, "omniroute-api-key", "", "OmniRoute API key (OMNIROUTE_API_KEY)")
	flags.StringVar(&omniModel, "omniroute-model", "", "OmniRoute model (OMNIROUTE_MODEL)")
	flags.StringVar(&omniChatEndpoint, "omniroute-chat-endpoint", "", "OmniRoute chat endpoint (OMNIROUTE_CHAT_ENDPOINT)")
	flags.StringVar(&omniTimeout, "omniroute-timeout", "", "OmniRoute timeout (OMNIROUTE_TIMEOUT)")
	flags.BoolVar(&omniSafeRoute, "omniroute-safe-route", false, "declare the static route safe; preflight evidence is still required")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(errOut, "run: invalid flags: %v\n", err)
		printRunHelp(errOut)
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(errOut, "run: unexpected argument %q\n", flags.Arg(0))
		printRunHelp(errOut)
		return exitUsage
	}

	omniOverrides := config.OmniRouteOverrides{
		BaseURL:              omniBaseURL,
		BaseURLSet:           flagWasSet(flags, "omniroute-base-url"),
		ManagementBaseURL:    omniManagementBaseURL,
		ManagementBaseURLSet: flagWasSet(flags, "omniroute-management-base-url"),
		APIKey:               omniAPIKey,
		APIKeySet:            flagWasSet(flags, "omniroute-api-key"),
		Model:                omniModel,
		ModelSet:             flagWasSet(flags, "omniroute-model"),
		ChatEndpoint:         omniChatEndpoint,
		ChatEndpointSet:      flagWasSet(flags, "omniroute-chat-endpoint"),
	}
	if flagWasSet(flags, "omniroute-timeout") {
		timeout, parseErr := time.ParseDuration(omniTimeout)
		if parseErr != nil {
			fmt.Fprintln(errOut, "run: invalid OmniRoute timeout")
			return exitUsage
		}
		omniOverrides.Timeout = timeout
		omniOverrides.TimeoutSet = true
	}
	if flagWasSet(flags, "omniroute-safe-route") {
		if omniSafeRoute {
			omniOverrides.RouteSafety = provider.SafeRouteSafety()
		}
		omniOverrides.RouteSafetySet = true
	}
	cfg, err := config.Resolve(config.Overrides{
		Workspace:    workspace,
		WorkspaceSet: flagWasSet(flags, "workspace"),
		LogLevel:     logLevel,
		LogLevelSet:  flagWasSet(flags, "log-level"),
		OmniRoute:    omniOverrides,
	}, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(errOut, "run: invalid configuration: %v\n", err)
		return exitUsage
	}
	level, err := trace.ParseLevel(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(errOut, "run: invalid configuration: %v\n", err)
		return exitUsage
	}
	logger := trace.NewLogger(errOut, level)

	if err := ctx.Err(); err != nil {
		logger.WarnContext(ctx, "run canceled before start", "error", err.Error())
		fmt.Fprintln(errOut, "run: canceled")
		return agent.OutcomeCanceled.ExitCode()
	}

	limits, err := resolveLimits(flags, maxSteps, maxCorrections, maxRepeatedActions, timeBudget, providerBudget)
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	taskPrompt, ok := resolveTask(flags, task)
	if !ok {
		fmt.Fprintln(errOut, "run: a task prompt is required (--task or RUNSTEAD_TASK)")
		printRunHelp(errOut)
		return exitUsage
	}
	scriptedPath, scriptedSet := resolveScripted(flags, scripted)

	if scriptedSet {
		if cfg.OmniRoute != nil {
			fmt.Fprintln(errOut, "run: scripted offline mode cannot be combined with OmniRoute configuration")
			return exitUsage
		}
	} else if cfg.OmniRoute != nil {
		fmt.Fprintln(errOut, "run: live OmniRoute execution remains blocked until a compatible attempt-receipt producer exists (#29 -> #30 -> #4). Use --scripted FILE for a deterministic offline run.")
		return exitUnavailable
	} else {
		fmt.Fprintln(errOut, "run: no provider configured. Use --scripted FILE for a deterministic offline run, or configure OmniRoute (live path remains blocked).")
		return exitUnavailable
	}

	accountConfig, err := resolveGovernorConfig(scriptedSet, cfg, minStartInterval, flagWasSet(flags, "min-start-interval"))
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}

	// Open the durable store before any execution: persistence is part of the
	// runtime, not an afterthought. A store that cannot be created or opened
	// is a hard failure.
	stateDirPath, err := resolveStateDir(stateDir, flagWasSet(flags, "state-dir"))
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	store, err := openStore(stateDirPath)
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	var restored *governor.PersistedState
	if snapshot, ok, loadErr := store.GovernorState(ctx); loadErr != nil {
		fmt.Fprintf(errOut, "run: cannot restore account protection state: %v\n", loadErr)
		return exitUnavailable
	} else if ok {
		restored = &snapshot
	}

	accountGovernor, err := governor.New(accountConfig, governor.Options{
		Events:      trace.NewPolicySink(logger),
		Persistence: store,
		Restore:     restored,
	})
	if err != nil {
		fmt.Fprintf(errOut, "run: invalid account policy: %v\n", err)
		return exitUsage
	}

	var client provider.Client
	var model string
	if scriptedSet {
		responses, loadErr := loadScriptedResponses(scriptedPath)
		if loadErr != nil {
			fmt.Fprintf(errOut, "run: %v\n", loadErr)
			return exitUsage
		}
		client = provider.NewFake(responses...)
		model = "scripted"
	} else {
		// Unreachable: the OmniRoute path is refused above while live receipts
		// are unavailable.
		fmt.Fprintln(errOut, "run: live provider path is not activatable in this milestone")
		return exitUnavailable
	}

	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		fmt.Fprintf(errOut, "run: executor unavailable: %v\n", err)
		return exitUnavailable
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: cfg.Workspace})
	if err != nil {
		fmt.Fprintf(errOut, "run: workspace unavailable: %v\n", err)
		return exitUnavailable
	}
	writePolicyConfig, err := resolveWritePolicy(writePolicy, flagWasSet(flags, "write-policy"))
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner:      executor,
		Registry:    registry,
		Limits:      limits,
		Model:       model,
		Trace:       cliTraceSink(errOut),
		State:       store,
		Policy:      policy.NewStatic(writePolicyConfig, storeApprovals(store)),
		WritePolicy: writePolicyConfig.Spec(),
	})
	if err != nil {
		fmt.Fprintf(errOut, "run: loop unavailable: %v\n", err)
		return exitUnavailable
	}

	taskID := "cli-" + fmt.Sprint(time.Now().UnixNano())
	logger.InfoContext(ctx, "run started", "task_id", taskID, "provider", "scripted", "workspace", cfg.Workspace)
	fmt.Fprintf(errOut, "task: %s\n", taskID)
	result := loop.Run(ctx, agent.Task{ID: taskID, Prompt: taskPrompt})
	printResult(out, errOut, taskID, result)
	return result.Outcome.ExitCode()
}

func resolveTask(flags *flag.FlagSet, task string) (string, bool) {
	if flagWasSet(flags, "task") {
		return strings.TrimSpace(task), strings.TrimSpace(task) != ""
	}
	if value, ok := os.LookupEnv(config.EnvTask); ok {
		value = strings.TrimSpace(value)
		return value, value != ""
	}
	return "", false
}

func resolveScripted(flags *flag.FlagSet, scripted string) (string, bool) {
	if flagWasSet(flags, "scripted") {
		return strings.TrimSpace(scripted), strings.TrimSpace(scripted) != ""
	}
	if value, ok := os.LookupEnv(config.EnvScriptedResponses); ok {
		value = strings.TrimSpace(value)
		return value, value != ""
	}
	return "", false
}

// storeApprovals adapts the store's approval lookup to the policy seam.
// Approvals are keyed by (task_id, fingerprint): the fingerprint is the
// repeat/loop identity of the write proposal.
func storeApprovals(store *state.Store) policy.Approvals {
	return policy.ApprovalsFunc(func(ctx context.Context, taskID, fingerprint string) (policy.Approval, bool, error) {
		approval, ok, err := store.Approval(ctx, taskID, fingerprint)
		if err != nil {
			return policy.Approval{}, false, err
		}
		return policy.Approval{Decision: approval.Decision, Reason: approval.Reason}, ok, nil
	})
}

// resolveWritePolicy resolves the write-tool policy configuration from the
// --write-policy flag, then RUNSTEAD_WRITE_POLICY, then the fail-closed
// default (approval_required for every write tool).
func resolveWritePolicy(flagValue string, flagSet bool) (policy.Config, error) {
	value := ""
	if flagSet {
		value = strings.TrimSpace(flagValue)
		if value == "" {
			return policy.Config{}, errors.New("write policy must not be empty")
		}
	} else if envValue, ok := os.LookupEnv(config.EnvWritePolicy); ok {
		value = strings.TrimSpace(envValue)
	}
	if value == "" {
		return policy.DefaultConfig(), nil
	}
	return policy.ParseConfig(value)
}

func resolveLimits(flags *flag.FlagSet, maxSteps, maxCorrections, maxRepeatedActions int, timeBudget string, providerBudget int) (agent.Limits, error) {
	limits := agent.DefaultLimits()
	parsePositiveInt := func(name, value string) (int, error) {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("invalid %s %q: must be a positive integer", name, value)
		}
		return parsed, nil
	}
	parseNonNegativeInt := func(name, value string) (int, error) {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed < 0 {
			return 0, fmt.Errorf("invalid %s %q: must be a non-negative integer", name, value)
		}
		return parsed, nil
	}
	parseDuration := func(name, value string) (time.Duration, error) {
		parsed, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("invalid %s %q: must be a positive duration", name, value)
		}
		return parsed, nil
	}

	if flagWasSet(flags, "max-steps") {
		if maxSteps <= 0 {
			return agent.Limits{}, fmt.Errorf("max-steps must be positive")
		}
		limits.MaxSteps = maxSteps
	} else if value, ok := os.LookupEnv(config.EnvMaxSteps); ok {
		parsed, err := parsePositiveInt(config.EnvMaxSteps, value)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.MaxSteps = parsed
	}
	if flagWasSet(flags, "max-corrections") {
		if maxCorrections < 0 {
			return agent.Limits{}, fmt.Errorf("max-corrections must not be negative")
		}
		limits.MaxCorrections = maxCorrections
	} else if value, ok := os.LookupEnv(config.EnvMaxCorrections); ok {
		parsed, err := parseNonNegativeInt(config.EnvMaxCorrections, value)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.MaxCorrections = parsed
	}
	if flagWasSet(flags, "max-repeated-actions") {
		if maxRepeatedActions < 0 {
			return agent.Limits{}, fmt.Errorf("max-repeated-actions must not be negative")
		}
		limits.MaxRepeatedActions = maxRepeatedActions
	} else if value, ok := os.LookupEnv(config.EnvMaxRepeatedActions); ok {
		parsed, err := parseNonNegativeInt(config.EnvMaxRepeatedActions, value)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.MaxRepeatedActions = parsed
	}
	if flagWasSet(flags, "time-budget") {
		parsed, err := parseDuration("time-budget", timeBudget)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.TimeBudget = parsed
	} else if value, ok := os.LookupEnv(config.EnvTimeBudget); ok {
		parsed, err := parseDuration(config.EnvTimeBudget, value)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.TimeBudget = parsed
	}
	if flagWasSet(flags, "provider-budget") {
		if providerBudget <= 0 {
			return agent.Limits{}, fmt.Errorf("provider-budget must be positive")
		}
		limits.ProviderBudget = providerBudget
	} else if value, ok := os.LookupEnv(config.EnvProviderBudget); ok {
		parsed, err := parsePositiveInt(config.EnvProviderBudget, value)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.ProviderBudget = parsed
	}
	return limits, nil
}

func resolveGovernorConfig(scripted bool, cfg config.Config, minStartInterval string, intervalSet bool) (governor.Config, error) {
	providerID := "scripted"
	model := "scripted"
	safety := provider.SafeRouteSafety()
	if !scripted {
		if cfg.OmniRoute == nil {
			return governor.Config{}, fmt.Errorf("no provider configuration for the account governor")
		}
		providerID = "omniroute"
		model = cfg.OmniRoute.Model
		safety = cfg.OmniRoute.RouteSafety
	}
	accountConfig := governor.DefaultInstantConfig("runstead-cli", providerID, "instant", safety)
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
	return accountConfig, nil
}

func printResult(out, errOut io.Writer, taskID string, result agent.Result) {
	fmt.Fprintf(out, "outcome: %s\n", result.Outcome)
	fmt.Fprintf(out, "reason: %s\n", result.StopReason)
	if result.Outcome == agent.OutcomeCompleted {
		if result.Summary != "" {
			fmt.Fprintf(out, "summary: %s\n", result.Summary)
		}
		for _, id := range result.Evidence {
			fmt.Fprintf(out, "evidence: %s\n", id)
		}
	}
	if result.Outcome == agent.OutcomeApprovalRequired && result.PendingActionID != "" {
		fmt.Fprintf(out, "pending approval: action %s\n", result.PendingActionID)
		fmt.Fprintf(out, "runstead decide %s %s approved|rejected\n", taskID, result.PendingActionID)
	}
	fmt.Fprintf(errOut, "run: turns=%d attempts=%d observations=%d corrections=%d repeated=%d mixed_prose=%d\n",
		result.Turns, result.Attempts, result.Observations, result.Corrections, result.Repeated, result.MixedProse)
	if result.Classification != "" {
		fmt.Fprintf(errOut, "run: classification=%s\n", result.Classification)
	}
}

func cliTraceSink(errOut io.Writer) agent.TraceSink {
	return func(line agent.TraceLine) {
		var builder strings.Builder
		fmt.Fprintf(&builder, "runstead: %s seq=%d status=%s", line.Kind, line.Sequence, line.Status)
		if line.Tool != "" {
			fmt.Fprintf(&builder, " tool=%s", line.Tool)
		}
		if line.EvidenceID != "" {
			fmt.Fprintf(&builder, " evidence=%s", line.EvidenceID)
		}
		if line.Code != "" {
			fmt.Fprintf(&builder, " code=%s retries=%d", line.Code, line.RetriesRemaining)
		}
		if line.Classification != "" {
			fmt.Fprintf(&builder, " classification=%s", line.Classification)
		}
		if line.Duration > 0 {
			fmt.Fprintf(&builder, " duration=%s", line.Duration)
		}
		if line.StopReason != "" {
			fmt.Fprintf(&builder, " reason=%q", line.StopReason)
		}
		fmt.Fprintln(errOut, builder.String())
	}
}

// inspectCommand renders the durable state of one task after the original
// `runstead run` process has exited. The human-readable output is produced by
// the store renderer: stable sections, no raw SQLite rows, no secrets.
func inspectCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if hasHelp(args) {
		printInspectHelp(out)
		return exitSuccess
	}

	// Parse manually so --state-dir may appear before or after the task id
	// (the flag package stops at the first positional argument).
	taskID := ""
	stateDir := ""
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--state-dir":
			if index+1 >= len(args) {
				fmt.Fprintln(errOut, "inspect: --state-dir requires a path")
				return exitUsage
			}
			index++
			stateDir = args[index]
		case strings.HasPrefix(arg, "--state-dir="):
			stateDir = strings.TrimPrefix(arg, "--state-dir=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(errOut, "inspect: unknown flag %q\n", arg)
			printInspectHelp(errOut)
			return exitUsage
		default:
			if taskID != "" {
				fmt.Fprintln(errOut, "inspect: exactly one task id is required")
				printInspectHelp(errOut)
				return exitUsage
			}
			taskID = arg
		}
	}
	if taskID == "" {
		fmt.Fprintln(errOut, "inspect: exactly one task id is required")
		printInspectHelp(errOut)
		return exitUsage
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(errOut, "inspect: canceled\n")
		return agent.OutcomeCanceled.ExitCode()
	}

	dir, err := resolveStateDir(stateDir, stateDir != "")
	if err != nil {
		fmt.Fprintf(errOut, "inspect: %v\n", err)
		return exitUsage
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "inspect: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	if err := store.RenderInspect(ctx, out, taskID); err != nil {
		if errors.Is(err, state.ErrTaskNotFound) {
			fmt.Fprintf(errOut, "inspect: task %q not found in %s\n", taskID, dir)
			return exitNotFound
		}
		fmt.Fprintf(errOut, "inspect: %v\n", err)
		return exitUnavailable
	}
	return exitSuccess
}

// decideCommand implements `runstead decide <task-id> <action-id> approved|rejected`.
// It is the operator control plane for write approvals (issue #10): only this
// command (or the equivalent state API) can create an approval record. Model
// prose, repository content and tool output can never approve a write.
func decideCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if hasHelp(args) {
		printDecideHelp(out)
		return exitSuccess
	}

	taskID := ""
	actionID := ""
	decision := ""
	stateDir := ""
	reason := ""
	// Parse manually so flags may appear before or after the positionals (the
	// flag package stops at the first positional argument).
	for index := 0; index < len(args); index++ {
		arg := args[index]
		value := func(name string) (string, bool) {
			if index+1 >= len(args) {
				fmt.Fprintf(errOut, "decide: %s requires a value\n", name)
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
		case arg == "--reason":
			if next, ok := value("--reason"); ok {
				reason = next
			} else {
				return exitUsage
			}
		case strings.HasPrefix(arg, "--reason="):
			reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(errOut, "decide: unknown flag %q\n", arg)
			printDecideHelp(errOut)
			return exitUsage
		default:
			switch {
			case taskID == "":
				taskID = arg
			case actionID == "":
				actionID = arg
			case decision == "":
				decision = strings.ToLower(arg)
			default:
				fmt.Fprintln(errOut, "decide: too many positional arguments")
				printDecideHelp(errOut)
				return exitUsage
			}
		}
	}
	if taskID == "" || actionID == "" || decision == "" {
		fmt.Fprintln(errOut, "decide: exactly one task id, action id and decision (approved|rejected) are required")
		printDecideHelp(errOut)
		return exitUsage
	}
	if decision != "approved" && decision != "rejected" {
		fmt.Fprintf(errOut, "decide: decision %q must be approved or rejected\n", decision)
		return exitUsage
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(errOut, "decide: canceled\n")
		return agent.OutcomeCanceled.ExitCode()
	}

	dir, err := resolveStateDir(stateDir, stateDir != "")
	if err != nil {
		fmt.Fprintf(errOut, "decide: %v\n", err)
		return exitUsage
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "decide: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	if _, err := store.TaskStatus(ctx, taskID); err != nil {
		if errors.Is(err, state.ErrTaskNotFound) {
			fmt.Fprintf(errOut, "decide: task %q not found in %s\n", taskID, dir)
			return exitNotFound
		}
		fmt.Fprintf(errOut, "decide: %v\n", err)
		return exitUnavailable
	}
	// The operator may only decide an action of this task that is actually
	// pending approval: a write action with a persisted approval_required
	// policy decision and no operator decision yet. Approvals for read
	// actions, unknown actions, or actions incompatible with the current
	// approval flow are meaningless and are rejected.
	pending, err := store.PendingApprovals(ctx, taskID)
	if err != nil {
		fmt.Fprintf(errOut, "decide: %v\n", err)
		return exitUnavailable
	}
	found := false
	for _, item := range pending {
		if item.ActionID == actionID {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(errOut, "decide: action %q of task %q is not pending approval; only write actions awaiting an operator decision can be decided\n", actionID, taskID)
		return exitUsage
	}
	approvalID, err := store.RecordApproval(ctx, state.Approval{
		TaskID: taskID, ActionID: actionID, Decision: decision, Reason: reason, Actor: "operator",
	})
	if err != nil {
		fmt.Fprintf(errOut, "decide: %v\n", err)
		return exitUnavailable
	}
	fmt.Fprintf(out, "approval: %s task=%s action=%s decision=%s\n", approvalID, taskID, actionID, decision)
	return exitSuccess
}

// resolveStateDir resolves the durable state directory from flag, then
// RUNSTEAD_STATE_DIR, then the XDG data home, then the home directory
// fallback. The directory itself is created by openStore.
func resolveStateDir(flagValue string, flagSet bool) (string, error) {
	if flagSet {
		value := strings.TrimSpace(flagValue)
		if value == "" {
			return "", errors.New("state directory must not be empty")
		}
		return value, nil
	}
	if value, ok := os.LookupEnv(config.EnvStateDir); ok {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", fmt.Errorf("%s must not be empty", config.EnvStateDir)
		}
		return value, nil
	}
	if value, ok := os.LookupEnv("XDG_DATA_HOME"); ok {
		value = strings.TrimSpace(value)
		if value != "" {
			return filepath.Join(value, "runstead"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the default state directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "runstead"), nil
}

// openStore opens (or creates) the state database inside the state
// directory. The directory and file permissions follow the documented
// persistence policy (0700 directory, 0600 database file).
func openStore(dir string) (*state.Store, error) {
	store, err := state.Open(state.Options{
		Path: filepath.Join(dir, state.DefaultDBFile),
	})
	if err != nil {
		return nil, fmt.Errorf("state database unavailable: %w", err)
	}
	return store, nil
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	wasSet := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func isHelp(value string) bool {
	return value == "--help" || value == "-h"
}

func hasHelp(args []string) bool {
	for _, arg := range args {
		if isHelp(arg) {
			return true
		}
	}
	return false
}

func printRootHelp(out io.Writer) {
	fmt.Fprintln(out, "Runstead is a local, stateful and verifiable agent runtime.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Usage: runstead <command> [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  run       run a bounded agent task with durable state and policy-bound writes")
	fmt.Fprintln(out, "  inspect   inspect durable task state by task id")
	fmt.Fprintln(out, "  resume    resume an interrupted task from durable state")
	fmt.Fprintln(out, "  decide    approve or reject a pending write action (operator control plane)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Configuration precedence: flags > environment > defaults")
	fmt.Fprintf(out, "  %s, %s, %s, %s\n", config.EnvWorkspace, config.EnvLogLevel, config.EnvTask, config.EnvStateDir)
	fmt.Fprintf(out, "  OmniRoute: %s, %s, %s, %s\n", config.EnvOmniRouteBaseURL, config.EnvOmniRouteAPIKey, config.EnvOmniRouteModel, config.EnvOmniRouteChatEndpoint)
	fmt.Fprintln(out, "Use 'runstead <command> --help' for command-specific help.")
}

func printRunHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead run [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runs one bounded agent task: every model turn is admitted by the")
	fmt.Fprintln(out, "account governor, actions are validated and executed by the tool")
	fmt.Fprintln(out, "registry (read-only tools plus policy-gated write_file/apply_patch),")
	fmt.Fprintln(out, "and a final answer is accepted only when grounded in real evidence")
	fmt.Fprintln(out, "IDs produced in this run. Writes are stale-state protected, stay inside")
	fmt.Fprintln(out, "the workspace, and never execute without control-plane approval when the")
	fmt.Fprintln(out, "policy requires it. When a write needs approval the run PAUSES with the")
	fmt.Fprintln(out, "typed approval_required outcome, reports the task and pending action for")
	fmt.Fprintln(out, "'runstead decide <task-id> <action-id> approved|rejected', and stays")
	fmt.Fprintln(out, "durably resumable; no correction budget is consumed. The task never")
	fmt.Fprintln(out, "shells out; durable task, action, attempt, evidence, journal, write-policy")
	fmt.Fprintln(out, "and account-protection state is persisted to SQLite (issues #8/#10) and")
	fmt.Fprintln(out, "can be inspected with 'runstead inspect <task-id>'.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "The deterministic offline mode replays scripted model responses (JSONL with")
	fmt.Fprintln(out, "one {\"text\":\"...\"} object per line) through the real governor and tools.")
	fmt.Fprintln(out, "Live OmniRoute execution remains blocked until a compatible attempt-receipt")
	fmt.Fprintln(out, "producer exists (#29 -> #30 -> #4).")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Flags:")
	fmt.Fprintln(out, "  --task PROMPT             task prompt (RUNSTEAD_TASK)")
	fmt.Fprintln(out, "  --scripted FILE           scripted responses for a deterministic offline run (RUNSTEAD_SCRIPTED_RESPONSES)")
	fmt.Fprintln(out, "  --workspace PATH          workspace path (RUNSTEAD_WORKSPACE, default .)")
	fmt.Fprintln(out, "  --state-dir PATH          durable state directory (RUNSTEAD_STATE_DIR, default $XDG_DATA_HOME/runstead or ~/.local/share/runstead)")
	fmt.Fprintln(out, "  --write-policy SPEC       write tool modes, e.g. write_file=allow,apply_patch=deny (RUNSTEAD_WRITE_POLICY, default approval_required)")
	fmt.Fprintln(out, "  --log-level LEVEL         debug, info, warn or error (RUNSTEAD_LOG_LEVEL, default info)")
	fmt.Fprintln(out, "  --max-steps N             maximum model turns (default 24)")
	fmt.Fprintln(out, "  --max-corrections N       protocol correction attempts (default 2)")
	fmt.Fprintln(out, "  --max-repeated-actions N  repeated-action corrections before stopping (default 2)")
	fmt.Fprintln(out, "  --time-budget DURATION    elapsed task time budget (default 10m)")
	fmt.Fprintln(out, "  --provider-budget N       governed provider attempts per task (default 80)")
	fmt.Fprintln(out, "  --min-start-interval DURATION  account governor pacing (default 5s)")
	fmt.Fprintln(out, "  --omniroute-base-url URL             base URL (OMNIROUTE_BASE_URL)")
	fmt.Fprintln(out, "  --omniroute-management-base-url URL  management URL (OMNIROUTE_MANAGEMENT_BASE_URL)")
	fmt.Fprintln(out, "  --omniroute-api-key KEY              API key (OMNIROUTE_API_KEY)")
	fmt.Fprintln(out, "  --omniroute-model MODEL              explicit model (OMNIROUTE_MODEL)")
	fmt.Fprintln(out, "  --omniroute-chat-endpoint PATH       endpoint (OMNIROUTE_CHAT_ENDPOINT)")
	fmt.Fprintln(out, "  --omniroute-timeout DURATION         timeout (OMNIROUTE_TIMEOUT)")
	fmt.Fprintln(out, "  --omniroute-safe-route               static declaration; remote preflight remains mandatory")
}

func printInspectHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead inspect <task-id> [--state-dir PATH]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Renders the durable state of one task after the original run process has")
	fmt.Fprintln(out, "exited: task identity, objective, status and typed outcome, the chronological")
	fmt.Fprintln(out, "event journal, logical actions, tool and provider attempts with evidence,")
	fmt.Fprintln(out, "uncertain or prepared states, and the relevant account-governor consumption,")
	fmt.Fprintln(out, "cooldown and circuit state. The output is stable, human-readable and")
	fmt.Fprintln(out, "sanitized; it never contains credentials or raw provider responses.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Exit codes: 0 rendered, 1 task not found, 2 usage, 3 state database unavailable.")
}

func printDecideHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead decide <task-id> <action-id> approved|rejected [--state-dir PATH] [--reason TEXT]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Records the operator control-plane decision for one write action. Only this")
	fmt.Fprintln(out, "command (or the equivalent state API) can approve or reject a write: model")
	fmt.Fprintln(out, "prose, repository content and tool output never authorize a write. The")
	fmt.Fprintln(out, "decision is persisted with its typed reason and rendered by")
	fmt.Fprintln(out, "'runstead inspect <task-id>'.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Exit codes: 0 recorded, 1 task not found, 2 usage, 3 state database unavailable.")
}
