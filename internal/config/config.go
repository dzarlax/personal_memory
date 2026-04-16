package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Server
	Port   string
	APIKey string

	// Qdrant
	QdrantURL string

	// Embeddings
	EmbedURL string

	// Memory
	MemoryUser       string
	CacheTTL         time.Duration
	DedupThreshold   float64
	ContradictionLow float64

	// Backup
	BackupInterval time.Duration
	KeepSnapshots  int

	// Todoist
	EnableTodoist bool
	TodoistToken  string

	// Viz
	EnableViz            bool
	VizSimilarityThreshold float64

	// Domain (for Traefik labels in docker-compose)
	MemoryDomain string
}

func Load() *Config {
	return &Config{
		Port:   envOrDefault("MCP_PORT", "8000"),
		APIKey: os.Getenv("API_KEY"),

		QdrantURL: envOrDefault("QDRANT_URL", "http://memory-qdrant:6333"),
		EmbedURL:  envOrDefault("EMBED_URL", "http://memory-embeddings:80"),

		MemoryUser:       envOrDefault("MEMORY_USER", "claude"),
		CacheTTL:         envDuration("CACHE_TTL", 60*time.Second),
		DedupThreshold:   envFloat("DEDUP_THRESHOLD", 0.97),
		ContradictionLow: envFloat("CONTRADICTION_LOW", 0.60),

		BackupInterval: envDuration("BACKUP_INTERVAL_HOURS", 24*time.Hour),
		KeepSnapshots:  envInt("KEEP_SNAPSHOTS", 7),

		EnableTodoist: envBool("ENABLE_TODOIST"),
		TodoistToken:  os.Getenv("TODOIST_TOKEN"),

		EnableViz:              envBool("ENABLE_VIZ"),
		VizSimilarityThreshold: envFloat("VIZ_SIMILARITY_THRESHOLD", 0.65),

		MemoryDomain: os.Getenv("MEMORY_DOMAIN"),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	return os.Getenv(key) == "true"
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if key == "BACKUP_INTERVAL_HOURS" {
		h, err := strconv.Atoi(v)
		if err != nil {
			return def
		}
		return time.Duration(h) * time.Hour
	}
	s, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return time.Duration(s) * time.Second
}
