package provider

import (
	"context"
	"errors"
	"sync"
)

var ErrNoPredefinedResponse = errors.New("fake provider: no predefined response")

type Fake struct {
	mu        sync.Mutex
	responses []Response
	err       error
	blocking  bool
	attempts  int
	requests  []Request
}

func NewFake(responses ...Response) *Fake {
	return &Fake{responses: append([]Response(nil), responses...)}
}

func NewErrorFake(err error) *Fake {
	return &Fake{err: err}
}

func NewBlockingFake() *Fake {
	return &Fake{blocking: true}
}

func (f *Fake) Complete(ctx context.Context, request Request) (Response, error) {
	f.mu.Lock()
	f.attempts++
	f.requests = append(f.requests, request)
	blocking := f.blocking
	err := f.err
	response := Response{}
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

func (f *Fake) Attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}
