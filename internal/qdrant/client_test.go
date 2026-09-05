package qdrant

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMutationsWaitForCompletionAndValidateStatus(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		statusCode int
		wantError  bool
	}{
		{name: "completed", response: `{"status":"ok","result":{"status":"completed"}}`, statusCode: http.StatusOK},
		{name: "acknowledged is not durable completion", response: `{"status":"ok","result":{"status":"acknowledged"}}`, statusCode: http.StatusOK, wantError: true},
		{name: "application failure", response: `{"status":"failed","result":{"status":"completed"}}`, statusCode: http.StatusOK, wantError: true},
		{name: "missing operation completion", response: `{"status":"ok"}`, statusCode: http.StatusOK, wantError: true},
		{name: "HTTP failure", response: `{"status":"ok"}`, statusCode: http.StatusBadGateway, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("wait") != "true" {
					t.Fatalf("wait query = %q, want true", r.URL.Query().Get("wait"))
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()
			err := NewClient(server.URL, "memory").Upsert(context.Background(), Point{ID: "id", Vector: []float32{1}})
			if (err != nil) != tt.wantError {
				t.Fatalf("Upsert error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestNewClientHasBoundedHTTPTimeout(t *testing.T) {
	client := NewClient("http://example.test", "memory")
	if client.httpClient.Timeout != defaultHTTPTimeout || client.httpClient.Timeout <= 0 {
		t.Fatalf("HTTP timeout = %s, want %s", client.httpClient.Timeout, defaultHTTPTimeout)
	}
}

func TestReadOperationsRejectSuccessfulEnvelopeWithoutUsableResult(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"error"}`, `{"result":null}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			client := NewClient(server.URL, "memory")
			if _, err := client.Search(context.Background(), []float32{1}, 1, nil, nil); err == nil {
				t.Fatal("Search accepted unusable result envelope")
			}
			if _, err := client.Scroll(context.Background(), 1, nil, nil, false); err == nil {
				t.Fatal("Scroll accepted unusable result envelope")
			}
			if _, _, err := client.Get(context.Background(), "1"); err == nil {
				t.Fatal("Get accepted unusable result envelope")
			}
		})
	}
}

func TestScrollRequiresExplicitPointsArray(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		wantError bool
	}{
		{name: "missing points", response: `{"result":{}}`, wantError: true},
		{name: "offset without points", response: `{"result":{"next_page_offset":null}}`, wantError: true},
		{name: "explicit empty page", response: `{"result":{"points":[],"next_page_offset":null}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()
			result, err := NewClient(server.URL, "memory").Scroll(context.Background(), 1, nil, nil, false)
			if (err != nil) != tt.wantError {
				t.Fatalf("Scroll error = %v, wantError %v", err, tt.wantError)
			}
			if err == nil && (result == nil || len(result.Points) != 0 || result.RawOffset != nil) {
				t.Fatalf("Scroll result = %#v", result)
			}
		})
	}
}

func TestCollectionName(t *testing.T) {
	if got := NewClient("http://example.test", "doc_chunks").CollectionName(); got != "doc_chunks" {
		t.Fatalf("CollectionName() = %q, want doc_chunks", got)
	}
}

func TestCollectionInfoExisting(t *testing.T) {
	const points = uint64(18446744073709551615)
	wantMetadata := map[string]any{
		"embedding": map[string]any{
			"model":    "BAAI/bge-small-en-v1.5",
			"revision": "abc123",
		},
		"dimensions": float64(384),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/collections/memory" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"ok",
			"result":{
				"points_count":18446744073709551615,
				"config":{
					"params":{"vectors":{"size":384,"distance":"Cosine"}},
					"metadata":{"embedding":{"model":"BAAI/bge-small-en-v1.5","revision":"abc123"},"dimensions":384}
				}
			}
		}`))
	}))
	defer server.Close()

	info, err := NewClient(server.URL, "memory").CollectionInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || info.Points != points || info.VectorSize != 384 {
		t.Fatalf("CollectionInfo() = %#v", info)
	}
	if !reflect.DeepEqual(info.Metadata, wantMetadata) {
		t.Fatalf("metadata = %#v, want %#v", info.Metadata, wantMetadata)
	}
}

