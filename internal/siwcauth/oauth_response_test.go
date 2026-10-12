package siwcauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func tokenPostFormFixture(t *testing.T, body string) (tokenResponse, error) {
	t.Helper()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/token" {
			t.Errorf("unexpected request method/path")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	var response tokenResponse
	err := postForm(context.Background(), server.Client(), server.URL+"/token", url.Values{"grant_type": {"authorization_code"}}, &response)
	if requests != 1 {
		t.Fatalf("expected exactly one physical token exchange, got %d", requests)
	}
	return response, err
}

func TestTokenPostFormIgnoresUnknownRFC6749Parameters(t *testing.T) {
	// RFC 6749 §5.1 requires unrecognized token response names to be ignored.
	// Unknown content is inert; it is never copied into the typed token result.
	body := `{"access_token":"fixture-access-private","refresh_token":"fixture-refresh-private","id_token":"fixture-id-private","token_type":"Bearer","expires_in":3600,"scope":"openid","earliest_refresh_at":"2026-10-11T00:00:00Z","unrecognized_future_parameter":{"nested":["fixture-unknown-private"]}}`
	response, err := tokenPostFormFixture(t, body)
	if err != nil {
		t.Fatalf("documented extension rejected: %v", err)
	}
	if response.AccessToken != "fixture-access-private" || response.RefreshToken != "fixture-refresh-private" || response.IDToken != "fixture-id-private" || response.ExpiresIn != 3600 {
		t.Fatal("known token fields were not decoded")
	}
	if strings.Contains(response.String()+response.GoString(), "private") {
		t.Fatal("token response rendering exposed private values")
	}
}

func TestTokenPostFormFixedDiagnosticsNeverExposeInput(t *testing.T) {
	cases := []struct {
		name, json, classification string
	}{
		{"duplicate_known", `{"access_token":"fixture-private","access_token":"again"}`, "duplicate_key"},
		{"duplicate_nested_unknown", `{"future":{"some_key":1,"some_key":2}}`, "duplicate_key"},
		{"trailing_value", `{"access_token":"fixture-private"} {"refresh_token":"extra"}`, "trailing_data"},
		{"known_type_mismatch", `{"expires_in":"fixture-private"}`, "known_field_type_mismatch"},
		{"malformed_json", `{"access_token":"fixture-private"`, "malformed_json"},
		{"non_object", `"fixture-private"`, "non_object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tokenPostFormFixture(t, tc.json)
			if err == nil || !strings.Contains(err.Error(), "("+tc.classification+")") {
				t.Fatalf("expected fixed diagnostic %s, got %v", tc.classification, err)
			}
			if strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "some_key") {
				t.Fatal("untrusted response content leaked in diagnostic")
			}
		})
	}
}

func TestTokenPostFormUnknownCaseVariantCannotOverrideKnownField(t *testing.T) {
	// JSON decoder case-insensitive struct matching would silently let a
	// noncanonical extension shadow a validated OAuth parameter.
	response, err := tokenPostFormFixture(t, `{"access_token":"canonical-fixture","Access_Token":"untrusted-case-variant"}`)
	if err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != "canonical-fixture" {
		t.Fatal("noncanonical parameter changed a known OAuth field")
	}
	response, err = tokenPostFormFixture(t, `{"Access_Token":"untrusted-only"}`)
	if err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != "" {
		t.Fatal("noncanonical token field was accepted as a known field")
	}
}

func TestPostFormNonTokenResponseRemainsStrict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"known":"value","unexpected":"extension"}`)
	}))
	defer server.Close()
	var dst struct {
		Known string `json:"known"`
	}
	err := postForm(context.Background(), server.Client(), server.URL+"/token", nil, &dst)
	if err == nil || !strings.Contains(err.Error(), "invalid SIWC endpoint response") {
		t.Fatalf("non-token response unexpectedly accepted unrecognized JSON: %v", err)
	}
}
