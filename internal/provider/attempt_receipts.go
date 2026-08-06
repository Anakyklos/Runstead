package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"
)

const (
	AttemptReceiptSchemaVersion      = 1
	MaxAttemptReceipts               = 16
	MaxAttemptReceiptSerializedBytes = 8 << 10
	MaxAttemptReceiptIdentifierBytes = 256
	MaxAttemptReceiptDuration        = 24 * time.Hour
	MaxAttemptReceiptClockSkew       = 5 * time.Minute
)

type AttemptTrigger string

const (
	AttemptTriggerInitial           AttemptTrigger = "initial"
	AttemptTriggerExecutorRetry     AttemptTrigger = "executor_retry"
	AttemptTriggerCredentialRefresh AttemptTrigger = "credential_refresh"
	AttemptTriggerFallback          AttemptTrigger = "fallback"
	AttemptTriggerCombo             AttemptTrigger = "combo"
	AttemptTriggerCooldownReplay    AttemptTrigger = "cooldown_replay"
)

type AttemptOutcome string

const (
	AttemptOutcomeSuccess               AttemptOutcome = "success"
	AttemptOutcomeError                 AttemptOutcome = "error"
	AttemptOutcomeHTTPError             AttemptOutcome = "http_error"
	AttemptOutcomeTransportError        AttemptOutcome = "transport_error"
	AttemptOutcomeRateCapacity          AttemptOutcome = "rate_or_capacity"
	AttemptOutcomeAuthenticationExpired AttemptOutcome = "authentication_expired"
	AttemptOutcomeAuthenticationDenied  AttemptOutcome = "authentication_denied"
	AttemptOutcomeHTTP403               AttemptOutcome = "http_403"
	AttemptOutcomeLoginChallenge        AttemptOutcome = "login_challenge"
	AttemptOutcomeCAPTCHA               AttemptOutcome = "captcha"
	AttemptOutcomeSuspiciousActivity    AttemptOutcome = "suspicious_activity"
	AttemptOutcomeAccountWarning        AttemptOutcome = "account_warning"
	AttemptOutcomeFeatureRestriction    AttemptOutcome = "feature_restriction"
	AttemptOutcomeConnectionReset       AttemptOutcome = "connection_reset"
	AttemptOutcomeTimeout               AttemptOutcome = "timeout"
	AttemptOutcomeEmptyResponse         AttemptOutcome = "empty_response"
	AttemptOutcomeMalformedUpstream     AttemptOutcome = "malformed_upstream_response"
	AttemptOutcomeUpstreamServerFailure AttemptOutcome = "upstream_server_failure"
	AttemptOutcomeCancelled             AttemptOutcome = "cancelled"
	AttemptOutcomeUncertain             AttemptOutcome = "uncertain"
)

