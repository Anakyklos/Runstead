package provider

import (
	"context"
	"errors"
	"sync"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

var ErrNoPredefinedResponse = errors.New("fake provider: no predefined response")

type Fake struct {
	mu        sync.Mutex
	responses []ProviderResponse
	legacyResponses []Response  // For backward compatibility with tests
	err       error
	blocking  bool
	attempts  int
	requests  []ProviderRequest
}

func (f *Fake) RouteSafety() RouteSafety { return SafeRouteSafety() }

func NewFake(responses ...ProviderResponse) *Fake {
	return &Fake{responses: append([]ProviderResponse(nil), responses...)}
}

func NewFakeLegacy(responses ...Response) *Fake {
	return &Fake{legacyResponses: append([]Response(nil), responses...)}
}

func NewErrorFake(err error) *Fake {
	return &Fake{err: err}
}

func NewBlockingFake() *Fake {
	return &Fake{blocking: true}
}

// Complete implements the legacy provider.LegacyClient interface.
func (f *Fake) Complete(ctx context.Context, req Request) (Response, error) {
	f.mu.Lock()
	f.attempts++
	f.requests = append(f.requests, ProviderRequest{
		ClientRequestID: req.ClientRequestID,
		Model:           req.Model,
		Messages: []Message{
			{Role: "system", Content: req.Prompt},
		},
		Stream: false,
	})
	blocking := f.blocking
	err := f.err
	
	var response Response
	if !blocking && err == nil {
		if len(f.legacyResponses) > 0 {
			response = f.legacyResponses[0]
			f.legacyResponses = f.legacyResponses[1:]
		} else if len(f.responses) > 0 {
			// Convert new response to legacy
			newResp := f.responses[0]
			f.responses = f.responses[1:]
			response = Response{
				Text: newResp.Content,
				Metadata: ResponseMetadata{
					StatusCode:      newResp.Metadata.StatusCode,
					RequestID:       newResp.Metadata.RequestID,
					SessionID:       newResp.Metadata.SessionID,
					Duration:        newResp.Metadata.Duration,
					RetryAfter:      newResp.Metadata.RetryAfter,
					ResetAt:         newResp.Metadata.ResetAt,
					Endpoint:        newResp.Metadata.Endpoint,
					Model:           newResp.Metadata.Model,
					DeliveryState:   DeliveryCompleted, // default for non-streaming
					AttemptReceipts: nil,
				},
			}
		} else {
			err = ErrNoPredefinedResponse
		}
	}
	f.mu.Unlock()

	if blocking {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	default:
	}
	if err != nil {
		return Response{}, err
	}

	return response, nil
}

// CompleteProvider implements the new provider.ProviderClient interface for tests.
func (f *Fake) CompleteProvider(ctx context.Context, request ProviderRequest) (ProviderResponse, error) {
	f.mu.Lock()
	f.attempts++
	f.requests = append(f.requests, request)
	blocking := f.blocking
	err := f.err
	response := ProviderResponse{}
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
		return ProviderResponse{}, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ProviderResponse{}, ctx.Err()
	default:
	}
	if err != nil {
		return ProviderResponse{}, err
	}
	return response, nil
}

func (f *Fake) Attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *Fake) Requests() []ProviderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ProviderRequest(nil), f.requests...)
}

func (f *Fake) LegacyRequests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	legacy := make([]Request, len(f.requests))
	for i, r := range f.requests {
		legacy[i] = Request{
			ClientRequestID: r.ClientRequestID,
			Model:           r.Model,
			Prompt:          "",
			Protocol:        protocol.Current,
		}
		if len(r.Messages) > 0 {
			legacy[i].Prompt = r.Messages[0].Content
		}
	}
	return legacy
}