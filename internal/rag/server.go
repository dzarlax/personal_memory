package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/config"
	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/Dzarlax-AI/personal-memory/internal/rerank"
	"github.com/Dzarlax-AI/personal-memory/internal/retrieval"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// autoReindexInitialDelay is how long we wait after StartAutoReindex before
// the first scheduled run. Gives the server a moment to finish booting
// (the first reindex can hit many files at once).
const autoReindexInitialDelay = 10 * time.Second

const maxSearchDocumentsLimit = 100

const (
	blendedRRFConstant        = 60
	blendedCandidateFactor    = 2
	blendedFilteredSource     = "folder_filtered"
	blendedFlatSource         = "flat"
	blendedRoutingStrategy    = "blended_rrf"
	reasonFolderFilteredMatch = "folder_filtered_match"
	reasonFlatMatch           = "flat_match"
	reasonFlatRescue          = "flat_rescue"
)

type pointSearcher interface {
	Search(context.Context, []float32, int, map[string]interface{}, *float64) ([]qdrant.Point, error)
}

type queryEmbedder interface {
	Embed(context.Context, string) ([]float32, error)
}

// Server exposes RAG as MCP tools registered on the shared memory MCP server.
type Server struct {
	chunks         *qdrant.Client
	folders        *qdrant.Client
	embed          *embeddings.Client
	searchChunks   pointSearcher
	searchFolders  pointSearcher
	validateChunks generationScroller
	queryEmbed     queryEmbedder
	reranker       rerank.Reranker
	rerankerCap    int
	cfg            *config.Config
	indexer        *Indexer
	lifeCtx        context.Context // cancelled on graceful shutdown
	reindexMu      sync.Mutex      // held while a background reindex is running
	workWG         sync.WaitGroup  // all background loops and on-demand reindexes
}

// SetBlendedReranker injects an optional reranker for blended searches. It is
// deliberately not enabled by environment configuration: production keeps
// deterministic routing unless an experiment wires this explicitly.
func (s *Server) SetBlendedReranker(service rerank.Reranker, candidateCap int) error {
	if service == nil {
		s.reranker, s.rerankerCap = nil, 0
		return nil
	}
	if candidateCap < 1 || candidateCap > rerank.MaxCandidates {
		return fmt.Errorf("reranker candidate cap must be between 1 and %d", rerank.MaxCandidates)
	}
	s.reranker, s.rerankerCap = service, candidateCap
	return nil
}

// NewServer builds the RAG MCP server. lifeCtx should be the long-lived
// server context so background reindex goroutines can be cancelled on shutdown.
func NewServer(lifeCtx context.Context, chunks, folders *qdrant.Client, embed *embeddings.Client, cfg *config.Config) *Server {
	idx := NewIndexer(chunks, folders, embed, cfg.RAGDocumentsDir, cfg.RAGChunkMaxBytes)
	return &Server{
		chunks:         chunks,
		folders:        folders,
		embed:          embed,
		searchChunks:   chunks,
		searchFolders:  folders,
		validateChunks: chunks,
		queryEmbed:     embed,
		cfg:            cfg,
		indexer:        idx,
		lifeCtx:        lifeCtx,
	}
}

// EnsureIndexes creates the payload indexes used by RAG. Collection creation
// and vector-space validation are owned by embeddingidentity before this runs.
func EnsureIndexes(ctx context.Context, chunks, folders *qdrant.Client) error {
	// Payload indexes for fast filtering.
	for _, field := range []string{"file_path", "folder_path"} {
		if err := chunks.CreateFieldIndex(ctx, field, "keyword"); err != nil {
			return fmt.Errorf("create chunk index %s: %w", field, err)
		}
	}
	if err := folders.CreateFieldIndex(ctx, "folder_path", "keyword"); err != nil {
		return fmt.Errorf("create folder index: %w", err)
	}

	return nil
}

// EnsureIndexes is the method form, delegating to the package helper.
func (s *Server) EnsureIndexes(ctx context.Context) error {
	return EnsureIndexes(ctx, s.chunks, s.folders)
}

