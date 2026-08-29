package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestNormalizeScopes(t *testing.T) {
	got := normalizeScopes("openid memory:mcp", []any{"memory:mcp", "profile"})
	want := []string{"openid", "memory:mcp", "profile"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %#v, got %#v", want, got)
	}
}

func TestMissingScopes(t *testing.T) {
	got := missingScopes([]string{"memory:mcp", "profile"}, []string{"memory:mcp"})
	want := []string{"profile"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %#v, got %#v", want, got)
	}
}

func TestNewJWTVerifierRequiresConfig(t *testing.T) {
	if _, err := NewJWTVerifier(JWTVerifierConfig{}); err == nil {
		t.Fatal("expected missing issuer error")
	}
	if _, err := NewJWTVerifier(JWTVerifierConfig{Issuer: "https://auth.example.com"}); err == nil {
		t.Fatal("expected missing audience error")
	}
	if _, err := NewJWTVerifier(JWTVerifierConfig{
		Issuer:   "https://auth.example.com",
		Audience: "https://mcp.example.com",
	}); err == nil {
		t.Fatal("expected missing jwks url error")
	}
	if _, err := NewJWTVerifier(JWTVerifierConfig{
		Issuer: "https://auth.example.com", Audience: "https://mcp.example.com", JWKSURL: "http://auth.example.com/jwks/",
	}); err == nil {
		t.Fatal("expected insecure jwks url error")
	}
}

func TestNewJWTVerifierPreservesIssuerAndSetsTimeout(t *testing.T) {
	verifier, err := NewJWTVerifier(JWTVerifierConfig{
		Issuer:   "https://auth.example.com/application/o/personal-memory/",
		Audience: "https://mcp.example.com",
		JWKSURL:  "https://auth.example.com/jwks/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.issuer != "https://auth.example.com/application/o/personal-memory/" {
		t.Fatalf("issuer should be preserved exactly, got %q", verifier.issuer)
	}
	if verifier.client == http.DefaultClient {
		t.Fatal("verifier should not use http.DefaultClient")
	}
	if verifier.client.Timeout == 0 {
		t.Fatal("verifier http client should have a timeout")
	}
}

func TestJWTVerifierRequiresExpirationClaim(t *testing.T) {
	verifier, key, jwks := testJWTVerifier(t, "https://auth.example.com/application/o/personal-memory/", "https://mcp.example.com", "memory:mcp")
	defer jwks.Close()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   "https://auth.example.com/application/o/personal-memory/",
		"aud":   "https://mcp.example.com",
		"sub":   "user",
		"scope": "memory:mcp",
		"iat":   time.Now().Unix(),
	})
	token.Header["kid"] = "test-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	_, err = verifier.Verify(context.Background(), signed)
	if err == nil {
		t.Fatal("expected token without exp to be rejected")
	}
	if !strings.Contains(err.Error(), "exp") {
		t.Fatalf("expected exp-related error, got %v", err)
	}
}

