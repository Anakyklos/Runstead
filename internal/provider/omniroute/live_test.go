package omniroute_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
)

// TestLiveOmniRouteRemainsDisabledUntilAttemptReceipts keeps the historical
// fail-closed check: without the pinned connection id the live lane stays
// refused before any model execution.
func TestLiveOmniRouteRemainsDisabledUntilAttemptReceipts(t *testing.T) {
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
	if err := client.Preflight(context.Background()); err == nil {
		t.Fatal("live OmniRoute has no authoritative attempt receipts but preflight passed")
	} else {
		var providerErr *omniroute.Error
		if !errors.As(err, &providerErr) {
			t.Fatalf("live OmniRoute preflight returned an unexpected error type: %T", err)
		}
		t.Logf("live OmniRoute refused before model execution: %s", providerErr)
	}
}

// TestLiveOmniRoutePinnedChatGPTWebLane is the opt-in live acceptance test for
// the pinned M1 lane (issue #30). It is disabled by default and requires an
// OmniRoute build of the documented compatible fork pin:
//
//	pedro-labsabs/OmniRoute@dbfb2c879aba3770a5dd1e195f0529bb236000dd
//
// Required environment:
//
//	RUNSTEAD_LIVE_OMNIROUTE=1
//	OMNIROUTE_BASE_URL           (e.g. http://127.0.0.1:20128/v1)
//	OMNIROUTE_MANAGEMENT_BASE_URL (optional; defaults from the base URL)
//	OMNIROUTE_API_KEY
//	OMNIROUTE_MODEL               (explicit chatgpt-web/<model>)
//	OMNIROUTE_CONNECTION_ID
//	RUNSTEAD_ALLOWANCE_PROFILE    (optional; default plus_go_instant)
//
// No secret is read from files or fixtures. The test proves: one Runstead
// admission, one physical upstream model attempt receipt, one governor debit,
// identical provider/model/lane, raw model text, and no credentials or private
// body persisted as evidence. It never deliberately drives the account into
// rate limits or restrictions.
func TestLiveOmniRoutePinnedChatGPTWebLane(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_OMNIROUTE") != "1" {
		t.Skip("set RUNSTEAD_LIVE_OMNIROUTE=1 to enable the live OmniRoute check")
	}
	connectionID := strings.TrimSpace(os.Getenv(config.EnvOmniRouteConnectionID))
	if connectionID == "" {
		t.Fatalf("%s is required for the live pinned lane", config.EnvOmniRouteConnectionID)
	}
	if strings.TrimSpace(os.Getenv(config.EnvOmniRouteBaseURL)) == "" {
		t.Fatalf("%s is required for the live pinned lane", config.EnvOmniRouteBaseURL)
	}
	if strings.TrimSpace(os.Getenv(config.EnvOmniRouteAPIKey)) == "" {
		t.Fatalf("%s is required for the live pinned lane", config.EnvOmniRouteAPIKey)
	}
	model := strings.TrimSpace(os.Getenv(config.EnvOmniRouteModel))
	if !strings.HasPrefix(model, "chatgpt-web/") {
		t.Fatalf("%s must be an explicit chatgpt-web/<model> for the live pinned lane", config.EnvOmniRouteModel)
	}

	resolved, err := config.Resolve(config.Overrides{}, os.LookupEnv)
	if err != nil || resolved.OmniRoute == nil {
		t.Fatalf("live pinned lane configuration failed: %v", err)
	}
	if !resolved.OmniRoute.EnableAttemptReceipts {
		t.Fatal("live pinned lane must enable attempt receipts")
	}
	client, err := omniroute.New(*resolved.OmniRoute, omniroute.Options{})
	if err != nil {
		t.Fatalf("live pinned lane client configuration failed: %v", err)
	}
	ctx := context.Background()
	health := client.ProbeGatewayContract(ctx)
	if !health.Healthy() {
		t.Fatalf("live OmniRoute gateway contract is not healthy: %s (%s)", health.State, health.ReasonCode)
	}
	if err := client.Preflight(ctx); err != nil {
		t.Fatalf("live OmniRoute preflight failed: %v", err)
	}

	safety := provider.ReceiptRouteSafety()
	accountConfig := governor.DefaultInstantConfig("runstead-live", "omniroute", "instant", safety)
	accountConfig.Model = model
	accountConfig.RequireSingleAttempt = false
	accountConfig.RequireAttemptReceipts = true
	accountConfig.AccountLaneHash = resolved.OmniRoute.AccountLaneHash
	accountConfig.AttemptProviderID = resolved.OmniRoute.Provider
	if err := accountConfig.Validate(); err != nil {
		t.Fatalf("live pinned lane governor config invalid: %v", err)
	}
	accountGovernor, err := governor.New(accountConfig, governor.Options{})
	if err != nil {
		t.Fatalf("live pinned lane governor unavailable: %v", err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatalf("live pinned lane executor unavailable: %v", err)
	}

	clientRequestID := "live-" + time.Now().UTC().Format("20060102T150405Z")
	result := executor.Execute(ctx, governor.AttemptRequest{
		TaskID:          "live-m1-acceptance",
		ClientRequestID: clientRequestID,
		ProviderRequest: provider.Request{
			Prompt:          "Reply with the single word: ok",
			Model:           model,
			ClientRequestID: clientRequestID,
		},
	})
	if !result.Admission.Admitted() {
		t.Fatalf("live attempt was not admitted: %s (%v)", result.Admission.Code, result.Admission.Err)
	}
	if result.Completion.AttemptDebited != 1 {
		t.Fatalf("governor debits = %d, want exactly 1", result.Completion.AttemptDebited)
	}
	if result.Response.Metadata.AttemptReceipts == nil || len(result.Response.Metadata.AttemptReceipts.Receipts) != 1 {
		t.Fatalf("live attempt receipts = %#v, want exactly one validated receipt", result.Response.Metadata.AttemptReceipts)
	}
	receipt := result.Response.Metadata.AttemptReceipts.Receipts[0]
	if receipt.Provider != resolved.OmniRoute.Provider || receipt.Model != model {
		t.Fatalf("live receipt provider/model = %q/%q, want %q/%q", receipt.Provider, receipt.Model, resolved.OmniRoute.Provider, model)
	}
	if receipt.AccountLaneHash != resolved.OmniRoute.AccountLaneHash {
		t.Fatalf("live receipt lane hash = %q, want derived hash %q", receipt.AccountLaneHash, resolved.OmniRoute.AccountLaneHash)
	}
	if strings.TrimSpace(result.Response.Text) == "" {
		t.Fatal("live attempt returned no model text")
	}
	if result.Err != nil {
		t.Fatalf("live attempt returned error: %v", result.Err)
	}
	t.Logf("live pinned lane OK: admission=1 debit=1 provider=%s model=%s text=%d chars", receipt.Provider, receipt.Model, len(result.Response.Text))
}
