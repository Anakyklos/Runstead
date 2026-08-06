package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAttemptReceiptSetAcceptsValidVersionedSet(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	set := AttemptReceiptSet{
		SchemaVersion:   AttemptReceiptSchemaVersion,
		ClientRequestID: "request-1",
		Finalized:       true,
		Receipts: []AttemptReceipt{
			{
				SchemaVersion:   AttemptReceiptSchemaVersion,
				AttemptID:       "attempt-1",
				ClientRequestID: "request-1",
				Sequence:        1,
				Provider:        "chatgpt-web",
				Model:           "gpt-5",
				AccountLaneHash: "lane-hash-1",
				StartedAt:       now,
				CompletedAt:     now.Add(time.Second),
				Outcome:         AttemptOutcomeSuccess,
				Trigger:         AttemptTriggerInitial,
				UpstreamReached: true,
			},
		},
	}
	if err := ValidateAttemptReceiptSet(set, AttemptReceiptExpectation{
		ClientRequestID: "request-1",
		Provider:        "chatgpt-web",
		Model:           "gpt-5",
		AccountLaneHash: "lane-hash-1",
		Now:             now,
	}); err != nil {
		t.Fatalf("valid receipt set rejected: %v", err)
	}
}

func TestDecodeAttemptReceiptSetRejectsUnknownFieldsWithoutEchoingSecrets(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"schema_version":    AttemptReceiptSchemaVersion,
		"client_request_id": "request-1",
		"finalized":         true,
		"prompt":            "private prompt token=secret",
		"receipts":          []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeAttemptReceiptSet(data)
	if err == nil || !strings.Contains(err.Error(), "invalid attempt receipts") {
		t.Fatalf("DecodeAttemptReceiptSet() error = %v", err)
	}
	if strings.Contains(err.Error(), "private prompt") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("receipt error leaked secret: %v", err)
	}
}

func TestAttemptReceiptSetRejectsSequenceGaps(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	set := AttemptReceiptSet{
		SchemaVersion:   AttemptReceiptSchemaVersion,
		ClientRequestID: "request-1",
		Finalized:       true,
		Receipts: []AttemptReceipt{
			{
				SchemaVersion:   AttemptReceiptSchemaVersion,
				AttemptID:       "attempt-1",
				ClientRequestID: "request-1",
				Sequence:        2,
				Provider:        "chatgpt-web",
				Model:           "gpt-5",
				AccountLaneHash: "lane-hash-1",
				StartedAt:       now,
				CompletedAt:     now.Add(time.Second),
				Outcome:         AttemptOutcomeUncertain,
				Trigger:         AttemptTriggerExecutorRetry,
				UpstreamReached: true,
			},
		},
	}
	if err := ValidateAttemptReceiptSet(set, AttemptReceiptExpectation{ClientRequestID: "request-1", Now: now}); err == nil {
		t.Fatal("sequence gap was accepted")
	}
}

