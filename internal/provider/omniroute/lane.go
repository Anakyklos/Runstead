package omniroute

import (
	"crypto/sha256"
	"encoding/hex"
)

// ProviderChatGPTWeb is the provider identity of the pinned M1 protected lane.
// The lane accepts exactly this provider and an explicitly configured
// chatgpt-web/<model> model; anything else fails closed before dispatch.
const ProviderChatGPTWeb = "chatgpt-web"

// connectionHeader carries the exact OmniRoute provider-connection pin on the
// protected model POST. OmniRoute selects exactly the configured account/
// connection; the raw id is configuration/transport state, never task truth.
const connectionHeader = "X-OmniRoute-Connection"

// DedicatedChatEndpoint is the provider-scoped OmniRoute route the protected
// lane must use, composed relative to the configured base URL. With the
// standard BaseURL ending in /v1 the effective URL becomes
// /v1/providers/chatgpt-web/chat/completions. An operator-supplied arbitrary
// chat endpoint must never weaken this path.
const DedicatedChatEndpoint = "providers/chatgpt-web/chat/completions"

// LaneHashForConnection derives the v1 account-lane hash from the configured
// OmniRoute connection id (contract #29/#30):
//
//	SHA-256( UTF-8("omniroute-connection-v1") || byte 0x00 || UTF-8(connection_id) )
//
// represented as lowercase hexadecimal (64 characters). The OmniRoute producer
// independently derives the receipt lane hash from the actually selected
// connection, so a selection mismatch fails receipt validation without
// exposing the raw connection id.
func LaneHashForConnection(connectionID string) string {
	input := make([]byte, 0, len(laneHashPrefix)+1+len(connectionID))
	input = append(input, laneHashPrefix...)
	input = append(input, laneHashSeparator)
	input = append(input, connectionID...)
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:])
}

var laneHashPrefix = []byte("omniroute-connection-v1")

const laneHashSeparator = 0x00
