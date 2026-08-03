package omniroute_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
)

func TestLiveOmniRouteRejectsUnprovenSingleAttemptRoute(t *testing.T) {
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
		t.Fatal("live OmniRoute exposed no verified single-attempt contract but preflight passed")
	} else {
		var providerErr *omniroute.Error
		if !errors.As(err, &providerErr) {
			t.Fatalf("live OmniRoute preflight returned an unexpected error type: %T", err)
		}
		t.Logf("live OmniRoute refused before model execution: %s", providerErr)
	}
}
