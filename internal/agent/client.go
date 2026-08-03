package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type Client struct {
	provider provider.Client
	logger   *slog.Logger
}

func New(client provider.Client, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{provider: client, logger: logger}
}

// Ask is the intentionally small agent-facing seam for issue #3. It forwards
// one request and returns the provider text; parsing, tools, corrections,
// verification and looping belong to later issues.
func (c *Client) Ask(ctx context.Context, prompt string) (string, error) {
	request := provider.Request{Protocol: protocol.Current, Prompt: prompt}
	c.logger.DebugContext(ctx, "provider attempt", "protocol", request.Protocol.String())
	response, err := c.provider.Complete(ctx, request)
	if err != nil {
		return "", fmt.Errorf("provider attempt: %w", err)
	}
	return response.Text, nil
}
