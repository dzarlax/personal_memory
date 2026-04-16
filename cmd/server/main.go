package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Dzarlax-AI/personal-memory/internal/backup"
	"github.com/Dzarlax-AI/personal-memory/internal/config"
	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/memory"
	"github.com/Dzarlax-AI/personal-memory/internal/middleware"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/Dzarlax-AI/personal-memory/internal/todoist"
	"github.com/Dzarlax-AI/personal-memory/internal/viz"
	"github.com/go-chi/chi/v5"
	mcpmiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	cfg := config.Load()

	slog.Info("starting personal-memory server",
		"port", cfg.Port,
		"todoist", cfg.EnableTodoist,
		"viz", cfg.EnableViz,
	)

	// Init clients.
	qc := qdrant.NewClient(cfg.QdrantURL)
	ec := embeddings.NewClient(cfg.EmbedURL)

	// Init memory server.
	cache := memory.NewCache(cfg.CacheTTL)
	memSrv := memory.NewServer(qc, ec, cache, cfg.MemoryUser, cfg.DedupThreshold, cfg.ContradictionLow)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Init Qdrant collection.
	if err := memSrv.InitCollection(ctx); err != nil {
		slog.Error("failed to init collection", "error", err)
		os.Exit(1)
	}
	slog.Info("qdrant collection ready")

	// Create MCP server for memory.
	mcpMemory := server.NewMCPServer("personal-memory", "1.0.0",
		server.WithToolCapabilities(true),
	)
	memSrv.RegisterTools(mcpMemory)

	// Main router.
	r := chi.NewRouter()
	r.Use(mcpmiddleware.Logger)
	r.Use(mcpmiddleware.Recoverer)

	// Health check (no auth).
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// MCP endpoints — protected by X-API-Key.
	r.Group(func(r chi.Router) {
		r.Use(middleware.APIKeyAuth(cfg.APIKey))

		memoryHTTP := server.NewStreamableHTTPServer(mcpMemory)
		r.Handle("/memory", memoryHTTP)
		r.Handle("/memory/", memoryHTTP)

		// Todoist MCP (optional).
		if cfg.EnableTodoist && cfg.TodoistToken != "" {
			tc := todoist.NewClient(cfg.TodoistToken)
			todoistSrv := todoist.NewServer(tc)
			mcpTodoist := server.NewMCPServer("personal-todoist", "1.0.0",
				server.WithToolCapabilities(true),
			)
			todoistSrv.RegisterTools(mcpTodoist)

			todoistHTTP := server.NewStreamableHTTPServer(mcpTodoist)
			r.Handle("/todoist", todoistHTTP)
			r.Handle("/todoist/", todoistHTTP)

			slog.Info("todoist MCP enabled")
		}
	})

	// Viz dashboard (optional) — no API key, protected by Traefik + Authentik ForwardAuth.
	if cfg.EnableViz {
		vizHandler := viz.NewHandler(qc, cfg.VizSimilarityThreshold)
		r.Mount("/viz", vizHandler.Router())
		slog.Info("viz dashboard enabled")
	}

	// Start backup loop.
	bl := backup.NewLoop(qc, cfg.BackupInterval, cfg.KeepSnapshots)
	go bl.Run(ctx)

	// Start HTTP server.
	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{Addr: addr, Handler: r}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")
		srv.Close()
	}()

	slog.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
