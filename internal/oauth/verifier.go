package oauth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	Subject string
	Scopes  []string
}

type TokenVerifier interface {
	Verify(ctx context.Context, token string) (*Claims, error)
}

// AnyVerifier accepts a token only when one explicitly configured verifier
// validates its signature, issuer, audience, expiry, and required scopes.
type AnyVerifier struct {
	verifiers []TokenVerifier
}

func NewAnyVerifier(verifiers ...TokenVerifier) (*AnyVerifier, error) {
	filtered := make([]TokenVerifier, 0, len(verifiers))
	for _, verifier := range verifiers {
		if verifier != nil {
			filtered = append(filtered, verifier)
		}
	}
	if len(filtered) == 0 {
		return nil, errors.New("at least one token verifier is required")
	}
	return &AnyVerifier{verifiers: filtered}, nil
}

func (v *AnyVerifier) Verify(ctx context.Context, token string) (*Claims, error) {
	for _, verifier := range v.verifiers {
		claims, err := verifier.Verify(ctx, token)
		if err == nil {
			return claims, nil
		}
	}
	return nil, errors.New("token was not accepted by a configured issuer")
}

type JWTVerifierConfig struct {
	Issuer   string
	Audience string
	JWKSURL  string
	Scopes   []string
}

type JWTVerifier struct {
	issuer   string
	audience string
	jwksURL  string
	scopes   []string
	client   *http.Client

	mu       sync.Mutex
	keys     map[string]any
	fetched  time.Time
	cacheTTL time.Duration
}

type jwtClaims struct {
	Scope  string `json:"scope"`
	Scopes any    `json:"scopes"`
	jwt.RegisteredClaims
}

func NewJWTVerifier(cfg JWTVerifierConfig) (*JWTVerifier, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("oauth issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("oauth audience is required")
	}
	if cfg.JWKSURL == "" {
		return nil, errors.New("oauth jwks url is required")
	}
	return &JWTVerifier{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		jwksURL:  cfg.JWKSURL,
		scopes:   cfg.Scopes,
		client:   &http.Client{Timeout: 5 * time.Second},
		cacheTTL: 10 * time.Minute,
	}, nil
}

func (v *JWTVerifier) Verify(ctx context.Context, token string) (*Claims, error) {
	claims := &jwtClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unsupported signing method %s", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing jwt kid")
		}
		return v.key(ctx, kid)
	}, jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, errors.New("invalid token")
	}

	tokenScopes := normalizeScopes(claims.Scope, claims.Scopes)
	if missing := missingScopes(v.scopes, tokenScopes); len(missing) > 0 {
		return nil, fmt.Errorf("missing required scopes: %s", strings.Join(missing, ", "))
	}

	return &Claims{Subject: claims.Subject, Scopes: tokenScopes}, nil
}

func (v *JWTVerifier) key(ctx context.Context, kid string) (any, error) {
	keys, err := v.cachedKeys(ctx)
	if err != nil {
		return nil, err
	}
	key, ok := keys[kid]
	if !ok {
		if err := v.refreshKeys(ctx); err != nil {
			return nil, err
		}
		keys, _ = v.cachedKeys(ctx)
		key, ok = keys[kid]
	}
	if !ok {
		return nil, fmt.Errorf("jwks key %q not found", kid)
	}
	return key, nil
}

func (v *JWTVerifier) cachedKeys(ctx context.Context) (map[string]any, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.keys) > 0 && time.Since(v.fetched) < v.cacheTTL {
		return v.keys, nil
	}
	if err := v.fetchKeysLocked(ctx); err != nil {
		return nil, err
	}
	return v.keys, nil
}

func (v *JWTVerifier) refreshKeys(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.fetchKeysLocked(ctx)
}

func (v *JWTVerifier) fetchKeysLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jwks request failed: %s", resp.Status)
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}
	keys := make(map[string]any, len(doc.Keys))
	for _, key := range doc.Keys {
		if key.Kty != "RSA" || (key.Use != "" && key.Use != "sig") {
			continue
		}
		pub, err := key.rsaPublicKey()
		if err != nil {
			return err
		}
		keys[key.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("jwks contained no usable RSA signing keys")
	}
	v.keys = keys
	v.fetched = time.Now()
	return nil
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwk) rsaPublicKey() (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode jwk n: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode jwk e: %w", err)
	}
	e := 0
	for _, b := range eb {
		e = e<<8 + int(b)
	}
	if e == 0 {
		return nil, errors.New("invalid jwk exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, nil
}

func normalizeScopes(spaceSeparated string, raw any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(scope string) {
		scope = strings.TrimSpace(scope)
		if scope != "" && !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	for _, scope := range strings.Fields(spaceSeparated) {
		add(scope)
	}
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	}
	return out
}

func missingScopes(required, actual []string) []string {
	have := map[string]bool{}
	for _, scope := range actual {
		have[scope] = true
	}
	var missing []string
	for _, scope := range required {
		if scope != "" && !have[scope] {
			missing = append(missing, scope)
		}
	}
	return missing
}
