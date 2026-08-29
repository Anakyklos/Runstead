package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// completeOnce performs exactly one HTTP request. It never retries or changes
// accounts; Complete adds the authoritative receipt gate around this seam.
func (c *Client) completeOnce(ctx context.Context, request provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		metadata := baseMetadata()
		metadata.DeliveryState = provider.DeliveryNotSent
		return provider.Response{Metadata: metadata}, contextError(err, false)
	}
	model := request.Model
	if model == "" {
		model = c.config.Model
	}
	body, err := json.Marshal(struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}{
		Model: model,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: request.Prompt}},
		Stream: false,
	})
	if err != nil {
		metadata := baseMetadata()
		metadata.DeliveryState = provider.DeliveryNotSent
		metadata.Model = model
		return provider.Response{Metadata: metadata}, &Error{Kind: ErrorTransport, Cause: err}
	}
	if len(body) > c.config.MaxRequestBytes {
		metadata := baseMetadata()
		metadata.DeliveryState = provider.DeliveryNotSent
		metadata.Model = model
		return provider.Response{Metadata: metadata}, &Error{Kind: ErrorRequestTooLarge}
	}
	requestURL, err := chatURL(c.config.BaseURL, c.config.ChatEndpoint)
	if err != nil {
		metadata := baseMetadata()
		metadata.DeliveryState = provider.DeliveryNotSent
		metadata.Model = model
		return provider.Response{Metadata: metadata}, unsafeError(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	observation := &deliveryObservation{now: c.now}
	callCtx = httptrace.WithClientTrace(callCtx, observation.trace())
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, requestURL, strings.NewReader(string(body)))
	if err != nil {
		metadata := baseMetadata()
		metadata.DeliveryState = provider.DeliveryNotSent
		metadata.Endpoint = logicalEndpoint(requestURL)
		metadata.Model = model
		return provider.Response{Metadata: metadata}, &Error{Kind: ErrorTransport, Cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set(noCacheHeader, "true")
	// The pinned protected lane sends the exact connection id so OmniRoute
	// selects the configured account/connection. Raw connection ids are
	// configuration/transport state: they never appear in traces, evidence or
	// sanitized errors.
	if c.config.ConnectionID != "" {
		req.Header.Set(connectionHeader, c.config.ConnectionID)
	}
	if request.ClientRequestID != "" {
		req.Header.Set(clientRequestIDHeader, request.ClientRequestID)
	}
	req.Header.Set(attemptReceiptRequestHeader, "v1")
	started := c.now()
	response, callErr := c.httpClient.Do(req)
	if callErr != nil {
		metadata := baseMetadata()
		metadata.DeliveryState = observation.stateAfterError()
		metadata.Endpoint = logicalEndpoint(requestURL)
		metadata.Model = model
		metadata.Duration = c.now().Sub(started)
		return provider.Response{Metadata: metadata}, transportError(callErr, true)
	}
	if response == nil {
		metadata := baseMetadata()
		metadata.DeliveryState = observation.stateAfterError()
		metadata.Endpoint = logicalEndpoint(requestURL)
		metadata.Model = model
		metadata.Duration = c.now().Sub(started)
		return provider.Response{Metadata: metadata}, &Error{Kind: ErrorTransport, UpstreamReached: true}
	}
	observation.markResponseStarted()
	metadata := responseMetadata(response, c.now().Sub(started), observation.firstTokenLatency(started), requestURL, model, c.now())
	metadata.DeliveryState = provider.DeliveryResponseStarted
	if rawReceipts := response.Header.Get(attemptReceiptHeader); rawReceipts != "" {
		set, receiptErr := provider.DecodeAttemptReceiptSet([]byte(rawReceipts))
		if receiptErr != nil {
			response.Body.Close()
			return provider.Response{Metadata: metadata}, &Error{Kind: ErrorAttemptReceiptsInvalid, StatusCode: response.StatusCode, RequestID: metadata.RequestID, UpstreamReached: true, Cause: receiptErr}
		}
		metadata.AttemptReceipts = &set
	}
	responseBody, readErr := readBody(response, c.config.MaxResponseBytes)
	result := provider.Response{Metadata: metadata}
	if readErr != nil {
		result.Metadata.DeliveryState = observation.stateAfterBody(false)
		if errors.Is(readErr, errResponseTooLarge) {
			return result, &Error{Kind: ErrorResponseTooLarge, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID, UpstreamReached: true}
		}
		return result, &Error{Kind: ErrorTransport, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID, UpstreamReached: true}
	}
	if metadata.StatusCode < http.StatusOK || metadata.StatusCode >= http.StatusMultipleChoices {
		result.Metadata.DeliveryState = observation.stateAfterBody(true)
		return result, httpError(metadata, responseBody)
	}
	result.Metadata.DeliveryState = observation.stateAfterBody(true)
	text, parseErr := responseText(responseBody)
	if parseErr != nil {
		parseErr.StatusCode = metadata.StatusCode
		parseErr.RequestID = metadata.RequestID
		parseErr.UpstreamReached = true
		return result, parseErr
	}
	result.Text = text
	return result, nil
}
