# Personal Memory Stack

Self-hosted semantic memory + Todoist integration for any MCP-compatible AI client. Stores and retrieves facts using vector embeddings, and exposes Todoist as a server-side MCP tool — no third-party cloud auth needed, all credentials stay on your VPS.

Written in Go as a single static binary.

## Stack

| Component | Role |
|---|---|
| [Qdrant](https://qdrant.tech/) | Vector database |
| [Text Embeddings Inference](https://github.com/huggingface/text-embeddings-inference) | Local embedding model server |
| [intfloat/multilingual-e5-small](https://huggingface.co/intfloat/multilingual-e5-small) | Embedding model (multilingual, ~470MB) |
| [mcp-go](https://github.com/mark3labs/mcp-go) | MCP server implementation |
| [Chi](https://github.com/go-chi/chi) | HTTP router |
| [vis.js](https://visjs.org/) | Interactive graph and timeline visualization |
| Traefik v3 | Reverse proxy, SSL, Authentik ForwardAuth (OIDC) for viz |

## Architecture

Two Docker services in this repo: `memory-embeddings` (TEI), `memory-mcp` (Go server). Qdrant is provided by the infra stack (`infra-qdrant`) and reached on the `infra` Docker network. TEI and Qdrant are internal — not exposed outside Docker networks.

```mermaid
graph TD
    Client["Claude Code / HTTP MCP client"]
    Browser["Browser"]
    TodoistAPI["api.todoist.com"]

    subgraph VPS["VPS"]
        Traefik["Traefik SSL"]
        TEI["memory-embeddings TEI"]
        Qdrant["infra-qdrant"]

        subgraph MCP["memory-mcp container :8000"]
            App["main.go Chi router"]
            Auth["X-API-Key middleware"]
            Memory["/memory MCP"]
            Todoist["/todoist MCP optional"]
            Viz["/viz dashboard optional"]
            Backup["backup goroutine"]
            App --> Auth
            Auth --> Memory
            Auth --> Todoist
            App --> Viz
        end
    end

    Client -->|HTTPS + X-API-Key| Traefik
    Browser -->|HTTPS + OIDC| Traefik
    Traefik --> App
    Memory -->|POST /embed| TEI
    Memory -->|search / upsert| Qdrant
    Backup -->|snapshots| Qdrant
    Viz -->|scroll + vectors| Qdrant
    Todoist -->|REST API| TodoistAPI
```

### Auth

- **MCP endpoints** (`/memory`, `/todoist`, `/health`) — protected by `X-API-Key` header checked in application code
- **Viz dashboard** (`/viz`) — protected by Authentik ForwardAuth (OIDC) at Traefik layer, so browsers get a proper OIDC login flow

### Visualization (`mcp.<domain>/viz`)

- **Graph** — interactive force-directed network (vis.js). Nodes = facts, edges = cosine similarity above threshold.
- **Timeline** — facts plotted by creation date, grouped by namespace.

## Data Model

Each stored fact is a Qdrant point with the following payload:

```mermaid
classDiagram
    class Fact {
        +string text
        +string user
        +string namespace
        +List~string~ tags
        +bool permanent
        +string created_at
        +string updated_at
        +string valid_until
        +int recall_count
        +string last_recalled_at
    }
```

- **namespace** — logical group (`work`, `personal`, `projects`, …)
- **permanent** — if `true`, never deleted by `forget_old()`
- **valid_until** — ISO date; expired facts are excluded from search results
- **recall_count** — incremented each time the fact is returned by `recall_facts`

Point IDs: new points use deterministic UUID-v5-like hex IDs (SHA1 of text). Legacy points created by the old Python implementation use integer IDs — the Go client handles both transparently.

## MCP Tools

### memory — Writing

| Tool | Description |
|---|---|
| `store_fact(fact, tags?, namespace?, permanent?, valid_until?)` | Embed and save a fact. Skips near-duplicates (cosine ≥ 0.97). Warns about potentially contradicting facts (cosine 0.60–0.97). |
| `update_fact(old_query, new_fact, tags?, namespace?, permanent?)` | Semantically find a fact and replace it. Preserves metadata unless overridden. |
| `delete_fact(query, namespace?)` | Semantically find and delete the closest matching fact. |
| `forget_old(days?, namespace?, dry_run?)` | Delete facts older than N days. Skips `permanent=true`. Default: `dry_run=true`. |
| `import_facts(facts)` | Bulk import from a JSON array (e.g. from `export_facts`). Deduplicates on import. |

### memory — Reading

| Tool | Description |
|---|---|
| `recall_facts(query, namespace?, limit?)` | Semantic search. Returns facts with scores. Filters expired facts. Increments `recall_count`. |
| `list_facts(namespace?)` | List all facts with metadata. |
| `find_related(query, namespace?, limit?)` | Find semantically related facts that are not direct duplicates (score 0.60–0.97). |
| `get_stats()` | Total counts, namespace breakdown, tag distribution, most recalled facts. |
| `list_tags(namespace?)` | All unique tags with usage counts. |
| `export_facts(namespace?)` | Export all facts as JSON for backup or migration. |

### todoist

| Tool | Description |
|---|---|
| `get_projects()` | List all Todoist projects with IDs. |
| `get_labels()` | List all personal labels with IDs. |
| `get_tasks(project_id?, filter?, limit?)` | List active tasks. `filter` uses Todoist filter syntax (e.g. `today`, `overdue`, `#Work`, `@label`). |
| `create_task(content, project_id?, due_string?, priority?, labels?)` | Create a task. Priority 1–4. |
| `complete_task(task_id)` | Mark a task as complete. |
| `update_task(task_id, content?, due_string?, priority?, labels?)` | Update an existing task. |
| `delete_task(task_id)` | Delete a task permanently. |

## Prerequisites (VPS)

- Docker + Docker Compose
- Traefik v3 with:
  - External network named `traefik`
  - `letsEncrypt` certresolver configured
  - `authentik-auth` ForwardAuth middleware configured (only needed if `ENABLE_VIZ=true`)

## Server Setup (VPS)

```bash
mkdir -p /root/memory
cp .env.example .env
nano .env
docker compose up -d
```

### `.env` variables

| Variable | Description |
|---|---|
| `MEMORY_DOMAIN` | Your domain, e.g. `example.com` — MCP available at `mcp.<domain>` |
| `API_KEY` | Shared secret for `X-API-Key` header on MCP endpoints. Generate with `openssl rand -hex 32`. |
| `EMBED_MODEL` | HuggingFace model ID, default `intfloat/multilingual-e5-small` |
| `MEMORY_USER` | Username stored as metadata on facts |
| `ENABLE_TODOIST` | Set to `true` to enable Todoist MCP server (default: `false`) |
| `ENABLE_VIZ` | Set to `true` to enable visualization dashboard (default: `false`) |
| `TODOIST_TOKEN` | Todoist API token — get it at Settings → Integrations → Developer (only needed when `ENABLE_TODOIST=true`) |
| `KEEP_SNAPSHOTS` | Number of snapshots to retain (default: `7`) |
| `BACKUP_INTERVAL_HOURS` | How often the backup runs (default: `24`) |
| `VIZ_SIMILARITY_THRESHOLD` | Default similarity threshold for graph edges (default: `0.65`) |
| `DEDUP_THRESHOLD` | Cosine similarity above which a new fact is treated as a duplicate (default: `0.97`) |
| `CONTRADICTION_LOW` | Lower bound for contradiction warnings (default: `0.60`) |
| `CACHE_TTL` | In-memory cache TTL for `recall_facts`, in seconds (default: `60`) |

Track TEI model download on first start:
```bash
docker logs -f memory-embeddings
# Ready when you see: Ready
```

Verify Qdrant (on VPS):
```bash
curl http://localhost:6333/healthz
# → {"title":"qdrant - Ready"}
```

## Backups

Backup runs as a goroutine inside `memory-mcp` — no separate service or cron needed.

- Creates a Qdrant snapshot every `BACKUP_INTERVAL_HOURS` hours (default: 24)
- Snapshots are stored at `/root/memory/qdrant_snapshots/` on the host
- Keeps the last `KEEP_SNAPSHOTS` snapshots (default: 7), deletes older ones

Backup logs appear in `docker logs memory-mcp`.

Snapshots are stored locally on the VPS only. Point rsync, rclone, or Resilio Sync at `/root/memory/qdrant_snapshots/` — snapshots are self-contained `.snapshot` files, safe to copy at any time.

To restore from a snapshot:
```bash
curl -X POST "http://localhost:6333/collections/memory/snapshots/recover" \
  -H "Content-Type: application/json" \
  -d '{"location": "file:///qdrant/snapshots/memory/<snapshot-name>.snapshot"}'
```

## Client Setup

Two separate MCP servers:

| Field | Memory | Todoist |
|---|---|---|
| Type | Streamable HTTP | Streamable HTTP |
| URL | `https://mcp.yourdomain.com/memory` | `https://mcp.yourdomain.com/todoist` |
| Header key | `X-API-Key` | `X-API-Key` |
| Header value | `<your API_KEY>` | `<your API_KEY>` |

**Claude Code** — add both with one command each (add `--scope user` to make them available across all projects):
```bash
export API_KEY='<your API_KEY>'

claude mcp add --transport http personal-memory https://mcp.yourdomain.com/memory \
  --header "X-API-Key: $API_KEY" \
  --scope user

claude mcp add --transport http todoist https://mcp.yourdomain.com/todoist \
  --header "X-API-Key: $API_KEY" \
  --scope user
```

## Building

```bash
go build ./cmd/server
go test ./...
```

Or via Docker (multi-stage build → ~32MB image):
```bash
docker build -t personal-memory .
```

## Project Layout

```
cmd/server/          entrypoint
internal/
  config/            env vars → struct
  middleware/        X-API-Key auth
  qdrant/            Qdrant REST client
  embeddings/        TEI REST client
  memory/            memory MCP server (11 tools) + in-memory cache
  todoist/           todoist MCP server (7 tools) + REST client
  viz/               viz dashboard handler + cosine similarity + embedded index.html
  backup/            Qdrant snapshot loop
```

## Best Practices

To get the most out of persistent memory, instruct your AI client to use it proactively. For Claude Code, add the following to your global CLAUDE.md:

| OS | Path |
|---|---|
| macOS / Linux | `~/.claude/CLAUDE.md` |
| Windows | `%USERPROFILE%\.claude\CLAUDE.md` |

````markdown
## Personal Memory (MCP: personal-memory)

The `personal-memory` MCP server is always available. Use it proactively — don't wait to be asked.

### When to recall
- At the start of any session involving a known project — run `recall_facts` to load context
- Before making architectural decisions — check if relevant preferences or past decisions are stored
- When the user references established context ("as usual", "like before", "you know I prefer...")

### When to store
- User states a preference or decision that should persist ("always use X", "never do Y")
- A non-obvious fact about a project is established (tech stack, naming convention, key dependency)
- Something important was learned that would be useful in future sessions

### Namespace convention
Always specify a namespace. Never store everything in `default`.

| Context | Namespace |
|---|---|
| Personal preferences, habits | `personal` |
| Current project | `<project-name>` |
| Cross-project technical preferences | `tech` |
| Work / professional context | `work` |

### Permanent facts
Use `permanent=True` for facts that should never expire:
fundamental preferences, identity facts, long-term architectural decisions.

### Tags
- `#decision` — architectural or product decisions
- `#preference` — personal or workflow preferences
- `#constraint` — things to avoid or never do
````
