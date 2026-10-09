package siwcauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	maxOAuthBody     = 1 << 20
	maxTokenLifetime = 365 * 24 * time.Hour
)

var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

type Endpoints struct {
	Issuer    string
	Authorize string
	Token     string
	Discovery string
	Catalog   string
}

func OpenAIEndpoints() Endpoints {
	return Endpoints{Issuer: Issuer, Authorize: AuthorizeURL, Token: TokenURL,
		Discovery: Issuer + "/.well-known/openid-configuration", Catalog: CatalogURL}
}

type PendingAuthorization struct {
	URL               string
	RedirectURI       string
	HostID            string
	State             string
	Nonce             string
	Verifier          string
	ClientID          string
	FirstRegistration bool
}

func (PendingAuthorization) String() string {
	return "SIWC pending authorization (credentials redacted)"
}
func (PendingAuthorization) GoString() string {
	return "SIWC pending authorization (credentials redacted)"
}

func randomURLSafe(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validExpiresIn(seconds int64) bool {
	return seconds > 0 && seconds <= int64(maxTokenLifetime/time.Second)
}

// NewAuthorization creates fresh, independent state, nonce and PKCE material.
// The caller must bind it to an exact 127.0.0.1 callback and discard it after
// one callback; callers must never log the returned URL or verifier.
func NewAuthorization(redirectURI, hostID, issuedClientID string) (PendingAuthorization, error) {
	return NewAuthorizationWithEndpoints(redirectURI, hostID, issuedClientID, OpenAIEndpoints())
}

func NewAuthorizationWithEndpoints(redirectURI, hostID, issuedClientID string, endpoints Endpoints) (PendingAuthorization, error) {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/auth/callback" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return PendingAuthorization{}, ErrInvalidCallback
	}
	if !validHostID(hostID) {
		return PendingAuthorization{}, errors.New("SIWC host ID is required")
	}
	clientID := issuedClientID
	first := clientID == ""
	if first {
		clientID = InitialClientID
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return PendingAuthorization{}, err
	}
	nonce, err := randomURLSafe(32)
	if err != nil {
		return PendingAuthorization{}, err
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		return PendingAuthorization{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {redirectURI},
		"scope": {strings.Join(requiredScopes[:], " ")}, "resource": {Resource}, "state": {state}, "nonce": {nonce},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"ext_agent_host_id": {hostID},
	}
	if first {
		query.Set("agent_name_hint", AgentName)
	}
	endpoint, err := url.Parse(endpoints.Authorize)
	if err != nil || !trustedOAuthURL(endpoints.Authorize) {
		return PendingAuthorization{}, errors.New("invalid SIWC authorization endpoint")
	}
	endpoint.RawQuery = query.Encode()
	return PendingAuthorization{URL: endpoint.String(), RedirectURI: redirectURI, HostID: hostID, State: state, Nonce: nonce, Verifier: verifier, ClientID: clientID, FirstRegistration: first}, nil
}

type tokenResponse struct {
	AccessToken       string `json:"access_token"`
	RefreshToken      string `json:"refresh_token"`
	IDToken           string `json:"id_token"`
	TokenType         string `json:"token_type"`
	ExpiresIn         int64  `json:"expires_in"`
	Scope             string `json:"scope"`
	EarliestRefreshAt string `json:"earliest_refresh_at,omitempty"`
}

func (tokenResponse) String() string   { return "SIWC token response (credentials redacted)" }
func (tokenResponse) GoString() string { return "SIWC token response (credentials redacted)" }

type catalogResponse struct {
	Models []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
		Visibility  string `json:"visibility"`
	} `json:"models"`
}

type Model struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

