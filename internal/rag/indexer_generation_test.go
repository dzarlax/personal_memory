package rag

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

type generationHarness struct {
	server         *httptest.Server
	idx            *Indexer
	points         map[string]qdrant.Point
	failEmbed      bool
	failUpsertAt   int
	failSeal       bool
	failDelete     bool
	deleteFailures chan bool
	upserts        int
	deletes        int
	embeds         int
}

func newGenerationHarness(t *testing.T, maxBytes int) *generationHarness {
	t.Helper()
	h := &generationHarness{points: map[string]qdrant.Point{}}
	h.server = httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	t.Cleanup(h.server.Close)
	h.idx = NewIndexer(
		qdrant.NewClient(h.server.URL, "chunks"),
		qdrant.NewClient(h.server.URL, "folders"),
		embeddings.NewClient(h.server.URL),
		t.TempDir(),
		maxBytes,
	)
	return h
}

func (h *generationHarness) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/embed":
		h.embeds++
		if h.failEmbed {
			http.Error(w, "embedding unavailable", http.StatusServiceUnavailable)
			return
		}
		var request struct {
			Inputs []string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		vectors := make([][]float32, len(request.Inputs))
		for i := range vectors {
			vectors[i] = []float32{float32(i + 1), 1}
		}
		_ = json.NewEncoder(w).Encode(vectors)

	case strings.HasSuffix(r.URL.Path, "/points/scroll"):
		var request struct {
			Filter map[string]interface{} `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		ids := make([]string, 0, len(h.points))
		for id := range h.points {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		points := make([]map[string]interface{}, 0, len(ids))
		for _, id := range ids {
			if !matchesFilter(h.points[id].Payload, request.Filter) {
				continue
			}
			points = append(points, map[string]interface{}{
				"id":      id,
				"payload": h.points[id].Payload,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"result": map[string]interface{}{"points": points, "next_page_offset": nil},
		})

	case strings.HasSuffix(r.URL.Path, "/points") && r.Method == http.MethodPut:
		h.upserts++
		var request struct {
			Points []qdrant.Point `json:"points"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if h.failUpsertAt > 0 && h.upserts == h.failUpsertAt {
			http.Error(w, "injected upsert failure", http.StatusInternalServerError)
			return
		}
		if h.failSeal && len(request.Points) == 1 && request.Points[0].Payload["publication"] != nil {
			http.Error(w, "injected seal response loss", http.StatusInternalServerError)
			return
		}
		for _, p := range request.Points {
			h.points[p.ID] = p
		}
		h.writeCompleted(w)

	case strings.HasSuffix(r.URL.Path, "/points/delete"):
		h.deletes++
		if h.deleteFailures != nil && <-h.deleteFailures {
			http.Error(w, "channel-controlled delete failure", http.StatusInternalServerError)
			return
		}
		if h.failDelete {
			http.Error(w, "injected delete failure", http.StatusInternalServerError)
			return
		}
		var request struct {
			Points []string `json:"points"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		for _, id := range request.Points {
			delete(h.points, id)
		}
		h.writeCompleted(w)

	default:
		http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func TestRun_CleanupFailureKeepsNewestSealedGenerationSelected(t *testing.T) {
	h := newGenerationHarness(t, 100)
	path := writeRAGFile(t, h.idx.docsDir, "replacement")
	oldGeneration := "old-generation"
	h.seed(path, oldGeneration, 1)
	// The one buffered failure is consumed exactly by replacement cleanup,
	// making this regression deterministic without timing or retries.
	h.deleteFailures = make(chan bool, 1)
	h.deleteFailures <- true

	err := h.idx.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remove prior generations") {
		t.Fatalf("Run error = %v, want replacement cleanup failure", err)
	}

	generations := make(map[string][]qdrant.ScrollPoint)
	var newGeneration string
	for _, point := range h.points {
		generation, _ := point.Payload["generation"].(string)
		if generation == "" {
			continue
		}
		filePath, _ := point.Payload["file_path"].(string)
		key := filePath + "\x00" + generation
		generations[key] = append(generations[key], qdrant.ScrollPoint{ID: point.ID, Payload: point.Payload})
		if generation != oldGeneration {
			newGeneration = generation
		}
	}
	if newGeneration == "" {
		t.Fatal("replacement generation was not sealed before cleanup failure")
	}
	srv := &Server{validateChunks: controlledGenerationScroller{generations: generations}}
	results, rejected := srv.validateSearchCandidates(context.Background(), []qdrant.Point{
		candidate(path, oldGeneration, 0, .99),
		candidate(path, newGeneration, 0, .90),
	})
	if rejected != (rejectedCandidates{}) || len(results) != 1 || results[0].Payload["generation"] != newGeneration {
		t.Fatalf("results=%#v rejected=%#v; stale generation must not win by score", results, rejected)
	}
}

func (h *generationHarness) writeCompleted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
}

func (h *generationHarness) state(t *testing.T, path string) *fileState {
	t.Helper()
	state, err := h.idx.snapshotState(context.Background())
	if err != nil {
		t.Fatalf("snapshot state: %v", err)
	}
	return state[path]
}

func (h *generationHarness) seed(path, generation string, total int) string {
	id := "00000000-0000-0000-0000-" + fmt.Sprintf("%012d", len(h.points)+1)
	payload := map[string]interface{}{
		"text":         "old",
		"file_path":    path,
		"folder_path":  filepath.Dir(path),
		"chunk_index":  0,
		"total_chunks": total,
		"file_hash":    generation,
		"layout":       chunkLayout(h.idx.maxBytes),
	}
	if generation != "" {
		payload["generation"] = generation
		digest, err := generationDigest(path, generation, chunkLayout(h.idx.maxBytes), generation, []qdrant.ScrollPoint{{ID: id, Payload: payload}})
		if err != nil {
			panic(err)
		}
		payload["publication"] = publicationDescriptor{Version: publicationVersion, FilePath: path, Generation: generation, Layout: chunkLayout(h.idx.maxBytes), FileHash: generation, SealedAt: "2020-01-01T00:00:00Z", TotalChunks: total, OrderedDigest: digest}
	}
	h.points[id] = qdrant.Point{ID: id, Vector: []float32{1, 1}, Payload: payload}
	return id
}

func matchesFilter(payload map[string]interface{}, filter map[string]interface{}) bool {
	if filter == nil {
		return true
	}
	must, _ := filter["must"].([]interface{})
	if must == nil {
		if typed, ok := filter["must"].([]map[string]interface{}); ok {
			must = make([]interface{}, len(typed))
			for i := range typed {
				must[i] = typed[i]
			}
		}
	}
	for _, raw := range must {
		condition, _ := raw.(map[string]interface{})
		key, _ := condition["key"].(string)
		match, _ := condition["match"].(map[string]interface{})
		if payload[key] != match["value"] {
			return false
		}
	}
	return true
}

func writeRAGFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func contentGeneration(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func TestIndexFile_EmbeddingFailurePreservesCompleteGeneration(t *testing.T) {
	h := newGenerationHarness(t, 100)
	path := writeRAGFile(t, h.idx.docsDir, "new content")
	oldID := h.seed(path, "old-generation", 1)
	h.failEmbed = true

	changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err == nil || changed {
		t.Fatalf("indexFile = changed %v, err %v; want unchanged error", changed, err)
	}
	if _, ok := h.points[oldID]; !ok {
		t.Fatal("embedding failure removed the last complete generation")
	}
	if h.deletes != 0 || len(h.points) != 1 {
		t.Fatalf("embedding failure mutated Qdrant: deletes=%d points=%d", h.deletes, len(h.points))
	}
}

func TestIndexFile_InvalidChunkLimitPreservesCompleteGeneration(t *testing.T) {
	for _, maxBytes := range []int{0, maxIndexerChunkBytes + 1} {
		t.Run(fmt.Sprintf("maxBytes_%d", maxBytes), func(t *testing.T) {
			h := newGenerationHarness(t, maxBytes)
			path := writeRAGFile(t, h.idx.docsDir, "")
			oldID := h.seed(path, "old-generation", 1)

			changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
			if err == nil || changed {
				t.Fatalf("indexFile = changed %v, err %v; want unchanged validation error", changed, err)
			}
			if _, ok := h.points[oldID]; !ok {
				t.Fatal("invalid chunk limit removed the last complete generation")
			}
			if h.deletes != 0 || h.embeds != 0 || h.upserts != 0 {
				t.Fatalf("invalid chunk limit mutated dependencies: deletes=%d embeds=%d upserts=%d", h.deletes, h.embeds, h.upserts)
			}
		})
	}
}

func TestIndexFile_MidUpsertFailureResumesThenSwitchesGeneration(t *testing.T) {
	h := newGenerationHarness(t, 6)
	content := "aaaa. bbbb."
	path := writeRAGFile(t, h.idx.docsDir, content)
	oldID := h.seed(path, "old-generation", 1)
	h.failUpsertAt = 2

	changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err == nil || !changed {
		t.Fatalf("first indexFile = changed %v, err %v; want partial-change error", changed, err)
	}
	if _, ok := h.points[oldID]; !ok {
		t.Fatal("mid-upsert failure removed the old complete generation")
	}
	if len(h.points) != 2 { // old + first point of the partial generation
		t.Fatalf("points after partial write = %d, want 2", len(h.points))
	}

	h.failUpsertAt = 0
	changed, err = h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err != nil || !changed {
		t.Fatalf("retry indexFile = changed %v, err %v; want changed success", changed, err)
	}
	if _, ok := h.points[oldID]; ok {
		t.Fatal("old generation remains after the replacement became complete")
	}
	state := h.state(t, path)
	if _, ok := state.complete(contentGeneration(content), chunkLayout(h.idx.maxBytes), 2); !ok || len(h.points) != 2 {
		t.Fatalf("replacement generation is not complete: %#v", state)
	}
}

func TestIndexFile_SuccessfulSwitchDeletesOnlyPriorGeneration(t *testing.T) {
	h := newGenerationHarness(t, 100)
	content := "replacement"
	path := writeRAGFile(t, h.idx.docsDir, content)
	oldID := h.seed(path, "old-generation", 1)

	changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err != nil || !changed {
		t.Fatalf("indexFile = changed %v, err %v", changed, err)
	}
	if _, ok := h.points[oldID]; ok {
		t.Fatal("prior generation was not deleted")
	}
	if _, ok := h.state(t, path).complete(contentGeneration(content), chunkLayout(h.idx.maxBytes), 1); !ok {
		t.Fatal("new generation is not complete")
	}
}

func TestIndexFile_SealFailureNeverCleansUpPriorGeneration(t *testing.T) {
	h := newGenerationHarness(t, 100)
	content := "replacement"
	path := writeRAGFile(t, h.idx.docsDir, content)
	oldID := h.seed(path, "old-generation", 1)
	h.failSeal = true

	changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err == nil || !changed {
		t.Fatalf("indexFile = changed %v, err %v; want ambiguous seal failure", changed, err)
	}
	if _, ok := h.points[oldID]; !ok {
		t.Fatal("prior generation was deleted after ambiguous seal failure")
	}
	if len(h.points) != 2 { // old sealed + new unsealed
		t.Fatalf("points after seal failure = %d, want 2", len(h.points))
	}
}

func TestIndexFile_UnchangedCompleteGenerationDoesNoWork(t *testing.T) {
	h := newGenerationHarness(t, 100)
	content := "stable"
	path := writeRAGFile(t, h.idx.docsDir, content)
	if changed, err := h.idx.indexFile(context.Background(), path, nil); err != nil || !changed {
		t.Fatalf("initial index = changed %v, err %v", changed, err)
	}
	h.embeds, h.upserts, h.deletes = 0, 0, 0

	changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err != nil || changed {
		t.Fatalf("unchanged index = changed %v, err %v", changed, err)
	}
	if h.embeds != 0 || h.upserts != 0 || h.deletes != 0 {
		t.Fatalf("unchanged generation caused work: embeds=%d upserts=%d deletes=%d", h.embeds, h.upserts, h.deletes)
	}
}

func TestRunRefreshesLegacyFolderSummaryWithoutRewritingChunks(t *testing.T) {
	h := newGenerationHarness(t, 100)
	content := "# Stable\nunchanged"
	path := writeRAGFile(t, h.idx.docsDir, content)
	if changed, err := h.idx.indexFile(context.Background(), path, nil); err != nil || !changed {
		t.Fatalf("initial index = changed %v, err %v", changed, err)
	}
	dir := filepath.Dir(path)
	folderID := folderPointID(dir)
	h.points[folderID] = qdrant.Point{ID: folderID, Payload: map[string]interface{}{
		"folder_path": dir, "summary": "legacy absolute summary",
	}}
	h.embeds, h.upserts, h.deletes = 0, 0, 0

	if err := h.idx.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.embeds != 1 || h.upserts != 1 || h.deletes != 0 {
		t.Fatalf("refresh calls embeds=%d upserts=%d deletes=%d", h.embeds, h.upserts, h.deletes)
	}
	payload := h.points[folderID].Payload
	if payload["summary_version"] != folderSummaryVersion || payload["relative_folder_path"] != "" {
		t.Fatalf("folder payload = %#v", payload)
	}
	if summary, _ := payload["summary"].(string); strings.Contains(summary, h.idx.docsDir) {
		t.Fatalf("summary leaked absolute root: %q", summary)
	}
}

func TestRunRemovesLegacyHiddenFolderSummaryWithoutRefreshingIt(t *testing.T) {
	h := newGenerationHarness(t, 100)
	hiddenDir := filepath.Join(h.idx.docsDir, ".sync")
	if err := os.Mkdir(hiddenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hiddenDir, "private.md"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	folderID := folderPointID(hiddenDir)
	h.points[folderID] = qdrant.Point{ID: folderID, Payload: map[string]interface{}{
		"folder_path": hiddenDir, "summary": "legacy hidden summary",
	}}

	if err := h.idx.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, exists := h.points[folderID]; exists {
		t.Fatal("legacy hidden-folder summary remains indexed")
	}
	if h.embeds != 0 || h.upserts != 0 || h.deletes != 1 {
		t.Fatalf("hidden cleanup calls embeds=%d upserts=%d deletes=%d", h.embeds, h.upserts, h.deletes)
	}
}

func TestIndexFile_LegacyUnversionedChunksAreUpgraded(t *testing.T) {
	h := newGenerationHarness(t, 100)
	content := "same content"
	path := writeRAGFile(t, h.idx.docsDir, content)
	legacyID := h.seed(path, "", 1)
	h.points[legacyID].Payload["file_hash"] = contentGeneration(content)

	changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err != nil || !changed {
		t.Fatalf("legacy upgrade = changed %v, err %v", changed, err)
	}
	if _, ok := h.points[legacyID]; ok {
		t.Fatal("legacy point remains after successful versioned replacement")
	}
	if _, ok := h.state(t, path).complete(contentGeneration(content), chunkLayout(h.idx.maxBytes), 1); !ok {
		t.Fatal("versioned replacement is not complete")
	}
}

func TestIndexFile_EmptyFileRemovesOldChunksWithoutEmbedding(t *testing.T) {
	h := newGenerationHarness(t, 100)
	path := writeRAGFile(t, h.idx.docsDir, "")
	oldID := h.seed(path, "old-generation", 1)

	changed, err := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if err != nil || !changed {
		t.Fatalf("empty cleanup = changed %v, err %v", changed, err)
	}
	if _, ok := h.points[oldID]; ok || len(h.points) != 0 {
		t.Fatal("empty file did not remove all old chunks")
	}
	if h.embeds != 0 || h.upserts != 0 || h.deletes != 1 {
		t.Fatalf("empty cleanup calls: embeds=%d upserts=%d deletes=%d", h.embeds, h.upserts, h.deletes)
	}
}

func TestReconcileFolder_HealthyWalkDeletesMissingOrEmptyFolderPoint(t *testing.T) {
	for _, test := range []struct {
		name string
		dir  func(*testing.T) string
	}{
		{name: "missing", dir: func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }},
		{name: "empty", dir: func(t *testing.T) string { return t.TempDir() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newGenerationHarness(t, 100)
			if err := h.idx.reconcileFolder(context.Background(), test.dir(t), true); err != nil {
				t.Fatalf("reconcileFolder: %v", err)
			}
			if h.deletes != 1 {
				t.Fatalf("folder deletes = %d, want 1", h.deletes)
			}
		})
	}
}

func TestReconcileFolder_UnhealthyWalkNeverDeletesFolderSummary(t *testing.T) {
	h := newGenerationHarness(t, 100)
	missing := filepath.Join(t.TempDir(), "not-observed")
	healthy, err := h.idx.cleanupStale(
		context.Background(),
		map[string]*fileState{"known.txt": {generations: map[string]*generationState{}}},
		map[string]bool{},
		map[string]bool{},
		true,
	)
	if err != nil {
		t.Fatalf("cleanupStale: %v", err)
	}
	if healthy {
		t.Fatal("walk with errors was classified as healthy")
	}

	if err := h.idx.reconcileFolder(context.Background(), missing, healthy); err != nil {
		t.Fatalf("reconcileFolder: %v", err)
	}
	if h.deletes != 0 {
		t.Fatalf("unhealthy walk deleted %d folder summaries", h.deletes)
	}
}

func TestCleanupStale_DeleteFailureIsReturned(t *testing.T) {
	h := newGenerationHarness(t, 100)
	h.failDelete = true
	stale := filepath.Join(h.idx.docsDir, "stale.txt")
	live := filepath.Join(h.idx.docsDir, "live.txt")
	state := map[string]*fileState{
		stale: {
			generations: map[string]*generationState{
				"old": {pointIDs: []string{"00000000-0000-0000-0000-000000000001"}},
			},
		},
		live: {
			generations: map[string]*generationState{
				"old": {pointIDs: []string{"00000000-0000-0000-0000-000000000002"}},
			},
		},
	}

	healthy, err := h.idx.cleanupStale(
		context.Background(),
		state,
		map[string]bool{live: true},
		map[string]bool{},
		false,
	)
	if !healthy {
		t.Fatal("healthy walk was incorrectly classified as unsafe")
	}
	if err == nil || !strings.Contains(err.Error(), "stale.txt") {
		t.Fatalf("cleanup error = %v, want observable stale-file delete failure", err)
	}
}
