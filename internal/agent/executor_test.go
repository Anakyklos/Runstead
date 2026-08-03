package agent

import (
	"context"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

var _ interface {
	Execute(context.Context, governor.AttemptRequest) governor.ExecutionResult
} = (*Executor)(nil)

func TestExecutorUsesGovernorAdmissionBoundary(t *testing.T) {
	config := governor.DefaultInstantConfig("policy-account-1", "fake", "instant", provider.SafeRouteSafety())
	accountGovernor, err := governor.New(config, governor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	client := provider.NewFake(provider.Response{Text: "response"})
	executor, err := NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}

	result := executor.Execute(context.Background(), governor.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ProviderRequest: provider.Request{Prompt: "private prompt"},
	})
	if !result.Admission.Admitted() || result.Err != nil {
		t.Fatalf("Executor.Execute() = %#v", result)
	}
	if client.Attempts() != 1 || accountGovernor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("execution accounting = provider %d, governor %d; want one each", client.Attempts(), accountGovernor.Snapshot().Budgets.Rolling3hUsed)
	}
}
