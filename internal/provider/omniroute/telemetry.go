package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Snapshot reads optional dashboard telemetry. It never turns absent optional
// fields into local quota values; governor admission remains authoritative.
func (c *Client) Snapshot(ctx context.Context) (governor.TelemetrySnapshot, error) {
	if c == nil {
		return governor.TelemetrySnapshot{}, &Error{Kind: ErrorTelemetry}
	}
	rateBody, _, err := c.getTelemetry(ctx, rateLimitsPath)
	if err != nil {
		return governor.TelemetrySnapshot{}, err
	}
	resilienceBody, _, err := c.getTelemetry(ctx, resiliencePath)
	if err != nil {
		return governor.TelemetrySnapshot{}, err
	}
	if !safeResilience(resilienceBody) {
		c.clearVerification()
		return governor.TelemetrySnapshot{}, &Error{Kind: ErrorTelemetry}
	}
	snapshot, err := parseTelemetry(rateBody)
	if err != nil {
		return governor.TelemetrySnapshot{}, err
	}
	resilienceSnapshot, err := parseTelemetry(resilienceBody)
	if err != nil {
		return governor.TelemetrySnapshot{}, err
	}
	mergeTelemetry(&snapshot, resilienceSnapshot)
	if safety := c.RouteSafety(); safety.Validate() == nil {
		snapshot.RouteSafety = &safety
	}
	return snapshot, nil
}

func (c *Client) getTelemetry(ctx context.Context, endpoint string) ([]byte, provider.ResponseMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, provider.ResponseMetadata{}, contextError(err, false)
	}
	requestURL, err := joinURL(c.config.ManagementBaseURL, endpoint)
	if err != nil {
		return nil, provider.ResponseMetadata{}, &Error{Kind: ErrorTelemetry}
	}
	callCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, provider.ResponseMetadata{}, &Error{Kind: ErrorTelemetry}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	started := c.now()
	response, callErr := c.httpClient.Do(req)
	if callErr != nil {
		if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			return nil, provider.ResponseMetadata{}, contextError(callErr, false)
		}
		return nil, provider.ResponseMetadata{}, &Error{Kind: ErrorTelemetry}
	}
	if response == nil {
		return nil, provider.ResponseMetadata{}, &Error{Kind: ErrorTelemetry}
	}
	metadata := responseMetadata(response, c.now().Sub(started), endpoint, c.config.Model, c.now())
	body, readErr := readBody(response, c.config.MaxResponseBytes)
	if readErr != nil {
		return nil, metadata, &Error{Kind: ErrorTelemetry, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID}
	}
	if metadata.StatusCode < http.StatusOK || metadata.StatusCode >= http.StatusMultipleChoices {
		return nil, metadata, &Error{Kind: ErrorTelemetry, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID}
	}
	return body, metadata, nil
}

func parseTelemetry(body []byte) (governor.TelemetrySnapshot, error) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return governor.TelemetrySnapshot{}, &Error{Kind: ErrorTelemetry}
	}
	snapshot := governor.TelemetrySnapshot{UpstreamCircuit: governor.UpstreamCircuitUnknown}
	if value, ok := findTelemetryValue(root, "remaining", "remainingRequests"); ok {
		if remaining, ok := telemetryInt(value); ok {
			snapshot.Remaining = &remaining
		}
	}
	if value, ok := findTelemetryValue(root, "resetAt", "reset_at"); ok {
		snapshot.ResetAt = telemetryTime(value)
	}
	if value, ok := findTelemetryValue(root, "cooldownUntil", "cooldown_until"); ok {
		snapshot.CooldownUntil = telemetryTime(value)
	}
	if value, ok := findTelemetryValue(root, "retryAfter", "retry_after"); ok {
		snapshot.RetryAfter = telemetryDuration(value)
	}
	if value, ok := findTelemetryValue(root, "rateLimited", "rate_limited"); ok {
		snapshot.RateLimited, _ = value.(bool)
	}
	if value, ok := findTelemetryValue(root, "capacityExhausted", "capacity_exhausted"); ok {
		snapshot.CapacityExhausted, _ = value.(bool)
	}
	if value, ok := findTelemetryValue(root, "upstreamCircuit", "upstream_circuit"); ok {
		if state, ok := value.(string); ok {
			switch strings.ToLower(strings.TrimSpace(state)) {
			case "open":
				snapshot.UpstreamCircuit = governor.UpstreamCircuitOpen
			case "closed":
				snapshot.UpstreamCircuit = governor.UpstreamCircuitClosed
			}
		}
	}
	return snapshot, nil
}

func mergeTelemetry(dst *governor.TelemetrySnapshot, src governor.TelemetrySnapshot) {
	if dst.Remaining == nil {
		dst.Remaining = src.Remaining
	}
	if dst.ResetAt.IsZero() {
		dst.ResetAt = src.ResetAt
	}
	if dst.CooldownUntil.IsZero() {
		dst.CooldownUntil = src.CooldownUntil
	}
	if dst.RetryAfter == 0 {
		dst.RetryAfter = src.RetryAfter
	}
	dst.RateLimited = dst.RateLimited || src.RateLimited
	dst.CapacityExhausted = dst.CapacityExhausted || src.CapacityExhausted
	if dst.UpstreamCircuit == governor.UpstreamCircuitUnknown {
		dst.UpstreamCircuit = src.UpstreamCircuit
	}
}

func findTelemetryValue(value any, names ...string) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			for _, name := range names {
				if key == name {
					return item, true
				}
			}
		}
		for _, item := range typed {
			if found, ok := findTelemetryValue(item, names...); ok {
				return found, true
			}
		}
	case []any:
		for _, item := range typed {
			if found, ok := findTelemetryValue(item, names...); ok {
				return found, true
			}
		}
	}
	return nil, false
}

func telemetryInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), typed == float64(int(typed))
	case json.Number:
		parsed, err := strconv.Atoi(string(typed))
		return parsed, err == nil
	case int:
		return typed, true
	default:
		return 0, false
	}
}

func telemetryTime(value any) time.Time {
	switch typed := value.(type) {
	case string:
		when, _ := time.Parse(time.RFC3339, typed)
		return when
	case float64:
		if typed > 1e12 {
			return time.UnixMilli(int64(typed)).UTC()
		}
		return time.Unix(int64(typed), 0).UTC()
	default:
		return time.Time{}
	}
}

func telemetryDuration(value any) time.Duration {
	if number, ok := telemetryInt(value); ok && number >= 0 {
		return time.Duration(number) * time.Second
	}
	if text, ok := value.(string); ok {
		if duration, err := time.ParseDuration(text); err == nil && duration >= 0 {
			return duration
		}
	}
	return 0
}

var _ governor.TelemetrySource = (*Client)(nil)
