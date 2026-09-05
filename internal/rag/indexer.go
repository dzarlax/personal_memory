package rag

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const maxIndexerChunkBytes = 1024 * 1024

// folderSummaryVersion changes whenever the deterministic summary input or
// payload contract changes. It lets reindex refresh folder vectors without
// touching already-complete chunk generations.
const folderSummaryVersion = "relative-metadata-v2"

func NewIndexer(chunks, folders *qdrant.Client, embed *embeddings.Client, docsDir string, maxBytes int) *Indexer {
	return &Indexer{
		chunks:   chunks,
		folders:  folders,
		embed:    embed,
		docsDir:  docsDir,
		maxBytes: maxBytes,
	}
}

// fileState is the snapshot of every generation Qdrant knows for one file.
// Keeping point IDs lets a completed replacement remove only prior
// generations, including legacy unversioned points.
type fileState struct {
	generations map[string]*generationState
}

// generationState describes one independently-addressable version of a file.
// An empty generation is the legacy layout, whose point IDs did not include
// the content hash.
type generationState struct {
	totalChunks  int
	chunkIndexes map[int]bool
	pointIDs     []string
	fileHash     string
	layout       string
	sealed       bool
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
	folderRefresh, err := idx.foldersNeedingRefresh(ctx)
	if err != nil {
		return fmt.Errorf("snapshot folder summaries: %w", err)
	}

	dirtyFolders := map[string]bool{}
	var indexErrors []error
	for dir := range folderRefresh {
		dirtyFolders[dir] = true
	}
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
		if changed {
			dirtyFolders[filepath.Dir(path)] = true
		}
		if err != nil {
			slog.Warn("failed to index file", "path", path, "error", err)
			indexErrors = append(indexErrors, fmt.Errorf("index file %s: %w", path, err))
			return nil
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk: %w", err)
	}

	// Stale-file cleanup, guarded. If the walk had errors OR the walked count
	// is suspiciously low vs. what Qdrant knows, skip — a transient filesystem
	// issue (Resilio mid-sync, permission race) must not wipe the index.
	cleanupHealthy, cleanupErr := idx.cleanupStale(ctx, state, walkedFiles, dirtyFolders, walkHadErrors)

	var reconcileErrors []error
	reconcileErrors = append(reconcileErrors, indexErrors...)
	if cleanupErr != nil {
		reconcileErrors = append(reconcileErrors, cleanupErr)
	}
	for dir := range dirtyFolders {
		if err := idx.reconcileFolder(ctx, dir, cleanupHealthy); err != nil {
			slog.Warn("failed to reconcile folder", "dir", dir, "error", err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile folder %s: %w", dir, err))
		}
	}

	slog.Info("RAG indexer finished", "dirty_folders", len(dirtyFolders))
	return errors.Join(reconcileErrors...)
}

// snapshotState scrolls the chunks collection once and returns a map keyed
// by file_path summarising each file's stored hash and chunk count. Only the
// fields needed for change detection are transferred — skipping the bulky
// `text` field cuts the scroll payload roughly 10x on a mature index.
func (idx *Indexer) snapshotState(ctx context.Context) (map[string]*fileState, error) {
	fields := []string{"file_path", "generation", "total_chunks", "chunk_index", "file_hash", "layout", "publication"}
	all, err := idx.chunks.ScrollAllWithPayload(ctx, nil, fields, false)
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
			s = &fileState{generations: map[string]*generationState{}}
			state[fp] = s
		}
		generation, _ := p.Payload["generation"].(string)
		g, ok := s.generations[generation]
		if !ok {
			g = &generationState{chunkIndexes: map[int]bool{}}
			s.generations[generation] = g
		}
		if tc, ok := payloadInt(p.Payload["total_chunks"]); ok && tc > g.totalChunks {
			g.totalChunks = tc
		}
		if ci, ok := payloadInt(p.Payload["chunk_index"]); ok {
			g.chunkIndexes[ci] = true
		}
		if hash, ok := p.Payload["file_hash"].(string); ok {
			g.fileHash = hash
		}
		if layout, ok := p.Payload["layout"].(string); ok {
			g.layout = layout
		}
		if ci, ok := payloadInt(p.Payload["chunk_index"]); ok && ci == 0 {
			if descriptor, err := parsePublicationDescriptor(p.Payload["publication"]); err == nil && descriptor.Version == publicationVersion && descriptor.Generation == generation {
				g.sealed = true
			}
		}
		g.pointIDs = append(g.pointIDs, p.ID)
	}
	return state, nil
}

