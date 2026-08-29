package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/config"
)

func TestNewEmbeddingCollectionsFollowsRAGFeatureGate(t *testing.T) {
	tests := []struct {
		name      string
		enableRAG bool
		want      []string
	}{
		{name: "memory only", want: []string{"memory"}},
		{name: "memory and RAG", enableRAG: true, want: []string{"memory", "custom_chunks", "custom_folders"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				QdrantURL:            "http://qdrant.test",
				EnableRAG:            tt.enableRAG,
				RAGCollectionChunks:  "custom_chunks",
				RAGCollectionFolders: "custom_folders",
			}
			memoryClient, chunksClient, foldersClient, all := newEmbeddingCollections(cfg)
			if memoryClient == nil {
				t.Fatal("memory client is nil")
			}
			if !tt.enableRAG && (chunksClient != nil || foldersClient != nil) {
				t.Fatal("disabled RAG created document collection clients")
			}
			if tt.enableRAG && (chunksClient == nil || foldersClient == nil) {
				t.Fatal("enabled RAG did not create document collection clients")
			}
			if got := collectionNames(all); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("collection names = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShutdownHTTPServerWaitsForActiveRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	ts.Start()
	defer ts.Close()

	requestDone := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Get(ts.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()
	<-started

	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { shutdownDone <- shutdownHTTPServer(ctx, ts.Config) }()

	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before active request completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
}

func TestServerTimeoutsAreBounded(t *testing.T) {
	if httpReadHeaderTimeout <= 0 || httpReadTimeout <= 0 || httpIdleTimeout <= 0 || shutdownTimeout <= 0 {
		t.Fatalf("server timeouts must be positive: read_header=%s read=%s idle=%s shutdown=%s", httpReadHeaderTimeout, httpReadTimeout, httpIdleTimeout, shutdownTimeout)
	}
	if mcpRequestBodyLimit <= 0 {
		t.Fatalf("MCP request body limit must be positive: %d", mcpRequestBodyLimit)
	}
}

func TestMemoryAuthRequiredOnlyBypassesForExplicitEmptyDevelopmentMode(t *testing.T) {
	for _, tt := range []struct {
		name          string
		apiKey        string
		oauth         bool
		allowInsecure bool
		want          bool
	}{
		{name: "default empty config stays protected", want: true},
		{name: "explicit insecure empty config", allowInsecure: true, want: false},
		{name: "api key still applies in insecure mode", apiKey: "key", allowInsecure: true, want: true},
		{name: "oauth still applies in insecure mode", oauth: true, allowInsecure: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := memoryAuthRequired(tt.apiKey, tt.oauth, tt.allowInsecure); got != tt.want {
				t.Fatalf("memoryAuthRequired()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestMCPEndpointHandlerAcceptsHeadWithoutCallingMCP(t *testing.T) {
	called := false
	handler := mcpEndpointHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodHead, "/memory", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d", rec.Code, http.StatusOK)
	}
	if called {
		t.Fatal("HEAD probe reached the MCP handler")
	}
}

func TestMCPEndpointHandlerDelegatesMCPMethods(t *testing.T) {
	called := false
	handler := mcpEndpointHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodPost, "/memory", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if !called {
		t.Fatal("POST did not reach the MCP handler")
	}
}
