package state

import (
	"regexp"
	"strings"
)

// Redact is the persistence-layer sanitization boundary. It extends the
// credential redaction semantics already exercised by the protocol experiment
// corpus (experiments/protocol/run.sh: Bearer tokens, sk-... keys and
// key=value credential pairs) so that credential-shaped content that appears
// in repository content, tool output or error text is never persisted in the
// SQLite database.
//
// Redact is deliberately conservative: it removes credential-shaped values
// even from legitimate content. Persisted state may therefore redact text
// that is not a real credential; that is the accepted trade-off for a
// durability store that must not leak secrets.
//
// The only secret-bearing values that may legitimately live in the store are
// the user's own task objective (the user provided them) and the sanitized
// configuration snapshot; both pass through Redact as well.
const redacted = "<redacted>"

var (
	// Authorization: Bearer eyJ... or plain "Bearer <token>" forms.
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=#-]{8,}`)

	// OpenAI-style secret keys: sk-<alphanumeric>.
	apiKeyPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)

	// credentialKey: value / credentialKey=value / "credentialKey":"value"
	// pairs. The value is everything up to a delimiter or end of line,
	// including spaces, so "authorization: Bearer <token>" is replaced as a
	// whole rather than leaving fragments.
	credentialPairPattern = regexp.MustCompile(`(?i)(["']?(?:authorization|set-cookie|cookie|session[_-]?id|access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|client[_-]?secret|password|passwd|secret|token)["']?\s*[:=]\s*["']?)[^,;}"'\n]+`)

	// ChatGPT Web session-style credentials: __Secure-...=... cookie blocks
	// and cf_clearance style values.
	sessionCookiePattern = regexp.MustCompile(`(__Secure-[A-Za-z0-9_]+|cf_clearance|__Host-[A-Za-z0-9_]+)=[A-Za-z0-9._~+/=-]{12,}`)
)

// Redact removes credential-shaped values from value. The result is safe to
// persist.
func Redact(value string) string {
	if value == "" {
		return ""
	}
	out := bearerPattern.ReplaceAllString(value, "Bearer "+redacted)
	out = apiKeyPattern.ReplaceAllString(out, redacted)
	out = credentialPairPattern.ReplaceAllString(out, "${1}"+redacted)
	out = sessionCookiePattern.ReplaceAllString(out, "${1}="+redacted)
	return out
}

// RedactJSON applies Redact to a JSON document while preserving JSON
// structure. Redact is a string transform, so JSON string escaping is left
// intact; the credential patterns are matched against the raw document bytes.
func RedactJSON(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	return []byte(Redact(string(data)))
}

// ContainsCredentialShape reports whether value contains any credential
// pattern outside of already-redacted spans. It is used by tests to prove
// sanitization, not by the runtime.
func ContainsCredentialShape(value string) bool {
	sanitized := strings.ReplaceAll(value, redacted, "")
	for _, pattern := range []*regexp.Regexp{bearerPattern, apiKeyPattern, credentialPairPattern, sessionCookiePattern} {
		if pattern.MatchString(sanitized) {
			return true
		}
	}
	return false
}
