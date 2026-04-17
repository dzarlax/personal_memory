package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/Dzarlax-AI/personal-memory/internal/config"
	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Server exposes RAG as MCP tools registered on the shared memory MCP server.
type Server struct {
	chunks    *qdrant.Client
	folders   *qdrant.Client
	embed     *embeddings.Client
	cfg       *config.Config
	indexer   *Indexer
	lifeCtx   context.Context // cancelled on graceful shutdown
	reindexMu sync.Mutex      // held while a background reindex is running
}

// NewServer builds the RAG MCP server. lifeCtx should be the long-lived
// server context so background reindex goroutines can be cancelled on shutdown.
func NewServer(lifeCtx context.Context, chunks, folders *qdrant.Client, embed *embeddings.Client, cfg *config.Config) *Server {
	idx := NewIndexer(chunks, folders, embed, cfg.RAGDocumentsDir, cfg.RAGChunkMaxBytes)
	return &Server{
		chunks:  chunks,
		folders: folders,
		embed:   embed,
		cfg:     cfg,
		indexer: idx,
		lifeCtx: lifeCtx,
	}
}

// EnsureCollections creates both Qdrant collections and their payload indexes.
// Safe to call on every boot — EnsureCollection/CreateFieldIndex are idempotent in Qdrant.
func EnsureCollections(ctx context.Context, chunks, folders *qdrant.Client, embed *embeddings.Client) error {
	vec, err := embed.Embed(ctx, "init")
	if err != nil {
		return fmt.Errorf("embed init: %w", err)
	}
	size := len(vec)

	if err := chunks.EnsureCollection(ctx, size); err != nil {
		return fmt.Errorf("ensure chunks collection: %w", err)
	}
	if err := folders.EnsureCollection(ctx, size); err != nil {
		return fmt.Errorf("ensure folders collection: %w", err)
	}

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

// EnsureCollections is the method form, delegating to the package helper.
func (s *Server) EnsureCollections(ctx context.Context) error {
	return EnsureCollections(ctx, s.chunks, s.folders, s.embed)
}

func (s *Server) RegisterTools(mcpSrv *server.MCPServer) {
	mcpSrv.AddTool(mcp.NewTool("search_documents",
		mcp.WithDescription("Search personal documents using semantic similarity. Uses hierarchical search: finds relevant folders first, then searches chunks within those folders. Falls back to flat search if no folder exceeds the threshold."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
		mcp.WithNumber("limit", mcp.Description("Max results to return (default 5)")),
		mcp.WithString("mode", mcp.Description("Search mode: 'hierarchical' (default) or 'flat'")),
	), s.handleSearchDocuments)

	mcpSrv.AddTool(mcp.NewTool("reindex_documents",
		mcp.WithDescription("Trigger a re-index of the personal documents directory in the background. Skips unchanged files (hash check). Returns immediately; only one reindex may run at a time."),
	), s.handleReindexDocuments)
}

func (s *Server) handleSearchDocuments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	query, _ := args["query"].(string)
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	limit := 5
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	mode := "hierarchical"
	if m, ok := args["mode"].(string); ok && m != "" {
		mode = m
	}

	vec, err := s.embed.Embed(ctx, query)
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
	results := make([]map[string]interface{}, 0, len(points))
	for _, p := range points {
		fp, _ := p.Payload["file_path"].(string)
		results = append(results, map[string]interface{}{
			"score":       p.Score,
			"text":        p.Payload["text"],
			"file_path":   relPath(docsDir, fp),
			"heading":     p.Payload["heading"],
			"chunk_index": p.Payload["chunk_index"],
		})
	}

	b, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// relPath returns path relative to base; falls back to the absolute path if
// they're unrelated (e.g. path on a different volume).
func relPath(base, path string) string {
	if base == "" || path == "" {
		return path
	}
	r, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return r
}

func (s *Server) hierarchicalSearch(ctx context.Context, vec []float32, limit int) ([]qdrant.Point, error) {
	threshold := s.cfg.RAGFolderThreshold
	folderPoints, err := s.folders.Search(ctx, vec, s.cfg.RAGFolderTopK, nil, &threshold)
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
	points, err := s.chunks.Search(ctx, vec, limit, filter, nil)
	if err != nil {
		return nil, err
	}

	if len(points) == 0 {
		return s.flatSearch(ctx, vec, limit)
	}
	return points, nil
}

func (s *Server) flatSearch(ctx context.Context, vec []float32, limit int) ([]qdrant.Point, error) {
	return s.chunks.Search(ctx, vec, limit, nil, nil)
}

func (s *Server) handleReindexDocuments(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !s.reindexMu.TryLock() {
		return mcp.NewToolResultError("reindex already in progress"), nil
	}
	go func() {
		defer s.reindexMu.Unlock()
		if err := s.indexer.Run(s.lifeCtx); err != nil {
			slog.Error("background reindex failed", "error", err)
		}
	}()
	return mcp.NewToolResultText(fmt.Sprintf("Reindex started in background. Directory: %s", s.cfg.RAGDocumentsDir)), nil
}
