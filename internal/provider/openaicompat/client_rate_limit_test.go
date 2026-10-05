package openaicompat_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestCompletePreservesRateLimitObservationsOnSuccessAnd429(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "29")
				w.Header().Set("X-Ratelimit-Limit-Requests", "120")
				w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
				w.Header().Set("X-Ratelimit-Reset-Requests", "1m")
				w.Header().Set("X-Ratelimit-Limit-Tokens", "9000")
				w.Header().Set("X-Ratelimit-Remaining-Tokens", "17")
				w.Header().Set("X-Ratelimit-Reset-Tokens", "2h")
				w.Header().Set("X-Provider-Secret", "secret-header-value")
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(validCompletionBody))
				} else {
					_, _ = w.Write([]byte(`{"error":{"message":"secret-body-value"}}`))
				}
			})
			client, _ := newTestClient(t, nil, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
			if status == http.StatusOK && err != nil {
				t.Fatalf("success response failed: %v", err)
			}
			if status == http.StatusTooManyRequests && err == nil {
				t.Fatal("429 response unexpectedly succeeded")
			}
			metadata := response.Metadata
			observation := metadata.RateLimitObservation
			if metadata.StatusCode != status || metadata.RetryAfter != 29*time.Second || observation.ObservedRetryAfter != 29*time.Second || observation.RemainingRequests == nil || *observation.RemainingRequests != 0 || observation.LimitRequests == nil || *observation.LimitRequests != 120 || observation.LimitTokens == nil || *observation.LimitTokens != 9000 || observation.RemainingTokens == nil || *observation.RemainingTokens != 17 || observation.ResetRequests != time.Minute || observation.ResetTokens != 2*time.Hour {
				t.Fatalf("status or rate-limit observation missing: metadata=%+v", metadata)
			}
			if strings.Contains(fmt.Sprintf("%+v", metadata), "secret-header-value") || strings.Contains(fmt.Sprintf("%+v", metadata), "secret-body-value") {
				t.Fatal("raw header/body text survived in provider metadata")
			}
		})
	}
}