func (s *Server) RegisterTools(mcpSrv *server.MCPServer) {
	mcpSrv.AddTool(mcp.NewTool("search_documents",
		mcp.WithDescription("Search personal documents using semantic similarity. Hierarchical mode (default) finds relevant folders first and falls back to flat search. Flat mode searches all chunks directly."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
		mcp.WithNumber("limit", mcp.Description("Max results to return (default 5)")),
		mcp.WithString("mode", mcp.Description("Search mode: 'hierarchical' (default) or 'flat'")),
	), s.handleSearchDocuments)

	mcpSrv.AddTool(mcp.NewTool("reindex_documents",
		mcp.WithDescription("Trigger a re-index of the personal documents directory in the background. Skips unchanged files (hash check). Returns immediately; only one reindex may run at a time."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
	), s.handleReindexDocuments)
}

func (s *Server) handleSearchDocuments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	query, limit, mode, validationErr := parseSearchDocumentsArgs(args)
	if validationErr != "" {
		return mcp.NewToolResultError(validationErr), nil
	}

	vec, err := s.queryEmbed.Embed(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embed error: %v", err)), nil
	}

	var points []qdrant.Point
	if mode == "flat" {
		points, err = s.flatSearch(ctx, vec, limit)
	} else {
		points, err = s.hierarchicalSearch(ctx, vec, limit)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search error: %v", err)), nil
	}

	docsDir := s.cfg.RAGDocumentsDir
	validated, rejected := s.validateSearchCandidates(ctx, points)
	validated = validated[:min(len(validated), limit)]

	results := make([]map[string]interface{}, 0, len(validated))
	for _, p := range validated {
		fp, _ := p.Payload["file_path"].(string)
		result := map[string]interface{}{
			"score":       p.Score,
			"text":        p.Payload["text"],
			"file_path":   relPath(docsDir, fp),
			"heading":     p.Payload["heading"],
			"chunk_index": p.Payload["chunk_index"],
		}
		results = append(results, result)
	}

	var response interface{} = results
	if rejected.incomplete > 0 || rejected.legacyUnverified > 0 || rejected.outOfRoot > 0 || rejected.staleGeneration > 0 {
		response = map[string]interface{}{
			"results":    results,
			"incomplete": true,
			"rejected_candidates": map[string]int{
				"incomplete":        rejected.incomplete,
				"legacy_unverified": rejected.legacyUnverified,
				"out_of_root":       rejected.outOfRoot,
				"stale_generation":  rejected.staleGeneration,
			},
		}
	}
	b, _ := json.MarshalIndent(response, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

type rejectedCandidates struct {
	incomplete       int
	legacyUnverified int
	outOfRoot        int
	staleGeneration  int
}

type candidateGeneration struct {
	filePath   string
	generation string
	validated  validatedGeneration
}

// validateSearchCandidates discards the search response payload and replaces
// it with the separately-scrolled, sealed generation payload. Search is only
// a ranking hint; it is never a publication proof or a source for formatted
// document text.
func (s *Server) validateSearchCandidates(ctx context.Context, candidates []qdrant.Point) ([]qdrant.Point, rejectedCandidates) {
	var rejected rejectedCandidates
	discoveredByFile := make(map[string]sealedGenerationDiscovery)
	discoveryFailed := make(map[string]bool)
	fileOrder := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		filePath, ok := candidate.Payload["file_path"].(string)
		if !ok || filePath == "" || !s.pathWithinDocumentsRoot(filePath) {
			continue
		}
		if _, exists := discoveredByFile[filePath]; !exists {
			discoveredByFile[filePath] = sealedGenerationDiscovery{}
			fileOrder = append(fileOrder, filePath)
		}
	}

	for _, filePath := range fileOrder {
		validated, err := discoverSealedGenerations(ctx, s.validateChunks, filePath)
		if err != nil {
			discoveryFailed[filePath] = true
			continue
		}
		discoveredByFile[filePath] = validated
	}

	// A failed cleanup can leave several valid sealed generations, including a
	// newer one that did not make the bounded semantic candidate list. Discover
	// every sealed generation for each candidate file before choosing freshness.
	selectedGeneration := make(map[string]string)
	for _, filePath := range fileOrder {
		if discoveryFailed[filePath] {
			continue
		}
		for generation, validated := range discoveredByFile[filePath].generations {
			entry := &candidateGeneration{filePath: filePath, generation: generation, validated: validated}
			current, exists := selectedGeneration[filePath]
			if !exists || newerGeneration(entry, &candidateGeneration{filePath: filePath, generation: current, validated: discoveredByFile[filePath].generations[current]}) {
				selectedGeneration[filePath] = generation
			}
		}
	}

	incompleteFiles := make(map[string]bool)
	returnedIndexes := make(map[string]map[int]bool)
	result := make([]qdrant.Point, 0, len(candidates))
	for _, candidate := range candidates {
		filePath, ok := candidate.Payload["file_path"].(string)
		if !ok || filePath == "" {
			rejected.incomplete++
			continue
		}
		if !s.pathWithinDocumentsRoot(filePath) {
			rejected.outOfRoot++
			continue
		}
		generation, ok := candidate.Payload["generation"].(string)
		if !ok || generation == "" {
			rejected.legacyUnverified++
			continue
		}
		layout, ok := candidate.Payload["layout"].(string)
		if !ok || layout == "" {
			rejected.legacyUnverified++
			continue
		}
		if discoveryFailed[filePath] {
			if !incompleteFiles[filePath] {
				rejected.incomplete++
				incompleteFiles[filePath] = true
			}
			continue
		}
		if discoveredByFile[filePath].pending && !incompleteFiles[filePath] {
			rejected.incomplete++
			incompleteFiles[filePath] = true
		}
		selected, exists := selectedGeneration[filePath]
		if !exists {
			if !incompleteFiles[filePath] {
				rejected.incomplete++
				incompleteFiles[filePath] = true
			}
			continue
		}
		if generation != selected {
			rejected.staleGeneration++
			continue
		}
		validated, known := discoveredByFile[filePath].generations[selected]
		if !known {
			continue
		}
		index, ok := payloadInt(candidate.Payload["chunk_index"])
		if !ok || index < 0 || index >= len(validated.points) {
			rejected.incomplete++
			continue
		}
		if returnedIndexes[filePath] == nil {
			returnedIndexes[filePath] = make(map[int]bool)
		}
		if returnedIndexes[filePath][index] {
			continue
		}
		returnedIndexes[filePath][index] = true
		// Keep the query score and routing key, but replace every raw payload
		// field with the validation readback.
		candidate.Payload = validated.points[index].Payload
		result = append(result, candidate)
	}
	return result, rejected
}

func newerGeneration(candidate, current *candidateGeneration) bool {
	if candidate.validated.sealedAt.Equal(current.validated.sealedAt) {
		return candidate.generation > current.generation
	}
	return candidate.validated.sealedAt.After(current.validated.sealedAt)
}

func parseSearchDocumentsArgs(args map[string]any) (query string, limit int, mode, validationErr string) {
	query, _ = args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return "", 0, "", "query is required"
	}

	limit = 5
	if raw, exists := args["limit"]; exists {
		v, ok := raw.(float64)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > maxSearchDocumentsLimit || math.Trunc(v) != v {
			return "", 0, "", fmt.Sprintf("limit must be an integer between 1 and %d", maxSearchDocumentsLimit)
		}
		limit = int(v)
	}

	mode = "hierarchical"
	if raw, exists := args["mode"]; exists {
		m, ok := raw.(string)
		if !ok || (m != "hierarchical" && m != "flat") {
			return "", 0, "", "mode must be 'hierarchical' or 'flat'"
		}
		mode = m
	}
	return query, limit, mode, ""
}

func (s *Server) pathWithinDocumentsRoot(path string) bool {
	return s.cfg == nil || s.cfg.RAGDocumentsDir == "" || pathWithinRoot(s.cfg.RAGDocumentsDir, path)
}

// relPath returns a path only when it can be represented inside base. It never
// falls back to an absolute or parent-escaping path.
func relPath(base, path string) string {
	if base == "" || path == "" {
		return ""
	}
	r, err := filepath.Rel(base, path)
	if err != nil || filepath.IsAbs(r) || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return ""
	}
	return r
}

