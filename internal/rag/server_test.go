package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Dzarlax-AI/personal-memory/internal/config"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/Dzarlax-AI/personal-memory/internal/rerank"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestParseSearchDocumentsArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		wantErr bool
	}{
		{name: "defaults", args: map[string]any{"query": "memory"}},
		{name: "flat", args: map[string]any{"query": "memory", "limit": float64(100), "mode": "flat"}},
		{name: "blended", args: map[string]any{"query": "memory", "mode": "blended"}},
		{name: "blank query", args: map[string]any{"query": "  "}, wantErr: true},
		{name: "zero limit", args: map[string]any{"query": "memory", "limit": float64(0)}, wantErr: true},
		{name: "huge limit", args: map[string]any{"query": "memory", "limit": float64(101)}, wantErr: true},
		{name: "fractional limit", args: map[string]any{"query": "memory", "limit": 1.5}, wantErr: true},
		{name: "unknown mode", args: map[string]any{"query": "memory", "mode": "magic"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, gotErr := parseSearchDocumentsArgs(tt.args)
			if (gotErr != "") != tt.wantErr {
				t.Fatalf("error=%q, wantErr=%v", gotErr, tt.wantErr)
			}
		})
	}
}

func TestParseSearchDocumentsArgsDefaultsRemainHierarchical(t *testing.T) {
	query, limit, mode, validationErr := parseSearchDocumentsArgs(map[string]any{"query": " memory "})
	if validationErr != "" {
		t.Fatal(validationErr)
	}
	if query != "memory" || limit != 5 || mode != "hierarchical" {
		t.Fatalf("defaults = query:%q limit:%d mode:%q", query, limit, mode)
	}
}

