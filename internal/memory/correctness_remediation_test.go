package memory

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

func TestValidUntilIsStrictUTCCalendarAndMalformedExpiryIsNeverCurrent(t *testing.T) {
	expiry, err := parseUTCCalendarDate("2026-09-05")
	if err != nil || expiry.Location() != time.UTC {
		t.Fatalf("expiry=%v err=%v", expiry, err)
	}
	for _, value := range []string{"2026-9-05", "2026-09-5", "2026-02-30", "2026-09-05T00:00:00Z", ""} {
		if _, err := parseUTCCalendarDate(value); err == nil {
			t.Fatalf("accepted malformed date %q", value)
		}
	}
	payload := map[string]interface{}{"valid_until": "2026-09-05"}
	if factExpiredAt(payload, time.Date(2026, 9, 5, 23, 59, 59, 0, time.FixedZone("local", 7200))) {
		t.Fatal("fact expired on its UTC calendar date")
	}
	if !factExpiredAt(payload, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("fact remained current after its UTC expiry date")
	}
	if !factExpiredAt(map[string]interface{}{"valid_until": "tomorrow"}, time.Now()) {
		t.Fatal("malformed explicit expiry was treated as current")
	}
}

func TestImportInvalidExpiryReportsInvalidWithoutEmbeddingOrWrite(t *testing.T) {
	embedCalls, qdrantCalls := 0, 0
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedCalls++
		http.Error(w, "embedding should not be called", http.StatusInternalServerError)
	}))
	defer embedServer.Close()
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qdrantCalls++
		http.Error(w, "qdrant should not be called", http.StatusInternalServerError)
	}))
	defer qdrantServer.Close()

	srv := NewServer(qdrant.NewClient(qdrantServer.URL, "memory"), embeddings.NewClient(embedServer.URL), NewCache(time.Minute), "test", .97, .60, .90)
	result, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{
		"facts": `[{"text":"bad expiry","valid_until":"tomorrow"}]`,
	}))
	if err != nil || result.IsError {
		t.Fatalf("import=%#v err=%v", result, err)
	}
	structured, ok := result.StructuredContent.(ImportFactsResult)
	if !ok || structured.Imported != 0 || len(structured.Outcomes) != 1 || structured.Outcomes[0].Status != "invalid" {
		t.Fatalf("outcomes=%#v", result.StructuredContent)
	}
	if !strings.Contains(toolResultText(t, result), "status: invalid") || embedCalls != 0 || qdrantCalls != 0 {
		t.Fatalf("fallback=%q embed=%d qdrant=%d", toolResultText(t, result), embedCalls, qdrantCalls)
	}
}

func TestDuplicateResponseIncludesValidUntil(t *testing.T) {
	srv, _, _ := relatedResponseServer(t,
		`{"result":[{"id":"existing","score":0.99,"payload":{"text":"renew me","valid_until":"2026-12-31"}}]}`,
		`{"result":[]}`,
	)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "new fact"}))
	if err != nil || result.IsError {
		t.Fatalf("store=%#v err=%v", result, err)
	}
	structured := result.StructuredContent.(StoreFactResult)
	if structured.Duplicate == nil || structured.Duplicate.ValidUntil != "2026-12-31" {
		t.Fatalf("duplicate=%#v", structured.Duplicate)
	}
	if !strings.Contains(toolResultText(t, result), `"valid_until":"2026-12-31"`) {
		t.Fatalf("fallback omitted duplicate expiry: %q", toolResultText(t, result))
	}
}

