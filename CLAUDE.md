# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Self-hosted semantic memory + Todoist integration. Written in Go as a single static binary.

Stack:
- **Qdrant** — vector database for storing embeddings (shared `infra-qdrant` in production)
- **text-embeddings-inference (TEI)** — local embedding model server
- **Traefik v3** — reverse proxy with Let's Encrypt SSL
- **mcp-go + Chi** — Go HTTP server and MCP implementation

## Architecture

Two Docker services in this repo: `memory-embeddings` (TEI), `memory-mcp` (Go server). Qdrant is provided by the infra stack (`infra-qdrant`) and reached on the `infra` Docker network. TEI and Qdrant are internal — not exposed outside Docker networks.

```
Client → Traefik (mcp.<domain>) → memory-mcp:8000 (Go)
           ├─ /memory   → memory MCP (X-API-Key)
           │             (tools include RAG when ENABLE_RAG=true)
           ├─ /todoist  → todoist MCP (X-API-Key, ENABLE_TODOIST)
           ├─ /viz/     → viz dashboard (Authentik ForwardAuth, ENABLE_VIZ)
           └─ /health   → liveness (no auth)
```

Single Go process serves all routes on one port via Chi router. MCP endpoints are protected by an `X-API-Key` middleware in application code. `/viz` is protected at the Traefik layer with Authentik ForwardAuth so browsers get a proper OIDC login flow.

Todoist, viz, and RAG are toggled by `ENABLE_TODOIST` / `ENABLE_VIZ` / `ENABLE_RAG` env vars. Backup runs as a goroutine.

### Qdrant collections

| Collection | Purpose |
|---|---|
| `memory` | facts written via `store_fact` (default memory layer) |
| `doc_chunks` | markdown chunks from `RAG_DOCUMENTS_DIR` (when `ENABLE_RAG=true`) |
| `doc_folders` | folder summaries for hierarchical search (when `ENABLE_RAG=true`) |

Collection name is now a `qdrant.Client` field, not a constant — one client per collection.

## Project Layout

```
cmd/
  server/main.go           — entrypoint, Chi router, graceful shutdown
  indexer/main.go          — standalone RAG indexer binary (cron-friendly)
internal/
  config/                  — env vars → struct
  middleware/auth.go       — X-API-Key + Bearer auth
  qdrant/client.go         — Qdrant REST client (upsert, search, scroll, delete, snapshots, field index)
  embeddings/client.go     — TEI REST client (Embed + EmbedBatch, batch size 32)
  memory/
    server.go              — 11 memory MCP tools
    cache.go               — in-memory cache with TTL + invalidation
  rag/
    chunker.go             — markdown-aware chunking (heading → paragraph → sentence)
    summarizer.go          — folder summaries (filenames + first H1/H2/H3)
    indexer.go             — walk + incremental upsert, stale cleanup, batched embeds
    server.go              — MCP tools: search_documents, reindex_documents
  todoist/
    client.go              — Todoist REST API v1 client
    server.go              — 7 MCP tools
  viz/
    handler.go             — Chi subrouter: /, /api/facts, /api/graph, /api/duplicates
    similarity.go          — cosine similarity
    static/index.html      — embedded via go:embed
  backup/loop.go           — snapshot + prune goroutine
```

## MCP Tools

### Writing
- `store_fact(fact, tags?, namespace?, permanent?, valid_until?)` — embed and save a fact; deduplicates (cosine ≥ 0.97); warns on contradictions (0.60–0.97)
- `update_fact(old_query, new_fact, ...)` — find by similarity, replace, preserve metadata
- `delete_fact(query, namespace?)` — find by similarity and delete
- `forget_old(days?, namespace?, dry_run?)` — delete old facts; skips `permanent=true`; defaults to dry run
- `import_facts(facts)` — bulk import from JSON string

### Reading
- `recall_facts(query, tags?, namespace?, limit?)` — semantic search with scores; filters expired; async-increments `recall_count`
- `list_facts(tags?, namespace?)` — list all facts with metadata
- `find_related(query, namespace?, limit?)` — related but non-duplicate facts (score 0.60–0.97)
- `get_stats()` — counts, namespace/tag breakdown, most recalled
- `list_tags(namespace?)` — all tags with counts
- `export_facts(namespace?)` — export as JSON

