---
title: Configuration
---

Use `.env` (not source control) for deployment values. Required values are enforced at startup unless a feature is disabled or its documented alternative applies.

## Server, auth, and dependencies

| Variable | Default | Requirement / purpose |
| --- | --- | --- |
| `MCP_PORT` | `8000` | HTTP port; integer 1–65535. |
| `MEMORY_DOMAIN` | none | Required by the Compose/Traefik deployment; also supplies OAuth resource defaults. |
| `API_KEY` | none | Required unless OAuth is enabled or isolated development enables `ALLOW_INSECURE_AUTH`; always required for Todoist. |
| `ALLOW_INSECURE_AUTH` | `false` | Isolated-development escape hatch for `/memory` without API-key/OAuth auth and for Viz without `VIZ_PROXY_SECRET`; it never removes Todoist's `API_KEY` requirement. Never enable in staging or production. |
| `QDRANT_URL` | `http://memory-qdrant:6333` | Qdrant base URL. |
| `EMBED_URL` | `http://memory-embeddings:80` | TEI base URL. |
| `EMBED_MODEL` | `intfloat/multilingual-e5-small` | Expected embedding model ID. |
| `EMBED_MODEL_REVISION` | `614241f622f53c4eeff9890bdc4f31cfecc418b3` | Required immutable model revision. |
| `EMBED_INPUT_PROFILE` | `legacy-raw-v1` | Server/indexer reject non-legacy profiles. |
| `ADOPT_EXISTING_EMBEDDING_IDENTITY` | `false` | One-shot adoption for verified legacy collections; never overrides a stored mismatch. |

## Memory and backups

| Variable | Default | Purpose |
| --- | --- | --- |
| `MEMORY_USER` | `claude` | Fact metadata user. |
| `CACHE_TTL` | `60` seconds | Recall-cache TTL; must be positive. |
| `DEDUP_THRESHOLD` | `0.97` | Duplicate cosine threshold. |
| `RELATED_FACT_LOW` | `0.60` | Minimum related-fact cosine threshold; less than `DEDUP_THRESHOLD`. |
| `CONTRADICTION_LOW` | none | Deprecated fallback for `RELATED_FACT_LOW`; the new variable wins when both are set. |
| `MUTATION_MATCH_THRESHOLD` | `0.90` | Similarity-based mutation threshold. |
| `BACKUP_INTERVAL_HOURS` | `24` hours | Snapshot interval; must be positive. |
| `KEEP_SNAPSHOTS` | `7` | Snapshots retained; at least one. |

## Optional features

| Variable | Default | Requirement / purpose |
| --- | --- | --- |
| `ENABLE_TODOIST` | `false` | Registers Todoist only when true; requires `TODOIST_TOKEN` and `API_KEY`. |
| `TODOIST_TOKEN` | none | Todoist API token, only when enabled. |
| `ENABLE_VIZ` | `false` | Registers visualization only when true. |
| `VIZ_PROXY_SECRET` | none | Required when Viz is enabled unless isolated development explicitly sets `ALLOW_INSECURE_AUTH=true`; in production, Traefik overwrites the trusted header after ForwardAuth. |
| `VIZ_SIMILARITY_THRESHOLD` | `0.65` | Visualization graph cosine threshold. |
| `ENABLE_RAG` | `false` | Enables document search and indexing. |
| `RAG_DOCUMENTS_DIR` | `/root/documents/personal` | Readable document root. |
| `RAG_CHUNK_MAX_BYTES` | `1500` | Maximum Markdown/text chunk size. |
| `RAG_FOLDER_TOP_K` | `3` | Folders considered by hierarchical search. |
| `RAG_FOLDER_THRESHOLD` | `0.50` | Minimum folder score before flat fallback. |
| `RAG_COLLECTION_CHUNKS` | `doc_chunks` | Chunk collection. |
| `RAG_COLLECTION_FOLDERS` | `doc_folders` | Folder-summary collection. |
| `RAG_REINDEX_INTERVAL_MINUTES` | `0` | Background re-index interval; zero disables it. |

## OAuth for `/memory`

| Variable | Default | Purpose |
| --- | --- | --- |
| `OAUTH_ENABLED` | `false` | Enables OAuth on `/memory`; additive to API-key clients. |
| `OAUTH_ISSUER` | none | Required when OAuth is enabled. |
| `OAUTH_ADDITIONAL_ISSUERS` | none | Optional comma-separated exact issuer URLs for separately reviewed OAuth clients. Empty entries, duplicates, and the primary issuer are rejected. Each additional issuer uses its own OIDC discovery and JWKS verification. |
| `OAUTH_RESOURCE` | `https://mcp.<MEMORY_DOMAIN>` | Protected resource URL. |
| `OAUTH_AUDIENCE` | `OAUTH_RESOURCE` | Expected token audience. |
| `OAUTH_SCOPES` | `memory:mcp` | Comma-separated required scopes. |
| `OAUTH_JWKS_URL` | discovered from issuer | Optional explicit JWKS URL. |
| `OAUTH_AUTHORIZATION_SERVERS` | primary and additional issuers | Optional comma-separated authorization servers. Configured issuers are always published in protected-resource metadata. |
| `OAUTH_RESOURCE_DOCUMENTATION` | none | Optional resource documentation URL. |