// indexFile embeds and upserts chunks for a single file. Returns true if
// anything in Qdrant changed. Embeds all chunks before touching Qdrant so
// that an embedding failure leaves the old state intact.
func (idx *Indexer) indexFile(ctx context.Context, path string, existing *fileState) (bool, error) {
	// Validate again at the mutation boundary. Config validation protects normal
	// constructors, but tests and internal callers can construct Indexer directly;
	// an invalid chunk size must never look like an intentionally empty file and
	// trigger deletion of its last complete generation.
	if idx.maxBytes < 1 || idx.maxBytes > maxIndexerChunkBytes {
		return false, fmt.Errorf("invalid chunk max bytes %d: must be between 1 and %d", idx.maxBytes, maxIndexerChunkBytes)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	layout := chunkLayout(idx.maxBytes)
	ext := strings.ToLower(filepath.Ext(path))
	isMarkdown := ext == ".md" || ext == ".markdown"

	chunks := chunk(string(content), idx.maxBytes, isMarkdown)
	total := len(chunks)
	if total == 0 {
		if existing == nil || len(existing.allPointIDs()) == 0 {
			return false, nil
		}
		// An empty, successfully-read file intentionally has no indexed chunks.
		// This is distinct from a read/chunking failure and is therefore safe to
		// use as the commit point for removing every older generation.
		if err := idx.chunks.Delete(ctx, existing.allPointIDs()); err != nil {
			return false, fmt.Errorf("remove chunks for empty file %s: %w", path, err)
		}
		slog.Info("removed chunks for empty file", "path", path)
		return true, nil
	}
	if total > maxPublicationValidationPoints {
		return false, fmt.Errorf("document %s has %d chunks; maximum supported by generation publication is %d", path, total, maxPublicationValidationPoints)
	}

	// A generation is complete only when all expected indices are present.
	// Legacy points have no generation payload and must be upgraded even when
	// their file_hash happens to equal the current content hash.
	if existing != nil {
		if current, ok := existing.complete(hash, layout, total); ok {
			// The compact startup snapshot is only a change detector. Re-read and
			// prove the seal before declaring an on-disk file unchanged; a stale
			// or malformed descriptor must be repaired by a new generation, not
			// silently keep the file invisible to strict readers.
			if _, err := validateGeneration(ctx, idx.chunks, path, current, true); err == nil {
				oldIDs := existing.pointIDsExcept(current)
				if len(oldIDs) == 0 {
					return false, nil
				}
				if err := idx.chunks.Delete(ctx, oldIDs); err != nil {
					return false, fmt.Errorf("remove prior generations of %s: %w", path, err)
				}
				slog.Info("removed prior file generations", "path", path, "points", len(oldIDs))
				return true, nil
			}
		}
	}

	generation, err := newGenerationID(hash, layout)
	if err != nil {
		return false, fmt.Errorf("create generation for %s: %w", path, err)
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

	now := time.Now().UTC().Format(time.RFC3339)
	wroteAny := false
	for i, c := range chunks {
		id := chunkPointID(path, generation, i)
		payload := map[string]interface{}{
			"text":         c.text,
			"file_path":    path,
			"folder_path":  filepath.Dir(path),
			"chunk_index":  i,
			"total_chunks": total,
			"heading":      c.heading,
			"file_hash":    hash,
			"generation":   generation,
			"layout":       layout,
			"indexed_at":   now,
		}
		if err := idx.chunks.Upsert(ctx, qdrant.Point{ID: id, Vector: vecs[i], Payload: payload}); err != nil {
			return wroteAny, fmt.Errorf("upsert chunk %d of %s: %w", i, path, err)
		}
		wroteAny = true
	}

	// A generation cannot be visible to ordinary reads until this exact
	// readback is complete. In particular, never infer completion from our own
	// successful upsert responses: Qdrant can accept only part of a batch during
	// a transport failure.
	validated, err := validateGeneration(ctx, idx.chunks, path, generation, false)
	if err != nil {
		return true, fmt.Errorf("validate unsealed generation for %s: %w", path, err)
	}
	chunkZero := validated.points[0]
	descriptor := publicationDescriptor{
		Version:       publicationVersion,
		FilePath:      path,
		Generation:    generation,
		Layout:        layout,
		FileHash:      hash,
		SealedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		TotalChunks:   total,
		OrderedDigest: validated.digest,
	}
	// The seal response is the publication commit. A lost/failed response is
	// intentionally ambiguous: retain every older sealed generation and let a
	// future indexing attempt create a fresh immutable generation.
	if err := idx.chunks.Upsert(ctx, qdrant.Point{ID: chunkZero.ID, Vector: vecs[0], Payload: sealPayload(chunkZero.Payload, descriptor)}); err != nil {
		return true, fmt.Errorf("seal generation for %s: %w", path, err)
	}

	// New and old IDs differ, so the previous complete generation remains
	// queryable throughout embedding and every confirmed upsert. Only after the
	// new generation is complete do we remove older/legacy points. If an upsert
	// fails, the partial generation is safely overwritten on the next run.
	if existing != nil {
		oldIDs := existing.pointIDsExcept(generation)
		if len(oldIDs) > 0 {
			if err := idx.chunks.Delete(ctx, oldIDs); err != nil {
				return true, fmt.Errorf("remove prior generations of %s: %w", path, err)
			}
		}
	}

	slog.Info("indexed file", "path", path, "chunks", total)
	return true, nil
}

// indexFolder builds and upserts a folder summary point.
func (idx *Indexer) indexFolder(ctx context.Context, dir string) error {
	relativePath, err := filepath.Rel(idx.docsDir, dir)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("folder %q is outside documents root", dir)
	}
	relativePath = filepath.ToSlash(relativePath)
	if relativePath == "." {
		relativePath = ""
	}
	summary, err := folderSummary(dir, relativePath)
	if err != nil {
		return err
	}
	vec, err := idx.embed.Embed(ctx, summary)
	if err != nil {
		return err
	}
	id := folderPointID(dir)
	payload := map[string]interface{}{
		"summary":              summary,
		"folder_path":          dir,
		"relative_folder_path": relativePath,
		"summary_version":      folderSummaryVersion,
		"indexed_at":           time.Now().UTC().Format(time.RFC3339),
	}
	return idx.folders.Upsert(ctx, qdrant.Point{ID: id, Vector: vec, Payload: payload})
}

// foldersNeedingRefresh returns stored, in-root folder paths whose summary
// predates the current contract, plus legacy hidden paths that need guarded
// removal rather than refresh.
func (idx *Indexer) foldersNeedingRefresh(ctx context.Context) (map[string]bool, error) {
	points, err := idx.folders.ScrollAllWithPayload(ctx, nil,
		[]string{"folder_path", "summary_version", "relative_folder_path"}, false)
	if err != nil {
		return nil, err
	}
	refresh := make(map[string]bool)
	for _, point := range points {
		dir, _ := point.Payload["folder_path"].(string)
		if dir == "" || !pathWithinRoot(idx.docsDir, dir) {
			continue
		}
		if pathContainsHiddenComponent(idx.docsDir, dir) {
			refresh[dir] = true
			continue
		}
		version, _ := point.Payload["summary_version"].(string)
		_, relativePresent := point.Payload["relative_folder_path"]
		if version != folderSummaryVersion || !relativePresent {
			refresh[dir] = true
		}
	}
	return refresh, nil
}

func pathWithinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathContainsHiddenComponent(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == "" {
		return false
	}
	for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
		if strings.HasPrefix(component, ".") {
			return true
		}
	}
	return false
}

