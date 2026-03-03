# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Self-hosted semantic memory infrastructure using:
- **Qdrant** — vector database for storing embeddings
- **text-embeddings-inference (TEI)** — local embedding model server
- **Traefik v3** — reverse proxy with Let's Encrypt SSL
- **FastMCP** — Python MCP server bridging Claude with the stack

## Architecture

Two transport modes, same `memory_server.py`:

```
# Mode 1: stdio (local, default)
Claude Desktop / Claude Code
     │
     ▼ MCP (stdio)
memory_server.py  (FastMCP)
     │
     ├──▶ https://embed.<domain>   →  TEI (embeddings)
     └──▶ https://qdrant.<domain>  →  Qdrant (vector storage)

# Mode 2: HTTP (remote, MCP_TRANSPORT=http)
Claude Code / any HTTP MCP client
     │
     ▼ HTTPS → mcp.<domain>/mcp
memory_server.py  (FastMCP, streamable-http)
     │
     ├──▶ https://embed.<domain>   →  TEI (embeddings)
     └──▶ https://qdrant.<domain>  →  Qdrant (vector storage)
```

All three services are behind Traefik with Basic Auth + Let's Encrypt.

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
| `MEMORY_DOMAIN` | required | Domain for embed/qdrant subdomains |
| `CACHE_TTL` | `60` | Search cache TTL in seconds |
| `DEDUP_THRESHOLD` | `0.97` | Cosine similarity for dedup |
| `CONTRADICTION_LOW` | `0.60` | Lower bound for contradiction warning |
| `MCP_TRANSPORT` | `stdio` | Transport mode: `stdio` or `http` |
| `MCP_PORT` | `8000` | HTTP port (only used when `MCP_TRANSPORT=http`) |
| `QDRANT_URL` | `https://qdrant.<DOMAIN>` | Override Qdrant URL (e.g. internal Docker: `http://memory-qdrant:6333`) |
| `EMBED_URL` | `https://embed.<DOMAIN>` | Override TEI URL (e.g. internal Docker: `http://memory-embeddings:80`) |

Never hardcode credentials. Use `.env` file (excluded from git).

## Key Implementation Details

- `_init_collection()` runs at startup — collection is created once, not per-request
- `_invalidate_cache()` is called after any write operation (delete, update, forget_old)
- `recall_count` is updated via `qdrant_set_payload` — no re-embedding needed
- `forget_old` defaults to `dry_run=True` — safe by default
- Point IDs are MD5 hashes of the fact text (first 8 hex chars as int)
- Transport is selected via `MCP_TRANSPORT` env var; `stdio` is default, `http` enables streamable-http on `MCP_PORT`
- In HTTP mode the server is stateless — in-memory cache (`_cache`) resets on container restart
- When `QDRANT_URL` / `EMBED_URL` start with `http://`, Basic Auth is skipped (internal Docker networking, no Traefik in the path)

## Local Setup

```bash
python3.12 -m venv venv
venv/bin/pip install -r requirements.txt
cp .env.example .env  # fill in credentials
```

## Verification

After setup, `https://qdrant.<your-domain>/dashboard` should show a `memory` collection after the first `store_fact` call.
