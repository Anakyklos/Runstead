// Package fake provides a test implementation of ProviderClient for research.
// This is a standalone experiment — NOT wired into production tests.
package fake

import (
	"context"
	"errors"
	"sync"
	"time"

	client "github.com/RenyEnnos/Runstead/experiments/provider-abstraction/client"
)

var ErrNoPredefinedResponse = errors.New("fake provider: no predefined response")

// Fake implements client.ProviderClient for testing.
type Fake struct {
	mu          sync.Mutex
	responses   []client.ProviderResponse
	err         error
	blocking    bool
	attempts    int
	requests    []client.ProviderRequest
	healthCheck client.HealthResult
	models      []client.ModelInfo
	name        string
}

func NewFake(responses ...client.ProviderResponse) *Fake {
	return &Fake{responses: append([]client.ProviderResponse(nil), responses...)}
}

func NewErrorFake(err error) *Fake {
	return &Fake{err: err}
}

func NewBlockingFake() *Fake {
	return &Fake{blocking: true}
}

func (f *Fake) WithHealthCheck(h client.HealthResult) *Fake {
	f.healthCheck = h
	return f
}

func (f *Fake) WithModels(models []client.ModelInfo) *Fake {
	f.models = models
	return f
}

func (f *Fake) WithName(name string) *Fake {
	f.name = name
	return f
}

// Complete implements client.ProviderClient.
func (f *Fake) Complete(ctx context.Context, req client.ProviderRequest) (client.ProviderResponse, error) {
	f.mu.Lock()
	f.attempts++
	f.requests = append(f.requests, req)
	blocking := f.blocking
	err := f.err
	response := client.ProviderResponse{}
	if !blocking && err == nil {
		if len(f.responses) == 0 {
			err = ErrNoPredefinedResponse
		} else {
			response = f.responses[0]
			f.responses = f.responses[1:]
		}
	}
	f.mu.Unlock()

	if blocking {
		<-ctx.Done()
		return client.ProviderResponse{}, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return client.ProviderResponse{}, ctx.Err()
	default:
	}
	if err != nil {
		return client.ProviderResponse{}, err
	}
	return response, nil
}

// HealthCheck implements client.ProviderClient.
func (f *Fake) HealthCheck(ctx context.Context) (client.HealthResult, error) {
	if f.healthCheck.Healthy || f.healthCheck.Reason != "" {
		return f.healthCheck, nil
	}
	return client.HealthResult{Healthy: true, Reason: ""}, nil
}

// Models implements client.ProviderClient.
func (f *Fake) Models(ctx context.Context) ([]client.ModelInfo, error) {
	if f.models != nil {
		return f.models, nil
	}
	return []client.ModelInfo{
		{ID: "test-model", DisplayName: "Test Model", ContextWindow: 4096},
	}, nil
}

// Name implements client.ProviderClient.
func (f *Fake) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}

func (f *Fake) Attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *Fake) Requests() []client.ProviderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.ProviderRequest(nil), f.requests...)
}