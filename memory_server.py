import os
import time
import hashlib
import httpx
from datetime import datetime, timezone, timedelta
from dotenv import load_dotenv
from mcp.server.fastmcp import FastMCP

load_dotenv()

USER = os.environ["MEMORY_USER"]
PASS = os.getenv("MEMORY_PASS", "")
DOMAIN = os.environ["MEMORY_DOMAIN"]

COLLECTION = "memory"
EMBED_MODEL = os.getenv("EMBED_MODEL", "google/embeddinggemma-300m")
CACHE_TTL = int(os.getenv("CACHE_TTL", "60"))
DEDUP_THRESHOLD = float(os.getenv("DEDUP_THRESHOLD", "0.97"))
CONTRADICTION_LOW = float(os.getenv("CONTRADICTION_LOW", "0.60"))

QDRANT_URL = os.getenv("QDRANT_URL", f"https://qdrant.{DOMAIN}")
EMBED_URL = os.getenv("EMBED_URL", f"https://embed.{DOMAIN}")

def _auth_for(url: str) -> httpx.BasicAuth | None:
    """Use Basic Auth only for external HTTPS URLs (Traefik); skip for internal Docker networking."""
    return httpx.BasicAuth(USER, PASS) if url.startswith("https://") else None

qdrant = httpx.Client(
    base_url=QDRANT_URL,
    auth=_auth_for(QDRANT_URL),
    timeout=10.0,
)

embedder = httpx.Client(
    base_url=EMBED_URL,
    auth=_auth_for(EMBED_URL),
    timeout=15.0,
)

_cache: dict[str, tuple[float, list]] = {}


# ---------------------------------------------------------------------------
# Low-level helpers
# ---------------------------------------------------------------------------

def embed(text: str) -> list[float]:
    r = embedder.post("/embed", json={"inputs": text})
    r.raise_for_status()
    return r.json()[0]


def ensure_collection(vector_size: int):
    r = qdrant.get(f"/collections/{COLLECTION}")
    if r.status_code == 404:
        qdrant.put(f"/collections/{COLLECTION}", json={
            "vectors": {"size": vector_size, "distance": "Cosine"}
        }).raise_for_status()


def qdrant_upsert(point_id: int, vector: list[float], payload: dict):
    qdrant.put(f"/collections/{COLLECTION}/points", json={
        "points": [{"id": point_id, "vector": vector, "payload": payload}]
    }).raise_for_status()


def qdrant_set_payload(point_id: int, payload: dict):
    qdrant.post(f"/collections/{COLLECTION}/points/payload", json={
        "payload": payload,
        "points": [point_id],
    }).raise_for_status()


def qdrant_delete(point_ids: list[int]):
    qdrant.post(f"/collections/{COLLECTION}/points/delete", json={
        "points": point_ids
    }).raise_for_status()


def qdrant_search(
    vector: list[float],
    limit: int = 5,
    tags: list[str] | None = None,
    namespace: str | None = None,
    score_threshold: float = 0.0,
) -> list[dict]:
    must = []
    if tags:
        must.append({"key": "tags", "match": {"any": tags}})
    if namespace:
        must.append({"key": "namespace", "match": {"value": namespace}})

    body: dict = {
        "vector": vector,
        "limit": limit,
        "with_payload": True,
        "with_vector": False,
        "score_threshold": score_threshold,
    }
    if must:
        body["filter"] = {"must": must}

    r = qdrant.post(f"/collections/{COLLECTION}/points/search", json=body)
    r.raise_for_status()
    return r.json()["result"]


def qdrant_scroll(limit: int = 100, offset: int | None = None, must: list | None = None) -> tuple[list[dict], int | None]:
    body: dict = {"limit": limit, "with_payload": True, "with_vector": False}
    if offset is not None:
        body["offset"] = offset
    if must:
        body["filter"] = {"must": must}
    r = qdrant.post(f"/collections/{COLLECTION}/points/scroll", json=body)
    r.raise_for_status()
    data = r.json()["result"]
    return data["points"], data.get("next_page_offset")


def scroll_all(must: list | None = None) -> list[dict]:
    all_points: list[dict] = []
    offset = None
    while True:
        points, next_offset = qdrant_scroll(limit=100, offset=offset, must=must)
        all_points.extend(points)
        if next_offset is None:
            break
        offset = next_offset
    return all_points


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _invalidate_cache():
    _cache.clear()


# ---------------------------------------------------------------------------
# MCP server
# ---------------------------------------------------------------------------

mcp = FastMCP("Personal-Memory")


def _init_collection():
    try:
        vec = embed("init")
        ensure_collection(len(vec))
    except Exception:
        pass


# ---------------------------------------------------------------------------
# Tools
# ---------------------------------------------------------------------------

