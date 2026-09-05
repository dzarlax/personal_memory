package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

func TestActiveMemoryFiltersIncludeExplicitAndLegacyActive(t *testing.T) {
	filter := activeMemoryFilters(map[string]interface{}{"must": []map[string]interface{}{{"key": "namespace", "match": map[string]interface{}{"value": "projects"}}}})
	encoded, _ := json.Marshal(filter)
	for _, want := range []string{`"namespace"`, `"maintenance_status"`, `"active"`, `"is_empty"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("filter %s missing %s", encoded, want)
		}
	}
}

func TestActiveMemoryPayloadRejectsQuarantineAndMalformedEmptyStatus(t *testing.T) {
	if !activeMemoryPayload(map[string]interface{}{}) || !activeMemoryPayload(map[string]interface{}{"maintenance_status": "active"}) {
		t.Fatal("legacy and explicit active payloads must remain readable")
	}
	if activeMemoryPayload(map[string]interface{}{"maintenance_status": nil}) || activeMemoryPayload(map[string]interface{}{"maintenance_status": "quarantined", "quarantined_at": "2026-08-14T00:00:00Z", "quarantine_reason": "expired", "quarantine_batch_id": "batch"}) {
		t.Fatal("malformed and quarantined payloads must not remain readable")
	}
}

func TestForgetOldDryRunIsContentFreeAndReadOnly(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/points/scroll") {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"result":{"points":[{"id":1,"payload":{"text":"private fact body","created_at":"2020-01-01T00:00:00Z","recall_count":0}},{"id":"00000000-0000-0000-0000-000000000002","payload":{"text":"private expired body","created_at":"2020-01-01T00:00:00Z","valid_until":"2020-01-02","recall_count":0}}],"next_page_offset":null}}`))
	}))
	defer server.Close()
	srv := &Server{qdrant: qdrant.NewClient(server.URL, "memory"), cache: NewCache(time.Minute)}
	result, err := srv.forgetOld(context.Background(), toolRequest(map[string]interface{}{"days": float64(90)}))
	if err != nil || result.IsError {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	text := toolResultText(t, result)
	if requests != 1 || strings.Contains(text, "private") || !strings.Contains(text, "eligible_for_quarantine=1") {
		t.Fatalf("requests=%d output=%q", requests, text)
	}
}

func TestOrdinaryWritesDoNotReplaceInactiveDeterministicIDs(t *testing.T) {
	for _, operation := range []string{"store", "import"} {
		t.Run(operation, func(t *testing.T) {
			const fact = "same inactive fact"
			const namespace = "projects"
			pointID := PointID(namespace, fact)
			embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`[[0.1,0.2]]`))
			}))
			defer embedServer.Close()
			searches, writes := 0, 0
			qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/points/"+pointID):
					_, _ = w.Write([]byte(`{"result":{"id":"` + pointID + `","vector":[],"payload":{"text":"same inactive fact","namespace":"projects","maintenance_status":"quarantined","quarantined_at":"2026-08-14T00:00:00Z","quarantine_reason":"expired","quarantine_batch_id":"batch"}}}`))
				case strings.HasSuffix(r.URL.Path, "/points/search"):
					searches++
					_, _ = w.Write([]byte(`{"result":[]}`))
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
					writes++
					_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer qdrantServer.Close()
			srv := NewServer(qdrant.NewClient(qdrantServer.URL, "memory"), embeddings.NewClient(embedServer.URL), NewCache(time.Minute), "test", .97, .60, .90)
			if operation == "store" {
				result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": fact, "namespace": namespace}))
				if err != nil || !result.IsError {
					t.Fatalf("store result=%#v err=%v", result, err)
				}
				structured, ok := result.StructuredContent.(StoreFactResult)
				if !ok || structured.Status != "inactive_collision" {
					t.Fatalf("store structured result=%#v", result.StructuredContent)
				}
			} else {
				facts, _ := json.Marshal([]map[string]interface{}{{"text": fact, "namespace": namespace}})
				result, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{"facts": string(facts)}))
				structured, ok := result.StructuredContent.(ImportFactsResult)
				if err != nil || result.IsError || !ok || structured.Imported != 0 || len(structured.Outcomes) != 1 || structured.Outcomes[0].Status != "inactive_collision" {
					t.Fatalf("import result=%#v err=%v", result, err)
				}
			}
			if searches != 0 || writes != 0 {
				t.Fatalf("searches=%d writes=%d, want exact inactive refusal", searches, writes)
			}
		})
	}
}
