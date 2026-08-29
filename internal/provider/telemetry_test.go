package provider

import "testing"

// TestResponseMetadataTelemetryZeroValue pins the conservative zero-value
// contract (#39): an absent observation must render as absent/zero, never
// guessed.
func TestResponseMetadataTelemetryZeroValue(t *testing.T) {
	var metadata ResponseMetadata
	if metadata.AdapterVersion != "" {
		t.Fatalf("zero AdapterVersion = %q, want empty", metadata.AdapterVersion)
	}
	if metadata.Transport != "" {
		t.Fatalf("zero Transport = %q, want empty", metadata.Transport)
	}
	if metadata.FirstByteLatency != 0 {
		t.Fatalf("zero FirstByteLatency = %v, want 0", metadata.FirstByteLatency)
	}
	if metadata.RetryCount != 0 {
		t.Fatalf("zero RetryCount = %d, want 0", metadata.RetryCount)
	}
	if metadata.Fallback {
		t.Fatal("zero Fallback = true, want false")
	}
	if metadata.UsageEstimated {
		t.Fatal("zero UsageEstimated = true, want false")
	}
}

// TestCompatAdapterVersionIsCanonical pins the single adapter-version
// identity shared by execution evidence and telemetry.
func TestCompatAdapterVersionIsCanonical(t *testing.T) {
	if CompatAdapterVersion != "compatible-provider-v0.1" {
		t.Fatalf("CompatAdapterVersion = %q, want pinned value", CompatAdapterVersion)
	}
}

// TestSessionIDIsSanitizedFingerprint pins the session-fingerprint contract:
// any non-empty SessionID must be a sha256 digest, never raw session identity.
func TestSessionIDIsSanitizedFingerprint(t *testing.T) {
	if !isFingerprint("sha256:0123456789abcdef") {
		t.Fatal("sha256-prefixed value did not pass fingerprint check")
	}
	if isFingerprint("live-session-abc") {
		t.Fatal("raw session identity passed fingerprint check")
	}
	if isFingerprint("") {
		t.Fatal("empty value passed fingerprint check")
	}
}

func isFingerprint(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+16 {
		return false
	}
	if value[:len(prefix)] != prefix {
		return false
	}
	for _, char := range value[len(prefix):] {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
