package omniroute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestAdapterRejectsProtectedExecutionUntilAuthoritativeAttemptReceipts(t *testing.T) {
	var posts atomic.Int32
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
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
	if result.Admission.Admitted() || result.Admission.Code != governor.AdmissionUnsafeProviderAmplification || !errors.Is(result.Admission.Err, provider.ErrUnsafeRoute) {
		t.Fatalf("Executor.Execute() = %#v, want fail-closed admission", result)
	}
	if posts.Load() != 0 || accountGovernor.Snapshot().Budgets.Rolling3hUsed != 0 {
		t.Fatalf("blocked accounting = posts %d, debits %d; want zero each", posts.Load(), accountGovernor.Snapshot().Budgets.Rolling3hUsed)
	}
}