func (s *Server) hierarchicalSearch(ctx context.Context, vec []float32, limit int) ([]qdrant.Point, error) {
	threshold := s.cfg.RAGFolderThreshold
	folderPoints, err := s.searchFolders.Search(ctx, vec, s.cfg.RAGFolderTopK, nil, &threshold)
	if err != nil {
		return nil, err
	}

	if len(folderPoints) == 0 {
		return s.flatSearch(ctx, vec, limit)
	}

	var conds []map[string]interface{}
	for _, fp := range folderPoints {
		if p, ok := fp.Payload["folder_path"].(string); ok {
			conds = append(conds, map[string]interface{}{
				"key":   "folder_path",
				"match": map[string]interface{}{"value": p},
			})
		}
	}
	if len(conds) == 0 {
		return s.flatSearch(ctx, vec, limit)
	}

	filter := map[string]interface{}{"should": conds}
	points, err := s.searchChunks.Search(ctx, vec, limit, filter, nil)
	if err != nil {
		return nil, err
	}

	if len(points) == 0 {
		return s.flatSearch(ctx, vec, limit)
	}
	return points, nil
}

func (s *Server) flatSearch(ctx context.Context, vec []float32, limit int) ([]qdrant.Point, error) {
	return s.searchChunks.Search(ctx, vec, limit, nil, nil)
}

