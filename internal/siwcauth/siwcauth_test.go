package siwcauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuthorizationAndCallbackBinding(t *testing.T) {
	e := Endpoints{Authorize: "http://127.0.0.1:42124/authorize"}
	p, err := NewAuthorizationWithEndpoints("http://127.0.0.1:42123/auth/callback", "urn:uuid:01234567-89ab-4cde-8fab-0123456789ab", "", e)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != InitialClientID || q.Get("agent_name_hint") != AgentName || q.Get("ext_agent_host_id") != p.HostID || q.Get("code_challenge_method") != "S256" || q.Get("scope") != strings.Join(requiredScopes[:], " ") {
		t.Fatalf("unexpected authorization parameters: %v", q)
	}
	if p.Verifier == "" || q.Get("code_challenge") == p.Verifier {
		t.Fatal("PKCE challenge was not derived from a fresh verifier")
	}
	valid := "http://127.0.0.1:42123/auth/callback?code=code&state=" + url.QueryEscape(p.State) + "&client_id=issued-client"
	if _, err := ParseCallback(valid, p.RedirectURI, p.State, p.ClientID, true); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"http://localhost:42123/auth/callback?code=code&state=" + p.State + "&client_id=issued-client",
		"http://127.0.0.1:42123/callback?code=code&state=" + p.State + "&client_id=issued-client",
		"http://127.0.0.1:42123/auth/callback?code=code&state=wrong&client_id=issued-client",
		"http://127.0.0.1:42123/auth/callback?code=code&state=" + p.State + "&client_id=dynamic_agent_client",
		"http://127.0.0.1:42123/auth/callback?code=code&state=" + p.State + "&client_id=issued-client&extra=ignored",
		"http://127.0.0.1:42123/auth/callback?code=code&state=" + p.State + "&state=" + p.State + "&client_id=issued-client",
	} {
		if _, err := ParseCallback(bad, p.RedirectURI, p.State, p.ClientID, true); err == nil {
			t.Fatalf("accepted bad callback %q", bad)
		}
	}
}

func TestDefaultStorePathHonorsXDGWithoutSilentFallback(t *testing.T) {
	got, err := DefaultStorePath("/home/operator", "/mnt/private/config")
	if err != nil || got != "/mnt/private/config/runstead/siwc" {
		t.Fatalf("XDG path = %q, %v", got, err)
	}
	got, err = DefaultStorePath("/home/operator", "")
	if err != nil || got != "/home/operator/.config/runstead/siwc" {
		t.Fatalf("HOME path = %q, %v", got, err)
	}
	if _, err := DefaultStorePath("/home/operator", "relative/config"); err == nil {
		t.Fatal("relative XDG_CONFIG_HOME silently fell back")
	}
	if _, err := DefaultStorePath("relative/home", ""); err == nil {
		t.Fatal("relative HOME accepted")
	}
}

func TestEndpointTrustIsFixedToOfficialHostsOrOfflineLoopback(t *testing.T) {
	for _, endpoint := range []string{"https://auth.openai.com/api/accounts/oauth/token", "https://auth.openai.com:443/api/accounts/oauth/token", "http://127.0.0.1:41234/token"} {
		if !trustedOAuthURL(endpoint) {
			t.Errorf("trusted OAuth endpoint rejected: %s", endpoint)
		}
	}
	for _, endpoint := range []string{"https://auth.openai.com:8443/token", "https://evil.invalid/token", "http://localhost:41234/token", "http://[::1]:41234/token"} {
		if trustedOAuthURL(endpoint) {
			t.Errorf("untrusted OAuth endpoint accepted: %s", endpoint)
		}
	}
	if !trustedCatalogURL("https://api.openai.com/v1/models") || trustedCatalogURL("https://auth.openai.com/v1/models") {
		t.Fatal("catalog trust was not restricted to the official API host")
	}
}

func TestTokenLifetimeIsBounded(t *testing.T) {
	if !validExpiresIn(3600) || validExpiresIn(0) || validExpiresIn(-1) || validExpiresIn(1<<62) {
		t.Fatal("token expiry bound accepted invalid lifetime or rejected one hour")
	}
}

