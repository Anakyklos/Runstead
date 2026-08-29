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
	"github.com/RenyEnnos/Runstead/internal/provider/compat"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/trace"
	"github.com/RenyEnnos/Runstead/internal/verifier"
	"github.com/RenyEnnos/Runstead/internal/workunit"
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
	maxConsecutiveFailures := 0
	maxVerificationRetries := 0
	timeBudget := ""
	providerBudget := 0
	minStartInterval := ""
	allowanceProfile := ""
	omniBaseURL := ""
	omniManagementBaseURL := ""
	omniAPIKey := ""
	omniConnectionID := ""
	omniModel := ""
	omniChatEndpoint := ""
	omniTimeout := ""
	omniSafeRoute := false
	stateDir := ""
	writePolicy := ""
	recipesFile := ""
	recipePolicy := ""
	acceptanceFile := ""
	workUnitsFile := ""
	providersFile := ""
	providerID := ""
	retryPolicy := ""
	flags.StringVar(&workspace, "workspace", "", "workspace path (default: RUNSTEAD_WORKSPACE or .)")
	flags.StringVar(&logLevel, "log-level", "", "log level: debug, info, warn or error")
	flags.StringVar(&task, "task", "", "task prompt (RUNSTEAD_TASK)")
	flags.StringVar(&scripted, "scripted", "", "JSONL file of scripted model responses for a deterministic offline run (RUNSTEAD_SCRIPTED_RESPONSES)")
	flags.StringVar(&stateDir, "state-dir", "", "durable state directory (RUNSTEAD_STATE_DIR; default: $XDG_DATA_HOME/runstead or ~/.local/share/runstead)")
	flags.StringVar(&writePolicy, "write-policy", "", "write tool policy modes, e.g. write_file=allow,apply_patch=approval_required (RUNSTEAD_WRITE_POLICY; default: approval_required for every write tool)")
	flags.StringVar(&recipesFile, "recipes", "", "operator-controlled recipe catalog file (RUNSTEAD_RECIPES): JSON array of recipes; run_recipe fails closed without it")
	flags.StringVar(&recipePolicy, "recipe-policy", "", "recipe policy modes, e.g. test=allow,vet=approval_required (RUNSTEAD_RECIPE_POLICY; default: approval_required for every recipe)")
	flags.StringVar(&acceptanceFile, "acceptance", "", "operator acceptance plan file (RUNSTEAD_ACCEPTANCE_PLAN): versioned JSON of typed acceptance checks; completion requires every check to pass. Without a plan, completion is refused (fail closed)")
	flags.StringVar(&workUnitsFile, "workunits", "", "operator Work Unit file (M9 Stage A, issue #106): versioned JSON of bounded subtask definitions executed serially before the parent task run")
	flags.StringVar(&providersFile, "providers", "", "provider declarations file (RUNSTEAD_PROVIDERS): JSON document of provider_config-style endpoints (provider_id, protocol_family, base_url, model, auth_requirement, profile, ...) resolved through the #79 contract before dispatch")
	flags.StringVar(&providerID, "provider-id", "", "exactly one configured provider_id to execute with (RUNSTEAD_PROVIDER_ID); incompatible with --scripted and OmniRoute configuration")
	flags.StringVar(&retryPolicy, "retry-policy", "", "bounded governor-owned retry for configured compatible providers (RUNSTEAD_RETRY_POLICY): off (default) or bounded; every retried physical attempt re-enters the governor with a new admission, accounting and evidence")
	flags.IntVar(&maxSteps, "max-steps", 0, "maximum model turns (RUNSTEAD_MAX_STEPS, default 24)")
	flags.IntVar(&maxCorrections, "max-corrections", 0, "protocol correction attempts (RUNSTEAD_MAX_CORRECTIONS, default 2)")
	flags.IntVar(&maxRepeatedActions, "max-repeated-actions", 0, "repeated-action corrections before stopping (RUNSTEAD_MAX_REPEATED_ACTIONS, default 2)")
	flags.IntVar(&maxConsecutiveFailures, "max-consecutive-failures", 0, "consecutive failing tool/process observations before stopping (RUNSTEAD_MAX_CONSECUTIVE_FAILURES, default 5)")
	flags.IntVar(&maxVerificationRetries, "max-verification-retries", 0, "consecutive failed completion verifications before stopping (RUNSTEAD_MAX_VERIFICATION_RETRIES, default 3)")
	flags.StringVar(&timeBudget, "time-budget", "", "elapsed task time budget (RUNSTEAD_TIME_BUDGET, default 10m)")
	flags.IntVar(&providerBudget, "provider-budget", 0, "governed provider attempts per task (RUNSTEAD_PROVIDER_BUDGET, default 80)")
	flags.StringVar(&minStartInterval, "min-start-interval", "", "account governor start-to-start pacing (RUNSTEAD_MIN_START_INTERVAL, default 5s)")
	flags.StringVar(&allowanceProfile, "allowance-profile", "", "explicit account allowance profile: plus_go_instant (default), luna_unlimited_text or unknown (RUNSTEAD_ALLOWANCE_PROFILE)")
	flags.StringVar(&omniBaseURL, "omniroute-base-url", "", "OmniRoute base URL (OMNIROUTE_BASE_URL)")
	flags.StringVar(&omniManagementBaseURL, "omniroute-management-base-url", "", "OmniRoute management URL (OMNIROUTE_MANAGEMENT_BASE_URL)")
	flags.StringVar(&omniAPIKey, "omniroute-api-key", "", "OmniRoute API key (OMNIROUTE_API_KEY)")
	flags.StringVar(&omniConnectionID, "omniroute-connection-id", "", "exact OmniRoute provider connection pin for the protected chatgpt-web receipt lane (OMNIROUTE_CONNECTION_ID)")
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
		ConnectionID:         omniConnectionID,
		ConnectionIDSet:      flagWasSet(flags, "omniroute-connection-id"),
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

	limits, err := resolveLimits(flags, maxSteps, maxCorrections, maxRepeatedActions, maxConsecutiveFailures, maxVerificationRetries, timeBudget, providerBudget)
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
	providersPath, providersSet := resolveProviders(flags, providersFile)
	selectedProviderID, providerSelected := resolveProviderID(flags, providerID)

	if scriptedSet && (cfg.OmniRoute != nil || providersSet) {
		fmt.Fprintln(errOut, "run: scripted offline mode cannot be combined with OmniRoute configuration or provider declarations")
		return exitUsage
	}
	if providersSet && cfg.OmniRoute != nil {
		fmt.Fprintln(errOut, "run: provider declarations cannot be combined with OmniRoute configuration")
		return exitUsage
	}
	if providersSet != providerSelected {
		fmt.Fprintln(errOut, "run: --providers and --provider-id must be used together")
		return exitUsage
	}

	// The live provider-neutral surface (#14): exactly ONE configured provider
	// endpoint is resolved through the #79 contract before any dispatch. The
	// resolved value is sanitized identity plus validated configuration; the
	// concrete adapter is selected by protocol family only in the composition
	// layer (internal/provider/compat), never in the agent loop.
	var resolvedProvider *provider.Resolved
	if providerSelected {
		registry, loadErr := loadProviderRegistry(providersPath)
		if loadErr != nil {
			fmt.Fprintf(errOut, "run: %v\n", loadErr)
			return exitUsage
		}
		resolved, resolveErr := registry.Resolve(selectedProviderID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
		if resolveErr != nil {
			fmt.Fprintf(errOut, "run: %v\n", resolveErr)
			return exitUsage
		}
		resolvedProvider = resolved
	}

	retriesEnabled, err := resolveRetryPolicy(retryPolicy, flagWasSet(flags, "retry-policy"))
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	if retriesEnabled && (scriptedSet || cfg.OmniRoute != nil) {
		fmt.Fprintln(errOut, "run: --retry-policy bounded applies only to configured compatible providers (--providers/--provider-id)")
		return exitUsage
	}

	if scriptedSet {
		if cfg.OmniRoute != nil {
			fmt.Fprintln(errOut, "run: scripted offline mode cannot be combined with OmniRoute configuration")
			return exitUsage
		}
	} else if cfg.OmniRoute == nil && resolvedProvider == nil {
		fmt.Fprintln(errOut, "run: no provider configured. Use --scripted FILE for a deterministic offline run, --providers FILE with --provider-id ID for a configured provider endpoint, or the pinned OmniRoute lane (OMNIROUTE_BASE_URL, OMNIROUTE_API_KEY, OMNIROUTE_MODEL, OMNIROUTE_CONNECTION_ID).")
		return exitUnavailable
	} else if cfg.OmniRoute != nil && !cfg.OmniRoute.EnableAttemptReceipts {
		// The live lane is protected: it requires the exact connection pin.
		// Without it the historical unconditional refusal stays in place.
		fmt.Fprintln(errOut, "run: live OmniRoute requires the pinned receipt lane: set OMNIROUTE_CONNECTION_ID (--omniroute-connection-id) to pin the exact chatgpt-web connection. The legacy single-attempt declaration cannot authorize this lane.")
		return exitUnavailable
	}

	// The protected live lane constructs the receipt-aware OmniRoute client
	// before any admission: the gateway contract must be healthy and the
	// protected-route preflight must pass, or no model request is ever made.
	var liveClient *omniroute.Client
	if !scriptedSet && cfg.OmniRoute != nil {
		omniClient, err := omniroute.New(*cfg.OmniRoute, omniroute.Options{})
		if err != nil {
			fmt.Fprintf(errOut, "run: live OmniRoute lane unavailable: %v\n", err)
			return exitUnavailable
		}
		health := omniClient.ProbeGatewayContract(ctx)
		if !health.Healthy() {
			fmt.Fprintf(errOut, "run: OmniRoute gateway contract is not healthy (%s): %s\n", health.State, health.ReasonCode)
			return exitUnavailable
		}
		if err := omniClient.Preflight(ctx); err != nil {
			fmt.Fprintf(errOut, "run: OmniRoute preflight failed: %v\n", err)
			return exitUnavailable
		}
		liveClient = omniClient
	}

	accountConfig, err := resolveGovernorConfig(scriptedSet, cfg, resolvedProvider, minStartInterval, flagWasSet(flags, "min-start-interval"), allowanceProfile, flagWasSet(flags, "allowance-profile"))
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
	var providerIdentity provider.Identity
	if scriptedSet {
		responses, loadErr := loadScriptedResponses(scriptedPath)
		if loadErr != nil {
			fmt.Fprintf(errOut, "run: %v\n", loadErr)
			return exitUsage
		}
		client = provider.NewFake(responses...)
		model = "scripted"
	} else if resolvedProvider != nil {
		model = resolvedProvider.Model
		providerIdentity = provider.IdentityFromResolved(*resolvedProvider, compat.AdapterVersion)
		// Durable operational profile (#91): configured capability bounds
		// with provenance, persisted before execution. Metadata only.
		if _, profileErr := syncOperationalConfiguredBounds(ctx, store, resolvedProvider, providerIdentity); profileErr != nil {
			fmt.Fprintf(errOut, "run: %v\n", profileErr)
			return exitUnavailable
		}
		// Effective envelope bounds (#93): the profile's effective
		// request/output size bounds become the execution frontier (adapters
		// enforce size bounds per request). Unreadable profile state fails
		// closed before any execution.
		effectiveProvider, effErr := applyEffectiveProfileBounds(ctx, store, providerIdentity, resolvedProvider)
		if effErr != nil {
			fmt.Fprintf(errOut, "run: %v\n", effErr)
			return exitUnavailable
		}
		// Exactly one configured endpoint: the composition layer selects the
		// family adapter; the agent loop keeps depending only on
		// provider.Client and the provider-neutral contract (#14).
		compatClient, buildErr := compat.New(*effectiveProvider, compat.EnvSecretResolver(os.LookupEnv))
		if buildErr != nil {
			fmt.Fprintf(errOut, "run: provider %q unavailable: %v\n", selectedProviderID, buildErr)
			return exitUnavailable
		}
		client = compatClient
	} else {
		// The protected lane client was constructed and verified above
		// (gateway contract + preflight); every model request still flows
		// through the governor below.
		client = liveClient
		model = cfg.OmniRoute.Model
	}

	// The composition-layer classifier maps typed adapter failures onto the
	// governor outcome taxonomy (provider-neutral; used whenever a configured
	// compatible provider executes, independent of retry policy).
	var outcomeClassifier governor.OutcomeClassifier
	if resolvedProvider != nil {
		outcomeClassifier = compat.NewClassifier()
	}
	var executorOptions []agent.ExecutorOptions
	if resolvedProvider != nil && store != nil {
		// Conservative envelope learning (#93) runs for every configured
		// provider execution, independent of retry policy: admitted attempt
		// outcomes are observed and durable evidence is persisted.
		opts := agent.ExecutorOptions{Observer: newProfileObserver(store, providerIdentity, nil)}
		if retriesEnabled {
			opts.EnableRetry = true
			opts.RetryProfileCooldown = func() time.Duration {
				profile, loadErr := store.LoadOperationalProfile(ctx, providerIdentity)
				if loadErr != nil || profile == nil {
					return 0
				}
				value := profile.Effective(provider.FieldCooldownMillis)
				if !value.Known() {
					return 0
				}
				return time.Duration(value.Value) * time.Millisecond
			}
		}
		executorOptions = append(executorOptions, opts)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, outcomeClassifier, executorOptions...)
	if err != nil {
		fmt.Fprintf(errOut, "run: executor unavailable: %v\n", err)
		return exitUnavailable
	}
	// The recipe catalog is operator-controlled control-plane input: it is
	// read once at startup and is never derived from workspace content. The
	// effective recipe policy defaults to approval_required for every recipe
	// and is persisted with the task configuration.
	recipes, err := resolveRecipeCatalog(recipesFile, flagWasSet(flags, "recipes"))
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: cfg.Workspace, Recipes: recipes})
	if err != nil {
		fmt.Fprintf(errOut, "run: workspace unavailable: %v\n", err)
		return exitUnavailable
	}
	writePolicyConfig, err := resolveWritePolicy(writePolicy, flagWasSet(flags, "write-policy"))
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	recipePolicyConfig, err := resolveRecipePolicy(recipePolicy, flagWasSet(flags, "recipe-policy"), recipes)
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	policyConfig := writePolicyConfig
	policyConfig.RecipeModes = recipePolicyConfig.RecipeModes
	acceptance, acceptanceDigest, err := resolveAcceptancePlan(acceptanceFile, flagWasSet(flags, "acceptance"))
	if err != nil {
		fmt.Fprintf(errOut, "run: %v\n", err)
		return exitUsage
	}
	// In work unit mode the parent loop skips the in-loop task bootstrap
	// (the durable task root was persisted before the serial chain) by
	// carrying a non-nil empty recovery seed.
	var parentRecovery *agent.RecoverySeed
	if workUnitsFile != "" {
		parentRecovery = &agent.RecoverySeed{}
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner:               executor,
		Registry:             registry,
		Limits:               limits,
		Model:                model,
		ProviderIdentity:     providerIdentity,
		Trace:                cliTraceSink(errOut),
		State:                store,
		Policy:               policy.NewStatic(policyConfig, storeApprovals(store)),
		WritePolicy:          writePolicyConfig.Spec(),
		RecipePolicy:         recipePolicyConfig.RecipeSpec(recipeIDs(recipes)),
		RecipeCatalogDigest:  recipes.Digest(),
		Verifier:             verifier.New(registry, acceptance),
		AcceptancePlanDigest: acceptanceDigest,
		Recovery:             parentRecovery,
	})
	if err != nil {
		fmt.Fprintf(errOut, "run: loop unavailable: %v\n", err)
		return exitUnavailable
	}

	taskID := "cli-" + fmt.Sprint(time.Now().UnixNano())
	providerLabel := "scripted"
	if resolvedProvider != nil {
		providerLabel = resolvedProvider.ProviderID
	} else if !scriptedSet {
		providerLabel = "omniroute"
	}
	logger.InfoContext(ctx, "run started", "task_id", taskID, "provider", providerLabel, "workspace", cfg.Workspace)
	fmt.Fprintf(errOut, "task: %s\n", taskID)
	if workUnitsFile != "" {
		definitions, err := loadWorkUnitFile(workUnitsFile)
		if err != nil {
			fmt.Fprintf(errOut, "run: %v\n", err)
			return exitUsage
		}
		if err := bootstrapTaskForWorkUnits(ctx, store, taskID, taskPrompt, cfg.Workspace, model, acceptance); err != nil {
			fmt.Fprintf(errOut, "run: %v\n", err)
			return exitUnavailable
		}
		// Unit loops skip the in-loop bootstrap by carrying a non-nil
		// (empty) recovery seed; the task row already exists.
		emptySeed := &agent.RecoverySeed{}
		pieces := unitLoopPieces{
			runner:              executor,
			registry:            registry,
			model:               model,
			providerIdentity:    providerIdentity,
			trace:               cliTraceSink(errOut),
			store:               store,
			policy:              policy.NewStatic(policyConfig, storeApprovals(store)),
			writePolicy:         writePolicyConfig.Spec(),
			recipePolicy:        recipePolicyConfig.RecipeSpec(recipeIDs(recipes)),
			recipeCatalogDigest: recipes.Digest(),
			limits:              limits,
			recovery:            emptySeed,
		}
		chainErr := runWorkUnitChain(ctx, store, taskID, cfg.Workspace, registry, definitions,
			func(ctx context.Context, unit state.WorkUnit) (workunit.RunResult, error) {
				return runUnitLoop(ctx, pieces, taskID, unit)
			})
		if chainErr != nil {
			if errors.Is(chainErr, context.Canceled) {
				return exitWorkUnitCanceled
			}
			fmt.Fprintf(errOut, "run: work units: %v\n", chainErr)
			return exitWorkUnitGated
		}
	}

	result := loop.Run(ctx, agent.Task{ID: taskID, Prompt: taskPrompt})
	if err := printFinalRuntimeResult(ctx, out, errOut, store, taskID, result, "run"); err != nil {
		return exitUnavailable
	}
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