func TestJWTVerifierRejectsJWKSRedirect(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer jwks.Close()
	verifier, err := NewJWTVerifier(JWTVerifierConfig{
		Issuer: "https://auth.example.com/application/o/memory/", Audience: "https://mcp.example.com", JWKSURL: jwks.URL, Scopes: []string{"memory:mcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier.client = newNoRedirectHTTPClient(jwks.Client())
	if _, err := verifier.Verify(context.Background(), signedTestToken(t, verifier.Issuer(), "https://mcp.example.com", "memory:mcp", "user", key)); err == nil {
		t.Fatal("expected redirected jwks request to fail")
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target was requested %d times", targetHits.Load())
	}
}

type issuerVerifierFunc struct {
	issuer string
	verify func(context.Context, string) (*Claims, error)
}

func (f issuerVerifierFunc) Issuer() string { return f.issuer }

func (f issuerVerifierFunc) Verify(ctx context.Context, token string) (*Claims, error) {
	return f.verify(ctx, token)
}

func TestAnyVerifierAcceptsOnlyConfiguredVerifier(t *testing.T) {
	rejected := issuerVerifierFunc{issuer: "https://auth.example.com/rejected", verify: func(context.Context, string) (*Claims, error) { return nil, errors.New("rejected") }}
	accepted := issuerVerifierFunc{issuer: "https://auth.example.com/accepted", verify: func(context.Context, string) (*Claims, error) { return &Claims{Subject: "alexey"}, nil }}

	verifier, err := NewAnyVerifier(rejected, accepted)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(context.Background(), unsignedTokenForIssuer(t, accepted.issuer))
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "alexey" {
		t.Fatalf("subject = %q, want alexey", claims.Subject)
	}
}

func TestAnyVerifierRejectsEmptyOrUnacceptedSet(t *testing.T) {
	if _, err := NewAnyVerifier(); err == nil {
		t.Fatal("expected empty verifier set to fail")
	}
	verifier, err := NewAnyVerifier(issuerVerifierFunc{issuer: "https://auth.example.com/rejected", verify: func(context.Context, string) (*Claims, error) {
		return nil, errors.New("rejected")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), unsignedTokenForIssuer(t, "https://auth.example.com/rejected")); err == nil {
		t.Fatal("expected unaccepted token to fail")
	}
}

func TestAnyVerifierFetchesOnlyMatchingIssuerJWKS(t *testing.T) {
	const audience = "https://mcp.example.com"
	const scope = "memory:mcp"
	primary, primaryKey, primaryJWKS, primaryHits := countedJWTVerifier(t, "https://auth.example.com/application/o/chatgpt/", audience, scope, "primary-key")
	defer primaryJWKS.Close()
	gemini, geminiKey, geminiJWKS, geminiHits := countedJWTVerifier(t, "https://auth.example.com/application/o/gemini/", audience, scope, "gemini-key")
	defer geminiJWKS.Close()
	verifier, err := NewAnyVerifier(primary, gemini)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := verifier.Verify(context.Background(), signedTestTokenWithKID(t, gemini.Issuer(), audience, scope, "gemini-user", "gemini-key", geminiKey)); err != nil {
		t.Fatal(err)
	}
	if primaryHits.Load() != 0 || geminiHits.Load() != 1 {
		t.Fatalf("jwks requests primary=%d gemini=%d, want 0 and 1", primaryHits.Load(), geminiHits.Load())
	}
	if _, err := verifier.Verify(context.Background(), signedTestTokenWithKID(t, "https://auth.example.com/application/o/untrusted/", audience, scope, "untrusted-user", "primary-key", primaryKey)); err == nil {
		t.Fatal("expected unconfigured issuer to fail")
	}
	if primaryHits.Load() != 0 || geminiHits.Load() != 1 {
		t.Fatalf("unconfigured issuer triggered jwks requests: primary=%d gemini=%d", primaryHits.Load(), geminiHits.Load())
	}
}

func TestAnyVerifierAcceptsEitherConfiguredJWTIssuer(t *testing.T) {
	const audience = "https://mcp.example.com"
	const scope = "memory:mcp"

	primaryIssuer, primaryKey, primaryJWKS := testJWTVerifier(t, "https://auth.example.com/application/o/chatgpt/", audience, scope)
	defer primaryJWKS.Close()
	geminiIssuer, geminiKey, geminiJWKS := testJWTVerifier(t, "https://auth.example.com/application/o/gemini/", audience, scope)
	defer geminiJWKS.Close()

	verifier, err := NewAnyVerifier(primaryIssuer, geminiIssuer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		issuer  string
		key     *rsa.PrivateKey
		subject string
	}{
		{name: "primary", issuer: "https://auth.example.com/application/o/chatgpt/", key: primaryKey, subject: "chatgpt-user"},
		{name: "gemini", issuer: "https://auth.example.com/application/o/gemini/", key: geminiKey, subject: "gemini-user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := verifier.Verify(context.Background(), signedTestToken(t, tc.issuer, audience, scope, tc.subject, tc.key))
			if err != nil {
				t.Fatal(err)
			}
			if claims.Subject != tc.subject {
				t.Fatalf("subject = %q, want %q", claims.Subject, tc.subject)
			}
		})
	}

	wrongIssuerToken := signedTestToken(t, "https://auth.example.com/application/o/untrusted/", audience, scope, "untrusted-user", primaryKey)
	if _, err := verifier.Verify(context.Background(), wrongIssuerToken); err == nil {
		t.Fatal("expected token from an unconfigured issuer to fail")
	}
	wrongScopeToken := signedTestToken(t, "https://auth.example.com/application/o/gemini/", audience, "profile", "gemini-user", geminiKey)
	if _, err := verifier.Verify(context.Background(), wrongScopeToken); err == nil {
		t.Fatal("expected token without required scope to fail")
	}
}

func testJWTVerifier(t *testing.T, issuer, audience, scope string) (*JWTVerifier, *rsa.PrivateKey, *httptest.Server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{rsaJWK("test-key", &key.PublicKey)},
		})
	}))
	verifier, err := NewJWTVerifier(JWTVerifierConfig{
		Issuer: issuer, Audience: audience, JWKSURL: jwks.URL, Scopes: []string{scope},
	})
	if err != nil {
		jwks.Close()
		t.Fatal(err)
	}
	verifier.client = newNoRedirectHTTPClient(jwks.Client())
	return verifier, key, jwks
}

func countedJWTVerifier(t *testing.T, issuer, audience, scope, kid string) (*JWTVerifier, *rsa.PrivateKey, *httptest.Server, *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	hits := &atomic.Int32{}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{rsaJWK(kid, &key.PublicKey)}})
	}))
	verifier, err := NewJWTVerifier(JWTVerifierConfig{Issuer: issuer, Audience: audience, JWKSURL: jwks.URL, Scopes: []string{scope}})
	if err != nil {
		jwks.Close()
		t.Fatal(err)
	}
	verifier.client = newNoRedirectHTTPClient(jwks.Client())
	return verifier, key, jwks, hits
}

func signedTestToken(t *testing.T, issuer, audience, scope, subject string, key *rsa.PrivateKey) string {
	return signedTestTokenWithKID(t, issuer, audience, scope, subject, "test-key", key)
}

func signedTestTokenWithKID(t *testing.T, issuer, audience, scope, subject, kid string, key *rsa.PrivateKey) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer, "aud": audience, "sub": subject, "scope": scope,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func unsignedTokenForIssuer(t *testing.T, issuer string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": issuer})
	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func rsaJWK(kid string, key *rsa.PublicKey) map[string]string {
	exponent := bigEndianInt(key.E)
	return map[string]string{
		"kid": kid,
		"kty": "RSA",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(exponent),
	}
}

func bigEndianInt(v int) []byte {
	var out []byte
	for v > 0 {
		out = append([]byte{byte(v)}, out...)
		v >>= 8
	}
	return out
}
