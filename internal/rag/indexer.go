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

// Run performs an incremental re-index of the documents directory.
func (idx *Indexer) Run(ctx context.Context) error {
	slog.Info("RAG indexer started", "dir", idx.docsDir)
	dirtyFolders := map[string]bool{}
	walkedFiles := map[string]bool{}

	err := filepath.WalkDir(idx.docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() || !isTextFile(path) {
			return nil
		}
		walkedFiles[path] = true
		changed, err := idx.indexFile(ctx, path)
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

	// Remove chunks for files that no longer exist on disk.
	deleted, err := idx.removeStaleFiles(ctx, walkedFiles)
	if err != nil {
		slog.Warn("stale file cleanup failed", "error", err)
	} else if deleted > 0 {
		slog.Info("removed stale file chunks", "files", deleted)
	}

	for dir := range dirtyFolders {
		if err := idx.indexFolder(ctx, dir); err != nil {
			slog.Warn("failed to index folder", "dir", dir, "error", err)
		}
	}

	slog.Info("RAG indexer finished", "dirty_folders", len(dirtyFolders))
	return nil
}

// indexFile embeds and upserts chunks for a single file. Returns true if anything changed.
// Embeds all chunks before touching Qdrant to avoid partial-failure data loss.
func (idx *Indexer) indexFile(ctx context.Context, path string) (bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	ext := strings.ToLower(filepath.Ext(path))
	isMarkdown := ext == ".md" || ext == ".markdown"

	existingHash := idx.existingFileHash(ctx, path)
	if existingHash == hash {
		return false, nil // unchanged
	}

	chunks := chunk(string(content), idx.maxBytes, isMarkdown)
	total := len(chunks)

	// Embed all chunks before touching Qdrant — if embedding fails,
	// old chunks remain intact.
	type embeddedChunk struct {
		c   chunkResult
		vec []float32
	}
	embedded := make([]embeddedChunk, 0, total)
	for i, c := range chunks {
		vec, err := idx.embed.Embed(ctx, c.text)
		if err != nil {
			return false, fmt.Errorf("embed chunk %d of %s: %w", i, path, err)
		}
		embedded = append(embedded, embeddedChunk{c: c, vec: vec})
	}

	// Delete old chunks only after all embeddings succeed.
	if existingHash != "" {
		if err := idx.deleteFileChunks(ctx, path); err != nil {
			slog.Warn("failed to delete old chunks", "path", path, "error", err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i, ec := range embedded {
		id := chunkPointID(path, i)
		payload := map[string]interface{}{
			"text":         ec.c.text,
			"file_path":    path,
			"folder_path":  filepath.Dir(path),
			"chunk_index":  i,
			"total_chunks": total,
			"heading":      ec.c.heading,
			"file_hash":    hash,
			"indexed_at":   now,
		}
		if err := idx.chunks.Upsert(ctx, qdrant.Point{ID: id, Vector: ec.vec, Payload: payload}); err != nil {
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

// removeStaleFiles deletes Qdrant chunks for files that are no longer on disk.
// Returns the number of stale files removed.
func (idx *Indexer) removeStaleFiles(ctx context.Context, walkedFiles map[string]bool) (int, error) {
	all, err := idx.chunks.ScrollAll(ctx, nil, false)
	if err != nil {
		return 0, err
	}

	// Collect unique file paths present in Qdrant.
	inQdrant := map[string]bool{}
	for _, p := range all {
		if fp, ok := p.Payload["file_path"].(string); ok {
			inQdrant[fp] = true
		}
	}

	removed := 0
	for fp := range inQdrant {
		if walkedFiles[fp] {
			continue
		}
		if err := idx.deleteFileChunks(ctx, fp); err != nil {
			slog.Warn("failed to delete stale chunks", "path", fp, "error", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// existingFileHash returns the stored file_hash for a file, or "" if not found.
func (idx *Indexer) existingFileHash(ctx context.Context, path string) string {
	filter := map[string]interface{}{
		"must": []map[string]interface{}{
			{"key": "file_path", "match": map[string]interface{}{"value": path}},
		},
	}
	result, err := idx.chunks.Scroll(ctx, 1, nil, filter, false)
	if err != nil || len(result.Points) == 0 {
		return ""
	}
	if h, ok := result.Points[0].Payload["file_hash"].(string); ok {
		return h
	}
	return ""
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
