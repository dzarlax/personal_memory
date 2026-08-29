package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDiscoverJWKSURLRejectsInsecureEndpointsAndRedirects(t *testing.T) {
	t.Run("http issuer", func(t *testing.T) {
		if _, err := DiscoverJWKSURL(context.Background(), "http://auth.example.com/application/o/memory/"); err == nil || !strings.Contains(err.Error(), "valid HTTPS URL") {
			t.Fatalf("DiscoverJWKSURL() error = %v, want HTTPS issuer rejection", err)
		}
	})
	t.Run("http jwks uri", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jwks_uri":"http://auth.example.com/jwks"}`))
		}))
		defer server.Close()
		withDiscoveryClient(t, server.Client())
		if _, err := DiscoverJWKSURL(context.Background(), server.URL); err == nil || !strings.Contains(err.Error(), "valid HTTPS URL") {
			t.Fatalf("DiscoverJWKSURL() error = %v, want HTTPS jwks_uri rejection", err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var targetHits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
		defer target.Close()
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer server.Close()
		withDiscoveryClient(t, server.Client())
		if _, err := DiscoverJWKSURL(context.Background(), server.URL); err == nil {
			t.Fatal("expected redirect to fail")
		}
		if targetHits.Load() != 0 {
			t.Fatalf("redirect target was requested %d times", targetHits.Load())
		}
	})
}

func withDiscoveryClient(t *testing.T, client *http.Client) {
	t.Helper()
	previous := discoveryHTTPClient
	discoveryHTTPClient = newNoRedirectHTTPClient(client)
	t.Cleanup(func() { discoveryHTTPClient = previous })
}