### Todoist
- `get_projects`, `get_labels`, `get_tasks`, `create_task`, `update_task`, `complete_task`, `delete_task`

### RAG (when `ENABLE_RAG=true`, registered on the `/memory` MCP endpoint)
- `search_documents(query, limit?, mode?)` — hierarchical search by default: top folders first, then chunks inside those folders, with flat fallback. `mode="flat"` forces a single-collection vector search.
- `reindex_documents()` — launches incremental re-indexing in a background goroutine. Skips unchanged files (SHA256 hash). Mutex-guarded — only one reindex at a time. Stale-file cleanup is aborted if the walk was incomplete or would remove >50% of the index.

## Data Model (Qdrant payload)

```
text              string    — the fact
namespace         string    — logical group (default: "default")
tags              []string  — labels
permanent         bool      — never deleted by forget_old
valid_until       string    — ISO date; expired facts excluded from search
created_at        string    — ISO datetime
updated_at        string    — ISO datetime (set on update)
recall_count      int       — times returned by recall_facts
last_recalled_at  string    — ISO datetime
user              string    — from MEMORY_USER env var
```

### Point IDs

- **New points**: deterministic UUID-v5-like hex (SHA1 of text, formatted `8-4-4-4-12`)
- **Legacy points** (from old Python implementation): numeric integer IDs

The Qdrant client unmarshals `id` into `interface{}` and converts to string with `parsePointID`. Don't assume IDs are always strings — Qdrant returns whatever was stored.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `API_KEY` | — | Shared secret for `X-API-Key` header (generate with `openssl rand -hex 32`). If empty, auth is disabled. |
| `MEMORY_USER` | `claude` | Username stored in fact metadata |
| `MEMORY_DOMAIN` | required | Domain — MCP at `mcp.<domain>` (used by Traefik labels in deploy) |
| `QDRANT_URL` | `http://memory-qdrant:6333` | Qdrant endpoint. In production: `http://infra-qdrant:6333` |
| `EMBED_URL` | `http://memory-embeddings:80` | TEI endpoint |
| `ENABLE_TODOIST` | `false` | Enable Todoist MCP server |
| `ENABLE_VIZ` | `false` | Enable visualization dashboard |
| `TODOIST_TOKEN` | — | Todoist API token (only when `ENABLE_TODOIST=true`) |
| `CACHE_TTL` | `60` | Search cache TTL in seconds |
| `DEDUP_THRESHOLD` | `0.97` | Cosine similarity for dedup |
| `CONTRADICTION_LOW` | `0.60` | Lower bound for contradiction warning |
| `KEEP_SNAPSHOTS` | `7` | Snapshots to retain |
| `BACKUP_INTERVAL_HOURS` | `24` | Backup frequency in hours |
| `VIZ_SIMILARITY_THRESHOLD` | `0.65` | Cosine similarity threshold for graph edges |
| `MCP_PORT` | `8000` | HTTP port |
| `ENABLE_RAG` | `false` | Enable document RAG tools (`search_documents`, `reindex_documents`) |
| `RAG_DOCUMENTS_DIR` | `/root/documents/personal` | Root directory to index. Hidden dirs (`.git`, `.sync`) are skipped. |
| `RAG_CHUNK_MAX_BYTES` | `1500` | Max chunk size in bytes (heading → paragraph → sentence → hard split) |
| `RAG_FOLDER_TOP_K` | `3` | Top N folders to consider in hierarchical search |
| `RAG_FOLDER_THRESHOLD` | `0.50` | Min folder similarity score; below this, fall back to flat chunk search |
| `RAG_COLLECTION_CHUNKS` | `doc_chunks` | Qdrant collection for chunks |
| `RAG_COLLECTION_FOLDERS` | `doc_folders` | Qdrant collection for folder summaries |

Never hardcode credentials. Use `.env` file (excluded from git).

## Key Implementation Details