func TestCollectionInfoMissing(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	info, err := NewClient(server.URL, "missing").CollectionInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Exists || info.Points != 0 || info.VectorSize != 0 || info.Metadata != nil {
		t.Fatalf("missing CollectionInfo() = %#v", info)
	}
}

func TestCollectionInfoErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{name: "HTTP error", statusCode: http.StatusBadGateway, body: `{"status":"error"}`, want: "status 502"},
		{name: "malformed response", statusCode: http.StatusOK, body: `{"result":{"points_count":"many"}}`, want: "decode collection info"},
		{name: "missing point count", statusCode: http.StatusOK, body: `{"result":{"config":{"params":{"vectors":{"size":384}}}}}`, want: "points_count is required"},
		{name: "null point count", statusCode: http.StatusOK, body: `{"result":{"points_count":null,"config":{"params":{"vectors":{"size":384}}}}}`, want: "points_count is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, err := NewClient(server.URL, "memory").CollectionInfo(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CollectionInfo() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestExactCount(t *testing.T) {
	const points = uint64(18446744073709551615)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/memory/points/count" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if exact, ok := body["exact"].(bool); !ok || !exact || len(body) != 1 {
			t.Fatalf("request body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"status":"ok","result":{"count":18446744073709551615}}`))
	}))
	defer server.Close()

	got, err := NewClient(server.URL, "memory").ExactCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != points {
		t.Fatalf("ExactCount() = %d, want %d", got, points)
	}
}

func TestExactCountErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{name: "HTTP error", statusCode: http.StatusBadGateway, body: `{"status":"error"}`, want: "count collection points"},
		{name: "malformed response", statusCode: http.StatusOK, body: `{"result":{"count":"many"}}`, want: "decode exact count response"},
		{name: "missing count", statusCode: http.StatusOK, body: `{"result":{}}`, want: "count is required"},
		{name: "null count", statusCode: http.StatusOK, body: `{"result":{"count":null}}`, want: "count is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, err := NewClient(server.URL, "memory").ExactCount(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ExactCount() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCreateAndUpdateCollectionMetadataContracts(t *testing.T) {
	metadata := map[string]any{
		"embedding": map[string]any{"model": "model-id", "revision": "revision-id"},
	}
	type request struct {
		method string
		body   map[string]any
	}
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, request{method: r.Method, body: body})
		if r.URL.Path != "/collections/memory" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"ok","result":true}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "memory")
	if err := client.CreateCollection(context.Background(), 384, metadata); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateCollectionMetadata(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateCollectionMetadata(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	if requests[0].method != http.MethodPut {
		t.Fatalf("create method = %s, want PUT", requests[0].method)
	}
	vectors, ok := requests[0].body["vectors"].(map[string]any)
	if !ok || vectors["size"] != float64(384) || vectors["distance"] != "Cosine" {
		t.Fatalf("create vectors = %#v", requests[0].body["vectors"])
	}
	if !reflect.DeepEqual(requests[0].body["metadata"], metadata) {
		t.Fatalf("create metadata = %#v, want %#v", requests[0].body["metadata"], metadata)
	}
	if requests[1].method != http.MethodPatch {
		t.Fatalf("metadata update method = %s, want PATCH", requests[1].method)
	}
	if len(requests[1].body) != 1 || !reflect.DeepEqual(requests[1].body["metadata"], metadata) {
		t.Fatalf("metadata update body = %#v", requests[1].body)
	}
	emptyMetadata, ok := requests[2].body["metadata"].(map[string]any)
	if len(requests[2].body) != 1 || !ok || len(emptyMetadata) != 0 {
		t.Fatalf("nil metadata update body = %#v, want empty metadata object", requests[2].body)
	}
}

func TestDeleteCollection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/collections/eval_test" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("timeout") != "15" {
			t.Fatalf("timeout = %q, want 15", r.URL.Query().Get("timeout"))
		}
		_, _ = w.Write([]byte(`{"status":"ok","result":true}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL, "eval_test").DeleteCollection(context.Background(), "eval_"); err != nil {
		t.Fatal(err)
	}
	if err := NewClient(server.URL, "memory").DeleteCollection(context.Background(), "eval_"); err == nil {
		t.Fatal("DeleteCollection() allowed a collection outside the required prefix")
	}
	if err := NewClient(server.URL, "eval_test").DeleteCollection(context.Background(), ""); err == nil {
		t.Fatal("DeleteCollection() allowed an empty required prefix")
	}
}

func TestDeleteCollectionIsIdempotentOnNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	if err := NewClient(server.URL, "eval_missing").DeleteCollection(
		context.Background(), "eval_",
	); err != nil {
		t.Fatalf("DeleteCollection() 404 error = %v", err)
	}
}

func TestUpsertWithPointIDPreservesKind(t *testing.T) {
	var ids []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Points []struct {
				ID any `json:"id"`
			} `json:"points"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, body.Points[0].ID)
		_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "eval_test")
	point := Point{ID: "42", Vector: []float32{1}}
	if err := client.UpsertWithPointID(context.Background(), point, true); err != nil {
		t.Fatal(err)
	}
	if err := client.UpsertWithPointID(context.Background(), point, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := ids[0].(float64); !ok {
		t.Fatalf("numeric ID decoded as %T", ids[0])
	}
	if _, ok := ids[1].(string); !ok {
		t.Fatalf("string ID decoded as %T", ids[1])
	}
	nonNumeric := Point{ID: "11111111-1111-4111-8111-111111111111", Vector: []float32{1}}
	if err := client.UpsertWithPointID(context.Background(), nonNumeric, true); err == nil {
		t.Fatal("UpsertWithPointID() accepted a non-numeric ID as numeric")
	}
}

func TestCollectionMutationErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "HTTP error", statusCode: http.StatusInternalServerError, body: `{"status":"error"}`},
		{name: "Qdrant error", statusCode: http.StatusOK, body: `{"status":"error","result":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client := NewClient(server.URL, "memory")
			if err := client.CreateCollection(context.Background(), 384, nil); err == nil {
				t.Fatal("CreateCollection() expected error")
			}
			if err := client.UpdateCollectionMetadata(context.Background(), map[string]any{"model": "id"}); err == nil {
				t.Fatal("UpdateCollectionMetadata() expected error")
			}
		})
	}
}

func TestReadLimitedBodyRejectsOversizedResponse(t *testing.T) {
	_, err := readLimitedBody(strings.NewReader("too large"), 3)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversized response error, got %v", err)
	}
}

func TestAllPointMutationsUseWaitTrue(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, fmt.Sprintf("%s %s wait=%s", r.Method, r.URL.Path, r.URL.Query().Get("wait")))
		_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "memory")
	mutations := []func() error{
		func() error { return client.Upsert(context.Background(), Point{ID: "id", Vector: []float32{1}}) },
		func() error { return client.Delete(context.Background(), []string{"id"}) },
		func() error {
			return client.DeleteByFilter(context.Background(), map[string]interface{}{"must": []interface{}{}})
		},
		func() error { return client.SetPayload(context.Background(), "id", map[string]interface{}{"x": 1}) },
		func() error { return client.CreateFieldIndex(context.Background(), "namespace", "keyword") },
	}
	for _, mutation := range mutations {
		if err := mutation(); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range requests {
		if !strings.HasSuffix(request, "wait=true") {
			t.Fatalf("mutation request missing wait=true: %s", request)
		}
	}
}

func TestQdrantPointID_NumericString(t *testing.T) {
	got := qdrantPointID("12345")
	if got != uint64(12345) {
		t.Fatalf("qdrantPointID numeric = %#v, want uint64(12345)", got)
	}
}

func TestQdrantPointID_MaxUint64(t *testing.T) {
	const id = "18446744073709551615"
	got := qdrantPointID(id)
	if got != uint64(18446744073709551615) {
		t.Fatalf("qdrantPointID numeric = %#v, want max uint64", got)
	}

	encoded, err := json.Marshal(map[string]interface{}{"id": got})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"id":18446744073709551615}` {
		t.Fatalf("encoded ID = %s", encoded)
	}
}

func TestQdrantPointID_UUIDString(t *testing.T) {
	id := "4f08ef2a-42c0-45df-a6c3-5ca86db4ddf8"
	got := qdrantPointID(id)
	if got != id {
		t.Fatalf("qdrantPointID uuid = %#v, want %q", got, id)
	}
}