func postForm(ctx context.Context, client *http.Client, endpoint string, values url.Values, dst any) error {
	if !trustedOAuthURL(endpoint) {
		return errors.New("untrusted SIWC OAuth endpoint")
	}
	if client == nil {
		client = defaultHTTPClient
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := clientCopy.Do(req)
	if err != nil {
		return errors.New("SIWC endpoint request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("SIWC endpoint refused request (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody+1))
	if err != nil || len(body) > maxOAuthBody {
		return errors.New("SIWC endpoint response exceeded limit")
	}
	if dst == nil {
		if len(strings.TrimSpace(string(body))) != 0 {
			return errors.New("unexpected SIWC endpoint response body")
		}
		return nil
	}
	if rejectDuplicateJSONKeys(body) != nil {
		return errors.New("invalid SIWC endpoint response")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid SIWC endpoint response")
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid SIWC endpoint response")
	}
	return nil
}

func (p PendingAuthorization) Exchange(ctx context.Context, client *http.Client, endpoints Endpoints, callback Callback, now time.Time) (Registration, error) {
	if callback.State != p.State || callback.Code == "" || callback.ClientID == "" || (p.FirstRegistration && callback.ClientID == InitialClientID) || (!p.FirstRegistration && callback.ClientID != p.ClientID) {
		return Registration{}, ErrInvalidCallback
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {callback.ClientID}, "code": {callback.Code}, "code_verifier": {p.Verifier}, "redirect_uri": {p.RedirectURI}, "resource": {Resource}}
	var response tokenResponse
	if err := postForm(ctx, client, endpoints.Token, form, &response); err != nil {
		return Registration{}, err
	}
	if response.TokenType != "Bearer" || response.AccessToken == "" || response.RefreshToken == "" || response.IDToken == "" || !validExpiresIn(response.ExpiresIn) {
		return Registration{}, fmt.Errorf("%w: incomplete token fields (access=%t refresh=%t id=%t bearer=%t expiry=%t)", ErrInvalidToken, response.AccessToken != "", response.RefreshToken != "", response.IDToken != "", response.TokenType == "Bearer", validExpiresIn(response.ExpiresIn))
	}
	scopes, err := requireScopes(response.Scope)
	if err != nil {
		return Registration{}, err
	}
	if endpoints.Issuer == "" {
		return Registration{}, errors.New("SIWC issuer required")
	}
	claims, err := VerifyIDToken(ctx, client, response.IDToken, p.Nonce, callback.ClientID, endpoints.Issuer, endpoints.Discovery, now)
	if err != nil {
		return Registration{}, fmt.Errorf("SIWC ID token validation failed: %w", err)
	}
	accessScopes, err := VerifyAccessToken(ctx, client, response.AccessToken, claims.Subject, callback.ClientID, endpoints.Issuer, endpoints.Discovery, now)
	if err != nil || !sameScopes(scopes, accessScopes) {
		return Registration{}, ErrInvalidToken
	}
	return Registration{Issuer: claims.Issuer, Subject: claims.Subject, ClientID: callback.ClientID, HostID: p.HostID, Email: claims.Email, Scopes: scopes, ExpiresAt: now.Add(time.Duration(response.ExpiresIn) * time.Second), tokens: TokenSet{response.AccessToken, response.RefreshToken, response.IDToken, response.TokenType}}, nil
}

type discoveryDocument struct {
	Issuer             string `json:"issuer"`
	JWKSURI            string `json:"jwks_uri"`
	RevocationEndpoint string `json:"revocation_endpoint"`
}

func fetchJSON(ctx context.Context, client *http.Client, endpoint string, dst any) error {
	if !trustedOAuthURL(endpoint) {
		return errors.New("untrusted SIWC issuer endpoint")
	}
	if client == nil {
		client = defaultHTTPClient
	}
	// OAuth credentials and callbacks must never be forwarded by redirects.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := copyClient.Do(req)
	if err != nil {
		return errors.New("SIWC issuer request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SIWC issuer refused request (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody+1))
	if err != nil || len(body) > maxOAuthBody {
		return errors.New("SIWC issuer response exceeded limit")
	}
	if rejectDuplicateJSONKeys(body) != nil {
		return errors.New("invalid SIWC issuer metadata")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid SIWC issuer metadata")
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid SIWC issuer metadata")
	}
	return nil
}

// Catalog fetches only the selected account's displayable models. Listing is
// discovery metadata; it is never an entitlement or admission decision.
func (s *Store) Catalog(ctx context.Context, client *http.Client, endpoints Endpoints, clientID string) ([]Model, error) {
	if !trustedCatalogURL(endpoints.Catalog) {
		return nil, errors.New("untrusted SIWC catalog endpoint")
	}
	reg, err := s.Load(clientID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if !reg.ExpiresAt.After(now.Add(5 * time.Minute)) {
		reg, err = s.RefreshOAuth(ctx, client, endpoints, clientID, now)
		if err != nil {
			return nil, err
		}
	}
	if !reg.ExpiresAt.After(time.Now()) {
		return nil, errors.New("SIWC access token expired")
	}
	if !hasScope(reg.Scopes, "resource.invoke") {
		return nil, ErrMissingScopes
	}
	if client == nil {
		client = defaultHTTPClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoints.Catalog, nil)
	if err != nil {
		return nil, errors.New("invalid SIWC catalog endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+reg.tokens.AccessToken)
	resp, err := copyClient.Do(req)
	if err != nil {
		return nil, errors.New("SIWC catalog request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SIWC catalog refused request (HTTP %d)", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody+1))
	if err != nil || len(b) > maxOAuthBody {
		return nil, errors.New("SIWC catalog response exceeded limit")
	}
	if rejectDuplicateJSONKeys(b) != nil {
		return nil, errors.New("invalid SIWC catalog response")
	}
	var decoded catalogResponse
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if dec.Decode(&decoded) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid SIWC catalog response")
	}
	models := make([]Model, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		if m.Visibility == "list" && strings.TrimSpace(m.Slug) != "" {
			models = append(models, Model{Slug: m.Slug, DisplayName: m.DisplayName})
		}
	}
	return models, nil
}

func trustedOAuthURL(raw string) bool   { return trustedURL(raw, "auth.openai.com") }
func trustedCatalogURL(raw string) bool { return trustedURL(raw, "api.openai.com") }
func trustedURL(raw string, officialHost string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return strings.EqualFold(u.Hostname(), officialHost) && (u.Port() == "" || u.Port() == "443")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		return ip != nil && ip.Equal(net.ParseIP("127.0.0.1"))
	}
	return false
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func sameScopes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// RefreshOAuth serializes access through the protected store and persists a
// preflight intent before the one refresh request. A lost response leaves the
// registration marked uncertain and cannot be replayed.
func (s *Store) RefreshOAuth(ctx context.Context, client *http.Client, endpoints Endpoints, clientID string, now time.Time) (Registration, error) {
	return s.refresh(clientID, now.Add(5*time.Minute), func(current Registration) (Registration, error) {
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {current.ClientID}, "refresh_token": {current.tokens.RefreshToken}, "resource": {Resource}}
		var response tokenResponse
		if err := postForm(ctx, client, endpoints.Token, form, &response); err != nil {
			return Registration{}, err
		}
		if response.TokenType != "Bearer" || response.AccessToken == "" || response.RefreshToken == "" || !validExpiresIn(response.ExpiresIn) {
			return Registration{}, ErrInvalidToken
		}
		scopes, err := requireScopes(response.Scope)
		if err != nil {
			return Registration{}, err
		}
		idToken := response.IDToken
		if idToken == "" {
			idToken = current.tokens.IDToken
		} else {
			var claims identityClaims
			if err := verifySignedJWT(ctx, client, idToken, current.Issuer, endpoints.Discovery, &claims); err != nil || claims.Issuer != current.Issuer || claims.Subject != current.Subject || claims.ClientID != current.ClientID || !hasAudience(claims.Audience, current.ClientID) || (audienceCount(claims.Audience) > 1 && claims.AuthorizedParty != current.ClientID) || claims.Expires <= now.Unix() || (claims.NotBefore != 0 && claims.NotBefore > now.Unix()) || claims.IssuedAt > now.Add(2*time.Minute).Unix() {
				return Registration{}, ErrIdentityChanged
			}
		}
		accessScopes, err := VerifyAccessToken(ctx, client, response.AccessToken, current.Subject, current.ClientID, current.Issuer, endpoints.Discovery, now)
		if err != nil || !sameScopes(scopes, accessScopes) {
			return Registration{}, ErrInvalidToken
		}
		return Registration{Issuer: current.Issuer, Subject: current.Subject, ClientID: current.ClientID, HostID: current.HostID, Email: current.Email, Scopes: scopes, ExpiresAt: now.Add(time.Duration(response.ExpiresIn) * time.Second), Model: current.Model, tokens: TokenSet{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, IDToken: idToken, TokenType: response.TokenType}}, nil
	})
}

// RevokeAndClear uses discovery's revocation endpoint. A failed revocation
// keeps local tokens so the operator can retry; it never claims sign-out.
func (s *Store) RevokeAndClear(ctx context.Context, client *http.Client, endpoints Endpoints, clientID string) error {
	return s.withLock(func() error {
		var disk diskRecord
		if err := s.findRecord(clientID, &disk); err != nil {
			return err
		}
		if disk.RefreshPending {
			return ErrRefreshUncertain
		}
		if disk.Tokens.RefreshToken == "" {
			return errors.New("SIWC registration has no renewable session")
		}
		var discovery discoveryDocument
		if err := fetchJSON(ctx, client, endpoints.Discovery, &discovery); err != nil {
			return err
		}
		if discovery.Issuer != endpoints.Issuer || discovery.RevocationEndpoint == "" {
			return errors.New("invalid SIWC revocation metadata")
		}
		form := url.Values{"token": {disk.Tokens.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {disk.Registration.ClientID}}
		if err := postForm(ctx, client, discovery.RevocationEndpoint, form, nil); err != nil {
			return err
		}
		return s.removeTokensLocked(disk)
	})
}