type routingMetadata struct {
	Strategy            string                 `json:"strategy"`
	Sources             []retrieval.SourceRank `json:"sources"`
	ReasonCodes         []string               `json:"reason_codes"`
	SelectedFolderPaths []string               `json:"selected_folder_paths"`
}

func (s *Server) blendedSearch(ctx context.Context, query string, vec []float32, limit int) ([]qdrant.Point, map[string]routingMetadata, error) {
	resultLimit := min(limit, retrieval.MaxResults)
	poolLimit := resultLimit
	if s.reranker != nil {
		poolLimit = min(max(resultLimit, s.rerankerCap), retrieval.MaxResults)
	}
	folderLimit := min(s.cfg.RAGFolderTopK, retrieval.MaxCandidates)
	threshold := s.cfg.RAGFolderThreshold
	folderPoints, err := s.searchFolders.Search(ctx, vec, folderLimit, nil, &threshold)
	if err != nil {
		return nil, nil, err
	}

	conditions := make([]map[string]interface{}, 0, len(folderPoints))
	selectedFolders := make([]string, 0, len(folderPoints))
	seenFolders := make(map[string]struct{}, len(folderPoints))
	for _, folder := range folderPoints {
		path, ok := folder.Payload["folder_path"].(string)
		if !ok || path == "" {
			continue
		}
		if _, exists := seenFolders[path]; exists {
			continue
		}
		seenFolders[path] = struct{}{}
		conditions = append(conditions, map[string]interface{}{
			"key": "folder_path", "match": map[string]interface{}{"value": path},
		})
		if relative := relPath(s.cfg.RAGDocumentsDir, path); relative != "" {
			selectedFolders = append(selectedFolders, relative)
		}
	}

	candidateLimit := min(poolLimit*blendedCandidateFactor, retrieval.MaxCandidates/2)
	var filtered []qdrant.Point
	if len(conditions) > 0 {
		filtered, err = s.searchChunks.Search(ctx, vec, candidateLimit, map[string]interface{}{"should": conditions}, nil)
		if err != nil {
			return nil, nil, err
		}
	}
	flat, err := s.searchChunks.Search(ctx, vec, candidateLimit, nil, nil)
	if err != nil {
		return nil, nil, err
	}

	lists := make([]retrieval.RankedList, 0, 2)
	pointsByID := make(map[string]qdrant.Point, len(filtered)+len(flat))
	if len(filtered) > 0 {
		lists = append(lists, retrieval.RankedList{Name: blendedFilteredSource, IDs: pointIDs(filtered)})
		for _, point := range filtered {
			if _, exists := pointsByID[point.ID]; !exists {
				pointsByID[point.ID] = point
			}
		}
	}
	if len(flat) > 0 {
		lists = append(lists, retrieval.RankedList{Name: blendedFlatSource, IDs: pointIDs(flat)})
		for _, point := range flat {
			if _, exists := pointsByID[point.ID]; !exists {
				pointsByID[point.ID] = point
			}
		}
	}
	if len(lists) == 0 {
		return nil, nil, nil
	}

	options := retrieval.MultiListOptions{RRFConstant: blendedRRFConstant, Limit: poolLimit}
	if len(flat) > 0 {
		options.FlatSource = blendedFlatSource
	}
	fused, _, err := retrieval.FuseRankedLists(lists, options)
	if err != nil {
		return nil, nil, fmt.Errorf("fuse blended search results: %w", err)
	}

	points := make([]qdrant.Point, len(fused))
	routing := make(map[string]routingMetadata, len(fused))
	for i, result := range fused {
		points[i] = pointsByID[result.ID]
		routing[result.ID] = routingForFusedResult(result, selectedFolders)
	}
	// Publication validation (and optional reranking) runs in the request
	// handler. This function intentionally returns a bounded raw ranking only.
	return points, routing, nil
}

