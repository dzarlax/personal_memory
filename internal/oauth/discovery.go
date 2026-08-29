package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var discoveryHTTPClient = newNoRedirectHTTPClient(nil)

func DiscoverJWKSURL(ctx context.Context, issuer string) (string, error) {
	if err := validateHTTPSURL("oauth issuer", issuer); err != nil {
		return "", err
	}
	metadataURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := discoveryHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openid discovery failed: %s", resp.Status)
	}

	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("openid discovery at %s did not include jwks_uri", metadataURL)
	}
	if err := validateHTTPSURL("openid discovery jwks_uri", doc.JWKSURI); err != nil {
		return "", err
	}
	return doc.JWKSURI, nil
}

func newNoRedirectHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	client := *base
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("oauth HTTP redirects are not allowed")
	}
	return &client
}

func validateHTTPSURL(name, raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s must be a valid HTTPS URL", name)
	}
	return nil
}
