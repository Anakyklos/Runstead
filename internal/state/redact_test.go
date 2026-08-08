package state

import (
	"strings"
	"testing"
)

func TestRedactBearerTokens(t *testing.T) {
	cases := map[string]string{
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig": "Authorization: <redacted>",
		"Bearer abcdefgh12345678":                                "Bearer <redacted>",
		"no credentials here":                                    "no credentials here",
	}
	for input, want := range cases {
		if got := Redact(input); got != want {
			t.Errorf("Redact(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRedactAPIKeys(t *testing.T) {
	cases := map[string]string{
		"key=sk-abcdefghijklmnop": "key=<redacted>",
		"sk-ABCDEFGH12345678":     "<redacted>",
		"sk-ab":                   "sk-ab", // too short to be a key
		"the sk-abcdefgh12 value": "the <redacted> value",
	}
	for input, want := range cases {
		if got := Redact(input); got != want {
			t.Errorf("Redact(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRedactCredentialPairs(t *testing.T) {
	cases := map[string]string{
		"api_key=abc123secret":                 "api_key=<redacted>",
		"api-key: abc123secret":                "api-key: <redacted>",
		"\"token\":\"xyz-secret-value\"":       "\"token\":\"<redacted>\"",
		"password = hunter2":                   "password = <redacted>",
		"authorization: bearer abc":            "authorization: <redacted>",
		"session_id=0x1234abcd5678":            "session_id=<redacted>",
		"client_secret=abcdefghijklmnop":       "client_secret=<redacted>",
		"cookie: __Secure-session=abcdefgh123": "cookie: <redacted>",
	}
	for input, want := range cases {
		if got := Redact(input); got != want {
			t.Errorf("Redact(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRedactSessionCookies(t *testing.T) {
	cases := map[string]string{
		"__Secure-next-auth.session-token=eyJhbGciOiJIUzI1NiJ9.abc.def": "__Secure-next-auth.session-token=<redacted>",
		"cf_clearance=abc.def.ghi-jkl_mno/pqr=":                         "cf_clearance=<redacted>",
	}
	for input, want := range cases {
		if got := Redact(input); got != want {
			t.Errorf("Redact(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRedactPreservesPlainContent(t *testing.T) {
	input := "alpha\nbeta\ngamma\n"
	if got := Redact(input); got != input {
		t.Fatalf("plain content must pass through unchanged: %q", got)
	}
	if Redact("") != "" {
		t.Fatal("empty input must stay empty")
	}
}

func TestRedactJSON(t *testing.T) {
	input := []byte(`{"content":"api_key=super-secret-value","path":"a.txt"}`)
	got := string(RedactJSON(input))
	if strings.Contains(got, "super-secret-value") {
		t.Fatalf("RedactJSON leaked credential: %s", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("RedactJSON did not redact: %s", got)
	}
	if !strings.Contains(got, `"path":"a.txt"`) {
		t.Fatalf("RedactJSON corrupted plain fields: %s", got)
	}
	if RedactJSON(nil) != nil {
		t.Fatal("RedactJSON(nil) must return nil")
	}
}

func TestContainsCredentialShape(t *testing.T) {
	if !ContainsCredentialShape("Bearer abcdefgh12345678") {
		t.Fatal("bearer token must be detected")
	}
	if !ContainsCredentialShape("api_key=sk-abcdefghijklmnop") {
		t.Fatal("api key must be detected")
	}
	if ContainsCredentialShape("plain content") {
		t.Fatal("plain content must not be detected")
	}
}
