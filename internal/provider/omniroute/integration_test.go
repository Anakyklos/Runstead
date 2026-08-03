package omniroute

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestAdapterUsesGovernorAndExecutorForOneDebitedAttempt(t *testing.T) {
	var posts atomic.Int32
	client, server := newReadyClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"governed response"}}]}`)
	}))
	defer server.Close()
	policy := governor.DefaultInstantConfig("test-account", "omniroute", "chatgpt-web/model", provider.SafeRouteSafety())
	accountGovernor, err := governor.New(policy, governor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, Classify)
	if err != nil {
		t.Fatal(err)
	}
	result := executor.Execute(context.Background(), governor.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "chatgpt-web/model",
		ProviderRequest: provider.Request{Prompt: "prompt"},
	})
	if result.Err != nil || !result.Admission.Admitted() || result.Response.Text != "governed response" {
		t.Fatalf("Executor.Execute() = %#v", result)
	}
	if posts.Load() != 1 || accountGovernor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("governed accounting = posts %d, debits %d; want one each", posts.Load(), accountGovernor.Snapshot().Budgets.Rolling3hUsed)
	}
}
