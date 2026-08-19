// Package provider provides the minimal provider interface for research.
// This is a standalone experiment — NOT wired into runstead run/governor/executor.
package provider

import (
	"context"
	"time"
)

// ProviderClient is the minimal provider boundary.
// Excludes: RouteSafety, GatewayContractHealth, AttemptReceipts, DeliveryState
type ProviderClient interface {
	// Complete executes one model completion request.
	// ClientRequestID is the local deduplication identity (governor-generated).
	// Returns ProviderResponse with transport metadata (NO DeliveryState).
	Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error)

	// HealthCheck validates provider reachability, auth, and model availability.
	// MUST NOT perform model-effect sends/retries/fan-out. Explicitly prohibited.
	HealthCheck(ctx context.Context) (HealthResult, error)

	// Models returns available models for validation/discovery.
	Models(ctx context.Context) ([]ModelInfo, error)

	// Name returns stable provider identifier (e.g., "omniroute", "chatgptweb").
	Name() string
}

// ProviderRequest represents one completion request.
type ProviderRequest struct {
	// ClientRequestID is the LOCAL deduplication identity (governor-generated).
	// Must be distinct from upstream RequestID (transport evidence).
	ClientRequestID string

	// Model is the model identifier to use (e.g., "gpt-5.6-luna").
	// Empty = refusal (fail-closed), not default.
	Model string

	// Messages is the conversation history.
	Messages []Message

	// Stream indicates streaming request.
	Stream bool
}

// Message is a single conversation message.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

// ProviderResponse is the provider's completion response.
type ProviderResponse struct {
	// Content is the raw model output text.
	Content string

	// Metadata carries transport-level information.
	// NO DeliveryState — that is transport evidence, not derivable from success.
	Metadata ProviderResponseMetadata
}

// ProviderResponseMetadata contains transport metadata.
type ProviderResponseMetadata struct {
	// StatusCode is the HTTP status code (or equivalent) from the upstream.
	StatusCode int

	// RequestID is the UPSTREAM provider request identifier (transport evidence).
	// Distinct from ClientRequestID (local deduplication identity).
	RequestID string

	// SessionID is the upstream conversation/session identifier (if applicable).
	SessionID string

	// Duration is the total time the provider spent processing the request.
	Duration time.Duration

	// RetryAfter indicates how long to wait before retrying.
	RetryAfter time.Duration

	// ResetAt is the time when rate limits reset.
	ResetAt time.Time

	// Endpoint is the logical endpoint path that was called.
	Endpoint string

	// Model is the model that actually generated the response.
	Model string

	// TransportState is the observable transport evidence state.
	// Values: "no_send_observed", "send_observed", "response_started",
	// "completed", "canceled", "timeout_uncertain", "transport_failed", "unknown"
	TransportState string

	// SendCount is the physical model-effect send count observed.
	SendCount int

	// ErrorCode is the typed error classification if transport failed.
	// Values: "authentication_required", "human_challenge_required", "rate_limited",
	// "contract_drift", "transport_failed", "timeout_uncertain", "configuration_error"
	ErrorCode string

	// ChallengeType is the challenge type if auth/challenge was detected.
	ChallengeType string
}

// HealthResult is the result of a provider health check.
type HealthResult struct {
	// Healthy indicates whether the provider is ready to accept requests.
	Healthy bool

	// Reason explains why the provider is unhealthy (if applicable).
	// Values: "auth_expired", "model_unavailable", "endpoint_unreachable",
	// "challenge_detected", "rate_limited", "drift_detected", "configuration_error"
	Reason string

	// RateLimit contains current rate limit information if available.
	RateLimit *RateLimitInfo

	// ModelInfo contains information about the configured model if available.
	ModelInfo *ModelInfo
}

// RateLimitInfo contains rate limit details from the provider.
type RateLimitInfo struct {
	// Remaining is the number of requests remaining in the current window.
	Remaining int

	// ResetAt is when the current window resets.
	ResetAt time.Time

	// Limit is the total limit for the window (if known).
	Limit int

	// Window is the window duration (e.g., 1 hour, 1 week).
	Window time.Duration
}

// ModelInfo describes a model available from the provider.
type ModelInfo struct {
	// ID is the model identifier used in requests.
	ID string

	// DisplayName is a human-readable name.
	DisplayName string

	// ContextWindow is the maximum context size in tokens (if known).
	ContextWindow int

	// Capabilities lists special capabilities (e.g., "reasoning", "tools", "vision").
	Capabilities []string
}
