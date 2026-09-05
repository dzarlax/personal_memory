package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/mark3labs/mcp-go/server"
)

func relatedResponseServer(t *testing.T, searchResponses ...string) (*Server, *int, *int) {
	t.Helper()
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	t.Cleanup(embedServer.Close)

	searches := 0
	writes := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/points/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/points/search") {
			if searches >= len(searchResponses) {
				http.Error(w, "unexpected search", http.StatusInternalServerError)
				return
			}
			response := searchResponses[searches]
			searches++
			if response == "ERROR" {
				http.Error(w, "search unavailable", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(response))
			return
		}
		writes++
		_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
	}))
	t.Cleanup(qdrantServer.Close)

	return NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	), &searches, &writes
}

func TestRelatedResultJSONUsesStableSnakeCaseAndEmptyArrays(t *testing.T) {
	for name, value := range map[string]interface{}{
		"store": StoreFactResult{Status: "stored", Stored: true, RelatedFacts: []RelatedFactCandidate{}},
		"find":  FindRelatedResult{RelatedFacts: []RelatedFactCandidate{}, Count: 0},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]interface{}
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			if related, ok := object["related_facts"].([]interface{}); !ok || len(related) != 0 {
				t.Fatalf("related_facts = %#v, want [] in %s", object["related_facts"], encoded)
			}
			if _, camelCase := object["relatedFacts"]; camelCase {
				t.Fatalf("camelCase JSON key leaked: %s", encoded)
			}
		})
	}
}

func TestRegisterRelatedToolsDeclaresOutputSchemasAndNeutralDescriptions(t *testing.T) {
	memoryServer := &Server{}
	mcpServer := server.NewMCPServer("test", "1.0")
	memoryServer.RegisterTools(mcpServer)
	for _, name := range []string{"store_fact", "find_related"} {
		tool := mcpServer.GetTool(name)
		if tool == nil || tool.Tool.OutputSchema.Type != "object" {
			t.Fatalf("%s has no output schema", name)
		}
		description := strings.ToLower(tool.Tool.Description)
		if strings.Contains(description, "contradiction") {
			t.Fatalf("%s description uses obsolete wording: %q", name, description)
		}
	}
	storeDescription := strings.ToLower(mcpServer.GetTool("store_fact").Tool.Description)
	if !strings.Contains(storeDescription, "duplicate") || !strings.Contains(storeDescription, "superseded") || !strings.Contains(storeDescription, "related") {
		t.Fatalf("store_fact description does not explain duplicate/related/superseded semantics: %q", storeDescription)
	}
}

