package provider

import "testing"

func TestDeliveryStateNamesAndValidity(t *testing.T) {
	tests := []struct {
		name       string
		state      DeliveryState
		wantString string
		wantValid  bool
		wantReplay bool
	}{
		{name: "not sent", state: DeliveryNotSent, wantString: "not_sent", wantValid: true, wantReplay: true},
		{name: "sent confirmed", state: DeliverySentConfirmed, wantString: "sent_confirmed", wantValid: true},
		{name: "sent unconfirmed", state: DeliverySentUnconfirmed, wantString: "sent_unconfirmed", wantValid: true},
		{name: "response started", state: DeliveryResponseStarted, wantString: "response_started", wantValid: true},
		{name: "completed", state: DeliveryCompleted, wantString: "completed", wantValid: true},
		{name: "unobserved", state: DeliveryState(0), wantString: "unobserved"},
		{name: "unknown", state: DeliveryState(99), wantString: "unobserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.Valid(); got != tt.wantValid {
				t.Fatalf("Valid() = %t, want %t", got, tt.wantValid)
			}
			if got := tt.state.String(); got != tt.wantString {
				t.Fatalf("String() = %q, want %q", got, tt.wantString)
			}
			if got := tt.state.ReplaySafe(); got != tt.wantReplay {
				t.Fatalf("ReplaySafe() = %t, want %t", got, tt.wantReplay)
			}
		})
	}
}

func TestResponseMetadataKeepsRawDeliveryState(t *testing.T) {
	response := Response{
		Text: "model text says completed",
		Metadata: ResponseMetadata{
			DeliveryState: DeliverySentUnconfirmed,
		},
	}
	if response.Metadata.DeliveryState != DeliverySentUnconfirmed {
		t.Fatalf("delivery state = %v, want sent_unconfirmed", response.Metadata.DeliveryState)
	}
}
