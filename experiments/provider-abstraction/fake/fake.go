// Package fake provides a test implementation of ProviderClient for research.
// This is a standalone experiment — NOT wired into production tests.
package fake

import (
	"context"
	"errors"
	"sync"

	provider "experiments/provider-abstraction/provider"
)

var ErrNoPredefinedResponse = errors.New("fake provider: no predefined response")

// Fake implements provider.ProviderClient for testing.
type Fake struct {
	mu          sync.Mutex
	responses   []provider.ProviderResponse
	err         error
	blocking    bool
	attempts    int
	requests    []provider.ProviderRequest
	healthCheck provider.HealthResult
	models      []provider.ModelInfo
	name        string
}

func NewFake(responses ...provider.ProviderResponse) *Fake {
	return &Fake{responses: append([]provider.ProviderResponse(nil), responses...)}
}

func NewErrorFake(err error) *Fake {
	return &Fake{err: err}
}

func NewBlockingFake() *Fake {
	return &Fake{blocking: true}
}

func (f *Fake) WithHealthCheck(h provider.HealthResult) *Fake {
	f.healthCheck = h
	return f
}

func (f *Fake) WithModels(models []provider.ModelInfo) *Fake {
	f.models = models
	return f
}

func (f *Fake) WithName(name string) *Fake {
	f.name = name
	return f
}

// Complete implements provider.ProviderClient.
func (f *Fake) Complete(ctx context.Context, req provider.ProviderRequest) (provider.ProviderResponse, error) {
	f.mu.Lock()
	f.attempts++
	f.requests = append(f.requests, req)
	blocking := f.blocking
	err := f.err
	response := provider.ProviderResponse{}
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
		return provider.ProviderResponse{}, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return provider.ProviderResponse{}, ctx.Err()
	default:
	}
	if err != nil {
		return provider.ProviderResponse{}, err
	}
	return response, nil
}

// HealthCheck implements provider.ProviderClient.
func (f *Fake) HealthCheck(ctx context.Context) (provider.HealthResult, error) {
	if f.healthCheck.Healthy || f.healthCheck.Reason != "" {
		return f.healthCheck, nil
	}
	return provider.HealthResult{Healthy: true, Reason: ""}, nil
}

// Models implements provider.ProviderClient.
func (f *Fake) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	if f.models != nil {
		return f.models, nil
	}
	return []provider.ModelInfo{
		{ID: "test-model", DisplayName: "Test Model", ContextWindow: 4096},
	}, nil
}

// Name implements provider.ProviderClient.
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

func (f *Fake) Requests() []provider.ProviderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.ProviderRequest(nil), f.requests...)
}

// Compile-time interface assertion
var _ provider.ProviderClient = (*Fake)(nil)
