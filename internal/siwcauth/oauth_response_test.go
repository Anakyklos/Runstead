package siwcauth

import (
	"context"
	"fmt"
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

func TestTokenPostFormKnownFieldKindDiagnostics(t *testing.T) {
	fields := []struct {
		name, key, expected string
		values              []struct{ raw, observed string }
	}{
		{"access_token", "access_token", "string", []struct{ raw, observed string }{{`42`, "number"}, {`true`, "boolean"}, {`{"SENTINEL_VALUE":"SENTINEL_TOKEN"}`, "object"}, {`["SENTINEL_TOKEN"]`, "array"}}},
		{"refresh_token", "refresh_token", "string", []struct{ raw, observed string }{{`42`, "number"}, {`true`, "boolean"}, {`{"SENTINEL_VALUE":"SENTINEL_TOKEN"}`, "object"}, {`["SENTINEL_TOKEN"]`, "array"}}},
		{"id_token", "id_token", "string", []struct{ raw, observed string }{{`42`, "number"}, {`true`, "boolean"}, {`{"SENTINEL_VALUE":"SENTINEL_TOKEN"}`, "object"}, {`["SENTINEL_TOKEN"]`, "array"}}},
		{"token_type", "token_type", "string", []struct{ raw, observed string }{{`42`, "number"}, {`true`, "boolean"}, {`{"SENTINEL_VALUE":"SENTINEL_TOKEN"}`, "object"}, {`["SENTINEL_TOKEN"]`, "array"}}},
		{"expires_in", "expires_in", "number", []struct{ raw, observed string }{{`"SENTINEL_TOKEN"`, "string"}, {`true`, "boolean"}, {`{"SENTINEL_VALUE":"SENTINEL_TOKEN"}`, "object"}, {`["SENTINEL_TOKEN"]`, "array"}}},
		{"scope", "scope", "string", []struct{ raw, observed string }{{`42`, "number"}, {`true`, "boolean"}, {`{"SENTINEL_VALUE":"SENTINEL_TOKEN"}`, "object"}, {`["SENTINEL_TOKEN"]`, "array"}}},
		{"earliest_refresh_at", "earliest_refresh_at", "string", []struct{ raw, observed string }{{`42`, "number"}, {`true`, "boolean"}, {`{"SENTINEL_VALUE":"SENTINEL_TOKEN"}`, "object"}, {`["SENTINEL_TOKEN"]`, "array"}}},
	}
	for _, field := range fields {
		for _, value := range field.values {
			t.Run(field.name+"/"+value.observed, func(t *testing.T) {
				_, err := tokenPostFormFixture(t, `{"`+field.key+`":`+value.raw+`,"SENTINEL_UNKNOWN_NAME":{"private":"SENTINEL_TOKEN"}}`)
				want := "invalid SIWC endpoint response (known_field_type_mismatch): field=" + field.name + " expected=" + field.expected + " observed=" + value.observed
				if err == nil || err.Error() != want {
					t.Fatalf("got %v, want %q", err, want)
				}
				for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
					for _, secret := range []string{"SENTINEL", "private", `json:`, "cannot unmarshal", value.raw, `http://127.0.0.1`} {
						if strings.Contains(rendered, secret) {
							t.Fatalf("diagnostic leaked %q: %s", secret, rendered)
						}
					}
				}
			})
		}
	}
}

func TestTokenPostFormKnownFieldKindDeterministic(t *testing.T) {
	bodies := []string{
		`{"scope":42,"expires_in":true,"access_token":true}`,
		`{"access_token":true,"expires_in":true,"scope":42}`,
		`{"expires_in":true,"scope":42,"access_token":true}`,
	}
	for range 6 {
		for _, body := range bodies {
			_, err := tokenPostFormFixture(t, body)
			if err == nil || err.Error() != "invalid SIWC endpoint response (known_field_type_mismatch): field=access_token expected=string observed=boolean" {
				t.Fatalf("nondeterministic diagnostic: %v", err)
			}
		}
	}
}

