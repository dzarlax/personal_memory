package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

const (
	defaultRecallQueueSize     = 512
	defaultRecallFlushInterval = 100 * time.Millisecond
	recallShutdownTimeout      = 5 * time.Second
)

// recallCounter serializes Qdrant read-modify-write operations. Qdrant does
// not expose an atomic payload increment, so a single worker prevents recalls
// handled by this process from overwriting each other's counters.
type recallCounter struct {
	qdrant     *qdrant.Client
	invalidate func()
	queue      chan []string
	done       chan struct{}
	cancel     context.CancelFunc
	stopCh     chan struct{}

	mu        sync.Mutex
	enqueueWG sync.WaitGroup
	accepting bool
	stopOnce  sync.Once
}

func newRecallCounter(parent context.Context, qc *qdrant.Client, invalidate func(), queueSize int, flushInterval time.Duration) *recallCounter {
	ctx, cancel := context.WithCancel(parent)
	c := &recallCounter{
		qdrant:     qc,
		invalidate: invalidate,
		queue:      make(chan []string, queueSize),
		done:       make(chan struct{}),
		stopCh:     make(chan struct{}),
		cancel:     cancel,
		accepting:  true,
	}
	go c.run(ctx, flushInterval)
	return c
}

func (c *recallCounter) enqueue(ctx context.Context, id string) error {
	return c.enqueueBatch(ctx, []string{id})
}

func (c *recallCounter) enqueueBatch(ctx context.Context, ids []string) error {
	batch := append([]string(nil), ids...)
	if len(batch) == 0 {
		return nil
	}
	c.mu.Lock()
	if !c.accepting {
		c.mu.Unlock()
		return fmt.Errorf("recall counter is stopping")
	}
	c.enqueueWG.Add(1)
	c.mu.Unlock()
	defer c.enqueueWG.Done()
	select {
	case c.queue <- batch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("enqueue recall increment: %w", ctx.Err())
	case <-c.stopCh:
		return fmt.Errorf("recall counter is stopping")
	}
}

func (c *recallCounter) stop(ctx context.Context) error {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.accepting = false
		close(c.stopCh)
		c.mu.Unlock()
		c.enqueueWG.Wait()
		c.cancel()
	})
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop recall counter: %w", ctx.Err())
	}
}

func (c *recallCounter) run(ctx context.Context, flushInterval time.Duration) {
	defer close(c.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	pending := make(map[string]int)

	for {
		select {
		case batch := <-c.queue:
			addRecallBatch(pending, batch)
		case <-ticker.C:
			c.flush(ctx, pending)
		case <-ctx.Done():
			for {
				select {
				case batch := <-c.queue:
					addRecallBatch(pending, batch)
				default:
					shutdownCtx, cancel := context.WithTimeout(context.Background(), recallShutdownTimeout)
					for len(pending) > 0 {
						c.flush(shutdownCtx, pending)
						if len(pending) == 0 {
							break
						}
						select {
						case <-shutdownCtx.Done():
							slog.Error("recall counter: shutdown timed out with pending increments", "point_count", len(pending))
							cancel()
							return
						case <-time.After(defaultRecallFlushInterval):
						}
					}
					cancel()
					return
				}
			}
		}
	}
}

func addRecallBatch(pending map[string]int, batch []string) {
	for _, id := range batch {
		pending[id]++
	}
}

func (c *recallCounter) flush(ctx context.Context, pending map[string]int) {
	for id, delta := range pending {
		point, found, err := c.qdrant.Get(ctx, id)
		if err != nil {
			slog.Error("recall counter: get point failed", "point_id", id, "error", err)
			continue
		}
		if !found {
			slog.Warn("recall counter: point disappeared before increment", "point_id", id)
			delete(pending, id)
			continue
		}
		count := payloadInt(point.Payload["recall_count"])
		if err := c.qdrant.SetPayload(ctx, id, map[string]interface{}{
			"recall_count":     count + delta,
			"last_recalled_at": nowISO(),
		}); err != nil {
			// SetPayload is a read-modify-write operation. Once its request has
			// been dispatched, an error cannot tell us whether Qdrant applied the
			// increment before the response was lost. Retrying this delta could
			// double count, so deliberately drop it and keep the log content-free.
			slog.Error("recall counter: update outcome ambiguous; dropped increment")
			c.invalidateCache()
			delete(pending, id)
			continue
		}
		c.invalidateCache()
		delete(pending, id)
	}
}

func (c *recallCounter) invalidateCache() {
	if c.invalidate != nil {
		c.invalidate()
	}
}

func payloadInt(value interface{}) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}