func (s *Server) rerankValidated(ctx context.Context, query string, points []qdrant.Point, routing map[string]routingMetadata) ([]qdrant.Point, map[string]routingMetadata) {
	if s.reranker == nil || len(points) == 0 {
		return points, routing
	}
	candidateCount := min(len(points), s.rerankerCap)
	candidates := make([]rerank.Candidate, candidateCount)
	for i := 0; i < candidateCount; i++ {
		text, _ := points[i].Payload["text"].(string)
		candidates[i] = rerank.Candidate{ID: points[i].ID, Text: text}
	}
	reranked, reason := rerank.ApplyFailOpen(ctx, s.reranker, query, candidates)
	if reason == rerank.ReasonApplied {
		byID := make(map[string]qdrant.Point, candidateCount)
		for _, point := range points[:candidateCount] {
			byID[point.ID] = point
		}
		for i := 0; i < len(reranked) && i < candidateCount; i++ {
			if point, ok := byID[reranked[i].ID]; ok {
				points[i] = point
			}
		}
	}
	for _, point := range points[:candidateCount] {
		metadata := routing[point.ID]
		metadata.ReasonCodes = append(metadata.ReasonCodes, reason)
		routing[point.ID] = metadata
	}
	return points, routing
}

func pointIDs(points []qdrant.Point) []string {
	ids := make([]string, len(points))
	for i := range points {
		ids[i] = points[i].ID
	}
	return ids
}

func routingForFusedResult(result retrieval.FusedResult, selectedFolders []string) routingMetadata {
	reasons := make([]string, 0, 3)
	for _, source := range result.Sources {
		switch source.Source {
		case blendedFilteredSource:
			reasons = append(reasons, reasonFolderFilteredMatch)
		case blendedFlatSource:
			reasons = append(reasons, reasonFlatMatch)
		}
	}
	if result.FlatRescued {
		reasons = append(reasons, reasonFlatRescue)
	}
	return routingMetadata{
		Strategy:            blendedRoutingStrategy,
		Sources:             append([]retrieval.SourceRank(nil), result.Sources...),
		ReasonCodes:         reasons,
		SelectedFolderPaths: append(make([]string, 0, len(selectedFolders)), selectedFolders...),
	}
}

func (s *Server) handleReindexDocuments(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !s.reindexMu.TryLock() {
		return mcp.NewToolResultError("reindex already in progress"), nil
	}
	s.workWG.Add(1)
	go func() {
		defer s.workWG.Done()
		defer s.reindexMu.Unlock()
		if err := s.indexer.Run(s.lifeCtx); err != nil {
			slog.Error("background reindex failed", "error", err)
		}
	}()
	return mcp.NewToolResultText(fmt.Sprintf("Reindex started in background. Directory: %s", s.cfg.RAGDocumentsDir)), nil
}

// StartAutoReindex spawns a goroutine that runs the indexer on a fixed
// interval (cfg.RAGReindexInterval). A zero interval disables the loop —
// in that case the server is purely on-demand via the MCP tool or the
// standalone cmd/indexer binary. Shares the reindex mutex with the MCP
// handler so a manual trigger and a scheduled tick can't race.
func (s *Server) StartAutoReindex(ctx context.Context) {
	if s.cfg.RAGReindexInterval <= 0 {
		slog.Info("RAG auto-rescan disabled (set RAG_REINDEX_INTERVAL_MINUTES to enable)")
		return
	}
	s.workWG.Add(1)
	go func() {
		defer s.workWG.Done()
		s.autoReindexLoop(ctx)
	}()
}

// Wait blocks until every background RAG loop and in-flight reindex exits.
func (s *Server) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.workWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for RAG background work: %w", ctx.Err())
	}
}

func (s *Server) autoReindexLoop(ctx context.Context) {
	interval := s.cfg.RAGReindexInterval
	slog.Info("RAG auto-rescan started", "interval", interval)

	// Initial delay — don't slam the system during server boot.
	select {
	case <-ctx.Done():
		return
	case <-time.After(autoReindexInitialDelay):
	}
	s.runScheduledReindex(ctx)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("RAG auto-rescan stopping")
			return
		case <-tick.C:
			s.runScheduledReindex(ctx)
		}
	}
}

func (s *Server) runScheduledReindex(ctx context.Context) {
	if !s.reindexMu.TryLock() {
		slog.Info("scheduled reindex skipped — another run in progress")
		return
	}
	defer s.reindexMu.Unlock()
	if err := s.indexer.Run(ctx); err != nil {
		slog.Error("scheduled reindex failed", "error", err)
	}
}
