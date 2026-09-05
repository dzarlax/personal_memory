package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/memory/lifecycle"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

func newRecallCounterTestServer(t *testing.T, initial int) (*Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	count := initial
	qs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/points/fact-id"):
			_, _ = fmt.Fprintf(w, `{"result":{"id":"fact-id","payload":{"recall_count":%d}}}`, count)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/payload"):
			var body struct {
				Payload map[string]interface{} `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode payload: %v", err)
				return
			}
			count = int(body.Payload["recall_count"].(float64))
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
		default:
			t.Errorf("unexpected Qdrant request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(qs.Close)

	cache := NewCache(time.Minute)
	cache.SetRecall(recallFactsCacheKey("query", "", nil, 1, LifecycleRecallOptions{Mode: RecallLifecycleCurrent}), RecallFactsResult{
		Count:         1,
		LifecycleMode: RecallLifecycleCurrent,
		Facts: []RecallFact{{
			PointID:       "fact-id",
			Text:          "cached fact",
			Namespace:     "personal",
			Tags:          []string{},
			RecallCount:   initial,
			SemanticScore: 0.99,
			SemanticRank:  1,
			FinalRank:     1,
			Lifecycle: lifecycle.View{
				State:        lifecycle.Current,
				Legacy:       true,
				Supersedes:   []string{},
				SupersededBy: []string{},
				Valid:        true,
			},
			Decision:    LifecycleDecisionInclude,
			ReasonCodes: []LifecycleReasonCode{LifecycleReasonCurrentTruth},
		}},
	})
	srv := &Server{qdrant: qdrant.NewClient(qs.URL, "memory"), cache: cache}
	srv.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

func TestRecallCounterDropsAmbiguousWriteFailureWithoutRetryingDelta(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	qs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"result":{"id":"fact-id","payload":{"recall_count":7}}}`))
			return
		}
		mu.Lock()
		posts++
		mu.Unlock()
		// Simulate Qdrant applying the increment but the response failing. A
		// retry of the same read-modify-write delta would double count.
		http.Error(w, "response lost after apply", http.StatusServiceUnavailable)
	}))
	defer qs.Close()
	counter := newRecallCounter(context.Background(), qdrant.NewClient(qs.URL, "memory"), 2, 5*time.Millisecond)
	if err := counter.enqueue(context.Background(), "fact-id"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		got := posts
		mu.Unlock()
		if got >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("write was not attempted; attempts=%d", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := counter.stop(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("ambiguous delta was retried: attempts=%d", posts)
	}
}

func TestRecallFactsCacheHitIncrementsWithoutLeakingPointID(t *testing.T) {
	srv, count := newRecallCounterTestServer(t, 4)
	result, err := srv.recallFacts(context.Background(), toolRequest(map[string]interface{}{
		"query": "query", "limit": float64(1),
	}))
	if err != nil || result.IsError {
		t.Fatalf("recall failed: result=%#v err=%v", result, err)
	}
	if strings.Contains(toolResultText(t, result), "fact-id") {
		t.Fatal("internal point ID leaked in formatted recall output")
	}
	if !strings.Contains(toolResultText(t, result), "recalls:5") {
		t.Fatalf("cache-visible recall count was not advanced: %q", toolResultText(t, result))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 5 {
		t.Fatalf("recall_count = %d, want 5", got)
	}
}

func TestCountRecallsBatchAdmissionFailureHasNoPartialEffect(t *testing.T) {
	queue := make(chan []string, 1)
	counter := &recallCounter{
		queue:     queue,
		stopCh:    make(chan struct{}),
		accepting: false,
	}
	srv := &Server{recallCounter: counter}
	result := RecallFactsResult{Facts: []RecallFact{
		{PointID: "first", RecallCount: 3},
		{PointID: "second", RecallCount: 7},
	}}
	if err := srv.countRecalls(context.Background(), &result); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("expected stopped batch admission, got %v", err)
	}
	if result.Facts[0].RecallCount != 3 || result.Facts[1].RecallCount != 7 {
		t.Fatalf("failed batch changed visible counts: %#v", result.Facts)
	}
	if len(queue) != 0 {
		t.Fatalf("failed batch partially entered queue: len=%d", len(queue))
	}
}

func TestRecallCounterEnqueueBatchCopiesIDs(t *testing.T) {
	queue := make(chan []string, 1)
	counter := &recallCounter{queue: queue, stopCh: make(chan struct{}), accepting: true}
	ids := []string{"first", "second"}
	if err := counter.enqueueBatch(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	ids[0] = "mutated"
	batch := <-queue
	if batch[0] != "first" || batch[1] != "second" {
		t.Fatalf("queued batch aliases caller IDs: %v", batch)
	}
}

func TestRecallFactsConcurrentCacheHitsReturnUniqueMonotonicCounts(t *testing.T) {
	const (
		initial = 10
		calls   = 64
	)
	srv, _ := newRecallCounterTestServer(t, initial)
	start := make(chan struct{})
	counts := make(chan int, calls)
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := srv.recallFacts(context.Background(), toolRequest(map[string]interface{}{
				"query": "query", "limit": float64(1),
			}))
			if err != nil || result.IsError {
				errs <- fmt.Errorf("recall failed: result=%#v err=%v", result, err)
				return
			}
			structured := result.StructuredContent.(RecallFactsResult)
			counts <- structured.Facts[0].RecallCount
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(counts)
	seen := make(map[int]bool, calls)
	for count := range counts {
		if seen[count] {
			t.Fatalf("duplicate visible recall_count %d", count)
		}
		seen[count] = true
	}
	for want := initial + 1; want <= initial+calls; want++ {
		if !seen[want] {
			t.Fatalf("missing visible recall_count %d; got %v", want, seen)
		}
	}
	key := recallFactsCacheKey("query", "", nil, 1, LifecycleRecallOptions{Mode: RecallLifecycleCurrent})
	cached, ok := srv.cache.GetRecall(key)
	if !ok || cached.Facts[0].RecallCount != initial+calls {
		t.Fatalf("cached visible count = %#v ok=%v, want %d", cached, ok, initial+calls)
	}
}

func TestRecallCounterCoalescesConcurrentIncrementsAndDrains(t *testing.T) {
	srv, count := newRecallCounterTestServer(t, 2)
	const recalls = 100
	var wg sync.WaitGroup
	for i := 0; i < recalls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := srv.recallFacts(context.Background(), toolRequest(map[string]interface{}{
				"query": "query", "limit": float64(1),
			}))
			if err != nil || result.IsError {
				t.Errorf("recall failed: result=%#v err=%v", result, err)
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 2+recalls {
		t.Fatalf("recall_count = %d, want %d", got, 2+recalls)
	}
}

func TestRecallCounterAppliesBackpressureInsteadOfDropping(t *testing.T) {
	blockGet := make(chan struct{})
	enteredGet := make(chan struct{})
	var enteredOnce sync.Once
	qs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			enteredOnce.Do(func() { close(enteredGet) })
			<-blockGet
			_, _ = w.Write([]byte(`{"result":{"id":"first","payload":{"recall_count":0}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
	}))
	defer qs.Close()
	counter := newRecallCounter(context.Background(), qdrant.NewClient(qs.URL, "memory"), 1, time.Millisecond)
	if err := counter.enqueue(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-enteredGet:
	case <-time.After(time.Second):
		t.Fatal("worker did not begin flushing first increment")
	}
	if err := counter.enqueue(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := counter.enqueue(ctx, "third"); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected bounded backpressure deadline, got %v", err)
	}
	close(blockGet)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := counter.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
