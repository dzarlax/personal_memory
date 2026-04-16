# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Self-hosted semantic memory + Todoist integration infrastructure using:
- **Qdrant** — vector database for storing embeddings
- **text-embeddings-inference (TEI)** — local embedding model server
- **Traefik v3** — reverse proxy with Let's Encrypt SSL
- **FastMCP** — Python MCP server bridging Claude with the stack

## Architecture

Three Docker services: `memory-embeddings` (TEI), `memory-qdrant` (Qdrant), `memory-mcp` (all Python code). TEI and Qdrant are internal — not exposed outside Docker network.

```
Client → Traefik (mcp.<domain>) → app.py (port 8000)
           ├─ /memory/mcp  → FastMCP (memory)
           ├─ /todoist/mcp → FastMCP (todoist, ENABLE_TODOIST)
           └─ /viz/        → Starlette (viz, ENABLE_VIZ)
```

Single process (`app.py`) serves all routes on one port via Starlette + mounted FastMCP sub-apps. Traefik only needs one router — no path rewrites. Todoist and viz are toggled by `ENABLE_TODOIST` / `ENABLE_VIZ` env vars. Backup runs as a background thread.

## MCP Tools

### Writing
- `store_fact(fact, tags?, namespace?, permanent?, valid_until?)` — embed and save a fact; deduplicates (cosine ≥ 0.97); warns on contradictions (0.60–0.97)
- `update_fact(old_query, new_fact, ...)` — find by similarity, replace, preserve metadata
- `delete_fact(query, namespace?)` — find by similarity and delete
- `forget_old(days?, namespace?, dry_run?)` — delete old facts; skips `permanent=true`; defaults to dry run
- `import_facts(facts)` — bulk import from JSON

### Reading
- `recall_facts(query, tags?, namespace?, limit?)` — semantic search with scores; filters expired; increments `recall_count`
- `list_facts(tags?, namespace?)` — list all facts with metadata
- `find_related(query, namespace?, limit?)` — related but non-duplicate facts (score 0.60–0.97)
- `get_stats()` — counts, namespace/tag breakdown, most recalled
- `list_tags(namespace?)` — all tags with counts
- `export_facts(namespace?)` — export as JSON

## Data Model (Qdrant payload)

```
text          string    — the fact
namespace     string    — logical group (default: "default")
tags          string[]  — labels
permanent     bool      — never deleted by forget_old
valid_until   string    — ISO date; expired facts excluded from search
created_at    string    — ISO datetime
updated_at    string    — ISO datetime (set on update)
recall_count  int       — times returned by recall_facts
last_recalled_at string — ISO datetime
user          string    — from MEMORY_USER env var
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `MEMORY_USER` | `claude` | Username stored in fact metadata |
| `MEMORY_DOMAIN` | required | Domain — MCP at `mcp.<domain>` (used by Traefik labels) |
| `ENABLE_TODOIST` | `false` | Enable Todoist MCP server |
| `ENABLE_VIZ` | `false` | Enable visualization dashboard |
| `TODOIST_TOKEN` | — | Todoist API token (only when `ENABLE_TODOIST=true`) |
| `CACHE_TTL` | `60` | Search cache TTL in seconds |
| `DEDUP_THRESHOLD` | `0.97` | Cosine similarity for dedup |
| `CONTRADICTION_LOW` | `0.60` | Lower bound for contradiction warning |
| `KEEP_SNAPSHOTS` | `7` | Snapshots to retain |
| `BACKUP_INTERVAL_HOURS` | `24` | Backup frequency in hours |
| `VIZ_SIMILARITY_THRESHOLD` | `0.65` | Cosine similarity threshold for graph edges |

Never hardcode credentials. Use `.env` file (excluded from git).

## Key Implementation Details

### memory_server.py
- `_init_collection()` runs at startup — collection is created once, not per-request
- `_invalidate_cache()` is called after any write operation (delete, update, forget_old)
- `recall_count` is updated via `qdrant_set_payload` — no re-embedding needed
- `forget_old` defaults to `dry_run=True` — safe by default
- Point IDs are UUID5 (deterministic, based on fact text) — collision-safe 128-bit space
- TEI and Qdrant accessed via internal Docker network (no auth needed)
- Backup runs as a daemon thread (`_backup_loop`): snapshots Qdrant every `BACKUP_INTERVAL_HOURS`, prunes old snapshots

### todoist_server.py
- Thin wrapper over Todoist REST API v1 (`https://api.todoist.com/api/v1`)
- `TODOIST_TOKEN` is read from env at startup — never passed by the client
- Stateless — no caching, no local storage
- Filter queries use dedicated `/tasks/filter?query=` endpoint (not a param of `/tasks`)
- `/tasks` and `/projects` responses are paginated — results are in `results` key
- Labels are strings (label names), not IDs — pass `labels: ["name1", "name2"]`

### viz_server.py
- Starlette app — serves HTML + JSON API
- `GET /` — serves `static/index.html` (vis.js graph + timeline)
- `GET /api/graph?threshold=0.65` — pairwise cosine similarity, returns nodes + edges
- `GET /api/facts` — all facts without vectors (lightweight, for timeline)
- Queries Qdrant directly via `QDRANT_URL` — no dependency on memory_server.py

### app.py
- Unified entrypoint: mounts memory, todoist, viz as Starlette sub-apps on one port
- Calls `_init_collection()` and starts `_backup_loop` thread on startup
- Conditionally loads todoist/viz based on env vars

### Common
- All Python services run in one process/container (`ghcr.io/dzarlax-ai/personal-memory`), built via GitHub Actions
- Single Traefik router — no path rewrites needed, app handles routing internally
- Servers are stateless — in-memory cache resets on container restart
- Snapshots land in `/qdrant/snapshots` → bind-mounted to `/root/memory/qdrant_snapshots` on the host
- Qdrant port `6333` is exposed on `127.0.0.1` only

## Setup

```bash
cp .env.example .env  # fill in credentials, set ENABLE_TODOIST / ENABLE_VIZ
docker compose up -d
```

## Verification

After setup, `http://localhost:6333/dashboard` on the VPS should show a `memory` collection after the first `store_fact` call.
