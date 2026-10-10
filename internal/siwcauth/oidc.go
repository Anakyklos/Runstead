package siwcauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const maxJWTBytes = 32 << 10

type identityClaims struct {
	Issuer          string `json:"iss"`
	Subject         string `json:"sub"`
	Audience        any    `json:"aud"`
	Expires         int64  `json:"exp"`
	IssuedAt        int64  `json:"iat"`
	NotBefore       int64  `json:"nbf"`
	Nonce           string `json:"nonce"`
	AuthorizedParty string `json:"azp"`
	Email           string `json:"email"`
	EmailVerified   bool   `json:"email_verified"`
}

type VerifiedClaims struct{ Issuer, Subject, ClientID, Email string }
type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}
type jwks struct {
	Keys []jwk `json:"keys"`
}
type jwk struct {
	KeyType   string `json:"kty"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
}

func VerifyIDToken(ctx context.Context, client *http.Client, rawToken, nonce, clientID, issuer, discoveryURL string, now time.Time) (VerifiedClaims, error) {
	if len(rawToken) == 0 || len(rawToken) > maxJWTBytes || nonce == "" || clientID == "" || issuer == "" || discoveryURL == "" {
		return VerifiedClaims{}, ErrInvalidToken
	}
	var claims identityClaims
	if err := verifySignedJWT(ctx, client, rawToken, issuer, discoveryURL, &claims); err != nil {
		return VerifiedClaims{}, ErrInvalidToken
	}
	if claims.Issuer != issuer || strings.TrimSpace(claims.Subject) == "" || claims.Nonce != nonce || claims.Expires <= now.Unix() || (claims.NotBefore != 0 && claims.NotBefore > now.Unix()) || claims.IssuedAt > now.Add(2*time.Minute).Unix() || !hasAudience(claims.Audience, clientID) || (audienceCount(claims.Audience) > 1 && claims.AuthorizedParty != clientID) {
		return VerifiedClaims{}, ErrInvalidToken
	}
	// The issued client ID is bound by the validated audience (and azp when
	// multiple audiences are present). OpenID Connect does not require a
	// non-standard client_id claim in an ID token.
	return VerifiedClaims{Issuer: claims.Issuer, Subject: claims.Subject, ClientID: clientID, Email: claims.Email}, nil
}

func verifySignedJWT(ctx context.Context, client *http.Client, rawToken, issuer, discoveryURL string, dst any) error {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return ErrInvalidToken
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ErrInvalidToken
	}
	if rejectDuplicateJSONKeys(headerBytes) != nil {
		return ErrInvalidToken
	}
	var header jwtHeader
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, dst) != nil {
		return ErrInvalidToken
	}
	if rejectDuplicateJSONKeys(payload) != nil {
		return ErrInvalidToken
	}
	var metadata discoveryDocument
	if err := fetchJSON(ctx, client, discoveryURL, &metadata); err != nil {
		return err
	}
	if metadata.Issuer != issuer || metadata.JWKSURI == "" {
		return ErrInvalidToken
	}
	var keys jwks
	if err := fetchJSON(ctx, client, metadata.JWKSURI, &keys); err != nil {
		return err
	}
	var selected *jwk
	for i := range keys.Keys {
		if keys.Keys[i].KeyID == header.KeyID {
			if selected != nil {
				return ErrInvalidToken
			}
			selected = &keys.Keys[i]
		}
	}
	if selected == nil || selected.KeyType != "RSA" || (selected.Use != "" && selected.Use != "sig") || (selected.Algorithm != "" && selected.Algorithm != "RS256") {
		return ErrInvalidToken
	}
	modulus, err := base64.RawURLEncoding.DecodeString(selected.Modulus)
	if err != nil {
		return ErrInvalidToken
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(selected.Exponent)
	if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
		return ErrInvalidToken
	}
	exponent := 0
	for _, b := range exponentBytes {
		exponent = exponent<<8 | int(b)
	}
	key := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}
	if key.N.Sign() <= 0 || key.E < 3 {
		return ErrInvalidToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ErrInvalidToken
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], signature)
}

type accessClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  any    `json:"aud"`
	Expires   int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	ClientID  string `json:"client_id"`
	Scope     string `json:"scope"`
}

func VerifyAccessToken(ctx context.Context, client *http.Client, rawToken, subject, clientID, issuer, discoveryURL string, now time.Time) ([]string, error) {
	if len(rawToken) == 0 || len(rawToken) > maxJWTBytes {
		return nil, ErrInvalidToken
	}
	var claims accessClaims
	if err := verifySignedJWT(ctx, client, rawToken, issuer, discoveryURL, &claims); err != nil {
		return nil, ErrInvalidToken
	}
	if claims.Issuer != issuer || claims.Subject != subject || claims.ClientID != clientID || claims.Expires <= now.Unix() || (claims.NotBefore != 0 && claims.NotBefore > now.Unix()) || claims.IssuedAt > now.Add(2*time.Minute).Unix() || !hasAudience(claims.Audience, Resource) {
		return nil, ErrIdentityChanged
	}
	return requireScopes(claims.Scope)
}

func audienceCount(value any) int {
	switch v := value.(type) {
	case string:
		return 1
	case []any:
		return len(v)
	default:
		return 0
	}
}

func hasAudience(value any, expected string) bool {
	switch v := value.(type) {
	case string:
		return v == expected
	case []any:
		for _, entry := range v {
			if text, ok := entry.(string); ok && text == expected {
				return true
			}
		}
	}
	return false
}

func (r Registration) validateIdentity(expected VerifiedClaims, hostID string) error {
	if r.Issuer != expected.Issuer || r.Subject != expected.Subject || r.ClientID != expected.ClientID || r.HostID != hostID {
		return fmt.Errorf("%w: issuer, subject, client or host changed", ErrIdentityChanged)
	}
	return nil
}