func TestRelPathNeverReturnsAbsoluteOrEscapingPaths(t *testing.T) {
	tests := []struct {
		name string
		base string
		path string
		want string
	}{
		{name: "child", base: "/documents", path: "/documents/project/note.md", want: "project/note.md"},
		{name: "outside", base: "/documents", path: "/private/secret.md", want: ""},
		{name: "empty base", path: "/private/secret.md", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relPath(tt.base, tt.path); got != tt.want {
				t.Fatalf("relPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSearchDocumentsBlendedRescuesFlatTopAndReturnsPrivacySafeRouting(t *testing.T) {
	const documentsRoot = "/documents"
	var calls []searchCall
	folders := fakePointSearcher{name: "folders", calls: &calls, search: func(_ map[string]any) []qdrant.Point {
		return []qdrant.Point{{ID: "folder", Score: .8, Payload: map[string]any{"folder_path": "/documents/project"}}}
	}}
	chunks := fakePointSearcher{name: "chunks", calls: &calls, search: func(filter map[string]any) []qdrant.Point {
		if filter != nil {
			return []qdrant.Point{
				{ID: "a", Score: .91, Payload: searchPayload("filtered a", "/documents/project/a.md", "A", "g-a")},
				{ID: "b", Score: .90, Payload: searchPayload("filtered b", "/documents/project/b.md", "B", "g-b")},
			}
		}
		return []qdrant.Point{
			{ID: "flat-top", Score: .99, Payload: mergePayload(searchPayload("strong flat", "/private/secret.md", "Secret", "g-private"), map[string]any{"unrestricted": "/do/not/expose"})},
			{ID: "a", Score: .89, Payload: searchPayload("flat a", "/documents/project/a.md", "A", "g-a")},
			{ID: "b", Score: .88, Payload: searchPayload("flat b", "/documents/project/b.md", "B", "g-b")},
		}
	}}
	srv := &Server{
		queryEmbed: fakeQueryEmbedder{}, searchChunks: chunks, searchFolders: folders,
		validateChunks: sealedSearchValidator(map[string]qdrant.Point{
			"/documents/project/a.md\x00g-a":  {ID: "a", Payload: searchPayload("filtered a", "/documents/project/a.md", "A", "g-a")},
			"/documents/project/b.md\x00g-b":  {ID: "b", Payload: searchPayload("filtered b", "/documents/project/b.md", "B", "g-b")},
			"/private/secret.md\x00g-private": {ID: "flat-top", Payload: searchPayload("strong flat", "/private/secret.md", "Secret", "g-private")},
		}),
		cfg: &config.Config{RAGDocumentsDir: documentsRoot, RAGFolderTopK: 3, RAGFolderThreshold: .5},
	}
	result, err := srv.handleSearchDocuments(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"query": "needle", "limit": float64(2), "mode": "blended"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", ragToolResultText(t, result))
	}

	var results []struct {
		Score    float64 `json:"score"`
		Text     string  `json:"text"`
		FilePath string  `json:"file_path"`
		Routing  struct {
			Strategy string `json:"strategy"`
			Sources  []struct {
				Source string `json:"source"`
				Rank   int    `json:"rank"`
			} `json:"sources"`
			ReasonCodes         []string `json:"reason_codes"`
			SelectedFolderPaths []string `json:"selected_folder_paths"`
		} `json:"routing"`
	}
	responseText := ragToolResultText(t, result)
	if err := json.Unmarshal([]byte(responseText), &results); err != nil {
		t.Fatalf("decode response: %v\n%s", err, responseText)
	}
	if len(results) != 2 || results[0].Text != "filtered a" || results[1].Text != "strong flat" {
		t.Fatalf("unexpected blended results: %#v", results)
	}
	if results[1].Score != .99 {
		t.Fatalf("rescued cosine score = %v, want .99", results[1].Score)
	}
	if results[1].FilePath != "" {
		t.Fatalf("outside file path leaked as %q", results[1].FilePath)
	}
	if results[1].Routing.Strategy != "blended_rrf" ||
		!reflect.DeepEqual(results[1].Routing.ReasonCodes, []string{"flat_match", "flat_rescue"}) ||
		!reflect.DeepEqual(results[1].Routing.SelectedFolderPaths, []string{"project"}) {
		t.Fatalf("rescued routing = %#v", results[1].Routing)
	}
	if len(results[1].Routing.Sources) != 1 || results[1].Routing.Sources[0].Source != "flat" || results[1].Routing.Sources[0].Rank != 1 {
		t.Fatalf("rescued source explanation = %#v", results[1].Routing.Sources)
	}
	if strings.Contains(responseText, "/private/") || strings.Contains(responseText, "unrestricted") || strings.Contains(responseText, "/do/not/expose") {
		t.Fatalf("response leaked path or unrestricted payload: %s", responseText)
	}
	if want := []searchCall{
		{Collection: "folders", Limit: 3},
		{Collection: "chunks", Limit: 4, Filtered: true},
		{Collection: "chunks", Limit: 4},
	}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("bounded search calls = %#v, want %#v", calls, want)
	}
}

func TestSearchDocumentsDefaultDoesNotBecomeBlended(t *testing.T) {
	chunkSearches := 0
	srv := &Server{
		queryEmbed: fakeQueryEmbedder{},
		searchFolders: fakePointSearcher{search: func(_ map[string]any) []qdrant.Point {
			return []qdrant.Point{{ID: "folder", Score: .8, Payload: map[string]any{"folder_path": "/documents/project"}}}
		}},
		searchChunks: fakePointSearcher{search: func(_ map[string]any) []qdrant.Point {
			chunkSearches++
			return []qdrant.Point{{ID: "a", Score: .9, Payload: searchPayload("a", "/documents/project/a.md", "", "g-a")}}
		}},
		validateChunks: sealedSearchValidator(map[string]qdrant.Point{
			"/documents/project/a.md\x00g-a": {ID: "a", Payload: searchPayload("a", "/documents/project/a.md", "", "g-a")},
		}),
		cfg: &config.Config{RAGDocumentsDir: "/documents", RAGFolderTopK: 3, RAGFolderThreshold: .5},
	}
	result, err := srv.handleSearchDocuments(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"query": "needle", "limit": float64(2)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := ragToolResultText(t, result)
	if chunkSearches != 1 {
		t.Fatalf("default chunk searches = %d, want hierarchical-only 1", chunkSearches)
	}
	if strings.Contains(text, `"routing"`) {
		t.Fatalf("default response unexpectedly gained blended routing: %s", text)
	}
}

type fakeReranker struct {
	ranked []rerank.Ranked
	err    error
}

func (r fakeReranker) Rerank(context.Context, string, []rerank.Candidate) ([]rerank.Ranked, error) {
	return r.ranked, r.err
}

func TestBlendedSearchReranksCandidatePoolBeforeFinalLimit(t *testing.T) {
	for _, test := range []struct {
		name       string
		reranker   rerank.Reranker
		wantIDs    []string
		wantReason string
	}{
		{
			name: "applied",
			reranker: fakeReranker{ranked: []rerank.Ranked{
				{Index: 2, Score: .9}, {Index: 0, Score: .8}, {Index: 1, Score: .7},
			}},
			wantIDs: []string{"c", "a"}, wantReason: rerank.ReasonApplied,
		},
		{
			name:     "fallback",
			reranker: fakeReranker{err: fmt.Errorf("unavailable")},
			wantIDs:  []string{"a", "b"}, wantReason: rerank.ReasonFallback,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []searchCall
			srv := &Server{
				searchFolders: fakePointSearcher{name: "folders", calls: &calls, search: func(map[string]any) []qdrant.Point { return nil }},
				searchChunks: fakePointSearcher{name: "chunks", calls: &calls, search: func(map[string]any) []qdrant.Point {
					return []qdrant.Point{
						{ID: "a", Payload: map[string]any{"text": "A"}},
						{ID: "b", Payload: map[string]any{"text": "B"}},
						{ID: "c", Payload: map[string]any{"text": "C"}},
					}
				}},
				reranker: test.reranker, rerankerCap: 3,
				cfg: &config.Config{RAGDocumentsDir: "/documents", RAGFolderTopK: 3, RAGFolderThreshold: .5},
			}
			points, routing, err := srv.blendedSearch(context.Background(), "query", []float32{1}, 2)
			if err != nil {
				t.Fatal(err)
			}
			points, routing = srv.rerankValidated(context.Background(), "query", points, routing)
			points = points[:2]
			if got := pointIDs(points); !reflect.DeepEqual(got, test.wantIDs) {
				t.Fatalf("point IDs = %v, want %v", got, test.wantIDs)
			}
			for _, point := range points {
				if reasons := routing[point.ID].ReasonCodes; len(reasons) == 0 || reasons[len(reasons)-1] != test.wantReason {
					t.Fatalf("routing reasons for %s = %v", point.ID, reasons)
				}
			}
			if got := calls[len(calls)-1].Limit; got != 6 {
				t.Fatalf("chunk candidate limit = %d, want 6", got)
			}
		})
	}
}

func ragToolResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("unexpected content: %#v", result.Content)
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type: %T", result.Content[0])
	}
	return content.Text
}

type searchCall struct {
	Collection string
	Limit      int
	Filtered   bool
}

type fakePointSearcher struct {
	name   string
	calls  *[]searchCall
	search func(filter map[string]any) []qdrant.Point
}

func (f fakePointSearcher) Search(_ context.Context, _ []float32, limit int, filter map[string]interface{}, _ *float64) ([]qdrant.Point, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, searchCall{Collection: f.name, Limit: limit, Filtered: filter != nil})
	}
	return f.search(filter), nil
}