func TestTokenPostFormKnownFieldNullAndValidKinds(t *testing.T) {
	fields := []string{"access_token", "refresh_token", "id_token", "token_type", "expires_in", "scope", "earliest_refresh_at"}
	for _, field := range fields {
		response, err := tokenPostFormFixture(t, `{"`+field+`":null}`)
		if err != nil || response != (tokenResponse{}) {
			t.Fatalf("%s null changed zero value: response=%+v err=%v", field, response, err)
		}
	}
	response, err := tokenPostFormFixture(t, `{"access_token":"a","refresh_token":"r","id_token":"i","token_type":"Bearer","expires_in":3600,"scope":"openid","earliest_refresh_at":null}`)
	if err != nil || response.AccessToken != "a" || response.ExpiresIn != 3600 || response.EarliestRefreshAt != "" {
		t.Fatalf("valid kinds/null changed: response=%+v err=%v", response, err)
	}
	for _, body := range []string{`{}`, `{"SENTINEL_UNKNOWN_NAME":42}`} {
		response, err = tokenPostFormFixture(t, body)
		if err != nil || response != (tokenResponse{}) {
			t.Fatalf("missing fields rejected: response=%+v err=%v", response, err)
		}
	}
}

func TestTokenPostFormKnownFieldNumberRepresentation(t *testing.T) {
	for _, value := range []string{`1.5`, `1e3`, `9223372036854775808`} {
		_, err := tokenPostFormFixture(t, `{"expires_in":`+value+`}`)
		if err == nil || err.Error() != "invalid SIWC endpoint response (known_field_type_mismatch): field=expires_in expected=number observed=number" {
			t.Fatalf("%s: got %v", value, err)
		}
	}
	for _, value := range []struct {
		raw  string
		want int64
	}{{`-1`, -1}, {`0`, 0}} {
		response, err := tokenPostFormFixture(t, `{"expires_in":`+value.raw+`}`)
		if err != nil || response.ExpiresIn != value.want {
			t.Fatalf("%s must retain int64 decoding: response=%+v err=%v", value.raw, response, err)
		}
	}
}

func TestTokenPostFormStructuralFailuresPrecedeFieldDiagnostics(t *testing.T) {
	for _, tc := range []struct{ body, classification string }{
		{`{"access_token":42,"access_token":"again"}`, "duplicate_key"},
		{`{"access_token":42,"extension":{"SENTINEL_UNKNOWN_NAME":1,"SENTINEL_UNKNOWN_NAME":2}}`, "duplicate_key"},
		{`{"access_token":42`, "malformed_json"},
		{`{"access_token":42} {}`, "trailing_data"},
		{`{"access_token":42} junk`, "trailing_data"},
		{`"not an object"`, "non_object"},
	} {
		_, err := tokenPostFormFixture(t, tc.body)
		if err == nil || !strings.Contains(err.Error(), "("+tc.classification+")") || strings.Contains(err.Error(), "field=") {
			t.Fatalf("structural classification lost for %s: %v", tc.classification, err)
		}
	}
	deep := `{"access_token":42,"extension":` + strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65) + `}`
	_, err := tokenPostFormFixture(t, deep)
	if err == nil || !strings.Contains(err.Error(), "(malformed_json)") || strings.Contains(err.Error(), "field=") {
		t.Fatalf("depth refusal changed: %v", err)
	}

	_, err = tokenPostFormFixture(t, `{"access_token":42,"extension":"`+strings.Repeat("x", (1<<20))+`"}`)
	if err == nil || !strings.Contains(err.Error(), "exceeded limit") || strings.Contains(err.Error(), "field=") {
		t.Fatalf("oversized response refusal changed: %v", err)
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

func TestTokenPostFormAllCaseVariantsAreIgnored(t *testing.T) {
	body := `{"access_token":"a","Access_Token":42,"refresh_token":"r","Refresh_Token":42,"id_token":"i","Id_Token":42,"token_type":"Bearer","Token_Type":42,"expires_in":3600,"Expires_In":"private","scope":"openid","Scope":42,"earliest_refresh_at":"time","Earliest_Refresh_At":42}`
	response, err := tokenPostFormFixture(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != "a" || response.RefreshToken != "r" || response.IDToken != "i" ||
		response.TokenType != "Bearer" || response.ExpiresIn != 3600 || response.Scope != "openid" || response.EarliestRefreshAt != "time" {
		t.Fatalf("case variants overrode canonical values: %+v", response)
	}
	_, err = tokenPostFormFixture(t, `{"Access_Token":42,"SENTINEL_UNKNOWN_NAME":"SENTINEL_TOKEN"}`)
	if err != nil {
		t.Fatalf("case variant emitted known-field diagnostic: %v", err)
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
