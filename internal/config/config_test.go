package config

import (
	"reflect"
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
