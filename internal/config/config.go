package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
)

const (
	EnvWorkspace = "RUNSTEAD_WORKSPACE"
	EnvLogLevel  = "RUNSTEAD_LOG_LEVEL"
	EnvTask      = "RUNSTEAD_TASK"

	EnvScriptedResponses = "RUNSTEAD_SCRIPTED_RESPONSES"
	EnvMinStartInterval  = "RUNSTEAD_MIN_START_INTERVAL"

	EnvOmniRouteBaseURL           = "OMNIROUTE_BASE_URL"
	EnvOmniRouteAPIKey            = "OMNIROUTE_API_KEY"
	EnvOmniRouteModel             = "OMNIROUTE_MODEL"
	EnvOmniRouteChatEndpoint      = "OMNIROUTE_CHAT_ENDPOINT"
	EnvOmniRouteManagementBaseURL = "OMNIROUTE_MANAGEMENT_BASE_URL"
	EnvOmniRouteTimeout           = "OMNIROUTE_TIMEOUT"
	EnvOmniRouteMaxRequestBytes   = "OMNIROUTE_MAX_REQUEST_BYTES"
	EnvOmniRouteMaxResponseBytes  = "OMNIROUTE_MAX_RESPONSE_BYTES"

	EnvOmniRouteSingleAttemptGuaranteed = "OMNIROUTE_SINGLE_ATTEMPT_GUARANTEED"
	EnvOmniRouteInternalRetriesDisabled = "OMNIROUTE_INTERNAL_RETRIES_DISABLED"
	EnvOmniRouteCooldownReplayDisabled  = "OMNIROUTE_COOLDOWN_REPLAY_DISABLED"
	EnvOmniRouteAccountPoolingDisabled  = "OMNIROUTE_ACCOUNT_POOLING_DISABLED"
	EnvOmniRouteFallbackDisabled        = "OMNIROUTE_AUTOMATIC_FALLBACK_DISABLED"
	EnvOmniRouteComboRoutingDisabled    = "OMNIROUTE_COMBO_ROUTING_DISABLED"

	DefaultWorkspace        = "."
	DefaultLogLevel         = "info"
	DefaultOmniRouteBaseURL = "http://127.0.0.1:20128/v1"
	DefaultOmniRouteModel   = "chatgpt-web/model"
)

type Config struct {
	Workspace     string
	LogLevel      string
	AccountPolicy *governor.Config
	OmniRoute     *omniroute.Config
}

type Overrides struct {
	Workspace        string
	WorkspaceSet     bool
	LogLevel         string
	LogLevelSet      bool
	AccountPolicy    *governor.Config
	AccountPolicySet bool
	OmniRoute        OmniRouteOverrides
}

type OmniRouteOverrides struct {
	BaseURL              string
	BaseURLSet           bool
	ManagementBaseURL    string
	ManagementBaseURLSet bool
	APIKey               string
	APIKeySet            bool
	Model                string
	ModelSet             bool
	ChatEndpoint         string
	ChatEndpointSet      bool
	Timeout              time.Duration
	TimeoutSet           bool
	MaxRequestBytes      int
	MaxRequestBytesSet   bool
	MaxResponseBytes     int
	MaxResponseBytesSet  bool
	RouteSafety          provider.RouteSafety
	RouteSafetySet       bool
}

type LookupEnv func(string) (string, bool)

func Resolve(overrides Overrides, lookupEnv LookupEnv) (Config, error) {
	workspace := DefaultWorkspace
	logLevel := DefaultLogLevel
	var accountPolicy *governor.Config
	if lookupEnv != nil {
		if value, ok := lookupEnv(EnvWorkspace); ok {
			workspace = value
		}
		if value, ok := lookupEnv(EnvLogLevel); ok {
			logLevel = value
		}
	}
	if overrides.WorkspaceSet {
		workspace = overrides.Workspace
	}
	if overrides.LogLevelSet {
		logLevel = overrides.LogLevel
	}
	if overrides.AccountPolicySet {
		if overrides.AccountPolicy == nil {
			return Config{}, fmt.Errorf("account policy must be provided when configured")
		}
		copy := *overrides.AccountPolicy
		if err := copy.Validate(); err != nil {
			return Config{}, fmt.Errorf("invalid account policy: %w", err)
		}
		accountPolicy = &copy
	}

	omniRoute, err := resolveOmniRoute(overrides.OmniRoute, lookupEnv)
	if err != nil {
		return Config{}, err
	}

	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return Config{}, fmt.Errorf("workspace must not be empty")
	}
	logLevel = strings.ToLower(strings.TrimSpace(logLevel))
	if !validLogLevel(logLevel) {
		return Config{}, fmt.Errorf("log level %q must be one of debug, info, warn or error", logLevel)
	}

	return Config{Workspace: workspace, LogLevel: logLevel, AccountPolicy: accountPolicy, OmniRoute: omniRoute}, nil
}