func TestVerifyIDTokenAndScopes(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1900000000, 0)
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/jwks"})
		case "/jwks":
			n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
			eb := []byte{}
			for e := key.E; e > 0; e >>= 8 {
				eb = append([]byte{byte(e)}, eb...)
			}
			e := base64.RawURLEncoding.EncodeToString(eb)
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key", "n": n, "e": e}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	token := signedIDToken(t, key, map[string]any{"iss": issuer, "sub": "subject-1", "aud": "issued-client", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": "nonce-1", "email": "private@example.invalid"})
	claims, err := VerifyIDToken(context.Background(), server.Client(), token, "nonce-1", "issued-client", issuer, issuer+"/.well-known/openid-configuration", now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "subject-1" || claims.ClientID != "issued-client" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
	for name, mutate := range map[string]func(map[string]any){"issuer": func(c map[string]any) { c["iss"] = "https://wrong.invalid" }, "audience": func(c map[string]any) { c["aud"] = "other" }, "nonce": func(c map[string]any) { c["nonce"] = "wrong" }, "expired": func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() }} {
		claims := map[string]any{"iss": issuer, "sub": "subject-1", "aud": "issued-client", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": "nonce-1"}
		mutate(claims)
		bad := signedIDToken(t, key, claims)
		if _, err := VerifyIDToken(context.Background(), server.Client(), bad, "nonce-1", "issued-client", issuer, issuer+"/.well-known/openid-configuration", now); err == nil {
			t.Errorf("accepted %s claim failure", name)
		}
	}
	parts := strings.Split(token, ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sig[0] ^= 0xff
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	if _, err := VerifyIDToken(context.Background(), server.Client(), strings.Join(parts, "."), "nonce-1", "issued-client", issuer, issuer+"/.well-known/openid-configuration", now); err == nil {
		t.Fatal("accepted invalid signature")
	}
	if _, err := requireScopes("openid profile email offline_access resource.invoke"); err == nil {
		t.Fatal("accepted missing plan-use scope")
	}
}

func signedIDToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestBindingsAreOpaqueAndRegistrationRedactsTokens(t *testing.T) {
	r := PublicRegistration{Issuer: Issuer, Subject: "subject-private", ClientID: "issued-client", HostID: "urn:uuid:01234567-89ab-4cde-8fab-0123456789ab"}
	a, c, err := Bindings(make([]byte, 32), r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "hmac-sha256:v1:") || !strings.HasPrefix(c, "hmac-sha256:v1:") || a == c || strings.Contains(a, r.Subject) || strings.Contains(c, r.ClientID) {
		t.Fatalf("invalid opaque bindings %q %q", a, c)
	}
	reg := Registration{Issuer: Issuer, Subject: r.Subject, ClientID: r.ClientID, HostID: r.HostID, tokens: TokenSet{AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: "id-secret"}}
	if got := reg.String() + " " + reg.GoString(); strings.Contains(got, "secret") {
		t.Fatalf("registration rendered secret: %q", got)
	}
}

func TestOfflineLoopbackLoginEndToEnd(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer, expectedNonce, baseURL := Issuer, "", ""
	var jwksURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": jwksURL, "revocation_endpoint": baseURL + "/revoke"})
		case "/jwks":
			eb := []byte{1, 0, 1}
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(eb)}}})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				if r.Form.Get("client_id") != "issued-client" || r.Form.Get("code_verifier") == "" || r.Form.Get("redirect_uri") == "" || r.Form.Get("resource") != Resource {
					t.Errorf("bad exchange form: %v", r.Form)
				}
				access := signedIDToken(t, key, map[string]any{"iss": issuer, "sub": "fixture-subject", "aud": Resource, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "client_id": "issued-client", "scope": strings.Join(requiredScopes[:], " ")})
				id := signedIDToken(t, key, map[string]any{"iss": issuer, "sub": "fixture-subject", "aud": "issued-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": expectedNonce, "client_id": "issued-client", "email": "fixture@example.invalid"})
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "fixture-refresh", "id_token": id, "token_type": "Bearer", "expires_in": 3600, "scope": strings.Join(requiredScopes[:], " "), "earliest_refresh_at": "2099-01-01T00:00:00Z"})
			case "refresh_token":
				if r.Form.Get("client_id") != "issued-client" || r.Form.Get("refresh_token") != "fixture-refresh" || r.Form.Get("resource") != Resource || r.Form.Has("scope") {
					t.Errorf("bad refresh form: %v", r.Form)
				}
				access := signedIDToken(t, key, map[string]any{"iss": issuer, "sub": "fixture-subject", "aud": Resource, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "client_id": "issued-client", "scope": strings.Join(requiredScopes[:], " ")})
				id := signedIDToken(t, key, map[string]any{"iss": issuer, "sub": "fixture-subject", "aud": "issued-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "fixture-refresh-rotated", "id_token": id, "token_type": "Bearer", "expires_in": 3600, "scope": strings.Join(requiredScopes[:], " "), "earliest_refresh_at": "2099-01-01T00:00:00Z"})
			default:
				t.Errorf("unexpected grant type %q", r.Form.Get("grant_type"))
			}
		case "/catalog":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer eyJ") {
				t.Errorf("bad catalog authorization")
			}
			_, _ = io.WriteString(w, `{"models":[{"slug":"shown-model","display_name":"Shown","visibility":"list"},{"slug":"hidden-model","visibility":"hidden"}]}`)
		case "/revoke":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("token") != "fixture-refresh-rotated" || r.Form.Get("client_id") != "issued-client" || r.Form.Get("token_type_hint") != "refresh_token" {
				t.Errorf("bad revocation form: %v", r.Form)
			}
		case "/authorize":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	baseURL = server.URL
	jwksURL = server.URL + "/jwks"
	endpoints := Endpoints{Issuer: issuer, Authorize: server.URL + "/authorize", Token: server.URL + "/token", Discovery: server.URL + "/.well-known/openid-configuration", Catalog: server.URL + "/catalog"}
	store, err := OpenStore(filepath.Join(t.TempDir(), "config", "runstead", "siwc"), true)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := loginWithEndpoints(context.Background(), store, server.Client(), endpoints, "", func(ctx context.Context, authURL string) error {
		auth, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		expectedNonce = auth.Query().Get("nonce")
		redirect := auth.Query().Get("redirect_uri")
		callback, _ := url.Parse(redirect)
		q := callback.Query()
		q.Set("code", "fixture-code")
		q.Set("state", auth.Query().Get("state"))
		q.Set("client_id", "issued-client")
		callback.RawQuery = q.Encode()
		if _, err := ParseCallback(callback.String(), redirect, auth.Query().Get("state"), InitialClientID, true); err != nil {
			t.Errorf("locally parsed callback: %v URL=%s redirect=%s", err, callback.String(), redirect)
		}
		resp, err := http.Get(callback.String())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("callback status %d body %s", resp.StatusCode, body)
			return errors.New("callback refused")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reg.Subject != "fixture-subject" || reg.ClientID != "issued-client" || reg.HostID == "" {
		t.Fatalf("unexpected registration: %#v", reg.Public())
	}
	reg.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	models, err := store.Catalog(context.Background(), server.Client(), endpoints, reg.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Slug != "shown-model" {
		t.Fatalf("catalog=%#v", models)
	}
	if err := store.SetModel(reg.ClientID, "shown-model", models); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModel(reg.ClientID, "hidden-model", models); err == nil {
		t.Fatal("accepted model that was not visibility=list")
	}
	account, credential, err := store.Bindings(reg.Public(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyBindings(account, credential); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAndClear(context.Background(), server.Client(), endpoints, reg.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(reg.ClientID); err == nil {
		t.Fatal("signed-out token remained usable")
	}
	if err := store.VerifyBindings(account, credential); err == nil {
		t.Fatal("historical identity binding authorized an operational session after sign-out")
	}
	registrations, err := store.Registrations()
	if err != nil || len(registrations) != 1 || registrations[0].Subject != "fixture-subject" {
		t.Fatalf("historical identity registration was not retained: %#v err=%v", registrations, err)
	}
}
