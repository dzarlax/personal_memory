import os
import math
import httpx
from pathlib import Path
from starlette.applications import Starlette
from starlette.responses import HTMLResponse, JSONResponse
from starlette.routing import Route
from dotenv import load_dotenv

load_dotenv()

COLLECTION = "memory"
QDRANT_URL = os.environ.get("QDRANT_URL", "http://memory-qdrant:6333")

SIMILARITY_THRESHOLD = float(os.getenv("VIZ_SIMILARITY_THRESHOLD", "0.85"))
MAX_EDGES = int(os.getenv("VIZ_MAX_EDGES", "500"))

qdrant = httpx.Client(base_url=QDRANT_URL, timeout=30.0)


def scroll_all(with_vector: bool = False) -> list[dict]:
    all_points: list[dict] = []
    offset = None
    while True:
        body: dict = {"limit": 100, "with_payload": True, "with_vector": with_vector}
        if offset is not None:
            body["offset"] = offset
        r = qdrant.post(f"/collections/{COLLECTION}/points/scroll", json=body)
        r.raise_for_status()
        data = r.json()["result"]
        all_points.extend(data["points"])
        if data.get("next_page_offset") is None:
            break
        offset = data["next_page_offset"]
    return all_points


def cosine_similarity(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    norm_a = math.sqrt(sum(x * x for x in a))
    norm_b = math.sqrt(sum(x * x for x in b))
    if norm_a == 0 or norm_b == 0:
        return 0.0
    return dot / (norm_a * norm_b)


def _point_to_node(p: dict) -> dict:
    pl = p["payload"]
    return {
        "id": str(p["id"]),
        "text": pl.get("text", ""),
        "namespace": pl.get("namespace", "default"),
        "tags": pl.get("tags", []),
        "permanent": pl.get("permanent", False),
        "created_at": pl.get("created_at", ""),
        "recall_count": pl.get("recall_count", 0),
        "last_recalled_at": pl.get("last_recalled_at"),
    }


async def api_graph(request):
    """Return nodes + similarity edges for the graph view."""
    threshold = float(request.query_params.get("threshold", str(SIMILARITY_THRESHOLD)))
    points = scroll_all(with_vector=True)

    nodes = []
    vectors: dict[str, list[float]] = {}
    for p in points:
        pid = str(p["id"])
        nodes.append(_point_to_node(p))
        vectors[pid] = p.get("vector", [])

    # Pairwise cosine similarity → edges
    max_edges = int(request.query_params.get("max_edges", str(MAX_EDGES)))
    edges = []
    ids = list(vectors.keys())
    for i in range(len(ids)):
        for j in range(i + 1, len(ids)):
            sim = cosine_similarity(vectors[ids[i]], vectors[ids[j]])
            if sim >= threshold:
                edges.append({
                    "from": ids[i],
                    "to": ids[j],
                    "similarity": round(sim, 3),
                })

    # Keep only the strongest edges to avoid overwhelming the graph
    if len(edges) > max_edges:
        edges.sort(key=lambda e: e["similarity"], reverse=True)
        edges = edges[:max_edges]

    return JSONResponse({"nodes": nodes, "edges": edges})


async def api_facts(request):
    """Return all facts (no vectors) for the timeline view."""
    points = scroll_all(with_vector=False)
    nodes = [_point_to_node(p) for p in points]
    return JSONResponse({"nodes": nodes})


async def api_duplicates(request):
    """Return pairs of near-duplicate facts (similarity > threshold)."""
    threshold = float(request.query_params.get("threshold", "0.90"))
    points = scroll_all(with_vector=True)

    nodes = {}
    vectors: dict[str, list[float]] = {}
    for p in points:
        pid = str(p["id"])
        nodes[pid] = _point_to_node(p)
        vectors[pid] = p.get("vector", [])

    pairs = []
    ids = list(vectors.keys())
    for i in range(len(ids)):
        for j in range(i + 1, len(ids)):
            sim = cosine_similarity(vectors[ids[i]], vectors[ids[j]])
            if sim >= threshold:
                pairs.append({
                    "a": nodes[ids[i]],
                    "b": nodes[ids[j]],
                    "similarity": round(sim, 3),
                })

    pairs.sort(key=lambda p: p["similarity"], reverse=True)
    return JSONResponse({"pairs": pairs})


async def index(request):
    """Serve the main visualization page."""
    html_path = Path(__file__).parent / "static" / "index.html"
    return HTMLResponse(html_path.read_text(encoding="utf-8"))


app = Starlette(routes=[
    Route("/", index),
    Route("/api/facts", api_facts),
    Route("/api/graph", api_graph),
    Route("/api/duplicates", api_duplicates),
])