func resolveOmniRoute(overrides OmniRouteOverrides, lookupEnv LookupEnv) (*omniroute.Config, error) {
	configured := overrides.BaseURLSet || overrides.ManagementBaseURLSet || overrides.APIKeySet || overrides.ModelSet || overrides.ChatEndpointSet || overrides.TimeoutSet || overrides.MaxRequestBytesSet || overrides.MaxResponseBytesSet || overrides.RouteSafetySet
	read := func(key string) (string, bool) {
		if lookupEnv == nil {
			return "", false
		}
		value, ok := lookupEnv(key)
		if ok {
			configured = true
		}
		return value, ok
	}
	baseURL := DefaultOmniRouteBaseURL
	if value, ok := read(EnvOmniRouteBaseURL); ok {
		baseURL = value
	}
	managementBaseURL := ""
	if value, ok := read(EnvOmniRouteManagementBaseURL); ok {
		managementBaseURL = value
	}
	apiKey := ""
	if value, ok := read(EnvOmniRouteAPIKey); ok {
		apiKey = value
	}
	model := DefaultOmniRouteModel
	if value, ok := read(EnvOmniRouteModel); ok {
		model = value
	}
	chatEndpoint := ""
	if value, ok := read(EnvOmniRouteChatEndpoint); ok {
		chatEndpoint = value
	}
	timeout := 30 * time.Second
	if value, ok := read(EnvOmniRouteTimeout); ok {
		parsed, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid OmniRoute timeout")
		}
		timeout = parsed
	}
	maxRequestBytes := 1 << 20
	if value, ok := read(EnvOmniRouteMaxRequestBytes); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid OmniRoute request body limit")
		}
		maxRequestBytes = parsed
	}
	maxResponseBytes := 1 << 20
	if value, ok := read(EnvOmniRouteMaxResponseBytes); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid OmniRoute response body limit")
		}
		maxResponseBytes = parsed
	}

	if overrides.BaseURLSet {
		baseURL = overrides.BaseURL
	}
	if overrides.ManagementBaseURLSet {
		managementBaseURL = overrides.ManagementBaseURL
	}
	if overrides.APIKeySet {
		apiKey = overrides.APIKey
	}
	if overrides.ModelSet {
		model = overrides.Model
	}
	if overrides.ChatEndpointSet {
		chatEndpoint = overrides.ChatEndpoint
	}
	if overrides.TimeoutSet {
		timeout = overrides.Timeout
	}
	if overrides.MaxRequestBytesSet {
		maxRequestBytes = overrides.MaxRequestBytes
	}
	if overrides.MaxResponseBytesSet {
		maxResponseBytes = overrides.MaxResponseBytes
	}

	safety, safetyConfigured, err := resolveRouteSafety(overrides, read)
	if err != nil {
		return nil, err
	}
	configured = configured || safetyConfigured
	if !configured {
		return nil, nil
	}
	if !overrides.RouteSafetySet && !safetyConfigured {
		return nil, fmt.Errorf("OmniRoute route safety must be explicitly configured")
	}
	resolved := &omniroute.Config{
		BaseURL:           baseURL,
		ManagementBaseURL: managementBaseURL,
		APIKey:            apiKey,
		Model:             model,
		ChatEndpoint:      chatEndpoint,
		Timeout:           timeout,
		MaxRequestBytes:   maxRequestBytes,
		MaxResponseBytes:  maxResponseBytes,
		RouteSafety:       safety,
	}
	if _, err := omniroute.New(*resolved, omniroute.Options{}); err != nil {
		return nil, fmt.Errorf("invalid OmniRoute configuration")
	}
	return resolved, nil
}

func resolveRouteSafety(overrides OmniRouteOverrides, read func(string) (string, bool)) (provider.RouteSafety, bool, error) {
	if overrides.RouteSafetySet {
		return overrides.RouteSafety, true, nil
	}
	safety := provider.RouteSafety{}
	configured := false
	setSingle := func(key string) error {
		value, ok := read(key)
		if !ok {
			return nil
		}
		configured = true
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("invalid OmniRoute route safety value")
		}
		if parsed {
			safety.AttemptAccounting = provider.AttemptAccountingSingle
			safety.SingleAttempt = provider.SingleAttemptGuaranteed
		}
		return nil
	}
	if err := setSingle(EnvOmniRouteSingleAttemptGuaranteed); err != nil {
		return provider.RouteSafety{}, configured, err
	}
	setDisabled := func(key string, target *provider.AmplificationStatus) error {
		value, ok := read(key)
		if !ok {
			return nil
		}
		configured = true
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("invalid OmniRoute route safety value")
		}
		if parsed {
			*target = provider.AmplificationDisabled
		} else {
			*target = provider.AmplificationEnabled
		}
		return nil
	}
	if err := setDisabled(EnvOmniRouteInternalRetriesDisabled, &safety.InternalRetries); err != nil {
		return provider.RouteSafety{}, configured, err
	}
	if err := setDisabled(EnvOmniRouteCooldownReplayDisabled, &safety.CooldownReplay); err != nil {
		return provider.RouteSafety{}, configured, err
	}
	if err := setDisabled(EnvOmniRouteAccountPoolingDisabled, &safety.AccountPooling); err != nil {
		return provider.RouteSafety{}, configured, err
	}
	if err := setDisabled(EnvOmniRouteFallbackDisabled, &safety.AutomaticFallback); err != nil {
		return provider.RouteSafety{}, configured, err
	}
	if err := setDisabled(EnvOmniRouteComboRoutingDisabled, &safety.ComboRouting); err != nil {
		return provider.RouteSafety{}, configured, err
	}
	return safety, configured, nil
}

func validLogLevel(value string) bool {
	switch value {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
