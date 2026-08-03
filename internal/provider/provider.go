package provider

import (
	"context"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

type Request struct {
	Protocol protocol.Version
	Prompt   string
}

type Response struct {
	Text string
}

// Client performs at most one upstream model attempt per Complete invocation.
// Implementations must not retry, rotate accounts, select fallbacks, schedule
// work or apply quota policy; those decisions belong above this boundary.
type Client interface {
	Complete(context.Context, Request) (Response, error)
}
