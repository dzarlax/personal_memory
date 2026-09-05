---
title: What's new
description: Unreleased context-correctness improvements and upgrade notes.
---

## Unreleased: context correctness

This release hardens the boundary between stored context and what a client is
allowed to treat as current, complete, and safe to use.

### More explicit fact outcomes

- A deterministic fact ID is no longer silently overwritten by `store_fact` or
  `import_facts`.
- Ambiguous writes and saturated recall windows are reported explicitly instead
  of being presented as ordinary absence or success.
- Expiry uses one strict UTC calendar-date rule across runtime and evaluation.

### Safer document retrieval

- Document search returns only sealed, fully validated RAG generations; it does
  not mix old and partial replacements.
- A stale generation's score is never attached to newer document text.
- Stored candidates outside `RAG_DOCUMENTS_DIR` and malformed Qdrant point IDs
  are rejected before they reach a client.
- The public document-search modes are `hierarchical` and `flat`.

### Upgrade note for RAG operators

Existing unsealed RAG chunks are intentionally withheld as
`legacy_unverified`. After upgrading an RAG-enabled service, run a normal
reindex from the configured documents directory before relying on
`search_documents`. This is an explicit operator action: application startup
does not automatically reindex, and no fact-memory migration is involved.

See [MCP tools](../reference/tools/) for the response contract and the
[upgrade guide](../operations/upgrade-rollback/) for the normal deployment
boundary.
