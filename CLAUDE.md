# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Self-hosted semantic memory + Todoist integration infrastructure using:
- **Qdrant** — vector database for storing embeddings
- **text-embeddings-inference (TEI)** — local embedding model server
- **Traefik v3** — reverse proxy with Let's Encrypt SSL
- **FastMCP** — Python MCP server bridging Claude with the stack

## Architecture

Three servers (`memory_server.py`, `todoist_server.py`, `viz_server.py`), all HTTP-only:

```
Claude Code / any HTTP MCP client
     │
     ├──▶ HTTPS → mcp.<domain>/memory/mcp  →  memory_server.py  (port 8000)
     ├──▶ HTTPS → mcp.<domain>/todoist/mcp →  todoist_server.py (port 8001)
     └──▶ HTTPS → mcp.<domain>/viz         →  viz_server.py     (port 8080)
```

Traefik uses `replacepathregex` middleware to rewrite `/memory(.*)` → `/mcp$1` and `/todoist(.*)` → `/mcp$1` before forwarding — FastMCP always serves at `/mcp`.

A visualization dashboard (`viz_server.py`) is available at `mcp.<domain>/viz` — interactive graph of fact relationships and a timeline view.

All services are behind Traefik with Authentik SSO (browser) + Basic Auth (programmatic). A sixth service, `memory-backup`, runs inside Docker and creates Qdrant snapshots on a schedule — no cron needed.

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
| `MEMORY_USER` | required | Basic Auth username |
| `MEMORY_PASS` | required | Basic Auth password |
| `MEMORY_DOMAIN` | required | Domain for embed/qdrant/mcp subdomains |
| `TODOIST_TOKEN` | required | Todoist API token (todoist_server.py only) |
| `CACHE_TTL` | `60` | Search cache TTL in seconds |
| `DEDUP_THRESHOLD` | `0.97` | Cosine similarity for dedup |
| `CONTRADICTION_LOW` | `0.60` | Lower bound for contradiction warning |
| `MCP_PORT` | `8000` / `8001` | HTTP port (`memory_server.py` uses 8000, `todoist_server.py` uses 8001) |
| `QDRANT_URL` | `https://qdrant.<DOMAIN>` | Override Qdrant URL (e.g. internal Docker: `http://memory-qdrant:6333`) |
| `EMBED_URL` | `https://embed.<DOMAIN>` | Override TEI URL (e.g. internal Docker: `http://memory-embeddings:80`) |
| `KEEP_SNAPSHOTS` | `7` | Snapshots to retain (backup service) |
| `BACKUP_INTERVAL_HOURS` | `24` | Backup frequency in hours (backup service) |
| `VIZ_PORT` | `8080` | HTTP port for viz_server.py |
| `VIZ_SIMILARITY_THRESHOLD` | `0.65` | Default cosine similarity threshold for graph edges |

Never hardcode credentials. Use `.env` file (excluded from git).

## Key Implementation Details

### memory_server.py
- `_init_collection()` runs at startup — collection is created once, not per-request
- `_invalidate_cache()` is called after any write operation (delete, update, forget_old)
- `recall_count` is updated via `qdrant_set_payload` — no re-embedding needed
- `forget_old` defaults to `dry_run=True` — safe by default
- Point IDs are UUID5 (deterministic, based on fact text) — collision-safe 128-bit space
- When `QDRANT_URL` / `EMBED_URL` start with `http://`, Basic Auth is skipped (internal Docker networking)

### todoist_server.py
- Thin wrapper over Todoist REST API v1 (`https://api.todoist.com/api/v1`)
- `TODOIST_TOKEN` is read from env at startup — never passed by the client
- Stateless — no caching, no local storage
- Filter queries use dedicated `/tasks/filter?query=` endpoint (not a param of `/tasks`)
- `/tasks` and `/projects` responses are paginated — results are in `results` key
- Labels are strings (label names), not IDs — pass `labels: ["name1", "name2"]`

### viz_server.py
- Standalone Starlette app (not FastMCP) — serves HTML + JSON API
- `GET /` — serves `static/index.html` (vis.js graph + timeline)
- `GET /api/graph?threshold=0.65` — scrolls all facts with vectors from Qdrant, computes pairwise cosine similarity, returns nodes + edges
- `GET /api/facts` — scrolls all facts without vectors (lightweight, for timeline)
- Queries Qdrant directly via `QDRANT_URL` — no dependency on memory_server.py
- Separate `requirements-viz.txt` (starlette, uvicorn, httpx, python-dotenv)
- Auth handled by Traefik (Authentik ForwardAuth) — no app-level auth needed
- Traefik strips `/viz` prefix before forwarding (`stripprefix` middleware)

### Common
- Servers use streamable-http transport on `MCP_PORT`
- Traefik rewrites `/memory(.*)` → `/mcp$1` and `/todoist(.*)` → `/mcp$1` — FastMCP always serves at `/mcp`
- Servers are stateless — in-memory cache resets on container restart
- `memory-backup` service connects to Qdrant via `http://memory-qdrant:6333` (internal network, no auth); snapshots land in `/qdrant/snapshots` → bind-mounted to `/root/memory/qdrant_snapshots` on the host
- Qdrant port `6333` is exposed on `127.0.0.1` only — for manual backup runs from the host (`backup.sh` with default `QDRANT_URL=http://localhost:6333`)

## Local Setup

```bash
python3.12 -m venv venv
venv/bin/pip install -r requirements.txt
cp .env.example .env  # fill in credentials
```

## Verification

After setup, `https://qdrant.<your-domain>/dashboard` should show a `memory` collection after the first `store_fact` call.
