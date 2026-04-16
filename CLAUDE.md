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
           ├─ /todoist  → todoist MCP (X-API-Key, ENABLE_TODOIST)
           ├─ /viz/     → viz dashboard (Authentik ForwardAuth, ENABLE_VIZ)
           └─ /health   → liveness (no auth)
```

Single Go process serves all routes on one port via Chi router. MCP endpoints are protected by an `X-API-Key` middleware in application code. `/viz` is protected at the Traefik layer with Authentik ForwardAuth so browsers get a proper OIDC login flow.

Todoist and viz are toggled by `ENABLE_TODOIST` / `ENABLE_VIZ` env vars. Backup runs as a goroutine.

## Project Layout

```
cmd/server/main.go         — entrypoint, Chi router, graceful shutdown
internal/
  config/                  — env vars → struct
  middleware/auth.go       — X-API-Key middleware
  qdrant/client.go         — Qdrant REST client (upsert, search, scroll, delete, snapshots)
  embeddings/client.go     — TEI REST client
  memory/
    server.go              — 11 MCP tools
    cache.go               — in-memory cache with TTL + invalidation
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
- Both `Search` and `Scroll` receive point IDs as `interface{}` and normalize via `parsePointID`
- Scroll uses `interface{}` for offset to handle both string and numeric next_page_offset
- Supports snapshot create/list/delete for the backup loop

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