func TestSetPayloadUsesPartialUpdateEndpoint(t *testing.T) {
	var method string
	var path string
	var body struct {
		Payload map[string]interface{} `json:"payload"`
		Points  []interface{}          `json:"points"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "memory")
	if err := client.SetPayload(context.Background(), "12345", map[string]interface{}{"recall_count": 2}); err != nil {
		t.Fatalf("SetPayload returned error: %v", err)
	}

	if method != http.MethodPost {
		t.Fatalf("SetPayload method = %s, want POST partial update", method)
	}
	if path != "/collections/memory/points/payload" {
		t.Fatalf("SetPayload path = %s", path)
	}
	if got := body.Payload["recall_count"]; got != float64(2) {
		t.Fatalf("payload recall_count = %#v, want 2", got)
	}
	if len(body.Points) != 1 || body.Points[0] != float64(12345) {
		t.Fatalf("points = %#v, want [12345]", body.Points)
	}
}

func TestReplaceLifecyclePayloadUsesOrderedStrongBatch(t *testing.T) {
	type capturedRequest struct {
		method   string
		path     string
		wait     string
		ordering string
		body     map[string]interface{}
	}
	var requests []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		requests = append(requests, capturedRequest{
			method:   r.Method,
			path:     r.URL.Path,
			wait:     r.URL.Query().Get("wait"),
			ordering: r.URL.Query().Get("ordering"),
			body:     body,
		})
		_, _ = w.Write([]byte(`{"status":"ok","result":[{"status":"completed"}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "memory")
	set := map[string]interface{}{
		"lifecycle_state": "current",
		"canonical":       false,
		"supersedes":      []string{},
		"superseded_by":   []string{},
	}
	for _, id := range []string{"12345", "4f08ef2a-42c0-45df-a6c3-5ca86db4ddf8"} {
		if err := client.ReplaceLifecyclePayload(context.Background(), id, set, []string{"provenance", "verified_at"}); err != nil {
			t.Fatalf("ReplaceLifecyclePayload(%q): %v", id, err)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if request.method != http.MethodPost || request.path != "/collections/memory/points/batch" {
			t.Fatalf("unexpected request: %s %s", request.method, request.path)
		}
		if request.wait != "true" || request.ordering != "strong" {
			t.Fatalf("query wait=%q ordering=%q", request.wait, request.ordering)
		}
		operations, ok := request.body["operations"].([]interface{})
		if !ok || len(operations) != 2 {
			t.Fatalf("operations = %#v, want set + delete", request.body["operations"])
		}
	}
	numericOperations := requests[0].body["operations"].([]interface{})
	numericSet := numericOperations[0].(map[string]interface{})["set_payload"].(map[string]interface{})
	if got := numericSet["points"].([]interface{})[0]; got != float64(12345) {
		t.Fatalf("numeric point ID = %#v, want 12345", got)
	}
	uuidOperations := requests[1].body["operations"].([]interface{})
	uuidSet := uuidOperations[0].(map[string]interface{})["set_payload"].(map[string]interface{})
	if got := uuidSet["points"].([]interface{})[0]; got != "4f08ef2a-42c0-45df-a6c3-5ca86db4ddf8" {
		t.Fatalf("UUID point ID = %#v", got)
	}
}

func TestReplaceLifecyclePayloadRejectsUnrelatedKeysBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	err := NewClient(server.URL, "memory").ReplaceLifecyclePayload(
		context.Background(),
		"1",
		map[string]interface{}{"text": "must not be rewritten"},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "not lifecycle metadata") {
		t.Fatalf("error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestMaintenanceMutationsUseCompleteTypedStrongBatches(t *testing.T) {
	var got []struct {
		wait     string
		ordering string
		body     map[string]interface{}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := struct {
			wait     string
			ordering string
			body     map[string]interface{}
		}{wait: r.URL.Query().Get("wait"), ordering: r.URL.Query().Get("ordering")}
		if err := json.NewDecoder(r.Body).Decode(&request.body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		got = append(got, request)
		_, _ = w.Write([]byte(`{"status":"ok","result":[{"status":"completed"},{"status":"completed"}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "memory")
	if err := client.QuarantineMaintenance(context.Background(), "12345", time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC), "expired", "batch"); err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreMaintenance(context.Background(), "4f08ef2a-42c0-45df-a6c3-5ca86db4ddf8"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].wait != "true" || got[0].ordering != "strong" || got[1].wait != "true" || got[1].ordering != "strong" {
		t.Fatalf("requests = %#v", got)
	}
	operations := got[0].body["operations"].([]interface{})
	set := operations[0].(map[string]interface{})["set_payload"].(map[string]interface{})
	if payload := set["payload"].(map[string]interface{}); !reflect.DeepEqual(payload, map[string]interface{}{"maintenance_status": "quarantined", "quarantined_at": "2026-08-14T10:00:00Z", "quarantine_reason": "expired", "quarantine_batch_id": "batch"}) {
		t.Fatalf("set payload = %#v", set["payload"])
	}
	if point := set["points"].([]interface{})[0]; point != float64(12345) {
		t.Fatalf("point = %#v", point)
	}
	restoreOperations := got[1].body["operations"].([]interface{})
	restoreSet := restoreOperations[0].(map[string]interface{})["set_payload"].(map[string]interface{})
	if !reflect.DeepEqual(restoreSet["payload"], map[string]interface{}{"maintenance_status": "active"}) {
		t.Fatalf("restore set = %#v", restoreSet)
	}
	restoreDelete := restoreOperations[1].(map[string]interface{})["delete_payload"].(map[string]interface{})
	if !reflect.DeepEqual(restoreDelete["keys"], []interface{}{"quarantined_at", "quarantine_reason", "quarantine_batch_id"}) {
		t.Fatalf("restore delete = %#v", restoreDelete)
	}
	for _, invalid := range []func() error{
		func() error {
			return client.QuarantineMaintenance(context.Background(), "", time.Now(), "expired", "batch")
		},
		func() error {
			return client.QuarantineMaintenance(context.Background(), "1", time.Time{}, "expired", "batch")
		},
		func() error {
			return client.QuarantineMaintenance(context.Background(), "1", time.Now(), "unknown", "batch")
		},
		func() error {
			return client.QuarantineMaintenance(context.Background(), "1", time.Now(), "expired", "")
		},
		func() error { return client.RestoreMaintenance(context.Background(), "") },
		func() error { return client.RestoreMaintenance(context.Background(), " 1") },
	} {
		if err := invalid(); err == nil {
			t.Fatal("expected typed validation error")
		}
	}
}

func TestPurgePrimitivesUseTypedSnapshotsAndExactStrongDelete(t *testing.T) {
	var deleteBody map[string]interface{}
	downloadCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/collections/memory/snapshots":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"name":"fresh.snapshot"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/collections/memory/snapshots":
			_, _ = w.Write([]byte(`{"status":"ok","result":[{"name":"fresh.snapshot"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/collections/memory/snapshots/fresh.snapshot":
			downloadCalls++
			_, _ = w.Write([]byte("snapshot bytes"))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/memory/points/delete":
			if r.URL.Query().Get("wait") != "true" || r.URL.Query().Get("ordering") != "strong" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			if err := json.NewDecoder(r.Body).Decode(&deleteBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "memory")
	snapshot, err := client.CreateSnapshotIdentity(context.Background())
	if err != nil || snapshot.Name != "fresh.snapshot" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	snapshots, err := client.ListSnapshotIdentities(context.Background())
	if err != nil || len(snapshots) != 1 || snapshots[0] != snapshot {
		t.Fatalf("snapshots=%#v err=%v", snapshots, err)
	}
	archive := filepath.Join(t.TempDir(), "purge-recovery.snapshot")
	digest, err := client.EnsureSnapshotArchive(context.Background(), snapshot, archive, "")
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("snapshot bytes")))
	if digest != wantDigest || downloadCalls != 1 {
		t.Fatalf("digest=%q downloads=%d", digest, downloadCalls)
	}
	info, err := os.Stat(archive)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive info=%v err=%v", info, err)
	}
	if got, err := client.EnsureSnapshotArchive(context.Background(), snapshot, archive, digest); err != nil || got != digest || downloadCalls != 1 {
		t.Fatalf("verified digest=%q downloads=%d err=%v", got, downloadCalls, err)
	}
	if err := os.WriteFile(archive, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EnsureSnapshotArchive(context.Background(), snapshot, archive, digest); err == nil {
		t.Fatal("accepted a tampered recovery archive")
	}
	if err := client.DeleteExactStrong(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	points := deleteBody["points"].([]interface{})
	if len(points) != 1 || points[0] != float64(42) {
		t.Fatalf("points=%#v", points)
	}
	if err := client.DeleteExactStrong(context.Background(), " "); err == nil {
		t.Fatal("accepted invalid exact ID")
	}
}

func TestEnsureSnapshotArchiveRejectsUnsafeParentsBeforeDownload(t *testing.T) {
	root := t.TempDir()
	managedParent := filepath.Join(root, "managed")
	if err := os.Mkdir(managedParent, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkParent := filepath.Join(root, "link")
	if err := os.Symlink(managedParent, symlinkParent); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSnapshotArchivePath(filepath.Join(symlinkParent, "recovery.snapshot"), managedParent); err == nil {
		t.Fatal("accepted a symlink redirect into the managed snapshot directory")
	}

	downloadCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadCalls++
		_, _ = w.Write([]byte("snapshot bytes"))
	}))
	defer server.Close()
	client := NewClient(server.URL, "memory")
	snapshot := SnapshotIdentity{Name: "fresh.snapshot"}

	tests := []struct {
		name        string
		destination string
	}{
		{name: "missing parent", destination: filepath.Join(root, "missing", "recovery.snapshot")},
		{name: "managed qdrant directory", destination: "/qdrant/snapshots/recovery.snapshot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.EnsureSnapshotArchive(context.Background(), snapshot, test.destination, ""); err == nil {
				t.Fatalf("accepted unsafe destination %q", test.destination)
			}
		})
	}
	if downloadCalls != 0 {
		t.Fatalf("download calls = %d, want 0", downloadCalls)
	}
	if _, err := os.Stat(filepath.Join(managedParent, "recovery.snapshot")); !os.IsNotExist(err) {
		t.Fatalf("redirected archive exists or stat failed unexpectedly: %v", err)
	}
}

func TestGetRetrievesExactPoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/collections/memory/points/exact-id" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("with_vector"); got != "true" {
			t.Fatalf("with_vector = %q, want true", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"id":"exact-id","vector":[0.1],"payload":{"text":"target"}}}`))
	}))
	defer server.Close()

	point, found, err := NewClient(server.URL, "memory").Get(context.Background(), "exact-id")
	if err != nil {
		t.Fatal(err)
	}
	if !found || point.ID != "exact-id" || point.Payload["text"] != "target" {
		t.Fatalf("unexpected point: found=%v point=%#v", found, point)
	}
}

func TestGetPreservesLargeNumericPointID(t *testing.T) {
	const id = "9007199254740993"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"id":9007199254740993,"vector":[0.1],"payload":{"recall_count":2}}}`))
	}))
	defer server.Close()

	point, found, err := NewClient(server.URL, "memory").Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !found || point.ID != id {
		t.Fatalf("found=%v ID=%q, want %q", found, point.ID, id)
	}
	if got, ok := point.Payload["recall_count"].(float64); !ok || got != 2 {
		t.Fatalf("payload number = %#v, want established float64 type", point.Payload["recall_count"])
	}
}

func TestScrollPreservesLargeNumericIDsAndOffsets(t *testing.T) {
	const id = "18446744073709551615"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]interface{}
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil {
			t.Fatal(err)
		}
		if requests == 1 {
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":18446744073709551615,"payload":{"chunk_index":1}}],"next_page_offset":18446744073709551615}}`))
			return
		}
		if got, ok := body["offset"].(json.Number); !ok || got.String() != id {
			t.Fatalf("offset = %#v, want exact %s", body["offset"], id)
		}
		_, _ = w.Write([]byte(`{"result":{"points":[],"next_page_offset":null}}`))
	}))
	defer server.Close()

	points, err := NewClient(server.URL, "memory").ScrollAll(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].ID != id {
		t.Fatalf("points = %#v, want exact ID %s", points, id)
	}
	if got, ok := points[0].Payload["chunk_index"].(float64); !ok || got != 1 {
		t.Fatalf("payload number = %#v, want established float64 type", points[0].Payload["chunk_index"])
	}
}

func TestGetReturnsNotFound(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, found, err := NewClient(server.URL, "memory").Get(context.Background(), "missing")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected missing point")
	}
}
