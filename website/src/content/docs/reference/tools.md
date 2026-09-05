---
title: MCP tools
---

Memory writes: `store_fact`, `update_fact`, `set_fact_lifecycle`, `delete_fact`, `forget_old`, and `import_facts`.

Memory reads: `recall_facts`, `list_facts`, `find_related`, `get_stats`, `list_tags`, and `export_facts`.

When enabled, RAG adds `search_documents` and `reindex_documents`; Todoist adds `get_projects`, `get_labels`, `get_tasks`, `create_task`, `update_task`, `complete_task`, and `delete_task`.

When RAG is disabled, its two tools are not registered on `/memory`. When Todoist is disabled, the `/todoist` route is absent. When Viz is disabled, the `/viz` route is absent.

Default recall returns valid, non-expired current facts. Lifecycle history requires explicit inspection modes. See the [lifecycle contract](../../lifecycle/fact-lifecycle-contract/).

## Document search contract

`search_documents` accepts `mode="hierarchical"` (the default) or `mode="flat"`.
It returns only fully validated, sealed document generations. A response with
`incomplete: true` includes `rejected_candidates` counts: `legacy_unverified`
means an older index layout has not been reindexed; `out_of_root` means a stored
path is outside `RAG_DOCUMENTS_DIR`; and `stale_generation` means an older
generation matched semantically but was withheld rather than attaching its score
to newer content.

### RAG upgrade requirement

After upgrading an RAG-enabled service that already has document chunks, run a
normal reindex from the configured source directory before relying on document
search. Older unsealed chunks are deliberately not returned, rather than being
treated as published. Reindexing is separate from application startup and is
not automatic; it does not migrate fact-memory data.