// resolveProviders resolves the provider declarations file path from the
// flag or RUNSTEAD_PROVIDERS, mirroring --scripted.
func resolveProviders(flags *flag.FlagSet, providersFile string) (string, bool) {
	if strings.TrimSpace(providersFile) != "" {
		return providersFile, true
	}
	if value, ok := os.LookupEnv(config.EnvProviders); ok {
		value = strings.TrimSpace(value)
		return value, value != ""
	}
	return "", false
}

// resolveProviderID resolves the exact configured provider_id from the flag
// or RUNSTEAD_PROVIDER_ID.
func resolveProviderID(flags *flag.FlagSet, providerID string) (string, bool) {
	if strings.TrimSpace(providerID) != "" {
		return strings.TrimSpace(providerID), true
	}
	if value, ok := os.LookupEnv(config.EnvProviderID); ok {
		value = strings.TrimSpace(value)
		return value, value != ""
	}
	return "", false
}

// loadProviderRegistry loads and validates the provider declarations file.
// Every declared provider must be self-consistent; the selected provider is
// resolved separately through the #79 contract before any dispatch.
func loadProviderRegistry(path string) (*provider.Registry, error) {
	return config.LoadProvidersFile(path)
}

// resolveRetryPolicy resolves the bounded retry policy from the flag or
// RUNSTEAD_RETRY_POLICY. The default is OFF: existing workloads never gain
// implicit retries just because the feature exists. Only "bounded" enables
// the governor-owned retry path for configured compatible providers.
func resolveRetryPolicy(flagValue string, flagSet bool) (bool, error) {
	value := strings.TrimSpace(flagValue)
	if value == "" && !flagSet {
		if env, ok := os.LookupEnv(config.EnvRetryPolicy); ok {
			value = strings.TrimSpace(env)
		}
	}
	switch value {
	case "", "off":
		return false, nil
	case "bounded":
		return true, nil
	default:
		return false, fmt.Errorf("retry policy %q must be %q or %q", value, "off", "bounded")
	}
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

// resolveRecipeCatalog loads the operator-controlled recipe catalog from the
// --recipes flag, then RUNSTEAD_RECIPES. A nil catalog (no flag and no env)
// makes run_recipe fail closed; an explicit but unreadable catalog is an
// error.
func resolveRecipeCatalog(flagValue string, flagSet bool) (*recipe.Catalog, error) {
	value := ""
	if flagSet {
		value = strings.TrimSpace(flagValue)
		if value == "" {
			return nil, errors.New("recipes file must not be empty")
		}
	} else if envValue, ok := os.LookupEnv(config.EnvRecipes); ok {
		value = strings.TrimSpace(envValue)
	}
	if value == "" {
		return nil, nil
	}
	catalog, err := recipe.LoadCatalog(value)
	if err != nil {
		return nil, err
	}
	return catalog, nil
}

// resolveAcceptancePlan loads the operator acceptance plan from the
// --acceptance flag, then RUNSTEAD_ACCEPTANCE_PLAN. A nil plan (no flag and no
// env) fails closed: the verifier refuses completion blocked, because without
// task-specific acceptance criteria a completion proposal can never be proven
// against the task objective (issue #11 review). The operator attaches a plan
// at resume with --acceptance for tasks that started without one. An explicit
// but unreadable or invalid plan is an error.
func resolveAcceptancePlan(flagValue string, flagSet bool) (*verifier.Plan, string, error) {
	value := ""
	if flagSet {
		value = strings.TrimSpace(flagValue)
		if value == "" {
			return nil, "", errors.New("acceptance plan file must not be empty")
		}
	} else if envValue, ok := os.LookupEnv(config.EnvAcceptancePlan); ok {
		value = strings.TrimSpace(envValue)
	}
	if value == "" {
		return nil, "", nil
	}
	data, err := os.ReadFile(value)
	if err != nil {
		return nil, "", fmt.Errorf("read acceptance plan: %w", err)
	}
	plan, err := verifier.ParsePlan(data)
	if err != nil {
		return nil, "", err
	}
	return plan, plan.Digest(), nil
}

// resolveRecipePolicy resolves the recipe policy modes from the
// --recipe-policy flag, then RUNSTEAD_RECIPE_POLICY, then the fail-closed
// default (approval_required for every recipe in the catalog). Modes for
// recipes that are not in the configured catalog are rejected: a policy for
// an unavailable recipe is meaningless and a typo must never silently
// reconfigure the effective policy.
func resolveRecipePolicy(flagValue string, flagSet bool, catalog *recipe.Catalog) (policy.Config, error) {
	value := ""
	if flagSet {
		value = strings.TrimSpace(flagValue)
		if value == "" {
			return policy.Config{}, errors.New("recipe policy must not be empty")
		}
	} else if envValue, ok := os.LookupEnv(config.EnvRecipePolicy); ok {
		value = strings.TrimSpace(envValue)
	}
	config := policy.Config{Modes: map[string]policy.Mode{}, RecipeModes: map[string]policy.Mode{}}
	if value == "" {
		return config, nil
	}
	parsed, err := policy.ParseRecipePolicy(value)
	if err != nil {
		return policy.Config{}, err
	}
	for id := range parsed.RecipeModes {
		if catalog == nil {
			return policy.Config{}, fmt.Errorf("recipe policy configures %q but no recipes are configured", id)
		}
		if _, ok := catalog.Get(id); !ok {
			return policy.Config{}, fmt.Errorf("recipe policy configures unknown recipe %q", id)
		}
	}
	config.RecipeModes = parsed.RecipeModes
	return config, nil
}

// recipeIDs returns the sorted recipe ids of the catalog (empty for nil).
func recipeIDs(catalog *recipe.Catalog) []string {
	if catalog == nil {
		return nil
	}
	return catalog.IDs()
}

func resolveLimits(flags *flag.FlagSet, maxSteps, maxCorrections, maxRepeatedActions, maxConsecutiveFailures, maxVerificationRetries int, timeBudget string, providerBudget int) (agent.Limits, error) {
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
	if flagWasSet(flags, "max-consecutive-failures") {
		if maxConsecutiveFailures <= 0 {
			return agent.Limits{}, fmt.Errorf("max-consecutive-failures must be positive")
		}
		limits.MaxConsecutiveFailures = maxConsecutiveFailures
	} else if value, ok := os.LookupEnv(config.EnvMaxConsecutiveFailures); ok {
		parsed, err := parsePositiveInt(config.EnvMaxConsecutiveFailures, value)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.MaxConsecutiveFailures = parsed
	}
	if flagWasSet(flags, "max-verification-retries") {
		if maxVerificationRetries <= 0 {
			return agent.Limits{}, fmt.Errorf("max-verification-retries must be positive")
		}
		limits.MaxVerificationRetries = maxVerificationRetries
	} else if value, ok := os.LookupEnv(config.EnvMaxVerificationRetries); ok {
		parsed, err := parsePositiveInt(config.EnvMaxVerificationRetries, value)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.MaxVerificationRetries = parsed
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

// resolveGovernorConfig builds the account governor policy for `run`. The
// allowance profile is explicit operator configuration (issue #58): it is
// never inferred from model naming or from request success. The default is
// the historical PlusGoInstant published-quota profile. Reasoning is not
// selectable here because it requires explicit rolling ceilings that the CLI
// does not expose; an operator needing it must configure the governor
// directly.
func resolveGovernorConfig(scripted bool, cfg config.Config, resolved *provider.Resolved, minStartInterval string, intervalSet bool, allowanceProfile string, profileSet bool) (governor.Config, error) {
	providerID := "scripted"
	model := "scripted"
	safety := provider.SafeRouteSafety()
	if !scripted && resolved != nil {
		// The configured provider endpoint: the governor admits attempts for
		// this exact provider identity and the exact configured model. Route
		// safety stays the executable SafeRouteSafety declaration the
		// adapters are proven against (#14).
		providerID = resolved.ProviderID
		model = resolved.Model
		safety = provider.SafeRouteSafety()
	} else if !scripted {
		if cfg.OmniRoute == nil {
			return governor.Config{}, fmt.Errorf("no provider configuration for the account governor")
		}
		providerID = "omniroute"
		model = cfg.OmniRoute.Model
		safety = cfg.OmniRoute.RouteSafety
	}
	if !profileSet {
		allowanceProfile = os.Getenv(config.EnvAllowanceProfile)
		profileSet = strings.TrimSpace(allowanceProfile) != ""
	}
	// The provider-neutral surface is an ARBITRARY configured compatible
	// endpoint: its upstream allowance semantics are unknown unless the
	// operator declares them explicitly. Never fabricate the historical
	// plus_go_instant published-quota contract for such an endpoint (#58/#14).
	// The legacy scripted and OmniRoute lanes keep their historical default.
	if !scripted && resolved != nil && !profileSet {
		allowanceProfile = string(governor.ProfileUnknown)
	}
	var accountConfig governor.Config
	switch strings.TrimSpace(allowanceProfile) {
	case "", string(governor.ProfileInstant):
		accountConfig = governor.DefaultInstantConfig("runstead-cli", providerID, "instant", safety)
	case string(governor.ProfileLunaUnlimitedText):
		accountConfig = governor.DefaultLunaUnlimitedTextConfig("runstead-cli", providerID, "instant", safety)
	case string(governor.ProfileUnknown):
		accountConfig = governor.DefaultUnknownConfig("runstead-cli", providerID, "instant", safety)
	case string(governor.ProfileReasoning):
		return governor.Config{}, fmt.Errorf("allowance profile %q requires explicit rolling ceilings that the CLI does not expose", governor.ProfileReasoning)
	default:
		return governor.Config{}, fmt.Errorf("unsupported allowance profile %q", allowanceProfile)
	}
	accountConfig.Model = model
	if resolved != nil {
		// Sanitized provider-neutral identity on every governed attempt
		// (persisted with provider_attempts, #14). Never wire types, never
		// secret material.
		accountConfig.ProtocolFamily = resolved.ProtocolFamily
		accountConfig.ConfigIdentity = resolved.ConfigIdentity
	}
	// The pinned receipt lane: the governor requires authoritative attempt
	// receipts, uses the same receipt-aware route safety as the adapter and
	// validates the derived account-lane hash against every receipt.
	if !scripted && cfg.OmniRoute != nil && cfg.OmniRoute.EnableAttemptReceipts {
		accountConfig.RequireSingleAttempt = false
		accountConfig.RequireAttemptReceipts = true
		accountConfig.AccountLaneHash = cfg.OmniRoute.AccountLaneHash
		accountConfig.AttemptProviderID = cfg.OmniRoute.Provider
	}
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
		// The model's own final text is unverified free text, kept explicitly
		// separate from the verified summary so it can never be mistaken for a
		// verified completion claim (issue #11 review).
		if result.Note != "" {
			fmt.Fprintf(out, "note (unverified): %s\n", result.Note)
		}
		for _, id := range result.Evidence {
			fmt.Fprintf(out, "evidence: %s\n", id)
		}
	}
	if result.Outcome == agent.OutcomeApprovalRequired && result.PendingActionID != "" {
		fmt.Fprintf(out, "pending approval: action %s\n", result.PendingActionID)
		fmt.Fprintf(out, "runstead decide %s %s approved|rejected\n", taskID, result.PendingActionID)
	}
	if result.Outcome == agent.OutcomeVerificationBlocked {
		fmt.Fprintf(out, "completion refused by the runtime verifier; runstead inspect %s explains the checks\n", taskID)
		fmt.Fprintf(out, "reconcile the uncertain effect or pending approval, then runstead resume %s\n", taskID)
	}
	fmt.Fprintf(errOut, "run: turns=%d attempts=%d observations=%d corrections=%d repeated=%d mixed_prose=%d\n",
		result.Turns, result.Attempts, result.Observations, result.Corrections, result.Repeated, result.MixedProse)
	if result.Classification != "" {
		fmt.Fprintf(errOut, "run: classification=%s\n", result.Classification)
	}
}

// printFinalRuntimeResult keeps the typed loop result and the model note
// separate from the completion projection. The latter is loaded from durable
// state only after a completed outcome and independently validates the
// persisted task/verifier state before rendering.
func printFinalRuntimeResult(ctx context.Context, out, errOut io.Writer, store *state.Store, taskID string, result agent.Result, command string) error {
	printResult(out, errOut, taskID, result)
	if result.Outcome != agent.OutcomeCompleted {
		return nil
	}
	if err := store.RenderFinal(ctx, out, taskID); err != nil {
		fmt.Fprintf(errOut, "%s: verified final output unavailable: %v\n", command, err)
		return err
	}
	return nil
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
		if line.ExitCode != 0 {
			fmt.Fprintf(&builder, " exit=%d", line.ExitCode)
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
		fmt.Fprintf(errOut, "decide: action %q of task %q is not pending approval; only write and recipe actions awaiting an operator decision can be decided\n", actionID, taskID)
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
	fmt.Fprintln(out, "  run       run a bounded agent task with durable state, policy-bound writes and recipes")
	fmt.Fprintln(out, "  inspect   inspect durable task state by task id")
	fmt.Fprintln(out, "  resume    resume an interrupted task from durable state")
	fmt.Fprintln(out, "  decide    approve or reject a pending write/recipe action (operator control plane)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Configuration precedence: flags > environment > defaults")
	fmt.Fprintf(out, "  %s, %s, %s, %s\n", config.EnvWorkspace, config.EnvLogLevel, config.EnvTask, config.EnvStateDir)
	fmt.Fprintf(out, "  OmniRoute: %s, %s, %s, %s, %s\n", config.EnvOmniRouteBaseURL, config.EnvOmniRouteAPIKey, config.EnvOmniRouteModel, config.EnvOmniRouteChatEndpoint, config.EnvOmniRouteConnectionID)
	fmt.Fprintln(out, "Use 'runstead <command> --help' for command-specific help.")
}

// providerRunHelpFragment documents the provider-neutral live surface (#14):
// the operator declares endpoints in a providers file and selects exactly one
// provider_id per execution.
const providerRunHelpFragment = `  --providers FILE         provider declarations file (RUNSTEAD_PROVIDERS): JSON document of exactly
                           configured endpoints (provider_id, protocol_family, base_url, model,
                           auth_requirement, auth_ref, options, profile{profile_version,
                           capabilities, route_safety}, config_version). Every declaration is
                           validated through the #79 contract before any dispatch.
  --provider-id ID         execute with exactly one configured provider_id (RUNSTEAD_PROVIDER_ID).
                           Two different provider IDs may share one protocol adapter; the agent
                           loop never branches on provider identity or protocol family.
`

func printRunHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead run [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Runs one bounded agent task: every model turn is admitted by the")
	fmt.Fprintln(out, "account governor, actions are validated and executed by the tool")
	fmt.Fprintln(out, "registry (read-only tools plus policy-gated write_file/apply_patch and")
	fmt.Fprintln(out, "operator-declared process recipes), and completion is decided only by the")
	fmt.Fprintln(out, "runtime verifier: a status \"complete\" final is a proposal, never proof.")
	fmt.Fprintln(out, "The verifier independently observes persisted evidence, the real filesystem,")
	fmt.Fprintln(out, "git state and the operator acceptance plan (--acceptance FILE); a failed")
	fmt.Fprintln(out, "verification returns a structured verification observation so execution can")
	fmt.Fprintln(out, "continue, and completion is refused while any effect is uncertain or an")
	fmt.Fprintln(out, "approval is pending (issue #11). Writes are")
	fmt.Fprintln(out, "stale-state protected, stay inside the workspace, and never execute")
	fmt.Fprintln(out, "without control-plane approval when the policy requires it. Process")
	fmt.Fprintln(out, "recipes (--recipes FILE) run their fixed operator-declared argv with an")
	fmt.Fprintln(out, "allowlisted environment, bounded output and full process-tree")
	fmt.Fprintln(out, "termination; the model never supplies commands or argv. When a")
	fmt.Fprintln(out, "policy-gated effect needs approval the run PAUSES with the typed")
	fmt.Fprintln(out, "approval_required outcome, reports the task and pending action for")
	fmt.Fprintln(out, "'runstead decide <task-id> <action-id> approved|rejected', and stays")
	fmt.Fprintln(out, "durably resumable; no correction budget is consumed. The task never")
	fmt.Fprintln(out, "shells out; durable task, action, attempt, evidence, journal, policy and")
	fmt.Fprintln(out, "account-protection state is persisted to SQLite (issues #8/#10/#26) and")
	fmt.Fprintln(out, "can be inspected with 'runstead inspect <task-id>'. After a verified completed")
	fmt.Fprintln(out, "run, stdout also includes a bounded 'Verified runtime result:' projection")
	fmt.Fprintln(out, "with durable evidence IDs, verifier checks, Git attribution/diff and recipe")
	fmt.Fprintln(out, "process results; the model's final text remains 'note (unverified)'.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "The deterministic offline mode replays scripted model responses (JSONL with")
	fmt.Fprintln(out, "one {\"text\":\"...\"} object per line) through the real governor and tools.")
	fmt.Fprintln(out, "Live mode runs the pinned OmniRoute receipt lane: exactly provider")
	fmt.Fprintln(out, "chatgpt-web, the configured chatgpt-web/<model>, the dedicated")
	fmt.Fprintln(out, "providers/chatgpt-web/chat/completions route and the exact")
	fmt.Fprintln(out, "OMNIROUTE_CONNECTION_ID pin, with one authoritative attempt receipt")
	fmt.Fprintln(out, "per model send. The gateway contract must be healthy and preflight")
	fmt.Fprintln(out, "must pass before any model request; the legacy --omniroute-safe-route")
	fmt.Fprintln(out, "declaration cannot authorize the live lane.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Flags:")
	fmt.Fprintln(out, "  --task PROMPT             task prompt (RUNSTEAD_TASK)")
	fmt.Fprintln(out, "  --scripted FILE           scripted responses for a deterministic offline run (RUNSTEAD_SCRIPTED_RESPONSES)")
	fmt.Fprintln(out, "  --workspace PATH          workspace path (RUNSTEAD_WORKSPACE, default .)")
	fmt.Fprintln(out, "  --state-dir PATH          durable state directory (RUNSTEAD_STATE_DIR, default $XDG_DATA_HOME/runstead or ~/.local/share/runstead)")
	fmt.Fprintln(out, "  --write-policy SPEC       write tool modes, e.g. write_file=allow,apply_patch=deny (RUNSTEAD_WRITE_POLICY, default approval_required)")
	fmt.Fprintln(out, "  --recipes FILE            operator-controlled recipe catalog (RUNSTEAD_RECIPES); run_recipe fails closed without it")
	fmt.Fprintln(out, "  --recipe-policy SPEC      recipe modes, e.g. test=allow,vet=deny (RUNSTEAD_RECIPE_POLICY, default approval_required)")
	fmt.Fprintln(out, "  --acceptance FILE         operator acceptance plan: versioned JSON of typed checks (RUNSTEAD_ACCEPTANCE_PLAN); completion requires every check to pass and is refused (fail closed) without a plan")
	fmt.Fprintln(out, "  --log-level LEVEL         debug, info, warn or error (RUNSTEAD_LOG_LEVEL, default info)")
	fmt.Fprintln(out, "  --max-steps N             maximum model turns (default 24)")
	fmt.Fprintln(out, "  --max-corrections N       protocol correction attempts (default 2)")
	fmt.Fprintln(out, "  --max-repeated-actions N  repeated-action corrections before stopping (default 2)")
	fmt.Fprintln(out, "  --max-consecutive-failures N  consecutive failing tool/process observations before stopping with a typed reason (default 5)")
	fmt.Fprintln(out, "  --max-verification-retries N  consecutive failed completion verifications before stopping with a typed reason (default 3)")
	fmt.Fprintln(out, "  --time-budget DURATION    elapsed task time budget (default 10m)")
	fmt.Fprintln(out, "  --provider-budget N       governed provider attempts per task (default 80)")
	fmt.Fprintln(out, "  --min-start-interval DURATION  account governor pacing (default 5s)")
	fmt.Fprintln(out, "  --allowance-profile PROFILE  explicit account allowance profile: plus_go_instant (default), luna_unlimited_text or unknown (RUNSTEAD_ALLOWANCE_PROFILE)")
	fmt.Fprintln(out, "  --omniroute-base-url URL             base URL (OMNIROUTE_BASE_URL)")
	fmt.Fprintln(out, "  --omniroute-management-base-url URL  management URL (OMNIROUTE_MANAGEMENT_BASE_URL)")
	fmt.Fprintln(out, "  --omniroute-api-key KEY              API key (OMNIROUTE_API_KEY)")
	fmt.Fprintln(out, "  --omniroute-connection-id ID         exact connection pin for the protected chatgpt-web receipt lane (OMNIROUTE_CONNECTION_ID); required for live mode")
	fmt.Fprintln(out, "  --omniroute-model MODEL              explicit chatgpt-web/<model> (OMNIROUTE_MODEL)")
	fmt.Fprintln(out, "  --omniroute-chat-endpoint PATH       generic endpoint (OMNIROUTE_CHAT_ENDPOINT); incompatible with the pinned live lane")
	fmt.Fprintln(out, "  --omniroute-timeout DURATION         timeout (OMNIROUTE_TIMEOUT)")
	fmt.Fprintln(out, "  --omniroute-safe-route               legacy static declaration; cannot authorize the live receipt lane")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Bounded governor-owned retry (#92): --retry-policy bounded (off by default) lets")
	fmt.Fprintln(out, "a configured compatible provider retry delivery-safe rate/capacity and transient")
	fmt.Fprintln(out, "server failures strictly under governor authority: every retried physical attempt")
	fmt.Fprintln(out, "re-enters the governor with a new admission, new accounting and new evidence,")
	fmt.Fprintln(out, "bounded by the existing retry/elapsed budgets and circuit/cooldown safety.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "The provider-neutral live surface (#14) selects exactly ONE configured provider")
	fmt.Fprintln(out, "endpoint per execution:")
	fmt.Fprint(out, providerRunHelpFragment)
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