func TestStoreFactReturnsStructuredStoredResultAndEquivalentFallback(t *testing.T) {
	srv, searches, writes := relatedResponseServer(t,
		`{"result":[]}`,
		`{"result":[{"id":"related","score":0.81,"payload":{"text":"nearby fact","namespace":"projects","lifecycle_state":"historical"}}]}`,
	)
	fact := "new durable fact"
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": fact, "namespace": "projects"}))
	if err != nil || result.IsError {
		t.Fatalf("store failed: result=%#v err=%v", result, err)
	}
	structured, ok := result.StructuredContent.(StoreFactResult)
	if !ok {
		t.Fatalf("structured content type = %T", result.StructuredContent)
	}
	wantID := PointID("projects", fact)
	if structured.Status != "stored" || !structured.Stored || structured.PointID != wantID || structured.Duplicate != nil {
		t.Fatalf("structured result = %#v", structured)
	}
	if len(structured.RelatedFacts) != 1 || structured.RelatedFacts[0].Text != "nearby fact" {
		t.Fatalf("related facts = %#v", structured.RelatedFacts)
	}
	text := toolResultText(t, result)
	for _, marker := range []string{"status: stored", "point_id: " + wantID, `"text":"nearby fact"`, `"score":0.81`, `"state":"historical"`, `"valid":true`} {
		if !strings.Contains(text, marker) {
			t.Errorf("fallback missing %q: %s", marker, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "contradiction") {
		t.Fatalf("obsolete wording in fallback: %s", text)
	}
	if *searches != 2 || *writes != 1 {
		t.Fatalf("searches=%d writes=%d, want 2/1", *searches, *writes)
	}
}

func fallbackRelatedCandidates(t *testing.T, fallback string) []RelatedFactCandidate {
	t.Helper()
	candidates := []RelatedFactCandidate{}
	for _, line := range strings.Split(fallback, "\n") {
		const prefix = "- candidate: "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		var candidate RelatedFactCandidate
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &candidate); err != nil {
			t.Fatalf("decode fallback candidate %q: %v", line, err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func TestRelatedFallbackCandidatesAreStructurallyEquivalentForBothTools(t *testing.T) {
	candidate := RelatedFactCandidate{
		PointID:    "candidate-id",
		Text:       "line one\nline two\tend",
		Score:      .8125,
		Namespace:  "projects",
		Tags:       []string{"memory", "architecture"},
		PrimaryTag: "memory",
		Lifecycle: lifecycleView("candidate-id", map[string]interface{}{
			"lifecycle_state": "superseded",
			"provenance":      map[string]interface{}{"source": "user", "reference": "decision-7"},
			"verified_at":     "2026-07-23T10:00:00Z",
			"supersedes":      []interface{}{"older-id"},
			"superseded_by":   []interface{}{"replacement-id"},
		}),
	}

	storeFallback := formatStoreFactResult(StoreFactResult{
		Status:       "stored",
		Stored:       true,
		PointID:      "stored-point-id",
		RelatedFacts: []RelatedFactCandidate{candidate},
	})
	for _, marker := range []string{"status: stored", "stored: true", "point_id: stored-point-id", "related_facts: 1"} {
		if !strings.Contains(storeFallback, marker) {
			t.Errorf("store fallback missing %q: %s", marker, storeFallback)
		}
	}
	storeCandidates := fallbackRelatedCandidates(t, storeFallback)
	if len(storeCandidates) != 1 || !reflect.DeepEqual(storeCandidates[0], candidate) {
		t.Fatalf("store fallback candidates = %#v, want related candidate %#v", storeCandidates, candidate)
	}

	findFallback := formatFindRelatedResult(FindRelatedResult{RelatedFacts: []RelatedFactCandidate{candidate}, Count: 1})
	if !strings.Contains(findFallback, "count: 1") {
		t.Fatalf("find fallback missing count: %s", findFallback)
	}
	findCandidates := fallbackRelatedCandidates(t, findFallback)
	if len(findCandidates) != 1 || !reflect.DeepEqual(findCandidates[0], candidate) {
		t.Fatalf("find fallback candidates = %#v, want %#v", findCandidates, candidate)
	}
	if strings.Contains(storeFallback, "line one\nline two") || strings.Contains(findFallback, "line one\nline two") {
		t.Fatalf("fallback emitted raw control characters: store=%q find=%q", storeFallback, findFallback)
	}
}

func TestStoreFactFallbackQuotesMultilineCandidateText(t *testing.T) {
	result := StoreFactResult{
		Status: "duplicate",
		Stored: false,
		Duplicate: &RelatedFactCandidate{
			Text:      "line one\nline two\tend",
			Score:     .99,
			Lifecycle: lifecycleView("candidate", map[string]interface{}{}),
		},
		RelatedFacts: []RelatedFactCandidate{},
	}
	fallback := formatStoreFactResult(result)
	if strings.Contains(fallback, "line one\nline two") {
		t.Fatalf("fallback contains raw multiline fact text: %q", fallback)
	}
	if !strings.Contains(fallback, `"line one\nline two\tend"`) {
		t.Fatalf("fallback does not contain an unambiguous quoted fact: %q", fallback)
	}
}

func TestRelatedCandidateFallbackMarshalFailureDoesNotLeakText(t *testing.T) {
	candidate := RelatedFactCandidate{Text: "ambiguous\nraw text", Score: math.NaN()}
	if fallback := formatRelatedCandidate(candidate); fallback != "- candidate: <unavailable>" {
		t.Fatalf("marshal failure fallback = %q", fallback)
	}
}

func TestStoreFactDuplicateKeepsOtherRelatedCandidatesWithoutRepeatingBlocker(t *testing.T) {
	srv, _, writes := relatedResponseServer(t,
		`{"result":[{"id":"blocker","score":0.99,"payload":{"text":"same fact","namespace":"projects","lifecycle_state":"current"}}]}`,
		`{"result":[
			{"id":"blocker","score":0.99,"payload":{"text":"same fact","namespace":"projects","lifecycle_state":"current"}},
			{"id":"superseded","score":0.98,"payload":{"text":"old version","namespace":"projects","lifecycle_state":"superseded","superseded_by":["replacement"]}},
			{"id":"related","score":0.80,"payload":{"text":"neighbor","namespace":"projects","lifecycle_state":"disputed"}}
		]}`,
	)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "same", "namespace": "projects"}))
	if err != nil || result.IsError {
		t.Fatalf("duplicate response failed: %#v %v", result, err)
	}
	structured := result.StructuredContent.(StoreFactResult)
	if structured.Status != "duplicate" || structured.Stored || structured.PointID != "" || structured.Duplicate == nil || structured.Duplicate.PointID != "blocker" {
		t.Fatalf("duplicate result = %#v", structured)
	}
	if len(structured.RelatedFacts) != 2 {
		t.Fatalf("related facts = %#v, want 2 entries", structured.RelatedFacts)
	}
	gotIDs := []string{structured.RelatedFacts[0].PointID, structured.RelatedFacts[1].PointID}
	if want := []string{"related", "superseded"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("related IDs = %v, want %v", gotIDs, want)
	}
	text := toolResultText(t, result)
	for _, marker := range []string{"status: duplicate", `"text":"same fact"`, `"score":0.99`, `"text":"old version"`, `"text":"neighbor"`, `"state":"superseded"`, `"state":"disputed"`} {
		if !strings.Contains(text, marker) {
			t.Errorf("fallback missing %q: %s", marker, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "contradiction") || *writes != 0 {
		t.Fatalf("fallback=%q writes=%d", text, *writes)
	}
}

func TestImportFactsUsesLifecycleAwareDuplicateSelection(t *testing.T) {
	tests := []struct {
		name           string
		searchResponse string
		wantStatus     string
		wantWrites     int
	}{
		{
			name:           "valid superseded candidate does not block",
			searchResponse: `{"result":[{"id":"old","score":0.99,"payload":{"text":"old version","namespace":"projects","lifecycle_state":"superseded","superseded_by":["replacement"]}}]}`,
			wantStatus:     "stored",
			wantWrites:     1,
		},
		{
			name:           "current candidate still blocks",
			searchResponse: `{"result":[{"id":"current","score":0.99,"payload":{"text":"same fact","namespace":"projects","lifecycle_state":"current"}}]}`,
			wantStatus:     "duplicate",
			wantWrites:     0,
		},
		{
			name:           "invalid superseded candidate still blocks",
			searchResponse: `{"result":[{"id":"invalid","score":0.99,"payload":{"text":"broken old version","namespace":"projects","lifecycle_state":"superseded"}}]}`,
			wantStatus:     "duplicate",
			wantWrites:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, searches, writes := relatedResponseServer(t, tt.searchResponse)
			result, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{
				"facts": `[{"text":"replacement","namespace":"projects"}]`,
			}))
			if err != nil || result.IsError {
				t.Fatalf("import failed: result=%#v err=%v", result, err)
			}
			structured, ok := result.StructuredContent.(ImportFactsResult)
			if !ok || len(structured.Outcomes) != 1 || structured.Outcomes[0].Status != tt.wantStatus {
				t.Fatalf("import result = %#v, want status %q", result, tt.wantStatus)
			}
			if *searches != 1 || *writes != tt.wantWrites {
				t.Fatalf("searches=%d writes=%d, want searches=1 writes=%d", *searches, *writes, tt.wantWrites)
			}
		})
	}
}

func TestImportFactsRefusesInconclusiveDedupWindow(t *testing.T) {
	dedupLimit := lifecycleCandidateLimit(relatedFactResultLimit)
	points := make([]string, dedupLimit)
	for i := range points {
		points[i] = fmt.Sprintf(
			`{"id":"superseded-%d","score":0.99,"payload":{"text":"old version %d","namespace":"projects","lifecycle_state":"superseded","superseded_by":["replacement"]}}`,
			i,
			i,
		)
	}
	srv, searches, writes := relatedResponseServer(t, `{"result":[`+strings.Join(points, ",")+`]}`)

	result, err := srv.importFacts(context.Background(), toolRequest(map[string]interface{}{
		"facts": `[{"text":"replacement","namespace":"projects"}]`,
	}))
	if err != nil || result.IsError {
		t.Fatalf("import failed: result=%#v err=%v", result, err)
	}
	structured, ok := result.StructuredContent.(ImportFactsResult)
	if !ok || structured.Imported != 0 || len(structured.Outcomes) != 1 || structured.Outcomes[0].Status != "inconclusive" {
		t.Fatalf("import result = %#v", result)
	}
	if *searches != 1 || *writes != 0 {
		t.Fatalf("searches=%d writes=%d, want searches=1 writes=0", *searches, *writes)
	}
}

func TestStoreFactDuplicateLifecycleRules(t *testing.T) {
	expired := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	tests := []struct {
		name      string
		lifecycle string
	}{
		{name: "legacy current"},
		{name: "historical", lifecycle: `,"lifecycle_state":"historical"`},
		{name: "disputed", lifecycle: `,"lifecycle_state":"disputed"`},
		{name: "invalid", lifecycle: `,"lifecycle_state":"wrong"`},
		{name: "expired", lifecycle: fmt.Sprintf(`,"lifecycle_state":"current","valid_until":%q`, expired)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dedup := fmt.Sprintf(`{"result":[{"id":"blocker","score":0.99,"payload":{"text":"existing","namespace":"projects"%s}}]}`, tt.lifecycle)
			srv, _, writes := relatedResponseServer(t, dedup, `{"result":[]}`)
			result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "candidate", "namespace": "projects"}))
			if err != nil || result.IsError {
				t.Fatalf("store response failed: %#v %v", result, err)
			}
			structured := result.StructuredContent.(StoreFactResult)
			if structured.Status != "duplicate" || structured.Duplicate == nil || *writes != 0 {
				t.Fatalf("result=%#v writes=%d", structured, *writes)
			}
		})
	}
}