func TestAttemptReceiptSetRejectsCorruptionAndOverlimits(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	valid := AttemptReceiptSet{
		SchemaVersion:   AttemptReceiptSchemaVersion,
		ClientRequestID: "request-1",
		Finalized:       true,
		Receipts: []AttemptReceipt{{
			SchemaVersion:   AttemptReceiptSchemaVersion,
			AttemptID:       "attempt-1",
			ClientRequestID: "request-1",
			Sequence:        1,
			Provider:        "provider",
			Model:           "model",
			AccountLaneHash: "lane",
			StartedAt:       now,
			CompletedAt:     now.Add(time.Second),
			Outcome:         AttemptOutcomeSuccess,
			Trigger:         AttemptTriggerInitial,
			UpstreamReached: true,
		}},
	}
	tests := []struct {
		name   string
		mutate func(*AttemptReceiptSet)
	}{
		{name: "unknown version", mutate: func(set *AttemptReceiptSet) { set.SchemaVersion = 99 }},
		{name: "duplicate id", mutate: func(set *AttemptReceiptSet) {
			set.Receipts = append(set.Receipts, set.Receipts[0])
			set.Receipts[1].Sequence = 2
		}},
		{name: "unknown outcome", mutate: func(set *AttemptReceiptSet) {
			set.Receipts[0].Outcome = AttemptOutcome("charged")
		}},
		{name: "future timestamp", mutate: func(set *AttemptReceiptSet) {
			set.Receipts[0].StartedAt = now.Add(25 * time.Hour)
			set.Receipts[0].CompletedAt = now.Add(25*time.Hour + time.Second)
		}},
		{name: "stale timestamp", mutate: func(set *AttemptReceiptSet) {
			set.Receipts[0].StartedAt = now.Add(-25 * time.Hour)
			set.Receipts[0].CompletedAt = now.Add(-25*time.Hour + time.Second)
		}},
		{name: "too many", mutate: func(set *AttemptReceiptSet) {
			for index := 2; index <= MaxAttemptReceipts+1; index++ {
				receipt := set.Receipts[0]
				receipt.AttemptID = "attempt-" + string(rune('a'+index))
				receipt.Sequence = index
				set.Receipts = append(set.Receipts, receipt)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := valid
			set.Receipts = append([]AttemptReceipt(nil), valid.Receipts...)
			test.mutate(&set)
			if err := ValidateAttemptReceiptSet(set, AttemptReceiptExpectation{Now: now}); err == nil {
				t.Fatal("corrupt receipt set was accepted")
			}
		})
	}
	oversized := valid
	oversized.ClientRequestID = strings.Repeat("r", MaxAttemptReceiptIdentifierBytes)
	oversized.Receipts = make([]AttemptReceipt, 0, MaxAttemptReceipts)
	for index := 1; index <= MaxAttemptReceipts; index++ {
		receipt := valid.Receipts[0]
		receipt.AttemptID = strings.Repeat(string(rune('a'+index)), MaxAttemptReceiptIdentifierBytes-1) + string(rune('0'+index))
		receipt.ClientRequestID = oversized.ClientRequestID
		receipt.Provider = strings.Repeat("p", MaxAttemptReceiptIdentifierBytes)
		receipt.Model = strings.Repeat("m", MaxAttemptReceiptIdentifierBytes)
		receipt.AccountLaneHash = strings.Repeat("l", MaxAttemptReceiptIdentifierBytes)
		receipt.Sequence = index
		receipt.StartedAt = now.Add(time.Duration(index) * time.Second)
		receipt.CompletedAt = receipt.StartedAt.Add(time.Second)
		oversized.Receipts = append(oversized.Receipts, receipt)
	}
	if err := ValidateAttemptReceiptSet(oversized, AttemptReceiptExpectation{Now: now}); err == nil {
		t.Fatal("serialized receipt size limit was not enforced")
	}
}

func TestAttemptReceiptSetRejectsUnboundCorrelationRouteAndUpstream(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	valid := AttemptReceiptSet{
		SchemaVersion:   AttemptReceiptSchemaVersion,
		ClientRequestID: "request-1",
		Finalized:       true,
		Receipts: []AttemptReceipt{{
			SchemaVersion:   AttemptReceiptSchemaVersion,
			AttemptID:       "attempt-1",
			ClientRequestID: "request-1",
			Sequence:        1,
			Provider:        "provider",
			Model:           "model",
			AccountLaneHash: "lane",
			StartedAt:       now,
			CompletedAt:     now.Add(time.Second),
			Outcome:         AttemptOutcomeSuccess,
			Trigger:         AttemptTriggerInitial,
			UpstreamReached: true,
		}},
	}
	expected := AttemptReceiptExpectation{
		ClientRequestID: "request-1",
		Provider:        "provider",
		Model:           "model",
		AccountLaneHash: "lane",
		Now:             now,
	}
	tests := []struct {
		name   string
		mutate func(*AttemptReceiptSet)
		code   AttemptReceiptErrorCode
	}{
		{name: "set correlation", mutate: func(set *AttemptReceiptSet) { set.ClientRequestID = "other" }, code: AttemptReceiptCorrelation},
		{name: "receipt correlation", mutate: func(set *AttemptReceiptSet) { set.Receipts[0].ClientRequestID = "other" }, code: AttemptReceiptCorrelation},
		{name: "provider route", mutate: func(set *AttemptReceiptSet) { set.Receipts[0].Provider = "other" }, code: AttemptReceiptRouteMismatch},
		{name: "model route", mutate: func(set *AttemptReceiptSet) { set.Receipts[0].Model = "other" }, code: AttemptReceiptRouteMismatch},
		{name: "lane route", mutate: func(set *AttemptReceiptSet) { set.Receipts[0].AccountLaneHash = "other" }, code: AttemptReceiptRouteMismatch},
		{name: "upstream not reached", mutate: func(set *AttemptReceiptSet) { set.Receipts[0].UpstreamReached = false }, code: AttemptReceiptInvalidField},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := valid
			set.Receipts = append([]AttemptReceipt(nil), valid.Receipts...)
			test.mutate(&set)
			err := ValidateAttemptReceiptSet(set, expected)
			var receiptErr *AttemptReceiptError
			if !errors.As(err, &receiptErr) || receiptErr.Code != test.code {
				t.Fatalf("ValidateAttemptReceiptSet() error = %v, want code %q", err, test.code)
			}
		})
	}
}