func TestRecallSaturationIsNotReportedAsNormalEmpty(t *testing.T) {
	points := make([]string, lifecycleCandidateLimit(1))
	for i := range points {
		points[i] = fmt.Sprintf(`{"id":"%08d-0000-0000-0000-000000000000","score":0.9,"payload":{"text":"hidden","valid_until":"tomorrow"}}`, i)
	}
	for name, response := range map[string]string{
		"saturated": `{"result":[` + strings.Join(points, ",") + `]}`,
		"empty":     `{"result":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`[[0.1,0.2]]`))
			}))
			defer embedServer.Close()
			qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/points/search") {
					_, _ = w.Write([]byte(response))
					return
				}
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}))
			defer qdrantServer.Close()
			srv := NewServer(qdrant.NewClient(qdrantServer.URL, "memory"), embeddings.NewClient(embedServer.URL), NewCache(time.Minute), "test", .97, .60, .90)
			srv.Start(context.Background())
			defer func() { _ = srv.Shutdown(context.Background()) }()
			result, err := srv.recallFacts(context.Background(), toolRequest(map[string]interface{}{"query": "query", "limit": float64(1)}))
			if err != nil || result.IsError {
				t.Fatalf("recall=%#v err=%v", result, err)
			}
			structured := result.StructuredContent.(RecallFactsResult)
			wantSaturated := name == "saturated"
			if structured.Count != 0 || structured.CandidateWindowSaturated != wantSaturated {
				t.Fatalf("result=%#v", structured)
			}
			text := toolResultText(t, result)
			if strings.Contains(text, "saturated") != wantSaturated {
				t.Fatalf("fallback=%q want saturated=%v", text, wantSaturated)
			}
		})
	}
}

func TestDispatchedStoreAndDeleteErrorsInvalidateRecallCache(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[[0.1,0.2]]`))
		}))
		defer embedServer.Close()
		qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet:
				http.NotFound(w, r)
			case strings.HasSuffix(r.URL.Path, "/points/search"):
				_, _ = w.Write([]byte(`{"result":[]}`))
			case r.Method == http.MethodPut:
				http.Error(w, "response lost", http.StatusBadGateway)
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer qdrantServer.Close()
		cache := NewCache(time.Minute)
		cache.SetRecall("cached", RecallFactsResult{})
		srv := NewServer(qdrant.NewClient(qdrantServer.URL, "memory"), embeddings.NewClient(embedServer.URL), cache, "test", .97, .60, .90)
		result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "new fact"}))
		if err != nil || !result.IsError {
			t.Fatalf("store=%#v err=%v", result, err)
		}
		if _, found := cache.GetRecall("cached"); found {
			t.Fatal("ambiguous store left a stale cache entry")
		}
	})

	t.Run("delete", func(t *testing.T) {
		const pointID = "123"
		qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`{"result":{"id":123,"payload":{"text":"old"}}}`))
			case strings.HasSuffix(r.URL.Path, "/points/delete"):
				http.Error(w, "response lost", http.StatusBadGateway)
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer qdrantServer.Close()
		cache := NewCache(time.Minute)
		cache.SetRecall("cached", RecallFactsResult{})
		srv := &Server{qdrant: qdrant.NewClient(qdrantServer.URL, "memory"), cache: cache}
		result, err := srv.deleteFact(context.Background(), toolRequest(map[string]interface{}{"point_id": pointID}))
		if err != nil || !result.IsError {
			t.Fatalf("delete=%#v err=%v", result, err)
		}
		if _, found := cache.GetRecall("cached"); found {
			t.Fatal("ambiguous delete left a stale cache entry")
		}
	})
}

func TestDeterministicPointLookupFailureIsNotReportedAsCollision(t *testing.T) {
	for name, lookupResponse := range map[string]struct {
		status int
		body   string
	}{
		"unavailable": {status: http.StatusServiceUnavailable, body: `{"status":"error"}`},
		"malformed":   {status: http.StatusOK, body: `{"result":{}}`},
	} {
		t.Run(name, func(t *testing.T) {
			embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`[[0.1,0.2]]`))
			}))
			defer embedServer.Close()
			writes, searches := 0, 0
			qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					w.WriteHeader(lookupResponse.status)
					_, _ = w.Write([]byte(lookupResponse.body))
				case strings.HasSuffix(r.URL.Path, "/points/search"):
					searches++
					http.Error(w, "must not search", http.StatusInternalServerError)
				case r.Method == http.MethodPut:
					writes++
					http.Error(w, "must not write", http.StatusInternalServerError)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer qdrantServer.Close()
			srv := NewServer(qdrant.NewClient(qdrantServer.URL, "memory"), embeddings.NewClient(embedServer.URL), NewCache(time.Minute), "test", .97, .60, .90)

			store, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "same", "namespace": "projects"}))
			if err != nil || !store.IsError {
				t.Fatalf("store=%#v err=%v", store, err)
			}
			storeOutcome, ok := store.StructuredContent.(StoreFactResult)
			if !ok || storeOutcome.Status != "dependency_failed" || storeOutcome.Status == "collision" {
				t.Fatalf("store outcome=%#v", store.StructuredContent)
			}

			importResult, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{
				"facts": `[{"text":"same","namespace":"projects"}]`,
			}))
			if err != nil || importResult.IsError {
				t.Fatalf("import=%#v err=%v", importResult, err)
			}
			importOutcome := importResult.StructuredContent.(ImportFactsResult)
			if importOutcome.Imported != 0 || len(importOutcome.Outcomes) != 1 || importOutcome.Outcomes[0].Status != "dependency_failed" {
				t.Fatalf("import outcome=%#v", importOutcome)
			}
			if writes != 0 || searches != 0 {
				t.Fatalf("writes=%d searches=%d, want 0/0", writes, searches)
			}
		})
	}
}