@mcp.tool()
def store_fact(
    fact: str,
    tags: list[str] | None = None,
    namespace: str = "default",
    permanent: bool = False,
    valid_until: str | None = None,
):
    """Store a new fact in long-term memory.

    Args:
        fact: The fact to remember.
        tags: Optional labels, e.g. ['work', 'project-x'].
        namespace: Logical group, e.g. 'work', 'personal'. Default: 'default'.
        permanent: If True, this fact is never deleted by forget_old().
        valid_until: ISO date string (e.g. '2026-12-31') after which the fact is considered expired.
    """
    try:
        vec = embed(fact)
        ensure_collection(len(vec))

        # Deduplication
        hits = qdrant_search(vec, limit=3, namespace=namespace)
        if hits and hits[0]["score"] >= DEDUP_THRESHOLD:
            existing = hits[0]["payload"]["text"]
            return f"Near-duplicate skipped (score={hits[0]['score']:.3f}). Existing: {existing}"

        # Contradiction warning: similar topic but meaningfully different
        contradictions = [
            h for h in hits
            if CONTRADICTION_LOW <= h["score"] < DEDUP_THRESHOLD
        ]
        warning = ""
        if contradictions:
            related = "; ".join(f"\"{h['payload']['text']}\" (score={h['score']:.3f})" for h in contradictions)
            warning = f"\nWarning: possibly related/contradicting facts found: {related}"

        point_id = int(hashlib.md5(fact.encode()).hexdigest()[:8], 16)
        payload = {
            "text": fact,
            "user": USER,
            "tags": tags or [],
            "namespace": namespace,
            "permanent": permanent,
            "valid_until": valid_until,
            "created_at": now_iso(),
            "recall_count": 0,
            "last_recalled_at": None,
        }
        qdrant_upsert(point_id, vec, payload)
        _invalidate_cache()
        return f"Stored: {fact}{warning}"
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def recall_facts(
    query: str,
    tags: list[str] | None = None,
    namespace: str | None = None,
    limit: int = 5,
):
    """Search for related facts in memory. Returns facts with relevance scores.

    Args:
        query: Natural language search query.
        tags: Filter by tags.
        namespace: Filter by namespace.
        limit: Max number of results.
    """
    try:
        now = time.monotonic()
        cache_key = f"{query}|{sorted(tags or [])}|{namespace}|{limit}"
        if cache_key in _cache and now - _cache[cache_key][0] < CACHE_TTL:
            hits = _cache[cache_key][1]
        else:
            vec = embed(query)
            results = qdrant_search(vec, limit=limit, tags=tags or None, namespace=namespace)

            # Filter expired facts
            today = datetime.now(timezone.utc).date().isoformat()
            results = [
                r for r in results
                if not r["payload"].get("valid_until") or r["payload"]["valid_until"] >= today
            ]

            hits = [
                {
                    "id": r["id"],
                    "text": r["payload"]["text"],
                    "score": round(r["score"], 3),
                    "tags": r["payload"].get("tags", []),
                    "namespace": r["payload"].get("namespace", "default"),
                    "recall_count": r["payload"].get("recall_count", 0),
                }
                for r in results
            ]
            _cache[cache_key] = (now, hits)

            # Increment recall_count asynchronously (best-effort)
            ts = now_iso()
            for h in hits:
                try:
                    qdrant_set_payload(h["id"], {
                        "recall_count": h["recall_count"] + 1,
                        "last_recalled_at": ts,
                    })
                except Exception:
                    pass

        if not hits:
            return "Nothing found."

        lines = []
        for h in hits:
            meta = f"[{h['score']}]"
            if h["tags"]:
                meta += f" {h['tags']}"
            if h["namespace"] != "default":
                meta += f" ns:{h['namespace']}"
            lines.append(f"- {meta} {h['text']}")
        return "\n".join(lines)
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def list_facts(tags: list[str] | None = None, namespace: str | None = None):
    """List all facts in memory. Optionally filter by tags or namespace."""
    try:
        must = []
        if namespace:
            must.append({"key": "namespace", "match": {"value": namespace}})

        points = scroll_all(must=must or None)

        if tags:
            tag_set = set(tags)
            points = [p for p in points if tag_set & set(p["payload"].get("tags", []))]

        if not points:
            return "Memory is empty."

        # Filter expired
        today = datetime.now(timezone.utc).date().isoformat()
        active = [p for p in points if not p["payload"].get("valid_until") or p["payload"]["valid_until"] >= today]
        expired = [p for p in points if p["payload"].get("valid_until") and p["payload"]["valid_until"] < today]

        lines = []
        for p in active:
            pl = p["payload"]
            tag_str = f" {pl['tags']}" if pl.get("tags") else ""
            ns_str = f" ns:{pl['namespace']}" if pl.get("namespace", "default") != "default" else ""
            perm_str = " [permanent]" if pl.get("permanent") else ""
            created = pl.get("created_at", "")[:10]
            recalled = pl.get("recall_count", 0)
            lines.append(f"- [{created}{ns_str}{tag_str}{perm_str} ×{recalled}] {pl['text']}")

        if expired:
            lines.append(f"\n[{len(expired)} expired facts not shown]")

        return "\n".join(lines)
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def delete_fact(query: str, namespace: str | None = None):
    """Find and delete the most semantically similar fact to the query."""
    try:
        vec = embed(query)
        hits = qdrant_search(vec, limit=1, namespace=namespace)
        if not hits:
            return "Nothing found to delete."
        top = hits[0]
        qdrant_delete([top["id"]])
        _invalidate_cache()
        return f"Deleted (score={top['score']:.3f}): {top['payload']['text']}"
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def update_fact(
    old_query: str,
    new_fact: str,
    tags: list[str] | None = None,
    namespace: str | None = None,
    permanent: bool | None = None,
):
    """Find the most similar fact to old_query and replace it with new_fact.
    Preserves original tags, namespace, permanent flag and created_at unless overridden.
    """
    try:
        vec_old = embed(old_query)
        hits = qdrant_search(vec_old, limit=1, namespace=namespace)
        if not hits:
            return "Nothing found to update."
        top = hits[0]
        old_payload = top["payload"]
        old_text = old_payload["text"]

        qdrant_delete([top["id"]])

        vec_new = embed(new_fact)
        point_id = int(hashlib.md5(new_fact.encode()).hexdigest()[:8], 16)
        payload = {
            "text": new_fact,
            "user": USER,
            "tags": tags if tags is not None else old_payload.get("tags", []),
            "namespace": namespace if namespace is not None else old_payload.get("namespace", "default"),
            "permanent": permanent if permanent is not None else old_payload.get("permanent", False),
            "valid_until": old_payload.get("valid_until"),
            "created_at": old_payload.get("created_at", now_iso()),
            "updated_at": now_iso(),
            "recall_count": old_payload.get("recall_count", 0),
            "last_recalled_at": old_payload.get("last_recalled_at"),
        }
        qdrant_upsert(point_id, vec_new, payload)
        _invalidate_cache()
        return f"Updated:\n  Old: {old_text}\n  New: {new_fact}"
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def forget_old(days: int = 90, namespace: str | None = None, dry_run: bool = True):
    """Delete facts older than `days` days. Skips permanent facts.

    Args:
        days: Age threshold in days.
        namespace: Only forget within this namespace.
        dry_run: If True (default), only show what would be deleted without actually deleting.
    """
    try:
        must = []
        if namespace:
            must.append({"key": "namespace", "match": {"value": namespace}})
        points = scroll_all(must=must or None)

        cutoff = (datetime.now(timezone.utc) - timedelta(days=days)).isoformat()
        to_delete = [
            p for p in points
            if not p["payload"].get("permanent")
            and p["payload"].get("created_at", "9999") < cutoff
        ]

        if not to_delete:
            return f"Nothing to delete (older than {days} days)."

        lines = [f"{'[DRY RUN] ' if dry_run else ''}Would delete {len(to_delete)} fact(s):"]
        for p in to_delete:
            created = p["payload"].get("created_at", "")[:10]
            lines.append(f"  - [{created}] {p['payload']['text']}")

        if not dry_run:
            qdrant_delete([p["id"] for p in to_delete])
            _invalidate_cache()
            lines[0] = f"Deleted {len(to_delete)} fact(s):"

        return "\n".join(lines)
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def get_stats():
    """Show memory statistics: total facts, namespace breakdown, tag distribution, oldest/newest."""
    try:
        points = scroll_all()

        if not points:
            return "Memory is empty."

        today = datetime.now(timezone.utc).date().isoformat()
        total = len(points)
        permanent = sum(1 for p in points if p["payload"].get("permanent"))
        expired = sum(1 for p in points if p["payload"].get("valid_until") and p["payload"]["valid_until"] < today)

        # Namespace breakdown
        ns_counts: dict[str, int] = {}
        for p in points:
            ns = p["payload"].get("namespace", "default")
            ns_counts[ns] = ns_counts.get(ns, 0) + 1

        # Tag distribution
        tag_counts: dict[str, int] = {}
        for p in points:
            for t in p["payload"].get("tags", []):
                tag_counts[t] = tag_counts.get(t, 0) + 1

        # Dates
        dates = [p["payload"].get("created_at", "") for p in points if p["payload"].get("created_at")]
        oldest = min(dates)[:10] if dates else "n/a"
        newest = max(dates)[:10] if dates else "n/a"

        # Most recalled
        top_recalled = sorted(points, key=lambda p: p["payload"].get("recall_count", 0), reverse=True)[:3]

        lines = [
            f"Total facts: {total} ({permanent} permanent, {expired} expired)",
            f"Date range: {oldest} → {newest}",
            "",
            "Namespaces:",
        ]
        for ns, count in sorted(ns_counts.items(), key=lambda x: -x[1]):
            lines.append(f"  {ns}: {count}")

        if tag_counts:
            lines.append("\nTop tags:")
            for tag, count in sorted(tag_counts.items(), key=lambda x: -x[1])[:10]:
                lines.append(f"  #{tag}: {count}")

        if top_recalled:
            lines.append("\nMost recalled:")
            for p in top_recalled:
                rc = p["payload"].get("recall_count", 0)
                lines.append(f"  ×{rc} {p['payload']['text'][:60]}")

        return "\n".join(lines)
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def list_tags(namespace: str | None = None):
    """List all unique tags used in memory with their counts."""
    try:
        must = []
        if namespace:
            must.append({"key": "namespace", "match": {"value": namespace}})
        points = scroll_all(must=must or None)

        tag_counts: dict[str, int] = {}
        for p in points:
            for t in p["payload"].get("tags", []):
                tag_counts[t] = tag_counts.get(t, 0) + 1

        if not tag_counts:
            return "No tags found."

        lines = [f"#{tag} ({count})" for tag, count in sorted(tag_counts.items(), key=lambda x: -x[1])]
        return "\n".join(lines)
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def find_related(query: str, namespace: str | None = None, limit: int = 5):
    """Find facts semantically related to the query but not direct duplicates.
    Useful for exploring a topic and discovering connected memories.
    """
    try:
        vec = embed(query)
        # Search wider, then filter out near-duplicates
        results = qdrant_search(vec, limit=limit + 5, namespace=namespace, score_threshold=CONTRADICTION_LOW)
        results = [r for r in results if r["score"] < DEDUP_THRESHOLD][:limit]

        if not results:
            return "No related facts found."

        today = datetime.now(timezone.utc).date().isoformat()
        results = [r for r in results if not r["payload"].get("valid_until") or r["payload"]["valid_until"] >= today]

        lines = []
        for r in results:
            tags = r["payload"].get("tags", [])
            tag_str = f" {tags}" if tags else ""
            lines.append(f"- [{r['score']:.3f}]{tag_str} {r['payload']['text']}")
        return "\n".join(lines) if lines else "No related facts found."
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def export_facts(namespace: str | None = None) -> str:
    """Export all facts as JSON. Useful for backup or migration."""
    import json
    try:
        must = []
        if namespace:
            must.append({"key": "namespace", "match": {"value": namespace}})
        points = scroll_all(must=must or None)
        data = [p["payload"] for p in points]
        return json.dumps(data, ensure_ascii=False, indent=2)
    except Exception as e:
        return f"Error: {str(e)}"


