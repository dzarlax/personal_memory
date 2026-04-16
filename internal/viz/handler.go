package viz

import (
	"embed"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/go-chi/chi/v5"
)

//go:embed static/index.html
var staticFS embed.FS

type Handler struct {
	qdrant              *qdrant.Client
	defaultThreshold    float64
	defaultMaxEdges     int
}

func NewHandler(qc *qdrant.Client, defaultThreshold float64) *Handler {
	return &Handler{
		qdrant:           qc,
		defaultThreshold: defaultThreshold,
		defaultMaxEdges:  500,
	}
}

func (h *Handler) Router() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.serveIndex)
	r.Get("/api/facts", h.apiFacts)
	r.Get("/api/graph", h.apiGraph)
	r.Get("/api/duplicates", h.apiDuplicates)
	return r
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (h *Handler) apiFacts(w http.ResponseWriter, r *http.Request) {
	points, err := h.qdrant.ScrollAll(r.Context(), nil, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	nodes := make([]map[string]interface{}, 0, len(points))
	for _, p := range points {
		nodes = append(nodes, pointToNode(p))
	}

	writeJSON(w, map[string]interface{}{"nodes": nodes})
}

func (h *Handler) apiGraph(w http.ResponseWriter, r *http.Request) {
	threshold := h.defaultThreshold
	if v := r.URL.Query().Get("threshold"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			threshold = f
		}
	}
	maxEdges := h.defaultMaxEdges
	if v := r.URL.Query().Get("max_edges"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxEdges = n
		}
	}

	points, err := h.qdrant.ScrollAll(r.Context(), nil, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	nodes := make([]map[string]interface{}, 0, len(points))
	for _, p := range points {
		nodes = append(nodes, pointToNode(p))
	}

	type edge struct {
		From  string  `json:"from"`
		To    string  `json:"to"`
		Value float64 `json:"value"`
	}

	var edges []edge
	for i := 0; i < len(points); i++ {
		for j := i + 1; j < len(points); j++ {
			sim := cosineSimilarity(points[i].Vector, points[j].Vector)
			if sim >= threshold {
				edges = append(edges, edge{
					From:  points[i].ID,
					To:    points[j].ID,
					Value: sim,
				})
			}
		}
	}

	// Keep only strongest edges.
	if len(edges) > maxEdges {
		sort.Slice(edges, func(i, j int) bool {
			return edges[i].Value > edges[j].Value
		})
		edges = edges[:maxEdges]
	}

	writeJSON(w, map[string]interface{}{
		"nodes": nodes,
		"edges": edges,
	})
}

func (h *Handler) apiDuplicates(w http.ResponseWriter, r *http.Request) {
	threshold := 0.90
	if v := r.URL.Query().Get("threshold"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			threshold = f
		}
	}

	points, err := h.qdrant.ScrollAll(r.Context(), nil, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type dupPair struct {
		A     map[string]interface{} `json:"a"`
		B     map[string]interface{} `json:"b"`
		Score float64                `json:"score"`
	}

	var pairs []dupPair
	for i := 0; i < len(points); i++ {
		for j := i + 1; j < len(points); j++ {
			sim := cosineSimilarity(points[i].Vector, points[j].Vector)
			if sim >= threshold {
				pairs = append(pairs, dupPair{
					A:     pointToNode(points[i]),
					B:     pointToNode(points[j]),
					Score: sim,
				})
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].Score > pairs[j].Score
	})

	writeJSON(w, pairs)
}

func pointToNode(p qdrant.ScrollPoint) map[string]interface{} {
	return map[string]interface{}{
		"id":           p.ID,
		"text":         p.Payload["text"],
		"namespace":    p.Payload["namespace"],
		"tags":         p.Payload["tags"],
		"created_at":   p.Payload["created_at"],
		"permanent":    p.Payload["permanent"],
		"recall_count": p.Payload["recall_count"],
	}
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