// reconcileFolder refreshes a live folder summary or removes its point once
// the folder has disappeared (or no longer contains indexable files). Deletes
// are allowed only after a healthy filesystem walk; an incomplete walk must
// never turn missing observations into destructive cleanup.
func (idx *Indexer) reconcileFolder(ctx context.Context, dir string, allowDelete bool) error {
	if pathContainsHiddenComponent(idx.docsDir, dir) {
		if !allowDelete {
			slog.Warn("skipping hidden folder summary cleanup after unhealthy walk", "dir", dir)
			return nil
		}
		if err := idx.folders.Delete(ctx, []string{folderPointID(dir)}); err != nil {
			return fmt.Errorf("delete hidden folder point: %w", err)
		}
		slog.Info("removed hidden folder point", "dir", dir)
		return nil
	}
	hasFiles, err := folderHasIndexableFiles(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && hasFiles {
		return idx.indexFolder(ctx, dir)
	}
	if !allowDelete {
		slog.Warn("skipping folder summary cleanup after unhealthy walk", "dir", dir)
		return nil
	}
	if err := idx.folders.Delete(ctx, []string{folderPointID(dir)}); err != nil {
		return fmt.Errorf("delete stale folder point: %w", err)
	}
	slog.Info("removed stale folder point", "dir", dir)
	return nil
}

func folderHasIndexableFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if isTextFile(entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}

// staleCleanupDecision decides whether to skip removing Qdrant chunks for
// files that aren't on disk. Returns (skip, reason). Suppressing cleanup on
// unhealthy walks is what protects the index against transient FS glitches
// (Resilio mid-sync, permission race) that would otherwise wipe large parts
// of it. Kept pure so it can be unit-tested without Qdrant.
func staleCleanupDecision(walkedFiles, knownFiles int, walkHadErrors bool) (skip bool, reason string) {
	if walkHadErrors {
		return true, "walk had errors"
	}
	// Guard only meaningful once we have something to compare against. On a
	// fresh index (knownFiles == 0) there's nothing to delete anyway.
	if knownFiles > 0 && walkedFiles*2 < knownFiles {
		return true, "walked file count suspiciously low"
	}
	return false, ""
}

// cleanupStale removes Qdrant chunks for files no longer on disk.
func (idx *Indexer) cleanupStale(
	ctx context.Context,
	state map[string]*fileState,
	walkedFiles map[string]bool,
	dirtyFolders map[string]bool,
	walkHadErrors bool,
) (healthy bool, cleanupErr error) {
	if skip, reason := staleCleanupDecision(len(walkedFiles), len(state), walkHadErrors); skip {
		slog.Warn("skipping stale cleanup",
			"reason", reason,
			"walked", len(walkedFiles),
			"in_qdrant", len(state))
		return false, nil
	}

	removed := 0
	var cleanupErrors []error
	for fp := range state {
		if walkedFiles[fp] {
			continue
		}
		if err := idx.deleteFileChunks(ctx, fp); err != nil {
			slog.Warn("failed to delete stale chunks", "path", fp, "error", err)
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete stale chunks for %s: %w", fp, err))
			continue
		}
		removed++
		dirtyFolders[filepath.Dir(fp)] = true
	}
	if removed > 0 {
		slog.Info("removed stale file chunks", "files", removed)
	}
	return true, errors.Join(cleanupErrors...)
}

// deleteFileChunks removes all chunk points for a file in a single request.
func (idx *Indexer) deleteFileChunks(ctx context.Context, path string) error {
	filter := map[string]interface{}{
		"must": []map[string]interface{}{
			{"key": "file_path", "match": map[string]interface{}{"value": path}},
		},
	}
	return idx.chunks.DeleteByFilter(ctx, filter)
}

func isTextFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".markdown", ".txt":
		return true
	}
	return false
}