### memory/server.go
- `InitCollection` runs at startup — embeds "init" to get vector size, creates collection if missing
- `cache.Invalidate()` is called after any write operation (store, delete, update, import, forget_old)
- `recall_count` is updated via `qdrant.SetPayload` in a background goroutine — no re-embedding
- `forget_old` defaults to `dry_run=true` — safe by default
- New point IDs are SHA1-based hex UUIDs (deterministic by text); legacy numeric IDs are handled on read
- TEI and Qdrant accessed via Docker network (no auth needed)

### qdrant/client.go
- Collection name is a struct field — `NewClient(url, collection string)` — one client per collection
- Both `Search` and `Scroll` receive point IDs as `interface{}` and normalize via `parsePointID`
- Scroll uses `interface{}` for offset to handle both string and numeric next_page_offset
- `CreateFieldIndex(field, schema)` creates payload indexes (used by RAG for fast `file_path` / `folder_path` filtering)
- Supports snapshot create/list/delete for the backup loop

### rag/indexer.go
- Single `ScrollAll` at the start of `Run` snapshots every file's hash + expected chunk count; per-file hash checks are in-memory afterwards (no N+1 round-trips)
- Files are truly "unchanged" only when hash matches AND `actualCount == totalChunks` — a half-indexed file (partial upsert from a prior run) is detected and rebuilt
- Embeds are batched via `embeddings.Client.EmbedBatch` (TEI sub-batches of 32)
- Embed-then-delete ordering: old chunks are deleted only after all embeddings succeed
- Stale cleanup aborts if the walk had any errors OR if it would remove more than half the known files (guards against transient Resilio/FS glitches wiping the index)
- Walk skips hidden dirs (`.git`, `.sync`, `.trash`, …) except for the root

### rag/server.go
- `Server.EnsureCollections` / package-level `rag.EnsureCollections` — create collections + payload indexes; shared between server and standalone indexer binary
- `reindex_documents` is mutex-guarded (`sync.Mutex.TryLock`) and runs on the server-lifetime context so graceful shutdown cancels in-flight reindexing
- `search_documents` returns file paths relative to `RAG_DOCUMENTS_DIR` (no absolute server paths leak to clients)

### todoist/server.go
- Thin wrapper over Todoist REST API v1 (`https://api.todoist.com/api/v1`)
- `TODOIST_TOKEN` is read from env at startup — never passed by the client
- Stateless — no caching, no local storage

### viz/handler.go
- Chi subrouter with 4 routes: `/`, `/api/facts`, `/api/graph`, `/api/duplicates`
- `static/index.html` is embedded via `go:embed`
- Graph API computes pairwise cosine similarity in-process; caps at `max_edges` strongest edges
- No auth check here — protected at Traefik layer by Authentik ForwardAuth

### cmd/server/main.go
- Chi router with logger + recoverer middleware
- Public: `/health` (no auth)
- `chi.Group` applies `APIKeyAuth` middleware to `/memory` and `/todoist`
- `/viz` mounted outside the auth group so Traefik/Authentik handles auth instead
- `signal.NotifyContext` for graceful shutdown; backup goroutine respects context
- Single `StreamableHTTPServer` per MCP server (memory, todoist)

## Build & Deploy

### Local build
```bash
go build ./cmd/server
go test ./...
```

### Docker (multi-stage)
```bash
docker build -t personal-memory .
```
Builder stage: `golang:1.24-alpine`, CGO disabled, static binary.
Runtime stage: `alpine:3.21` + `ca-certificates` — final image ~32MB.

### CI/CD
`.github/workflows/docker.yml`: on push to `main`, runs `go test` then builds and pushes to `ghcr.io/dzarlax-ai/personal-memory:{latest,sha}`.

### Deploy
Production deploy configs live in `personal_ai_stack/deploy/memory/`. Deploy skill (`deploy-personal`) handles syncing configs and pulling the latest image on the VPS.

## Verification

After setup:
- `curl https://mcp.<domain>/health` → `ok`
- `http://localhost:6333/dashboard` on the VPS shows the `memory` collection
- Legacy data from the Python implementation is read transparently (numeric IDs handled)