@mcp.tool()
def import_facts(facts: list[dict]) -> str:
    """Import facts from a list of dicts (as produced by export_facts).
    Each dict must have at least a 'text' field.
    """
    try:
        vec_sample = embed("init")
        ensure_collection(len(vec_sample))

        stored = 0
        skipped = 0
        for item in facts:
            text = item.get("text")
            if not text:
                skipped += 1
                continue
            vec = embed(text)
            hits = qdrant_search(vec, limit=1)
            if hits and hits[0]["score"] >= DEDUP_THRESHOLD:
                skipped += 1
                continue
            point_id = int(hashlib.md5(text.encode()).hexdigest()[:8], 16)
            payload = {
                "text": text,
                "user": item.get("user", USER),
                "tags": item.get("tags", []),
                "namespace": item.get("namespace", "default"),
                "permanent": item.get("permanent", False),
                "valid_until": item.get("valid_until"),
                "created_at": item.get("created_at", now_iso()),
                "recall_count": item.get("recall_count", 0),
                "last_recalled_at": item.get("last_recalled_at"),
            }
            qdrant_upsert(point_id, vec, payload)
            stored += 1

        _invalidate_cache()
        return f"Imported: {stored}, skipped (duplicates/invalid): {skipped}"
    except Exception as e:
        return f"Error: {str(e)}"


# ---------------------------------------------------------------------------

if __name__ == "__main__":
    _init_collection()
    transport = os.getenv("MCP_TRANSPORT", "stdio")
    if transport == "http":
        port = int(os.getenv("MCP_PORT", "8000"))
        mcp.run(transport="streamable-http", host="0.0.0.0", port=port)
    else:
        mcp.run()
