package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Dzarlax-AI/personal-memory/internal/config"
	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/Dzarlax-AI/personal-memory/internal/rag"
)

func main() {
	cfg := config.Load()

	if !cfg.EnableRAG {
		slog.Error("ENABLE_RAG is not set to true")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	qcChunks := qdrant.NewClient(cfg.QdrantURL, cfg.RAGCollectionChunks)
	qcFolders := qdrant.NewClient(cfg.QdrantURL, cfg.RAGCollectionFolders)
	ec := embeddings.NewClient(cfg.EmbedURL)

	if err := rag.EnsureCollections(ctx, qcChunks, qcFolders, ec); err != nil {
		slog.Error("failed to init RAG collections", "error", err)
		os.Exit(1)
	}

	indexer := rag.NewIndexer(qcChunks, qcFolders, ec, cfg.RAGDocumentsDir, cfg.RAGChunkMaxBytes)
	if err := indexer.Run(ctx); err != nil {
		slog.Error("indexer failed", "error", err)
		os.Exit(1)
	}
}
