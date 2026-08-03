package protocol

import "testing"

func TestCurrentVersionIsAdoptedProtocol(t *testing.T) {
	if Current != Version("runstead.protocol.v1") {
		t.Fatalf("Current = %q, want runstead.protocol.v1", Current)
	}
	if Current.String() != "runstead.protocol.v1" {
		t.Fatalf("Current.String() = %q, want runstead.protocol.v1", Current.String())
	}
}
