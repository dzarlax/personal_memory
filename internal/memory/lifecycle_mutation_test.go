package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/memory/lifecycle"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

func TestSetFactLifecycleUsesExactLifecycleOnlyBatch(t *testing.T) {
	const id = "123"
	var requests int
	var batch map[string]interface{}
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/memory/points/"+id:
			_, _ = w.Write([]byte(`{"result":{"id":123,"vector":[0.1,0.2],"payload":{"text":"PRIVATE FACT","namespace":"projects","recall_count":7,"lifecycle_state":"current","canonical":true,"provenance":{"source":"old"}}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/memory/points/batch":
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				t.Fatalf("decode batch: %v", err)
			}
			_, _ = w.Write([]byte(`{"status":"ok","result":[{"status":"completed"}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer qdrantServer.Close()

	cache := NewCache(time.Minute)
	cache.SetRecall("cached", RecallFactsResult{Facts: []RecallFact{{Text: "stale"}}})
	srv := &Server{qdrant: qdrant.NewClient(qdrantServer.URL, "memory"), cache: cache}
	result, err := srv.setFactLifecycle(context.Background(), toolRequest(map[string]interface{}{
		"point_id":        id,
		"lifecycle_state": "historical",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %#v", result)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want pre-lock Get + locked Get + batch", requests)
	}
	encodedBatch, _ := json.Marshal(batch)
	if strings.Contains(string(encodedBatch), "PRIVATE FACT") || strings.Contains(string(encodedBatch), "recall_count") {
		t.Fatalf("batch leaked unrelated payload: %s", encodedBatch)
	}
	if !strings.Contains(string(encodedBatch), "lifecycle_transitioned_at") {
		t.Fatalf("batch omitted lifecycle transition time: %s", encodedBatch)
	}
	if _, found := cache.GetRecall("cached"); found {
		t.Fatal("cache was not invalidated")
	}
}

func TestLifecycleTransitionTimeIsStableForIdempotentStateUpdates(t *testing.T) {
	const previous = "2026-08-01T00:00:00Z"
	const now = "2026-08-14T00:00:00Z"
	current := lifecycle.View{State: lifecycle.Superseded, Valid: true, TransitionedAt: previous}
	if got := lifecycleTransitionedAt(current, lifecycle.View{State: lifecycle.Superseded, Valid: true}, now); got != previous {
		t.Fatalf("same-state transition = %q, want %q", got, previous)
	}
	if got := lifecycleTransitionedAt(current, lifecycle.View{State: lifecycle.Current, Valid: true}, now); got != now {
		t.Fatalf("changed-state transition = %q, want %q", got, now)
	}
}

func TestSetFactLifecycleRejectsInvalidInputBeforeQdrant(t *testing.T) {
	requests := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer qdrantServer.Close()
	cache := NewCache(time.Minute)
	cache.SetRecall("cached", RecallFactsResult{Facts: []RecallFact{{Text: "kept"}}})
	srv := &Server{qdrant: qdrant.NewClient(qdrantServer.URL, "memory"), cache: cache}

	result, err := srv.setFactLifecycle(context.Background(), toolRequest(map[string]interface{}{
		"point_id":        "123",
		"lifecycle_state": "superseded",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("invalid superseded target was accepted")
	}
	if requests != 0 {
		t.Fatalf("Qdrant requests = %d, want 0", requests)
	}
	if _, found := cache.GetRecall("cached"); !found {
		t.Fatal("pre-dispatch validation invalidated cache")
	}
}

func TestSetFactLifecycleInvalidatesCacheAfterDispatchedError(t *testing.T) {
	const id = "123"
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"result":{"id":123,"vector":[],"payload":{"text":"private"}}}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"status":"error"}`))
	}))
	defer qdrantServer.Close()
	cache := NewCache(time.Minute)
	cache.SetRecall("cached", RecallFactsResult{Facts: []RecallFact{{Text: "stale"}}})
	srv := &Server{qdrant: qdrant.NewClient(qdrantServer.URL, "memory"), cache: cache}

	result, err := srv.setFactLifecycle(context.Background(), toolRequest(map[string]interface{}{
		"point_id":        id,
		"lifecycle_state": "historical",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("storage failure was not returned")
	}
	if _, found := cache.GetRecall("cached"); found {
		t.Fatal("dispatched failure left stale cache")
	}
}

func TestParseLifecycleInputBounds(t *testing.T) {
	tests := []map[string]interface{}{
		{"lifecycle_state": "current", "provenance_source": strings.Repeat("x", maxLifecycleSourceBytes+1)},
		{"lifecycle_state": "current", "supersedes": []interface{}{strings.Repeat("x", maxLifecyclePointIDBytes+1)}},
		{"lifecycle_state": "current", "verified_at": 123},
		{"lifecycle_state": "current", "provenance_source": true},
	}
	tooMany := make([]interface{}, maxLifecycleRelations+1)
	for i := range tooMany {
		tooMany[i] = "1"
	}
	tests = append(tests, map[string]interface{}{"lifecycle_state": "historical", "supersedes": tooMany})
	for _, args := range tests {
		if _, err := parseLifecycleInput(args, true); err == nil {
			t.Fatalf("accepted oversized lifecycle input: %#v", args)
		}
	}
}

func TestStoreFactRejectsInvalidLifecycleBeforeDependencies(t *testing.T) {
	embedRequests := 0
	embedServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		embedRequests++
	}))
	defer embedServer.Close()
	qdrantRequests := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		qdrantRequests++
	}))
	defer qdrantServer.Close()
	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{
		"fact":            "private",
		"lifecycle_state": "superseded",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || embedRequests != 0 || qdrantRequests != 0 {
		t.Fatalf("invalid store result=%#v embed=%d qdrant=%d", result, embedRequests, qdrantRequests)
	}
}

func TestStoreFactWritesExplicitLifecyclePayload(t *testing.T) {
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	defer embedServer.Close()
	searches := 0
	var stored map[string]interface{}
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/points/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/points/search") {
			searches++
			_, _ = w.Write([]byte(`{"result":[]}`))
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		points := body["points"].([]interface{})
		stored = points[0].(map[string]interface{})["payload"].(map[string]interface{})
		_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
	}))
	defer qdrantServer.Close()
	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{
		"fact":              "explicit current",
		"namespace":         "projects",
		"lifecycle_state":   "current",
		"canonical":         true,
		"provenance_source": "user",
		"supersedes":        []interface{}{"123"},
	}))
	if err != nil || result.IsError {
		t.Fatalf("store result=%#v err=%v", result, err)
	}
	if searches != 2 || stored["lifecycle_state"] != "current" || stored["canonical"] != true {
		t.Fatalf("searches=%d payload=%#v", searches, stored)
	}
	if provenance := stored["provenance"].(map[string]interface{}); provenance["source"] != "user" {
		t.Fatalf("provenance = %#v", provenance)
	}
}

func TestUpdateFactRejectsPreservedLifecycleSelfReferenceBeforeEmbedding(t *testing.T) {
	const oldID = "11111111-1111-1111-1111-111111111111"
	newFact := "replacement"
	newID := PointID("projects", newFact)
	embedRequests := 0
	embedServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		embedRequests++
	}))
	defer embedServer.Close()
	qdrantRequests := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qdrantRequests++
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"result":{"id":"` + oldID + `","vector":[0.1,0.2],"payload":{"text":"old","namespace":"projects","lifecycle_state":"current","supersedes":["` + newID + `"]}}}`))
	}))
	defer qdrantServer.Close()
	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	result, err := srv.updateFact(context.Background(), toolRequest(map[string]interface{}{
		"point_id": oldID,
		"new_fact": newFact,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(toolResultText(t, result), "itself") {
		t.Fatalf("self-reference was accepted: %#v", result)
	}
	if embedRequests != 0 || qdrantRequests != 1 {
		t.Fatalf("embed=%d qdrant=%d, want 0 and exact Get only", embedRequests, qdrantRequests)
	}
}

func TestUpdateFactRefusesTargetPointCollision(t *testing.T) {
	oldID := PointID("projects", "old")
	newFact := "already stored"
	newID := PointID("projects", newFact)
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	defer embedServer.Close()

	qdrantRequests := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qdrantRequests++
		if r.Method != http.MethodGet {
			t.Fatalf("collision check attempted mutation: %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/collections/memory/points/" + oldID:
			_, _ = w.Write([]byte(`{"result":{"id":"` + oldID + `","vector":[0.1,0.2],"payload":{"text":"old","namespace":"projects"}}}`))
		case "/collections/memory/points/" + newID:
			_, _ = w.Write([]byte(`{"result":{"id":"` + newID + `","vector":[0.3,0.4],"payload":{"text":"already stored","namespace":"projects"}}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer qdrantServer.Close()

	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	result, err := srv.updateFact(context.Background(), toolRequest(map[string]interface{}{
		"point_id": oldID,
		"new_fact": newFact,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(toolResultText(t, result), "collides with an existing fact") {
		t.Fatalf("collision was accepted: %#v", result)
	}
	if qdrantRequests != 3 {
		t.Fatalf("Qdrant requests = %d, want two old lookups plus target collision lookup", qdrantRequests)
	}
}

func TestUpdateFactLogsFailedOldPointDeletion(t *testing.T) {
	oldID := PointID("projects", "old")
	newFact := "replacement"
	newID := PointID("projects", newFact)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previousLogger)

	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	defer embedServer.Close()
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/memory/points/"+oldID:
			_, _ = w.Write([]byte(`{"result":{"id":"` + oldID + `","vector":[0.1,0.2],"payload":{"text":"old","namespace":"projects"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/collections/memory/points/"+newID:
			http.NotFound(w, r)
		case r.Method == http.MethodPut && r.URL.Path == "/collections/memory/points":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/memory/points/delete":
			http.Error(w, "forced delete failure", http.StatusBadGateway)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer qdrantServer.Close()

	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	result, err := srv.updateFact(context.Background(), toolRequest(map[string]interface{}{
		"point_id": oldID,
		"new_fact": newFact,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(toolResultText(t, result), "delete old failed") {
		t.Fatalf("delete failure was not returned: %#v", result)
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, "duplicate may remain") ||
		!strings.Contains(logOutput, oldID) ||
		!strings.Contains(logOutput, newID) {
		t.Fatalf("orphaned duplicate was not logged: %s", logOutput)
	}
}

func TestImportFactsPreservesLifecycleAndDoesNotLogPrivateText(t *testing.T) {
	const private = "PRIVATE_IMPORT_MARKER"
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previousLogger)

	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	defer embedServer.Close()
	var stored map[string]interface{}
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/points/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/points/search") {
			_, _ = w.Write([]byte(`{"result":[]}`))
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		stored = body["points"].([]interface{})[0].(map[string]interface{})["payload"].(map[string]interface{})
		_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
	}))
	defer qdrantServer.Close()
	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	facts, _ := json.Marshal([]map[string]interface{}{{
		"text":            private,
		"namespace":       "projects",
		"lifecycle_state": "historical",
		"provenance":      map[string]interface{}{"source": "import"},
	}})
	result, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{"facts": string(facts)}))
	if err != nil || result.IsError {
		t.Fatalf("import result=%#v err=%v", result, err)
	}
	if stored["lifecycle_state"] != "historical" {
		t.Fatalf("stored payload = %#v", stored)
	}
	if strings.Contains(logs.String(), private) {
		t.Fatalf("logs leaked fact text: %s", logs.String())
	}
}

func TestImportFactsBatchesEmbeddings(t *testing.T) {
	embedRequests := 0
	var embeddedInputs []string
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedRequests++
		var body struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		embeddedInputs = append(embeddedInputs, body.Inputs...)
		_, _ = w.Write([]byte(`[[0.1,0.2],[0.3,0.4]]`))
	}))
	defer embedServer.Close()

	upserts := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/points/"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(`{"result":[]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/memory/points":
			upserts++
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer qdrantServer.Close()

	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	facts, _ := json.Marshal([]map[string]interface{}{
		{"text": "first", "namespace": "projects"},
		{"text": "second", "namespace": "work"},
	})
	result, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{"facts": string(facts)}))
	if err != nil || result.IsError {
		t.Fatalf("import result=%#v err=%v", result, err)
	}
	if embedRequests != 1 || !reflect.DeepEqual(embeddedInputs, []string{"first", "second"}) {
		t.Fatalf("embed requests=%d inputs=%#v", embedRequests, embeddedInputs)
	}
	structured, ok := result.StructuredContent.(ImportFactsResult)
	if upserts != 2 || !ok || structured.Imported != 2 || len(structured.Outcomes) != 2 || structured.Outcomes[0].Status != "stored" || structured.Outcomes[1].Status != "stored" {
		t.Fatalf("upserts=%d result=%#v", upserts, result)
	}
}

func TestImportFactsFallsBackToPerItemEmbedding(t *testing.T) {
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Inputs) > 1 || body.Inputs[0] == "second" {
			http.Error(w, "forced embedding failure", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	defer embedServer.Close()

	upserts := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/points/"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(`{"result":[]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/memory/points":
			upserts++
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer qdrantServer.Close()

	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	facts, _ := json.Marshal([]map[string]interface{}{
		{"text": "first", "namespace": "projects"},
		{"text": "second", "namespace": "work"},
	})
	result, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{"facts": string(facts)}))
	if err != nil || result.IsError {
		t.Fatalf("import result=%#v err=%v", result, err)
	}
	structured, ok := result.StructuredContent.(ImportFactsResult)
	if upserts != 1 || !ok || structured.Imported != 1 || len(structured.Outcomes) != 2 || structured.Outcomes[0].Status != "stored" || structured.Outcomes[1].Status != "embedding_failed" {
		t.Fatalf("upserts=%d result=%#v", upserts, result)
	}
}

func TestForgetOldRefusesDirectDeletionBeforeNetwork(t *testing.T) {
	srv := &Server{qdrant: qdrant.NewClient("http://127.0.0.1:1", "memory"), cache: NewCache(time.Minute)}
	result, err := srv.forgetOld(context.Background(), toolRequest(map[string]interface{}{
		"days":    float64(90),
		"dry_run": false,
	}))
	if err != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "direct age-only deletion is disabled") {
		t.Fatalf("forget_old result=%#v err=%v", result, err)
	}
}
