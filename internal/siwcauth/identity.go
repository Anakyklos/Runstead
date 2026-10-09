// Package siwcauth contains the offline-testable Sign in with ChatGPT
// registration and credential lifecycle. It is deliberately separate from
// SQLite and from the Responses provider adapter.
package siwcauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	Issuer          = "https://auth.openai.com"
	Resource        = "https://api.openai.com/v1"
	AuthorizeURL    = Issuer + "/api/accounts/authorize"
	TokenURL        = Issuer + "/api/accounts/oauth/token"
	CatalogURL      = Resource + "/models"
	InitialClientID = "dynamic_agent_client"
	AgentName       = "Runstead"
)

var requiredScopes = [...]string{"openid", "profile", "email", "offline_access", "resource.invoke", "chatgpt.tokens.use.direct"}

func RequiredScopes() []string { return append([]string(nil), requiredScopes[:]...) }

var (
	ErrInvalidCallback  = errors.New("invalid SIWC OAuth callback")
	ErrInvalidToken     = errors.New("invalid SIWC token response")
	ErrMissingScopes    = errors.New("required SIWC scopes were not granted")
	ErrIdentityChanged  = errors.New("SIWC identity changed")
	ErrRefreshUncertain = errors.New("SIWC refresh outcome is uncertain; sign in again")
)

// Registration holds verified, non-secret identity and an opaque selected
// model. The tokens field is private so fmt/logging cannot accidentally render
// credentials through the exported value.
type Registration struct {
	Issuer         string
	Subject        string
	ClientID       string
	HostID         string
	Email          string
	Scopes         []string
	ExpiresAt      time.Time
	Model          string
	tokens         TokenSet
	refreshPending bool
}

// TokenSet is private to the custody and OAuth package APIs; callers receive
// access tokens only for one explicit authorized HTTP request.
type TokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
}

func (TokenSet) String() string   { return "SIWC token set (credentials redacted)" }
func (TokenSet) GoString() string { return "SIWC token set (credentials redacted)" }

// PublicRegistration is safe to display or persist in non-secret metadata.
type PublicRegistration struct {
	Issuer    string    `json:"issuer"`
	Subject   string    `json:"subject"`
	ClientID  string    `json:"client_id"`
	HostID    string    `json:"ext_agent_host_id"`
	Email     string    `json:"email,omitempty"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
	Model     string    `json:"model,omitempty"`
}

func (r Registration) Public() PublicRegistration {
	return PublicRegistration{Issuer: r.Issuer, Subject: r.Subject, ClientID: r.ClientID, HostID: r.HostID, Email: r.Email, Scopes: append([]string(nil), r.Scopes...), ExpiresAt: r.ExpiresAt, Model: r.Model}
}

func (Registration) String() string   { return "SIWC registration (credentials redacted)" }
func (Registration) GoString() string { return "SIWC registration (credentials redacted)" }

func requireScopes(scopes string) ([]string, error) {
	set := make(map[string]bool)
	for _, scope := range strings.Fields(scopes) {
		if set[scope] {
			return nil, ErrInvalidToken
		}
		set[scope] = true
	}
	for _, required := range requiredScopes {
		if !set[required] {
			return nil, fmt.Errorf("%w: %s", ErrMissingScopes, required)
		}
	}
	result := make([]string, 0, len(set))
	for scope := range set {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result, nil
}

// Callback is accepted only from a single exact loopback callback URI.
type Callback struct{ Code, State, ClientID, Error string }

func (Callback) String() string   { return "SIWC OAuth callback (credentials redacted)" }
func (Callback) GoString() string { return "SIWC OAuth callback (credentials redacted)" }

func ParseCallback(rawURL, expectedRedirect, expectedState, pendingClientID string, firstRegistration bool) (Callback, error) {
	got, err := url.Parse(rawURL)
	if err != nil {
		return Callback{}, ErrInvalidCallback
	}
	want, err := url.Parse(expectedRedirect)
	if err != nil || want.Scheme != "http" || want.Hostname() != "127.0.0.1" || want.Path != "/auth/callback" || want.User != nil || want.RawQuery != "" || want.Fragment != "" {
		return Callback{}, ErrInvalidCallback
	}
	if got.Scheme != want.Scheme || got.Host != want.Host || got.Path != want.Path || got.User != nil || got.Fragment != "" {
		return Callback{}, ErrInvalidCallback
	}
	q, err := url.ParseQuery(got.RawQuery)
	if err != nil {
		return Callback{}, ErrInvalidCallback
	}
	allowed := map[string]bool{"code": true, "state": true, "client_id": true, "error": true, "error_description": true, "scope": true}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 {
			return Callback{}, ErrInvalidCallback
		}
	}
	if q.Get("state") != expectedState || len(q["state"]) != 1 {
		return Callback{}, ErrInvalidCallback
	}
	cb := Callback{Code: q.Get("code"), State: q.Get("state"), ClientID: q.Get("client_id"), Error: q.Get("error")}
	if cb.Error != "" {
		if cb.Error == "access_denied" {
			return cb, fmt.Errorf("%w: access denied", ErrInvalidCallback)
		}
		return Callback{}, ErrInvalidCallback
	}
	if cb.Code == "" || len(q["code"]) != 1 {
		return Callback{}, ErrInvalidCallback
	}
	if firstRegistration {
		if cb.ClientID == "" || cb.ClientID == InitialClientID {
			return Callback{}, ErrInvalidCallback
		}
	} else {
		if cb.ClientID != "" && cb.ClientID != pendingClientID {
			return Callback{}, ErrIdentityChanged
		}
		cb.ClientID = pendingClientID
	}
	if cb.ClientID == "" || len(q["client_id"]) > 1 {
		return Callback{}, ErrInvalidCallback
	}
	return cb, nil
}

func binding(key []byte, purpose string, values ...string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("runstead.siwc.binding.v1\x00" + purpose))
	for _, value := range values {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(value))
	}
	return "hmac-sha256:v1:" + hex.EncodeToString(mac.Sum(nil))
}

// Bindings derives the already-approved opaque provider binding format from
// verified OIDC identity and the local host key. Email is intentionally not
// identity because it can change or be shared by distinct registrations.
func Bindings(key []byte, r PublicRegistration) (account, credential string, err error) {
	if len(key) != 32 || r.Issuer != Issuer || strings.TrimSpace(r.Subject) == "" || strings.TrimSpace(r.ClientID) == "" || !validHostID(r.HostID) {
		return "", "", errors.New("verified SIWC identity and 32-byte local binding key are required")
	}
	account = binding(key, "account", r.Issuer, r.Subject, r.ClientID)
	credential = binding(key, "credential", r.Issuer, r.Subject, r.ClientID, r.HostID)
	return account, credential, nil
}

func validHostID(hostID string) bool {
	if !strings.HasPrefix(hostID, "urn:uuid:") {
		return false
	}
	value := strings.TrimPrefix(hostID, "urn:uuid:")
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16
}