type AttemptReceipt struct {
	SchemaVersion   int            `json:"schema_version"`
	AttemptID       string         `json:"attempt_id"`
	ClientRequestID string         `json:"client_request_id"`
	Sequence        int            `json:"sequence"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	AccountLaneHash string         `json:"account_lane_hash"`
	StartedAt       time.Time      `json:"started_at"`
	CompletedAt     time.Time      `json:"completed_at"`
	Outcome         AttemptOutcome `json:"outcome"`
	Trigger         AttemptTrigger `json:"trigger"`
	UpstreamReached bool           `json:"upstream_reached"`
}

type AttemptReceiptSet struct {
	SchemaVersion   int              `json:"schema_version"`
	ClientRequestID string           `json:"client_request_id"`
	Finalized       bool             `json:"finalized"`
	Receipts        []AttemptReceipt `json:"receipts"`
}

type AttemptReceiptExpectation struct {
	ClientRequestID    string
	Provider           string
	Model              string
	AccountLaneHash    string
	RequestStartedAt   time.Time
	RequestCompletedAt time.Time
	SingleAttempt      bool
	Now                time.Time
}

type AttemptReceiptErrorCode string

const (
	AttemptReceiptMissing         AttemptReceiptErrorCode = "missing"
	AttemptReceiptMalformed       AttemptReceiptErrorCode = "malformed"
	AttemptReceiptUnknownVersion  AttemptReceiptErrorCode = "unknown_version"
	AttemptReceiptNotFinalized    AttemptReceiptErrorCode = "not_finalized"
	AttemptReceiptEmpty           AttemptReceiptErrorCode = "empty"
	AttemptReceiptTooMany         AttemptReceiptErrorCode = "too_many"
	AttemptReceiptTooLarge        AttemptReceiptErrorCode = "too_large"
	AttemptReceiptInvalidField    AttemptReceiptErrorCode = "invalid_field"
	AttemptReceiptDuplicateID     AttemptReceiptErrorCode = "duplicate_attempt_id"
	AttemptReceiptSequence        AttemptReceiptErrorCode = "invalid_sequence"
	AttemptReceiptCorrelation     AttemptReceiptErrorCode = "correlation_mismatch"
	AttemptReceiptRouteMismatch   AttemptReceiptErrorCode = "route_mismatch"
	AttemptReceiptTimestamp       AttemptReceiptErrorCode = "invalid_timestamp"
	AttemptReceiptUnknownOutcome  AttemptReceiptErrorCode = "unknown_outcome"
	AttemptReceiptUnknownTrigger  AttemptReceiptErrorCode = "unknown_trigger"
	AttemptReceiptTriggerMismatch AttemptReceiptErrorCode = "trigger_mismatch"
	AttemptReceiptAmplification   AttemptReceiptErrorCode = "amplification_disabled"
)

var ErrInvalidAttemptReceipts = errors.New("invalid attempt receipts")

type AttemptReceiptError struct {
	Code AttemptReceiptErrorCode
}

func (e *AttemptReceiptError) Error() string {
	if e == nil || e.Code == "" {
		return ErrInvalidAttemptReceipts.Error()
	}
	return "invalid attempt receipts: " + string(e.Code)
}

func (e *AttemptReceiptError) Unwrap() error { return ErrInvalidAttemptReceipts }

func receiptError(code AttemptReceiptErrorCode) error { return &AttemptReceiptError{Code: code} }

func DecodeAttemptReceiptSet(data []byte) (AttemptReceiptSet, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return AttemptReceiptSet{}, receiptError(AttemptReceiptMissing)
	}
	if len(data) > MaxAttemptReceiptSerializedBytes {
		return AttemptReceiptSet{}, receiptError(AttemptReceiptTooLarge)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var set AttemptReceiptSet
	if err := decoder.Decode(&set); err != nil {
		return AttemptReceiptSet{}, receiptError(AttemptReceiptMalformed)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return AttemptReceiptSet{}, receiptError(AttemptReceiptMalformed)
	}
	if err := ValidateAttemptReceiptSet(set, AttemptReceiptExpectation{}); err != nil {
		return AttemptReceiptSet{}, err
	}
	return set, nil
}

func ValidateAttemptReceiptSet(set AttemptReceiptSet, expected AttemptReceiptExpectation) error {
	if set.SchemaVersion != AttemptReceiptSchemaVersion {
		return receiptError(AttemptReceiptUnknownVersion)
	}
	if !set.Finalized {
		return receiptError(AttemptReceiptNotFinalized)
	}
	if len(set.Receipts) == 0 {
		return receiptError(AttemptReceiptEmpty)
	}
	if len(set.Receipts) > MaxAttemptReceipts {
		return receiptError(AttemptReceiptTooMany)
	}
	if err := validateIdentifier(set.ClientRequestID); err != nil {
		return err
	}
	if expected.ClientRequestID != "" && set.ClientRequestID != expected.ClientRequestID {
		return receiptError(AttemptReceiptCorrelation)
	}
	if expected.SingleAttempt && len(set.Receipts) != 1 {
		return receiptError(AttemptReceiptAmplification)
	}
	seen := make(map[string]struct{}, len(set.Receipts))
	for index, receipt := range set.Receipts {
		if receipt.SchemaVersion != AttemptReceiptSchemaVersion {
			return receiptError(AttemptReceiptUnknownVersion)
		}
		if err := validateIdentifier(receipt.AttemptID); err != nil {
			return err
		}
		if _, ok := seen[receipt.AttemptID]; ok {
			return receiptError(AttemptReceiptDuplicateID)
		}
		seen[receipt.AttemptID] = struct{}{}
		if receipt.ClientRequestID != set.ClientRequestID {
			return receiptError(AttemptReceiptCorrelation)
		}
		if !receipt.UpstreamReached {
			return receiptError(AttemptReceiptInvalidField)
		}
		if receipt.Sequence != index+1 {
			return receiptError(AttemptReceiptSequence)
		}
		if err := validateIdentifier(receipt.Provider); err != nil {
			return err
		}
		if err := validateIdentifier(receipt.Model); err != nil {
			return err
		}
		if err := validateIdentifier(receipt.AccountLaneHash); err != nil {
			return err
		}
		if expected.Provider != "" && receipt.Provider != expected.Provider {
			return receiptError(AttemptReceiptRouteMismatch)
		}
		if expected.Model != "" && receipt.Model != expected.Model {
			return receiptError(AttemptReceiptRouteMismatch)
		}
		if expected.AccountLaneHash != "" && receipt.AccountLaneHash != expected.AccountLaneHash {
			return receiptError(AttemptReceiptRouteMismatch)
		}
		if !validOutcome(receipt.Outcome) {
			return receiptError(AttemptReceiptUnknownOutcome)
		}
		if !validTrigger(receipt.Trigger) {
			return receiptError(AttemptReceiptUnknownTrigger)
		}
		if (index == 0 && receipt.Trigger != AttemptTriggerInitial) ||
			(index > 0 && receipt.Trigger == AttemptTriggerInitial) {
			return receiptError(AttemptReceiptTriggerMismatch)
		}
		if expected.SingleAttempt && receipt.Trigger != AttemptTriggerInitial {
			return receiptError(AttemptReceiptTriggerMismatch)
		}
		if err := validateTimestamps(receipt.StartedAt, receipt.CompletedAt, expected); err != nil {
			return err
		}
		if index > 0 && receipt.StartedAt.Before(set.Receipts[index-1].CompletedAt) {
			return receiptError(AttemptReceiptTimestamp)
		}
	}
	serialized, err := json.Marshal(set)
	if err != nil || len(serialized) > MaxAttemptReceiptSerializedBytes {
		return receiptError(AttemptReceiptTooLarge)
	}
	return nil
}

func validateIdentifier(value string) error {
	if value == "" || len(value) > MaxAttemptReceiptIdentifierBytes || !utf8.ValidString(value) {
		return receiptError(AttemptReceiptInvalidField)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return receiptError(AttemptReceiptInvalidField)
		}
	}
	return nil
}

func validateTimestamps(started, completed time.Time, expected AttemptReceiptExpectation) error {
	if started.IsZero() || completed.IsZero() || completed.Before(started) || completed.Sub(started) > MaxAttemptReceiptDuration {
		return receiptError(AttemptReceiptTimestamp)
	}
	if started.Year() < 2000 || completed.Year() < 2000 {
		return receiptError(AttemptReceiptTimestamp)
	}
	if !expected.RequestStartedAt.IsZero() && started.Before(expected.RequestStartedAt.Add(-MaxAttemptReceiptClockSkew)) {
		return receiptError(AttemptReceiptTimestamp)
	}
	if !expected.RequestCompletedAt.IsZero() {
		latest := expected.RequestCompletedAt.Add(MaxAttemptReceiptClockSkew)
		if started.After(latest) || completed.After(latest) {
			return receiptError(AttemptReceiptTimestamp)
		}
	}
	if !expected.Now.IsZero() && (started.Before(expected.Now.Add(-MaxAttemptReceiptDuration)) ||
		completed.Before(expected.Now.Add(-MaxAttemptReceiptDuration)) ||
		started.After(expected.Now.Add(MaxAttemptReceiptDuration)) ||
		completed.After(expected.Now.Add(MaxAttemptReceiptDuration))) {
		return receiptError(AttemptReceiptTimestamp)
	}
	return nil
}

func validOutcome(outcome AttemptOutcome) bool {
	switch outcome {
	case AttemptOutcomeSuccess, AttemptOutcomeError, AttemptOutcomeHTTPError, AttemptOutcomeTransportError,
		AttemptOutcomeRateCapacity, AttemptOutcomeAuthenticationExpired, AttemptOutcomeAuthenticationDenied,
		AttemptOutcomeHTTP403, AttemptOutcomeLoginChallenge, AttemptOutcomeCAPTCHA,
		AttemptOutcomeSuspiciousActivity, AttemptOutcomeAccountWarning, AttemptOutcomeFeatureRestriction,
		AttemptOutcomeConnectionReset, AttemptOutcomeTimeout, AttemptOutcomeEmptyResponse,
		AttemptOutcomeMalformedUpstream, AttemptOutcomeUpstreamServerFailure,
		AttemptOutcomeCancelled, AttemptOutcomeUncertain:
		return true
	default:
		return false
	}
}

func validTrigger(trigger AttemptTrigger) bool {
	switch trigger {
	case AttemptTriggerInitial, AttemptTriggerExecutorRetry, AttemptTriggerCredentialRefresh, AttemptTriggerFallback, AttemptTriggerCombo, AttemptTriggerCooldownReplay:
		return true
	default:
		return false
	}
}

func (s AttemptReceiptSet) ContainsUncertain() bool {
	for _, receipt := range s.Receipts {
		switch receipt.Outcome {
		case AttemptOutcomeError, AttemptOutcomeHTTPError, AttemptOutcomeTransportError,
			AttemptOutcomeConnectionReset, AttemptOutcomeTimeout, AttemptOutcomeCancelled, AttemptOutcomeUncertain:
			return true
		}
	}
	return false
}
