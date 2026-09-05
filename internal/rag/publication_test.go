package rag

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/Dzarlax-AI/personal-memory/internal/config"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/mark3labs/mcp-go/mcp"
)

type controlledGenerationScroller struct {
	generations map[string][]qdrant.ScrollPoint
}

func (s controlledGenerationScroller) ScrollWithPayload(_ context.Context, _ int, _ interface{}, filter map[string]interface{}, _ interface{}, _ bool) (*qdrant.ScrollResult, error) {
	must, _ := filter["must"].([]map[string]interface{})
	filePath, _ := must[0]["match"].(map[string]interface{})["value"].(string)
	if len(must) == 1 {
		keys := make([]string, 0)
		for key := range s.generations {
			if strings.HasPrefix(key, filePath+"\x00") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		var points []qdrant.ScrollPoint
		for _, key := range keys {
			points = append(points, s.generations[key]...)
		}
		return &qdrant.ScrollResult{Points: points}, nil
	}
	generation, _ := must[1]["match"].(map[string]interface{})["value"].(string)
	points := s.generations[filePath+"\x00"+generation]
	return &qdrant.ScrollResult{Points: append([]qdrant.ScrollPoint(nil), points...)}, nil
}

func generationPoints(t *testing.T, filePath, generation string, texts []string, sealed bool) []qdrant.ScrollPoint {
	t.Helper()
	points := make([]qdrant.ScrollPoint, len(texts))
	for i, text := range texts {
		points[i] = qdrant.ScrollPoint{ID: generation + "-" + string(rune('a'+i)), Payload: map[string]interface{}{
			"text": text, "heading": "H", "file_path": filePath, "generation": generation,
			"layout": "test-layout", "file_hash": "test-hash-" + generation, "total_chunks": len(texts), "chunk_index": i,
		}}
	}
	if sealed {
		fileHash, _ := points[0].Payload["file_hash"].(string)
		digest, err := generationDigest(filePath, generation, "test-layout", fileHash, points)
		if err != nil {
			t.Fatal(err)
		}
		points[0].Payload["publication"] = publicationDescriptor{Version: publicationVersion, FilePath: filePath, Generation: generation, Layout: "test-layout", FileHash: fileHash, SealedAt: "2026-09-05T00:00:00Z", TotalChunks: len(texts), OrderedDigest: digest}
	}
	return points
}

func candidate(filePath, generation string, index int, score float64) qdrant.Point {
	return qdrant.Point{ID: "search-" + generation, Score: score, Payload: map[string]interface{}{
		"text": "untrusted search payload", "file_path": filePath, "generation": generation,
		"layout": "untrusted-layout", "file_hash": "untrusted", "total_chunks": 1, "chunk_index": index,
	}}
}

func setSealTime(points []qdrant.ScrollPoint, value string) {
	descriptor := points[0].Payload["publication"].(publicationDescriptor)
	descriptor.SealedAt = value
	points[0].Payload["publication"] = descriptor
}

func TestValidateSearchCandidates_OldSealedSurvivesNewPartial(t *testing.T) {
	file := "/documents/a.md"
	old := generationPoints(t, file, "old", []string{"old sealed"}, true)
	partial := generationPoints(t, file, "new", []string{"new partial", "missing"}, false)[:1]
	srv := &Server{validateChunks: controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{
		file + "\x00old": old, file + "\x00new": partial,
	}}}
	results, rejected := srv.validateSearchCandidates(context.Background(), []qdrant.Point{
		candidate(file, "new", 0, .99), candidate(file, "old", 0, .90),
	})
	if rejected.incomplete != 1 || rejected.legacyUnverified != 0 {
		t.Fatalf("rejected = %#v", rejected)
	}
	if len(results) != 1 || results[0].Payload["text"] != "old sealed" {
		t.Fatalf("results = %#v", results)
	}
}

func TestValidateSearchCandidates_OmitsUnsealedAndDigestMismatchedGenerations(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]qdrant.ScrollPoint)
	}{
		{name: "all chunks unsealed", mutate: func(points []qdrant.ScrollPoint) { delete(points[0].Payload, "publication") }},
		{name: "digest mismatch", mutate: func(points []qdrant.ScrollPoint) {
			descriptor := points[0].Payload["publication"].(publicationDescriptor)
			descriptor.OrderedDigest = "wrong"
			points[0].Payload["publication"] = descriptor
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := "/documents/a.md"
			points := generationPoints(t, file, "g", []string{"complete"}, true)
			test.mutate(points)
			srv := &Server{validateChunks: controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{file + "\x00g": points}}}
			results, rejected := srv.validateSearchCandidates(context.Background(), []qdrant.Point{candidate(file, "g", 0, .9)})
			if len(results) != 0 || rejected.incomplete != 1 {
				t.Fatalf("results=%#v rejected=%#v", results, rejected)
			}
		})
	}
}

func TestValidateGeneration_RejectsInconsistentFileHash(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]qdrant.ScrollPoint)
	}{
		{name: "chunk mismatch", mutate: func(points []qdrant.ScrollPoint) { points[1].Payload["file_hash"] = "different-content" }},
		{name: "descriptor mismatch", mutate: func(points []qdrant.ScrollPoint) {
			descriptor := points[0].Payload["publication"].(publicationDescriptor)
			descriptor.FileHash = "different-content"
			points[0].Payload["publication"] = descriptor
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := "/documents/a.md"
			points := generationPoints(t, file, "g", []string{"first", "second"}, true)
			test.mutate(points)
			_, err := validateGeneration(context.Background(), controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{file + "\x00g": points}}, file, "g", true)
			if err == nil {
				t.Fatal("validation accepted an inconsistent file_hash binding")
			}
		})
	}
}

