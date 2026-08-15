package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestResolveConfigurationPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		overrides Overrides
		env       map[string]string
		want      Config
	}{
		{
			name: "defaults",
			want: Config{Workspace: DefaultWorkspace, LogLevel: DefaultLogLevel},
		},
		{
			name: "environment overrides defaults",
			env: map[string]string{
				EnvWorkspace: "/tmp/from-env",
				EnvLogLevel:  "debug",
			},
			want: Config{Workspace: "/tmp/from-env", LogLevel: "debug"},
		},
		{
			name: "flags override environment",
			overrides: Overrides{
				Workspace:    "/tmp/from-flag",
				WorkspaceSet: true,
				LogLevel:     "error",
				LogLevelSet:  true,
			},
			env: map[string]string{
				EnvWorkspace: "/tmp/from-env",
				EnvLogLevel:  "debug",
			},
			want: Config{Workspace: "/tmp/from-flag", LogLevel: "error"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.overrides, func(key string) (string, bool) {
				value, ok := tt.env[key]
				return value, ok
			})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolveValidatesOptionalAccountPolicy(t *testing.T) {
	valid := governor.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	got, err := Resolve(Overrides{AccountPolicy: &valid, AccountPolicySet: true}, nil)
	if err != nil {
		t.Fatalf("Resolve(valid policy) error = %v", err)
	}
	if got.AccountPolicy == nil || got.AccountPolicy.AccountPolicyID != "policy-account-1" {
		t.Fatalf("resolved account policy = %#v", got.AccountPolicy)
	}
	got.AccountPolicy.MinimumStartInterval = time.Second
	if valid.MinimumStartInterval == time.Second {
		t.Fatal("Resolve() retained caller-owned policy pointer")
	}

	invalid := valid
	invalid.MaxInFlight = 2
	if _, err := Resolve(Overrides{AccountPolicy: &invalid, AccountPolicySet: true}, nil); err == nil {
		t.Fatal("Resolve(invalid policy) error = nil")
	}
	if _, err := Resolve(Overrides{AccountPolicySet: true}, nil); err == nil {
		t.Fatal("Resolve(nil policy) error = nil")
	}
}

func TestResolveRejectsUnsupportedLogLevel(t *testing.T) {
	_, err := Resolve(Overrides{}, func(key string) (string, bool) {
		if key == EnvLogLevel {
			return "trace", true
		}
		return "", false
	})

	if err == nil {
		t.Fatal("Resolve() error = nil, want unsupported log level error")
	}
}

func TestResolveOmniRouteConfigurationPrecedenceAndSafety(t *testing.T) {
	env := map[string]string{
		EnvOmniRouteBaseURL:                 "http://env.example/v1",
		EnvOmniRouteAPIKey:                  "env-secret",
		EnvOmniRouteModel:                   "env-model",
		EnvOmniRouteSingleAttemptGuaranteed: "true",
		EnvOmniRouteInternalRetriesDisabled: "true",
		EnvOmniRouteCooldownReplayDisabled:  "true",
		EnvOmniRouteAccountPoolingDisabled:  "true",
		EnvOmniRouteFallbackDisabled:        "true",
		EnvOmniRouteComboRoutingDisabled:    "true",
	}
	got, err := Resolve(Overrides{OmniRoute: OmniRouteOverrides{
		BaseURL:    "http://flag.example/v1",
		BaseURLSet: true,
		Model:      "flag-model",
		ModelSet:   true,
	}}, func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.OmniRoute == nil {
		t.Fatal("Resolve() OmniRoute = nil")
	}
	if got.OmniRoute.BaseURL != "http://flag.example/v1" || got.OmniRoute.Model != "flag-model" || got.OmniRoute.APIKey != "env-secret" {
		t.Fatalf("resolved OmniRoute = %#v, want flags over env and env key", got.OmniRoute)
	}
	if err := got.OmniRoute.RouteSafety.Validate(); err != nil {
		t.Fatalf("resolved route safety = %#v: %v", got.OmniRoute.RouteSafety, err)
	}
	if strings.Contains(got.OmniRoute.String(), "env-secret") {
		t.Fatal("OmniRoute config String() leaked API key")
	}
}

func TestResolveOmniRouteRequiresExplicitSafety(t *testing.T) {
	_, err := Resolve(Overrides{}, func(key string) (string, bool) {
		if key == EnvOmniRouteAPIKey {
			return "secret", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "route safety") {
		t.Fatalf("Resolve() error = %v, want explicit route safety failure", err)
	}
}

func TestResolveOmniRouteDoesNotLogCredentialsOnValidationFailure(t *testing.T) {
	_, err := Resolve(Overrides{OmniRoute: OmniRouteOverrides{
		APIKey:    "secret-value",
		APIKeySet: true,
	}}, nil)
	if err == nil {
		t.Fatal("Resolve() error = nil, want missing safety failure")
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("Resolve() error leaked API key: %v", err)
	}
}

func resolveWithEnv(t *testing.T, env map[string]string, overrides Overrides) (Config, error) {
	t.Helper()
	return Resolve(overrides, func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	})
}

func TestResolveOmniRoutePinnedLaneActivatesReceipts(t *testing.T) {
	env := map[string]string{
		EnvOmniRouteBaseURL:      "http://omniroute.test/v1",
		EnvOmniRouteAPIKey:       "secret",
		EnvOmniRouteModel:        "chatgpt-web/gpt-5",
		EnvOmniRouteConnectionID: "conn-test-123",
	}
	got, err := resolveWithEnv(t, env, Overrides{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	config := got.OmniRoute
	if config == nil {
		t.Fatal("Resolve() OmniRoute = nil")
	}
	if !config.EnableAttemptReceipts {
		t.Fatal("pinned lane must enable attempt receipts")
	}
	if config.Provider != "chatgpt-web" {
		t.Fatalf("pinned lane provider = %q, want chatgpt-web", config.Provider)
	}
	if config.ChatEndpoint != "providers/chatgpt-web/chat/completions" {
		t.Fatalf("pinned lane endpoint = %q, want dedicated provider-scoped route", config.ChatEndpoint)
	}
	if config.AccountLaneHash != "ebae45b2394081da729b4006e58d00145162145bfae0bd2db50de6661961259f" {
		t.Fatalf("derived account lane hash = %q, want golden vector", config.AccountLaneHash)
	}
	if config.RouteSafety.AttemptAccounting != provider.AttemptAccountingReceipts {
		t.Fatalf("pinned lane route safety = %#v, want receipt-aware", config.RouteSafety)
	}
}

func TestResolveOmniRoutePinnedLaneFlagOverridesEnv(t *testing.T) {
	env := map[string]string{EnvOmniRouteConnectionID: "env-connection"}
	got, err := resolveWithEnv(t, env, Overrides{OmniRoute: OmniRouteOverrides{
		BaseURL:         "http://omniroute.test/v1",
		BaseURLSet:      true,
		APIKey:          "secret",
		APIKeySet:       true,
		Model:           "chatgpt-web/gpt-5",
		ModelSet:        true,
		ConnectionID:    "flag-connection",
		ConnectionIDSet: true,
	}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.OmniRoute == nil || got.OmniRoute.ConnectionID != "flag-connection" {
		t.Fatalf("flag must override env connection id: %#v", got.OmniRoute)
	}
	if got.OmniRoute.AccountLaneHash == "" {
		t.Fatal("pinned lane must derive an account lane hash")
	}
}

func TestResolveOmniRoutePinnedLaneRejectsEmptyConnectionID(t *testing.T) {
	_, err := resolveWithEnv(t, map[string]string{}, Overrides{OmniRoute: OmniRouteOverrides{
		BaseURL:         "http://omniroute.test/v1",
		BaseURLSet:      true,
		APIKey:          "secret",
		APIKeySet:       true,
		Model:           "chatgpt-web/gpt-5",
		ModelSet:        true,
		ConnectionID:    "",
		ConnectionIDSet: true,
	}})
	if err == nil || !strings.Contains(err.Error(), "connection id must not be empty") {
		t.Fatalf("Resolve() error = %v, want empty connection id failure", err)
	}
}

func TestResolveOmniRoutePinnedLaneRejectsSafeRouteDeclaration(t *testing.T) {
	_, err := resolveWithEnv(t, map[string]string{
		EnvOmniRouteConnectionID:            "conn-test-123",
		EnvOmniRouteSingleAttemptGuaranteed: "true",
		EnvOmniRouteInternalRetriesDisabled: "true",
		EnvOmniRouteCooldownReplayDisabled:  "true",
		EnvOmniRouteAccountPoolingDisabled:  "true",
		EnvOmniRouteFallbackDisabled:        "true",
		EnvOmniRouteComboRoutingDisabled:    "true",
		EnvOmniRouteBaseURL:                 "http://omniroute.test/v1",
		EnvOmniRouteAPIKey:                  "secret",
		EnvOmniRouteModel:                   "chatgpt-web/gpt-5",
	}, Overrides{})
	if err == nil || !strings.Contains(err.Error(), "receipt-aware route safety") {
		t.Fatalf("Resolve() error = %v, want legacy safe-route rejection on the pinned lane", err)
	}
}

func TestResolveOmniRoutePinnedLaneRejectsArbitraryEndpoint(t *testing.T) {
	_, err := resolveWithEnv(t, map[string]string{
		EnvOmniRouteConnectionID: "conn-test-123",
		EnvOmniRouteBaseURL:      "http://omniroute.test/v1",
		EnvOmniRouteAPIKey:       "secret",
		EnvOmniRouteModel:        "chatgpt-web/gpt-5",
		EnvOmniRouteChatEndpoint: "chat/completions",
	}, Overrides{})
	if err == nil || !strings.Contains(err.Error(), "dedicated providers/chatgpt-web/chat/completions") {
		t.Fatalf("Resolve() error = %v, want arbitrary endpoint rejection", err)
	}
}

func TestResolveOmniRoutePinnedLaneRejectsNonChatGPTWebModel(t *testing.T) {
	_, err := resolveWithEnv(t, map[string]string{
		EnvOmniRouteConnectionID: "conn-test-123",
		EnvOmniRouteBaseURL:      "http://omniroute.test/v1",
		EnvOmniRouteAPIKey:       "secret",
		EnvOmniRouteModel:        "openai/gpt-5",
	}, Overrides{})
	if err == nil || !strings.Contains(err.Error(), "chatgpt-web/<model>") {
		t.Fatalf("Resolve() error = %v, want explicit chatgpt-web model requirement", err)
	}
}

func TestResolveOmniRoutePinnedLaneRedactsConnectionID(t *testing.T) {
	env := map[string]string{
		EnvOmniRouteBaseURL:      "http://omniroute.test/v1",
		EnvOmniRouteAPIKey:       "secret",
		EnvOmniRouteModel:        "chatgpt-web/gpt-5",
		EnvOmniRouteConnectionID: "conn-private-77",
	}
	got, err := resolveWithEnv(t, env, Overrides{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if strings.Contains(got.OmniRoute.String(), "conn-private-77") {
		t.Fatalf("config String() leaked connection id: %s", got.OmniRoute.String())
	}
	if strings.Contains(got.OmniRoute.String(), "secret") {
		t.Fatalf("config String() leaked API key: %s", got.OmniRoute.String())
	}
}