func TestStoreFactValidSupersededHighScoreAllowsWrite(t *testing.T) {
	superseded := `{"id":"old","score":0.999,"payload":{"text":"old","namespace":"projects","lifecycle_state":"superseded","superseded_by":["new"]}}`
	srv, _, writes := relatedResponseServer(t, `{"result":[`+superseded+`]}`, `{"result":[`+superseded+`]}`)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "replacement", "namespace": "projects"}))
	if err != nil || result.IsError {
		t.Fatalf("store failed: %#v %v", result, err)
	}
	structured := result.StructuredContent.(StoreFactResult)
	if !structured.Stored || structured.Duplicate != nil || len(structured.RelatedFacts) != 1 || structured.RelatedFacts[0].PointID != "old" || *writes != 1 {
		t.Fatalf("result=%#v writes=%d", structured, *writes)
	}
}

func TestStoreFactDedupCandidateSaturationIsInconclusive(t *testing.T) {
	points := make([]string, 0, lifecycleCandidateLimit(3))
	for i := 0; i < lifecycleCandidateLimit(3); i++ {
		points = append(points, fmt.Sprintf(`{"id":"old-%d","score":0.99,"payload":{"text":"old","lifecycle_state":"superseded","superseded_by":["new"]}}`, i))
	}
	srv, searches, writes := relatedResponseServer(t, `{"result":[`+strings.Join(points, ",")+`]}`)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "candidate"}))
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(t, result)
	if !result.IsError || !strings.Contains(strings.ToLower(text), "inconclusive") || strings.Contains(text, "old-0") || *writes != 0 || *searches != 1 {
		t.Fatalf("result=%#v text=%q searches=%d writes=%d", result, text, *searches, *writes)
	}
}

