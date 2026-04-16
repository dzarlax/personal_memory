package backup

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

type Loop struct {
	qdrant        *qdrant.Client
	interval      time.Duration
	keepSnapshots int
}

func NewLoop(qc *qdrant.Client, interval time.Duration, keepSnapshots int) *Loop {
	return &Loop{
		qdrant:        qc,
		interval:      interval,
		keepSnapshots: keepSnapshots,
	}
}

// Run starts the backup loop. Blocks until ctx is cancelled.
func (l *Loop) Run(ctx context.Context) {
	slog.Info("backup loop started", "interval", l.interval, "keep_snapshots", l.keepSnapshots)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("backup loop stopped")
			return
		case <-ticker.C:
			l.doBackup(ctx)
		}
	}
}

func (l *Loop) doBackup(ctx context.Context) {
	name, err := l.qdrant.CreateSnapshot(ctx)
	if err != nil {
		slog.Error("backup: create snapshot failed", "error", err)
		return
	}
	slog.Info("backup: snapshot created", "name", name)

	// Prune old snapshots.
	snapshots, err := l.qdrant.ListSnapshots(ctx)
	if err != nil {
		slog.Error("backup: list snapshots failed", "error", err)
		return
	}

	if len(snapshots) <= l.keepSnapshots {
		return
	}

	// Sort alphabetically (snapshot names include timestamps).
	sort.Strings(snapshots)
	toDelete := snapshots[:len(snapshots)-l.keepSnapshots]
	for _, s := range toDelete {
		if err := l.qdrant.DeleteSnapshot(ctx, s); err != nil {
			slog.Error("backup: delete snapshot failed", "name", s, "error", err)
		} else {
			slog.Info("backup: pruned snapshot", "name", s)
		}
	}
}