func chunkPointID(filePath, generation string, index int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s:%s:%d", filePath, generation, index)))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func chunkLayout(maxBytes int) string {
	return fmt.Sprintf("markdown-heading-paragraph-sentence-v1:max-bytes=%d", maxBytes)
}

func newGenerationID(fileHash, layout string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	// The layout is part of the immutable identity, not merely a payload field;
	// equal content rechunked under a new layout can never overwrite old points.
	sum := sha256.Sum256([]byte(fileHash + "\x00" + layout + "\x00" + hex.EncodeToString(nonce[:])))
	return "g1-" + hex.EncodeToString(sum[:]), nil
}

func payloadInt(value interface{}) (int, bool) {
	switch n := value.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case float32:
		return int(n), n == float32(int(n))
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		v, err := n.Int64()
		return int(v), err == nil
	default:
		return 0, false
	}
}

func (s *fileState) complete(fileHash, layout string, total int) (string, bool) {
	if s == nil {
		return "", false
	}
	for generation, g := range s.generations {
		if g == nil || !g.sealed || g.fileHash != fileHash || g.layout != layout || g.totalChunks != total || len(g.chunkIndexes) != total {
			continue
		}
		complete := true
		for i := 0; i < total; i++ {
			if !g.chunkIndexes[i] {
				complete = false
				break
			}
		}
		if complete {
			return generation, true
		}
	}
	return "", false
}

func (s *fileState) allPointIDs() []string {
	if s == nil {
		return nil
	}
	var ids []string
	for _, g := range s.generations {
		ids = append(ids, g.pointIDs...)
	}
	return ids
}

func (s *fileState) pointIDsExcept(generation string) []string {
	if s == nil {
		return nil
	}
	var ids []string
	for name, g := range s.generations {
		if name != generation {
			ids = append(ids, g.pointIDs...)
		}
	}
	return ids
}

func folderPointID(dir string) string {
	h := sha1.Sum([]byte("folder:" + dir))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
