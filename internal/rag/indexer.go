package rag

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

// Indexer walks the documents directory and maintains doc_chunks + doc_folders collections.
type Indexer struct {
	chunks   *qdrant.Client
	folders  *qdrant.Client
	embed    *embeddings.Client
	docsDir  string
	maxBytes int
}

func NewIndexer(chunks, folders *qdrant.Client, embed *embeddings.Client, docsDir string, maxBytes int) *Indexer {
	return &Indexer{
		chunks:   chunks,
		folders:  folders,
		embed:    embed,
		docsDir:  docsDir,
		maxBytes: maxBytes,
	}
}

// fileState is the snapshot of what Qdrant knows about a given file path:
// the stored content hash, the expected chunk count, and how many chunks
// are actually present (used to detect a half-indexed file).
type fileState struct {
	hash        string
	totalChunks int
	actualCount int
}

// Run performs an incremental re-index of the documents directory.
func (idx *Indexer) Run(ctx context.Context) error {
	slog.Info("RAG indexer started", "dir", idx.docsDir)

	// One scroll at the start — reused for unchanged-file skipping and for
	// stale-file detection. Avoids an O(N) round-trip storm in indexFile.
	state, err := idx.snapshotState(ctx)
	if err != nil {
		return fmt.Errorf("snapshot qdrant state: %w", err)
	}
	slog.Info("qdrant state loaded", "files", len(state))

	dirtyFolders := map[string]bool{}
	walkedFiles := map[string]bool{}
	walkHadErrors := false

	err = filepath.WalkDir(idx.docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			walkHadErrors = true
			slog.Warn("walk error", "path", path, "error", err)
			return nil
		}
		if d.IsDir() {
			// Skip hidden / system dirs (but never the root itself).
			if path != idx.docsDir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isTextFile(path) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		walkedFiles[path] = true
		changed, err := idx.indexFile(ctx, path, state[path])
		if err != nil {
			slog.Warn("failed to index file", "path", path, "error", err)
			return nil
		}
		if changed {
			dirtyFolders[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk: %w", err)
	}

	// Stale-file cleanup, guarded. If the walk had errors OR the walked count
	// is suspiciously low vs. what Qdrant knows, skip — a transient filesystem
	// issue (Resilio mid-sync, permission race) must not wipe the index.
	idx.cleanupStale(ctx, state, walkedFiles, dirtyFolders, walkHadErrors)

	for dir := range dirtyFolders {
		if err := idx.indexFolder(ctx, dir); err != nil {
			slog.Warn("failed to index folder", "dir", dir, "error", err)
		}
	}

	slog.Info("RAG indexer finished", "dirty_folders", len(dirtyFolders))
	return nil
}

// snapshotState scrolls the chunks collection once and returns a map keyed
// by file_path summarising each file's stored hash and chunk count.
func (idx *Indexer) snapshotState(ctx context.Context) (map[string]*fileState, error) {
	all, err := idx.chunks.ScrollAll(ctx, nil, false)
	if err != nil {
		return nil, err
	}
	state := map[string]*fileState{}
	for _, p := range all {
		fp, _ := p.Payload["file_path"].(string)
		if fp == "" {
			continue
		}
		s, ok := state[fp]
		if !ok {
			s = &fileState{}
			state[fp] = s
		}
		if s.hash == "" {
			if h, ok := p.Payload["file_hash"].(string); ok {
				s.hash = h
			}
		}
		if s.totalChunks == 0 {
			if tc, ok := p.Payload["total_chunks"].(float64); ok {
				s.totalChunks = int(tc)
			}
		}
		s.actualCount++
	}
	return state, nil
}

// indexFile embeds and upserts chunks for a single file. Returns true if
// anything in Qdrant changed. Embeds all chunks before touching Qdrant so
// that an embedding failure leaves the old state intact.
func (idx *Indexer) indexFile(ctx context.Context, path string, existing *fileState) (bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	ext := strings.ToLower(filepath.Ext(path))
	isMarkdown := ext == ".md" || ext == ".markdown"

	// Truly unchanged: same content AND every expected chunk is present.
	// A prior half-indexed state (actualCount < totalChunks) falls through
	// and triggers a fresh rebuild.
	if existing != nil &&
		existing.hash == hash &&
		existing.totalChunks > 0 &&
		existing.actualCount == existing.totalChunks {
		return false, nil
	}

	chunks := chunk(string(content), idx.maxBytes, isMarkdown)
	total := len(chunks)
	if total == 0 {
		return false, nil // empty file
	}

	// Batch-embed all chunks for this file in one shot (the embeddings client
	// splits into TEI-sized sub-batches internally).
	texts := make([]string, total)
	for i, c := range chunks {
		texts[i] = c.text
	}
	vecs, err := idx.embed.EmbedBatch(ctx, texts)
	if err != nil {
		return false, fmt.Errorf("embed %s: %w", path, err)
	}
	if len(vecs) != total {
		return false, fmt.Errorf("embed returned %d vectors for %d chunks of %s", len(vecs), total, path)
	}

	// Delete old chunks only now — after all embeddings succeeded.
	if existing != nil && existing.actualCount > 0 {
		if err := idx.deleteFileChunks(ctx, path); err != nil {
			slog.Warn("failed to delete old chunks", "path", path, "error", err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i, c := range chunks {
		id := chunkPointID(path, i)
		payload := map[string]interface{}{
			"text":         c.text,
			"file_path":    path,
			"folder_path":  filepath.Dir(path),
			"chunk_index":  i,
			"total_chunks": total,
			"heading":      c.heading,
			"file_hash":    hash,
			"indexed_at":   now,
		}
		if err := idx.chunks.Upsert(ctx, qdrant.Point{ID: id, Vector: vecs[i], Payload: payload}); err != nil {
			return false, fmt.Errorf("upsert chunk %d of %s: %w", i, path, err)
		}
	}

	slog.Info("indexed file", "path", path, "chunks", total)
	return true, nil
}

// indexFolder builds and upserts a folder summary point.
func (idx *Indexer) indexFolder(ctx context.Context, dir string) error {
	summary, err := folderSummary(dir)
	if err != nil {
		return err
	}
	vec, err := idx.embed.Embed(ctx, summary)
	if err != nil {
		return err
	}
	id := folderPointID(dir)
	payload := map[string]interface{}{
		"summary":     summary,
		"folder_path": dir,
		"indexed_at":  time.Now().UTC().Format(time.RFC3339),
	}
	return idx.folders.Upsert(ctx, qdrant.Point{ID: id, Vector: vec, Payload: payload})
}

// cleanupStale removes Qdrant chunks for files no longer on disk. Aborts if
// the walk was incomplete or if deletion would remove more than half the
// known files — both cases suggest a transient filesystem issue rather than
// intentional deletions.
func (idx *Indexer) cleanupStale(
	ctx context.Context,
	state map[string]*fileState,
	walkedFiles map[string]bool,
	dirtyFolders map[string]bool,
	walkHadErrors bool,
) {
	if walkHadErrors {
		slog.Warn("skipping stale cleanup: walk had errors")
		return
	}
	if len(state) > 0 && len(walkedFiles)*2 < len(state) {
		slog.Warn("skipping stale cleanup: walked file count suspiciously low",
			"walked", len(walkedFiles), "in_qdrant", len(state))
		return
	}

	removed := 0
	for fp := range state {
		if walkedFiles[fp] {
			continue
		}
		if err := idx.deleteFileChunks(ctx, fp); err != nil {
			slog.Warn("failed to delete stale chunks", "path", fp, "error", err)
			continue
		}
		removed++
		dirtyFolders[filepath.Dir(fp)] = true
	}
	if removed > 0 {
		slog.Info("removed stale file chunks", "files", removed)
	}
}

// deleteFileChunks removes all chunk points for a file.
func (idx *Indexer) deleteFileChunks(ctx context.Context, path string) error {
	filter := map[string]interface{}{
		"must": []map[string]interface{}{
			{"key": "file_path", "match": map[string]interface{}{"value": path}},
		},
	}
	points, err := idx.chunks.ScrollAll(ctx, filter, false)
	if err != nil {
		return err
	}
	ids := make([]string, len(points))
	for i, p := range points {
		ids[i] = p.ID
	}
	if len(ids) > 0 {
		return idx.chunks.Delete(ctx, ids)
	}
	return nil
}

func isTextFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".markdown", ".txt":
		return true
	}
	return false
}

func chunkPointID(filePath string, index int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s:%d", filePath, index)))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:20])
}

func folderPointID(dir string) string {
	h := sha1.Sum([]byte("folder:" + dir))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:20])
}