func TestValidateSearchCandidates_OneSealedGenerationPerFile(t *testing.T) {
	fileA, fileB := "/documents/a.md", "/documents/b.md"
	newA := generationPoints(t, fileA, "a-new", []string{"a new"}, true)
	setSealTime(newA, "2026-09-05T00:00:01Z")
	srv := &Server{validateChunks: controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{
		fileA + "\x00a-old": generationPoints(t, fileA, "a-old", []string{"a old"}, true),
		fileA + "\x00a-new": newA,
		fileB + "\x00b":     generationPoints(t, fileB, "b", []string{"b"}, true),
	}}}
	results, rejected := srv.validateSearchCandidates(context.Background(), []qdrant.Point{
		candidate(fileA, "a-new", 0, .99), candidate(fileA, "a-old", 0, .98), candidate(fileB, "b", 0, .97),
	})
	if rejected.staleGeneration != 1 || len(results) != 2 || results[0].Payload["text"] != "a new" || results[1].Payload["text"] != "b" {
		t.Fatalf("results=%#v rejected=%#v", results, rejected)
	}
}

func TestValidateSearchCandidates_DiscoversNewerSealedGenerationOutsideSemanticCandidates(t *testing.T) {
	file := "/documents/a.md"
	old := generationPoints(t, file, "old", []string{"old text"}, true)
	newer := generationPoints(t, file, "new", []string{"new text"}, true)
	setSealTime(old, "2026-09-05T00:00:00Z")
	setSealTime(newer, "2026-09-05T00:00:01Z")
	srv := &Server{validateChunks: controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{
		file + "\x00old": old,
		file + "\x00new": newer,
	}}}
	// The semantic backend returned only the stale, higher-scoring chunk. File
	// discovery must still select the newer sealed publication.
	results, rejected := srv.validateSearchCandidates(context.Background(), []qdrant.Point{candidate(file, "old", 0, .99)})
	if len(results) != 0 || rejected.staleGeneration != 1 {
		t.Fatalf("results=%#v rejected=%#v; stale ranking must not be rebound to a fresh generation", results, rejected)
	}
}

func TestValidateSearchCandidates_PreservesScoreOnlyForSelectedGeneration(t *testing.T) {
	file := "/documents/a.md"
	old := generationPoints(t, file, "old", []string{"old text"}, true)
	newer := generationPoints(t, file, "new", []string{"new text"}, true)
	setSealTime(old, "2026-09-05T00:00:00Z")
	setSealTime(newer, "2026-09-05T00:00:01Z")
	srv := &Server{validateChunks: controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{
		file + "\x00old": old,
		file + "\x00new": newer,
	}}}
	results, rejected := srv.validateSearchCandidates(context.Background(), []qdrant.Point{candidate(file, "old", 0, .99), candidate(file, "new", 0, .50)})
	if len(results) != 1 || rejected.staleGeneration != 1 || results[0].Payload["generation"] != "new" || results[0].Score != .50 {
		t.Fatalf("results=%#v rejected=%#v; fresh generation must retain only its own score", results, rejected)
	}
}

func TestValidateSearchCandidates_EqualSealTimeUsesGenerationIDTieBreak(t *testing.T) {
	file := "/documents/a.md"
	low := generationPoints(t, file, "generation-a", []string{"a"}, true)
	high := generationPoints(t, file, "generation-z", []string{"z"}, true)
	setSealTime(low, "2026-09-05T00:00:00Z")
	setSealTime(high, "2026-09-05T00:00:00Z")
	srv := &Server{validateChunks: controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{
		file + "\x00generation-a": low,
		file + "\x00generation-z": high,
	}}}
	results, rejected := srv.validateSearchCandidates(context.Background(), []qdrant.Point{candidate(file, "generation-a", 0, .99)})
	if len(results) != 0 || rejected.staleGeneration != 1 {
		t.Fatalf("results=%#v rejected=%#v; stale candidate must not borrow the selected generation", results, rejected)
	}
}

func TestSearchDocuments_LegacyOmissionSignalsUnverifiedAndFormatsValidationPayload(t *testing.T) {
	file := "/documents/a.md"
	sealed := generationPoints(t, file, "sealed", []string{"validated text"}, true)
	srv := &Server{
		queryEmbed: fakeQueryEmbedder{},
		searchChunks: fakePointSearcher{search: func(map[string]any) []qdrant.Point {
			legacyWithoutGeneration := candidate(file, "", 0, .99)
			delete(legacyWithoutGeneration.Payload, "generation")
			legacyContentHashGeneration := candidate(file, "legacy-content-hash", 0, .98)
			delete(legacyContentHashGeneration.Payload, "layout")
			return []qdrant.Point{legacyWithoutGeneration, legacyContentHashGeneration, candidate(file, "sealed", 0, .90)}
		}},
		validateChunks: controlledGenerationScroller{generations: map[string][]qdrant.ScrollPoint{file + "\x00sealed": sealed}},
		cfg:            &config.Config{RAGDocumentsDir: "/documents"},
	}
	result, err := srv.handleSearchDocuments(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"query": "q", "mode": "flat"}}})
	if err != nil || result.IsError {
		t.Fatalf("tool result err=%v text=%s", err, ragToolResultText(t, result))
	}
	var response struct {
		Results []struct {
			Text string `json:"text"`
		} `json:"results"`
		Incomplete bool `json:"incomplete"`
		Rejected   struct {
			Legacy int `json:"legacy_unverified"`
		} `json:"rejected_candidates"`
	}
	if err := json.Unmarshal([]byte(ragToolResultText(t, result)), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Incomplete || response.Rejected.Legacy != 2 || len(response.Results) != 1 || response.Results[0].Text != "validated text" {
		t.Fatalf("response = %#v", response)
	}
}
