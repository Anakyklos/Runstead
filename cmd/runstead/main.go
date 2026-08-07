package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/trace"
)

const (
	exitSuccess     = 0
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
		return placeholderCommand(ctx, "inspect", args[1:], out, errOut)
	case "resume":
		return placeholderCommand(ctx, "resume", args[1:], out, errOut)
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
	flags.StringVar(&workspace, "workspace", "", "workspace path (default: RUNSTEAD_WORKSPACE or .)")
	flags.StringVar(&logLevel, "log-level", "", "log level: debug, info, warn or error")
	flags.StringVar(&task, "task", "", "task prompt (RUNSTEAD_TASK)")
	flags.StringVar(&scripted, "scripted", "", "JSONL file of scripted model responses for a deterministic offline run (RUNSTEAD_SCRIPTED_RESPONSES)")
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
	accountGovernor, err := governor.New(accountConfig, governor.Options{Events: trace.NewPolicySink(logger)})
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
	loop, err := agent.NewLoop(agent.Config{
		Runner:   executor,
		Registry: registry,
		Limits:   limits,
		Model:    model,
		Trace:    cliTraceSink(errOut),
	})
	if err != nil {
		fmt.Fprintf(errOut, "run: loop unavailable: %v\n", err)
		return exitUnavailable
	}

	taskID := "cli-" + fmt.Sprint(time.Now().UnixNano())
	logger.InfoContext(ctx, "run started", "task_id", taskID, "provider", "scripted", "workspace", cfg.Workspace)
	result := loop.Run(ctx, agent.Task{ID: taskID, Prompt: taskPrompt})
	printResult(out, errOut, result)
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

func printResult(out, errOut io.Writer, result agent.Result) {
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

func placeholderCommand(ctx context.Context, name string, args []string, out, errOut io.Writer) int {
	if hasHelp(args) {
		printPlaceholderHelp(out, name)
		return exitSuccess
	}

	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(errOut, "%s: invalid flags: %v\n", name, err)
		printPlaceholderHelp(errOut, name)
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(errOut, "%s: unexpected argument %q\n", name, flags.Arg(0))
		printPlaceholderHelp(errOut, name)
		return exitUsage
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(errOut, "%s: canceled\n", name)
		return agent.OutcomeCanceled.ExitCode()
	}
	fmt.Fprintf(errOut, "%s: persistent state and recovery are not implemented in issue #3\n", name)
	return exitUnavailable
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
	fmt.Fprintln(out, "  run       run a bounded read-only agent task")
	fmt.Fprintln(out, "  inspect   inspect durable task state (placeholder)")
	fmt.Fprintln(out, "  resume    resume durable task state (placeholder)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Configuration precedence: flags > environment > defaults")
	fmt.Fprintf(out, "  %s, %s, %s\n", config.EnvWorkspace, config.EnvLogLevel, config.EnvTask)
	fmt.Fprintf(out, "  OmniRoute: %s, %s, %s, %s\n", config.EnvOmniRouteBaseURL, config.EnvOmniRouteAPIKey, config.EnvOmniRouteModel, config.EnvOmniRouteChatEndpoint)
	fmt.Fprintln(out, "Use 'runstead <command> --help' for command-specific help.")
}

func printRunHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead run [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runs one bounded read-only agent task: every model turn is admitted by the")
	fmt.Fprintln(out, "account governor, actions are validated and executed by the read-only tool")
	fmt.Fprintln(out, "registry, and a final answer is accepted only when grounded in real evidence")
	fmt.Fprintln(out, "IDs produced in this run. The task never writes, shells out or persists.")
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

func printPlaceholderHelp(out io.Writer, name string) {
	fmt.Fprintf(out, "Usage: runstead %s\n\n", name)
	fmt.Fprintf(out, "The %s command is an explicit placeholder; persistent state and recovery are not implemented in issue #3.\n", name)
	fmt.Fprintln(out, "No flags are currently supported.")
}
