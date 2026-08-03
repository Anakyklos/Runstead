package omniroute_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
)

func TestLiveOmniRouteThroughExecutor(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_OMNIROUTE") != "1" {
		t.Skip("set RUNSTEAD_LIVE_OMNIROUTE=1 to enable the live OmniRoute check")
	}
	resolved, err := config.Resolve(config.Overrides{}, os.LookupEnv)
	if err != nil || resolved.OmniRoute == nil {
		t.Fatalf("live OmniRoute configuration is not safe or complete")
	}
	client, err := omniroute.New(*resolved.OmniRoute, omniroute.Options{})
	if err != nil {
		t.Fatalf("live OmniRoute client configuration failed")
	}
	if err := client.Preflight(context.Background()); err != nil {
		t.Fatalf("live OmniRoute safety preflight failed; refusing a model request")
	}
	policy := governor.DefaultInstantConfig("live-omniroute", "omniroute", resolved.OmniRoute.Model, provider.SafeRouteSafety())
	accountGovernor, err := governor.New(policy, governor.Options{Telemetry: client})
	if err != nil {
		t.Fatalf("live governor configuration failed")
	}
	executor, err := agent.NewExecutor(accountGovernor, client, omniroute.Classify)
	if err != nil {
		t.Fatalf("live executor configuration failed")
	}
	result := executor.Execute(context.Background(), governor.AttemptRequest{
		TaskID:          "live-omniroute-task",
		ClientRequestID: "live-omniroute-request",
		ModelPool:       resolved.OmniRoute.Model,
		ProviderRequest: provider.Request{Prompt: "Reply with one short confirmation."},
	})
	if result.Err != nil || !result.Admission.Admitted() || result.Completion.AttemptDebited != 1 {
		t.Fatalf("live OmniRoute execution did not complete one governed attempt")
	}
	if strings.TrimSpace(result.Response.Text) == "" {
		t.Fatalf("live OmniRoute returned empty text")
	}
	if snapshot := accountGovernor.Snapshot(); snapshot.Budgets.Rolling3hUsed != 1 {
		t.Fatalf("live governor debit = %d, want one", snapshot.Budgets.Rolling3hUsed)
	}
}