func TestStoreFactSupersededHeadroomDoesNotMaskLaterBlocker(t *testing.T) {
	points := make([]string, 0, lifecycleCandidateLimit(relatedFactResultLimit))
	for i := 0; i < lifecycleCandidateLimit(relatedFactResultLimit)-1; i++ {
		points = append(points, fmt.Sprintf(`{"id":"old-%d","score":%f,"payload":{"text":"old","lifecycle_state":"superseded","superseded_by":["new"]}}`, i, .999-float64(i)/1000))
	}
	points = append(points, `{"id":"blocker","score":0.97,"payload":{"text":"current blocker","lifecycle_state":"current"}}`)

	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	defer embedServer.Close()

	type searchRequest struct {
		Limit          int     `json:"limit"`
		ScoreThreshold float64 `json:"score_threshold"`
	}
	searchRequests := []searchRequest{}
	handlerErrors := make(chan error, 1)
	writes := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/points/") {
			http.NotFound(w, r)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/points/search") {
			writes++
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
			return
		}
		var request searchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			select {
			case handlerErrors <- fmt.Errorf("decode search request: %w", err):
			default:
			}
			http.Error(w, "invalid search request", http.StatusBadRequest)
			return
		}
		searchRequests = append(searchRequests, request)
		if len(searchRequests) == 1 {
			_, _ = w.Write([]byte(`{"result":[` + strings.Join(points, ",") + `]}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer qdrantServer.Close()

	srv := NewServer(
		qdrant.NewClient(qdrantServer.URL, "memory"),
		embeddings.NewClient(embedServer.URL),
		NewCache(time.Minute),
		"test", .97, .60, .90,
	)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "candidate"}))
	select {
	case handlerErr := <-handlerErrors:
		t.Fatal(handlerErr)
	default:
	}
	if err != nil || result.IsError {
		t.Fatalf("duplicate response failed: %#v %v", result, err)
	}
	structured := result.StructuredContent.(StoreFactResult)
	if structured.Duplicate == nil || structured.Duplicate.PointID != "blocker" || writes != 0 {
		t.Fatalf("result=%#v writes=%d", structured, writes)
	}
	if len(searchRequests) != 2 {
		t.Fatalf("search requests = %d, want 2", len(searchRequests))
	}
	wantLimit := lifecycleCandidateLimit(relatedFactResultLimit)
	if got := searchRequests[0]; got.Limit != wantLimit || got.ScoreThreshold != srv.dedupThreshold {
		t.Fatalf("dedup request = %#v, want limit=%d score_threshold=%v", got, wantLimit, srv.dedupThreshold)
	}
	if got := searchRequests[1]; got.Limit != wantLimit || got.ScoreThreshold != srv.relatedFactLow {
		t.Fatalf("related request = %#v, want limit=%d score_threshold=%v", got, wantLimit, srv.relatedFactLow)
	}
}

func TestFindRelatedReturnsStructuredAllStateResultsAfterFiltering(t *testing.T) {
	srv, _, writes := relatedResponseServer(t, `{"result":[
		{"id":"blocker","score":0.99,"payload":{"text":"current duplicate","lifecycle_state":"current"}},
		{"id":"old","score":0.98,"payload":{"text":"superseded high","lifecycle_state":"superseded","superseded_by":["new"]}},
		{"id":"historical","score":0.90,"payload":{"text":"historical related","lifecycle_state":"historical"}},
		{"id":"expired","score":0.80,"payload":{"text":"expired related","valid_until":"2020-01-01"}}
	]}`)
	result, err := srv.findRelated(context.Background(), toolRequest(map[string]interface{}{"query": "related", "limit": float64(5)}))
	if err != nil || result.IsError {
		t.Fatalf("find failed: %#v %v", result, err)
	}
	structured, ok := result.StructuredContent.(FindRelatedResult)
	if !ok {
		t.Fatalf("structured type = %T", result.StructuredContent)
	}
	if structured.Count != 2 || len(structured.RelatedFacts) != 2 || structured.RelatedFacts[0].PointID != "historical" || structured.RelatedFacts[1].PointID != "old" {
		t.Fatalf("structured = %#v", structured)
	}
	text := toolResultText(t, result)
	for _, marker := range []string{"count: 2", `"text":"superseded high"`, `"score":0.98`, `"state":"superseded"`, `"text":"historical related"`} {
		if !strings.Contains(text, marker) {
			t.Errorf("fallback missing %q: %s", marker, text)
		}
	}
	for _, excluded := range []string{"current duplicate", "expired related", "contradiction"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(excluded)) {
			t.Errorf("fallback contains %q: %s", excluded, text)
		}
	}
	if *writes != 0 {
		t.Fatalf("find_related performed %d writes", *writes)
	}
}

func TestFindRelatedSearchErrorKeepsExistingErrorContract(t *testing.T) {
	srv, searches, writes := relatedResponseServer(t, "ERROR")
	result, err := srv.findRelated(context.Background(), toolRequest(map[string]interface{}{"query": "related"}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("result IsError = false: %#v", result)
	}
	if result.StructuredContent != nil {
		t.Fatalf("structured content = %#v, want nil", result.StructuredContent)
	}
	if text := toolResultText(t, result); !strings.Contains(text, "search failed:") {
		t.Fatalf("error text = %q, want existing search failed prefix", text)
	}
	if *searches != 1 || *writes != 0 {
		t.Fatalf("searches=%d writes=%d, want 1/0", *searches, *writes)
	}
}

func TestStoreFactSearchErrorsPreserveFailOpenAndKnownDuplicate(t *testing.T) {
	t.Run("dedup failure remains fail open", func(t *testing.T) {
		srv, _, writes := relatedResponseServer(t, "ERROR", "ERROR")
		result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "write despite unavailable preflight"}))
		if err != nil || result.IsError {
			t.Fatalf("store failed: %#v %v", result, err)
		}
		structured := result.StructuredContent.(StoreFactResult)
		if !structured.Stored || structured.RelatedFacts == nil || *writes != 1 {
			t.Fatalf("result=%#v writes=%d", structured, *writes)
		}
	})

	t.Run("related search blocker closes dedup failure fail open", func(t *testing.T) {
		srv, _, writes := relatedResponseServer(t,
			"ERROR",
			`{"result":[{"id":"blocker","score":0.99,"payload":{"text":"same","lifecycle_state":"current"}}]}`,
		)
		result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "same"}))
		if err != nil || result.IsError {
			t.Fatalf("duplicate response failed: %#v %v", result, err)
		}
		structured, ok := result.StructuredContent.(StoreFactResult)
		if !ok || structured.Status != "duplicate" || structured.Stored || structured.Duplicate == nil || structured.Duplicate.PointID != "blocker" {
			t.Fatalf("structured result = %#v", result.StructuredContent)
		}
		fallback := toolResultText(t, result)
		for _, marker := range []string{"status: duplicate", `"text":"same"`, `"score":0.99`, `"state":"current"`, `"valid":true`} {
			if !strings.Contains(fallback, marker) {
				t.Errorf("fallback missing %q: %s", marker, fallback)
			}
		}
		if *writes != 0 {
			t.Fatalf("related-search blocker allowed %d writes", *writes)
		}
	})

	t.Run("related search blocker catches between-query race", func(t *testing.T) {
		srv, _, writes := relatedResponseServer(t,
			`{"result":[]}`,
			`{"result":[{"id":"racing-blocker","score":0.99,"payload":{"text":"appeared after preflight","lifecycle_state":"current"}}]}`,
		)
		result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "same"}))
		if err != nil || result.IsError {
			t.Fatalf("duplicate response failed: %#v %v", result, err)
		}
		structured := result.StructuredContent.(StoreFactResult)
		if structured.Status != "duplicate" || structured.Duplicate == nil || structured.Duplicate.PointID != "racing-blocker" || *writes != 0 {
			t.Fatalf("result=%#v writes=%d", structured, *writes)
		}
	})

	t.Run("related failure does not weaken duplicate", func(t *testing.T) {
		srv, _, writes := relatedResponseServer(t,
			`{"result":[{"id":"blocker","score":0.99,"payload":{"text":"same","lifecycle_state":"current"}}]}`,
			"ERROR",
		)
		result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": "same"}))
		if err != nil || result.IsError {
			t.Fatalf("duplicate response failed: %#v %v", result, err)
		}
		structured := result.StructuredContent.(StoreFactResult)
		if structured.Status != "duplicate" || structured.Duplicate == nil || structured.RelatedFacts == nil || len(structured.RelatedFacts) != 0 || *writes != 0 {
			t.Fatalf("result=%#v writes=%d", structured, *writes)
		}
	})
}

func TestStoreFactRefusesActiveExactIDBeforeUnavailablePreflight(t *testing.T) {
	fact := "existing canonical fact"
	pointID := PointID("projects", fact)
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2]]`))
	}))
	defer embedServer.Close()

	searches := 0
	writes := 0
	qdrantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/points/"+pointID):
			_, _ = w.Write([]byte(`{"result":{"id":"` + pointID + `","payload":{"text":"existing canonical fact","namespace":"projects","lifecycle_state":"current","canonical":true}}}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			searches++
			http.Error(w, "unavailable", http.StatusBadGateway)
		default:
			writes++
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
		}
	}))
	defer qdrantServer.Close()

	srv := NewServer(qdrant.NewClient(qdrantServer.URL, "memory"), embeddings.NewClient(embedServer.URL), NewCache(time.Minute), "test", .97, .60, .90)
	result, err := srv.storeFact(context.Background(), toolRequest(map[string]interface{}{"fact": fact, "namespace": "projects"}))
	if err != nil || !result.IsError {
		t.Fatalf("result=%#v err=%v, want exact-ID rejection", result, err)
	}
	structured, ok := result.StructuredContent.(StoreFactResult)
	if !ok || structured.Status != "collision" || structured.Stored || structured.PointID != pointID {
		t.Fatalf("structured collision = %#v", result.StructuredContent)
	}
	if text := toolResultText(t, result); !strings.Contains(text, "already exists") {
		t.Fatalf("result text=%q, want exact-ID explanation", text)
	}
	if searches != 0 || writes != 0 {
		t.Fatalf("searches=%d writes=%d, want 0/0", searches, writes)
	}
}