type fakeQueryEmbedder struct{}

func (fakeQueryEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{.1, .2}, nil
}

func searchPayload(text, filePath, heading, generation string) map[string]any {
	return map[string]any{
		"text": text, "file_path": filePath, "heading": heading, "generation": generation,
		"layout": "test-layout", "file_hash": "test-hash-" + generation, "total_chunks": 1, "chunk_index": 0,
	}
}

func mergePayload(base, extra map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

type sealedSearchValidator map[string]qdrant.Point

func (v sealedSearchValidator) ScrollWithPayload(_ context.Context, _ int, _ interface{}, filter map[string]interface{}, _ interface{}, _ bool) (*qdrant.ScrollResult, error) {
	must, _ := filter["must"].([]map[string]interface{})
	filePath, _ := must[0]["match"].(map[string]interface{})["value"].(string)
	if len(must) == 1 {
		keys := make([]string, 0)
		for key := range v {
			if strings.HasPrefix(key, filePath+"\x00") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		points := make([]qdrant.ScrollPoint, 0, len(keys))
		for _, key := range keys {
			point := v[key]
			payload := mergePayload(point.Payload, nil)
			generation, _ := payload["generation"].(string)
			fileHash, _ := payload["file_hash"].(string)
			digest, err := generationDigest(filePath, generation, "test-layout", fileHash, []qdrant.ScrollPoint{{ID: point.ID, Payload: payload}})
			if err != nil {
				return nil, err
			}
			payload["publication"] = publicationDescriptor{Version: publicationVersion, FilePath: filePath, Generation: generation, Layout: "test-layout", FileHash: fileHash, SealedAt: "2026-09-05T00:00:00Z", TotalChunks: 1, OrderedDigest: digest}
			points = append(points, qdrant.ScrollPoint{ID: point.ID, Payload: payload})
		}
		return &qdrant.ScrollResult{Points: points}, nil
	}
	generation, _ := must[1]["match"].(map[string]interface{})["value"].(string)
	point, ok := v[filePath+"\x00"+generation]
	if !ok {
		return &qdrant.ScrollResult{}, nil
	}
	payload := mergePayload(point.Payload, nil)
	fileHash, _ := payload["file_hash"].(string)
	digest, err := generationDigest(filePath, generation, "test-layout", fileHash, []qdrant.ScrollPoint{{ID: point.ID, Payload: payload}})
	if err != nil {
		return nil, err
	}
	payload["publication"] = publicationDescriptor{Version: publicationVersion, FilePath: filePath, Generation: generation, Layout: "test-layout", FileHash: fileHash, SealedAt: "2026-09-05T00:00:00Z", TotalChunks: 1, OrderedDigest: digest}
	return &qdrant.ScrollResult{Points: []qdrant.ScrollPoint{{ID: point.ID, Payload: payload}}}, nil
}
