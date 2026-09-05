package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/memory/lifecycle"
	"github.com/Dzarlax-AI/personal-memory/internal/memory/maintenance"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type Server struct {
	qdrant                 *qdrant.Client
	embed                  *embeddings.Client
	cache                  *Cache
	user                   string
	dedupThreshold         float64
	relatedFactLow         float64
	mutationMatchThreshold float64
	factMutationMu         [mutationStripeCount]sync.Mutex
	recallCounterMu        sync.Mutex
	recallCounter          *recallCounter
}

const mutationStripeCount = 64

func mutationStripe(namespace string) int {
	hash := uint32(2166136261)
	for index := 0; index < len(namespace); index++ {
		hash ^= uint32(namespace[index])
		hash *= 16777619
	}
	return int(hash % mutationStripeCount)
}

// lockFactMutations serializes mutations within a namespace while allowing
// unrelated namespaces to proceed. With no namespace it locks every stripe for
// collection-wide operations.
func (s *Server) lockFactMutations(namespaces ...string) func() {
	indexSet := make(map[int]struct{}, mutationStripeCount)
	if len(namespaces) == 0 {
		for index := range s.factMutationMu {
			indexSet[index] = struct{}{}
		}
	} else {
		for _, namespace := range namespaces {
			indexSet[mutationStripe(NormalizeNamespace(namespace))] = struct{}{}
		}
	}
	indexes := make([]int, 0, len(indexSet))
	for index := range indexSet {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		s.factMutationMu[index].Lock()
	}
	return func() {
		for index := len(indexes) - 1; index >= 0; index-- {
			s.factMutationMu[indexes[index]].Unlock()
		}
	}
}

func (s *Server) withFactMutationLock(namespace string, mutation func() bool) bool {
	unlockMutation := s.lockFactMutations(namespace)
	defer unlockMutation()
	return mutation()
}

type deterministicPointCheck string

const (
	deterministicPointAvailable         deterministicPointCheck = "available"
	deterministicPointCollision         deterministicPointCheck = "collision"
	deterministicPointInactiveCollision deterministicPointCheck = "inactive_collision"
	deterministicPointDependencyFailed  deterministicPointCheck = "dependency_failed"
)

func (s *Server) checkDeterministicPoint(ctx context.Context, pointID string) (deterministicPointCheck, string) {
	point, found, err := s.qdrant.Get(ctx, pointID)
	if err != nil {
		slog.Warn("deterministic point lookup failed", "point_id", pointID)
		return deterministicPointDependencyFailed, "deterministic point lookup failed; no write attempted"
	}
	if !found {
		return deterministicPointAvailable, ""
	}
	if !activeMemoryPayload(point.Payload) {
		return deterministicPointInactiveCollision, "deterministic point_id belongs to an inactive fact; use the maintenance restore workflow"
	}
	return deterministicPointCollision, "deterministic point_id already exists; use update_fact or set_fact_lifecycle"
}

// Start starts the bounded recall-counter worker. It is safe to call once.
func (s *Server) Start(ctx context.Context) {
	s.recallCounterMu.Lock()
	defer s.recallCounterMu.Unlock()
	if s.recallCounter == nil {
		s.recallCounter = newRecallCounter(ctx, s.qdrant, s.cache.Invalidate, defaultRecallQueueSize, defaultRecallFlushInterval)
	}
}

// Shutdown stops accepting recall increments and drains queued work.
func (s *Server) Shutdown(ctx context.Context) error {
	s.recallCounterMu.Lock()
	counter := s.recallCounter
	s.recallCounterMu.Unlock()
	if counter == nil {
		return nil
	}
	return counter.stop(ctx)
}

func (s *Server) countRecalls(ctx context.Context, result *RecallFactsResult) error {
	s.recallCounterMu.Lock()
	counter := s.recallCounter
	s.recallCounterMu.Unlock()
	if counter == nil {
		return fmt.Errorf("recall counter is not running")
	}
	ids := make([]string, 0, len(result.Facts))
	for _, fact := range result.Facts {
		if fact.PointID != "" {
			ids = append(ids, fact.PointID)
		}
	}
	if err := counter.enqueueBatch(ctx, ids); err != nil {
		return err
	}
	for index := range result.Facts {
		result.Facts[index].RecallCount++
	}
	return nil
}

const (
	mutationAmbiguityMargin = 0.01
	maxSearchLimit          = 100
	maxFactBytes            = 64 << 10
	maxQueryBytes           = 16 << 10
	maxImportBytes          = 4 << 20
	maxImportFacts          = 1000
	maxNamespaceBytes       = 255
	maxTags                 = 100
	maxTagBytes             = 255
	relatedFactResultLimit  = 3
)

var validPointIDPattern = regexp.MustCompile(`^(?:[0-9]+|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

func NewServer(qc *qdrant.Client, ec *embeddings.Client, cache *Cache, user string, dedupThreshold, relatedFactLow, mutationMatchThreshold float64) *Server {
	return &Server{
		qdrant:                 qc,
		embed:                  ec,
		cache:                  cache,
		user:                   user,
		dedupThreshold:         dedupThreshold,
		relatedFactLow:         relatedFactLow,
		mutationMatchThreshold: mutationMatchThreshold,
	}
}

// RegisterTools registers all memory MCP tools on the given MCP server.
func (s *Server) RegisterTools(srv *server.MCPServer) {
	srv.AddTool(mcp.NewTool("store_fact",
		mcp.WithDescription("Store a fact in semantic memory. Cosine similarity identifies related candidates and prevents duplicate writes at the deduplication threshold; valid superseded facts remain related context and do not block storage."),
		mcp.WithOutputSchema[StoreFactResult](),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("fact", mcp.Description("The fact to store"), mcp.Required()),
		mcp.WithString("tags", mcp.Description("Comma-separated semantic tags")),
		mcp.WithString("primary_tag", mcp.Description("Single primary tag for overview grouping; must also be present in tags")),
		mcp.WithString("namespace", mcp.Description("Namespace (default: default)")),
		mcp.WithBoolean("permanent", mcp.Description("Never deleted by forget_old")),
		mcp.WithString("valid_until", mcp.Description("ISO date after which fact expires")),
		mcp.WithString("lifecycle_state", mcp.Description("Optional explicit lifecycle state")),
		mcp.WithBoolean("canonical", mcp.Description("Current-only authority hint")),
		mcp.WithString("provenance_source", mcp.Description("Non-empty provenance source")),
		mcp.WithString("provenance_reference", mcp.Description("Optional provenance reference; requires provenance_source")),
		mcp.WithString("verified_at", mcp.Description("Optional RFC3339 verification timestamp")),
		mcp.WithArray("supersedes", mcp.Description("Point IDs replaced by this fact"), mcp.WithStringItems(), mcp.MaxItems(maxLifecycleRelations), mcp.UniqueItems(true)),
		mcp.WithArray("superseded_by", mcp.Description("Point IDs replacing this fact"), mcp.WithStringItems(), mcp.MaxItems(maxLifecycleRelations), mcp.UniqueItems(true)),
	), s.storeFact)

	srv.AddTool(mcp.NewTool("recall_facts",
		mcp.WithDescription("Semantic search for facts with explicit lifecycle intent. lifecycle_mode defaults to current; history returns valid current and non-current context, as_of applies its date only as the expiry reference and does not infer historical intervals, and include_all returns every valid lifecycle state. Disputed facts are marked uncertain."),
		mcp.WithOutputSchema[RecallFactsResult](),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("query", mcp.Description("Natural language search query"), mcp.Required()),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 5)")),
		mcp.WithString("lifecycle_mode", mcp.Description("Lifecycle intent: current (default), history, as_of, or include_all")),
		mcp.WithString("as_of", mcp.Description("Exact YYYY-MM-DD expiry reference; valid only with lifecycle_mode=as_of")),
	), s.recallFacts)

	srv.AddTool(mcp.NewTool("update_fact",
		mcp.WithDescription("Find a fact by similarity to old_query and replace it with new_fact."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("old_query", mcp.Description("Query to find the fact to update; required unless point_id is set")),
		mcp.WithString("point_id", mcp.Description("Exact fact ID; bypasses similarity matching after namespace validation")),
		mcp.WithString("new_fact", mcp.Description("New fact text"), mcp.Required()),
		mcp.WithString("tags", mcp.Description("Comma-separated semantic tags")),
		mcp.WithString("primary_tag", mcp.Description("Single primary tag for overview grouping; must also be present in tags")),
		mcp.WithString("namespace", mcp.Description("Namespace")),
		mcp.WithBoolean("permanent", mcp.Description("Set permanent flag")),
		mcp.WithString("lifecycle_state", mcp.Description("Optional replacement lifecycle state")),
		mcp.WithBoolean("canonical", mcp.Description("Current-only authority hint")),
		mcp.WithString("provenance_source", mcp.Description("Non-empty provenance source")),
		mcp.WithString("provenance_reference", mcp.Description("Optional provenance reference; requires provenance_source")),
		mcp.WithString("verified_at", mcp.Description("Optional RFC3339 verification timestamp")),
		mcp.WithArray("supersedes", mcp.Description("Point IDs replaced by this fact"), mcp.WithStringItems(), mcp.MaxItems(maxLifecycleRelations), mcp.UniqueItems(true)),
		mcp.WithArray("superseded_by", mcp.Description("Point IDs replacing this fact"), mcp.WithStringItems(), mcp.MaxItems(maxLifecycleRelations), mcp.UniqueItems(true)),
	), s.updateFact)

	srv.AddTool(mcp.NewTool("set_fact_lifecycle",
		mcp.WithDescription("Replace lifecycle metadata for one exact fact ID without changing its text or embedding."),
		mcp.WithOutputSchema[LifecycleMutationResult](),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("point_id", mcp.Description("Exact numeric legacy ID or UUID"), mcp.Required()),
		mcp.WithString("lifecycle_state", mcp.Description("current, historical, superseded, or disputed"), mcp.Required()),
		mcp.WithBoolean("canonical", mcp.Description("Current-only authority hint")),
		mcp.WithString("provenance_source", mcp.Description("Non-empty provenance source")),
		mcp.WithString("provenance_reference", mcp.Description("Optional provenance reference; requires provenance_source")),
		mcp.WithString("verified_at", mcp.Description("Optional RFC3339 verification timestamp")),
		mcp.WithArray("supersedes", mcp.Description("Point IDs replaced by this fact"), mcp.WithStringItems(), mcp.MaxItems(maxLifecycleRelations), mcp.UniqueItems(true)),
		mcp.WithArray("superseded_by", mcp.Description("Point IDs replacing this fact"), mcp.WithStringItems(), mcp.MaxItems(maxLifecycleRelations), mcp.UniqueItems(true)),
	), s.setFactLifecycle)

	srv.AddTool(mcp.NewTool("delete_fact",
		mcp.WithDescription("Find a fact by similarity and delete it."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("query", mcp.Description("Query to find the fact to delete; required unless point_id is set")),
		mcp.WithString("point_id", mcp.Description("Exact fact ID; bypasses similarity matching after namespace validation")),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
	), s.deleteFact)

	srv.AddTool(mcp.NewTool("forget_old",
		mcp.WithDescription("Deprecated read-only compatibility analysis for old facts. Direct deletion is refused; use the staged maintenance CLI."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithNumber("days", mcp.Description("Age threshold in days (default 90)")),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
		mcp.WithBoolean("dry_run", mcp.Description("If true, only report what would be deleted (default true)")),
	), s.forgetOld)

	srv.AddTool(mcp.NewTool("import_facts",
		mcp.WithDescription("Bulk import facts from JSON array."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("facts", mcp.Description("JSON array of fact objects"), mcp.Required()),
	), s.importFacts)

	srv.AddTool(mcp.NewTool("find_related",
		mcp.WithDescription("Find lifecycle-ranked related candidates by cosine similarity. Blocking duplicate candidates are excluded, while valid superseded candidates remain inspectable at any qualifying score."),
		mcp.WithOutputSchema[FindRelatedResult](),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("query", mcp.Description("Search query"), mcp.Required()),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 5)")),
	), s.findRelated)

	srv.AddTool(mcp.NewTool("list_facts",
		mcp.WithDescription("List all facts with metadata."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
	), s.listFacts)

	srv.AddTool(mcp.NewTool("get_stats",
		mcp.WithDescription("Get memory statistics: counts, namespaces, tags, most recalled."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), s.getStats)

	srv.AddTool(mcp.NewTool("list_tags",
		mcp.WithDescription("List all tags with counts."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
	), s.listTags)

	srv.AddTool(mcp.NewTool("export_facts",
		mcp.WithDescription("Export all facts as JSON."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
	), s.exportFacts)

	srv.AddTool(mcp.NewTool("get_operational_context",
		mcp.WithDescription("Return operational context: all permanent facts plus top facts by recall count. Call at session start for automatic context loading."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("namespace", mcp.Description("Filter by namespace")),
		mcp.WithNumber("top_recalled", mcp.Description("Number of top recalled non-permanent facts to include (default 10)")),
	), s.getOperationalContext)
}

// --- Tool parameter helpers ---

func strParam(args map[string]interface{}, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func intParam(args map[string]interface{}, key string, def int) (int, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n > float64(^uint(0)>>1) || n < -float64(^uint(0)>>1)-1 {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int(n), nil
	case int:
		return n, nil
	}
	return 0, fmt.Errorf("%s must be an integer", key)
}

func boolParam(args map[string]interface{}, key string, def bool) bool {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	b, _ := v.(bool)
	return b
}

func tagsParam(args map[string]interface{}) []string {
	v, ok := args["tags"]
	if !ok || v == nil {
		return nil
	}
	return tagsParamFromPayload(v)
}

func tagsParamFromPayload(v interface{}) []string {
	switch t := v.(type) {
	case []interface{}:
		tags := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				tags = append(tags, s)
			}
		}
		return tags
	case string:
		if t == "" {
			return nil
		}
		return strings.Split(t, ",")
	}
	return nil
}

func stringFromPayload(v interface{}) string {
	s, _ := v.(string)
	return s
}

func validateBoundedString(name, value string, maxBytes int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s must be at most %d bytes", name, maxBytes)
	}
	return nil
}

func validateNamespace(namespace string) error {
	return validateBoundedString("namespace", namespace, maxNamespaceBytes, false)
}

func validateTagsPayload(raw interface{}) error {
	var tags []string
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		tags = strings.Split(value, ",")
	case []string:
		tags = value
	case []interface{}:
		if len(value) > maxTags {
			return fmt.Errorf("tags must contain at most %d entries", maxTags)
		}
		tags = make([]string, 0, len(value))
		for i, item := range value {
			tag, ok := item.(string)
			if !ok {
				return fmt.Errorf("tags[%d] must be a string", i)
			}
			tags = append(tags, tag)
		}
	default:
		return fmt.Errorf("tags must be a comma-separated string or array of strings")
	}
	if len(tags) > maxTags {
		return fmt.Errorf("tags must contain at most %d entries", maxTags)
	}
	for i, tag := range tags {
		if len(tag) > maxTagBytes {
			return fmt.Errorf("tags[%d] must be at most %d bytes", i, maxTagBytes)
		}
	}
	return nil
}

func validateCommonMetadata(args map[string]interface{}) error {
	if err := validateNamespace(strParam(args, "namespace")); err != nil {
		return err
	}
	if raw, ok := args["tags"]; ok {
		if err := validateTagsPayload(raw); err != nil {
			return err
		}
	}
	if err := validateBoundedString("primary_tag", strParam(args, "primary_tag"), maxTagBytes, false); err != nil {
		return err
	}
	return nil
}

// --- Helpers ---

func normalizeFactTags(tags []string, primary string) ([]string, string) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		t := strings.TrimSpace(tag)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	primary = strings.TrimSpace(primary)
	if primary != "" {
		if _, ok := seen[primary]; !ok {
			out = append(out, primary)
		}
		return out, primary
	}
	if len(out) == 1 {
		return out, out[0]
	}
	return out, ""
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

const utcCalendarDateLayout = "2006-01-02"

// parseUTCCalendarDate accepts only a real, zero-padded ISO calendar date.
// Dates intentionally have no time-of-day or local-time interpretation.
func parseUTCCalendarDate(value string) (time.Time, error) {
	parsed, err := time.Parse(utcCalendarDateLayout, value)
	if err != nil || len(value) != len(utcCalendarDateLayout) || parsed.Format(utcCalendarDateLayout) != value {
		return time.Time{}, fmt.Errorf("must use exact YYYY-MM-DD format")
	}
	return parsed.UTC(), nil
}

// validUntilPayload returns whether an expiry is present and valid. A present
// malformed value is never treated as an unexpired current fact.
func validUntilPayload(payload map[string]interface{}) (time.Time, bool, error) {
	raw, exists := payload["valid_until"]
	if !exists || raw == nil {
		return time.Time{}, false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return time.Time{}, true, fmt.Errorf("valid_until must be an exact YYYY-MM-DD calendar date")
	}
	parsed, err := parseUTCCalendarDate(value)
	if err != nil {
		return time.Time{}, true, fmt.Errorf("valid_until %w", err)
	}
	return parsed, true, nil
}

func validUntilArgument(args map[string]interface{}) (string, error) {
	raw, exists := args["valid_until"]
	if !exists || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("valid_until must use exact YYYY-MM-DD format")
	}
	if _, err := parseUTCCalendarDate(value); err != nil {
		return "", fmt.Errorf("valid_until must use exact YYYY-MM-DD format")
	}
	return value, nil
}

func isExpired(payload map[string]interface{}) bool {
	return factExpiredAt(payload, time.Now().UTC())
}

func (s *Server) buildFilters(tags []string, namespace string) map[string]interface{} {
	var must []map[string]interface{}
	if namespace != "" {
		must = append(must, map[string]interface{}{
			"key": "namespace",
			"match": map[string]interface{}{
				"value": namespace,
			},
		})
	}
	for _, tag := range tags {
		must = append(must, map[string]interface{}{
			"key": "tags",
			"match": map[string]interface{}{
				"value": tag,
			},
		})
	}
	if len(must) == 0 {
		return nil
	}
	return map[string]interface{}{
		"must": must,
	}
}

func validatePositiveLimit(name string, value int) string {
	if value <= 0 {
		return fmt.Sprintf("%s must be greater than zero", name)
	}
	if value > maxSearchLimit {
		return fmt.Sprintf("%s must be at most %d", name, maxSearchLimit)
	}
	return ""
}

func mutationCandidates(points []qdrant.Point) string {
	parts := make([]string, 0, len(points))
	for _, point := range points {
		text, _ := point.Payload["text"].(string)
		parts = append(parts, fmt.Sprintf("id=%s score=%.3f text=%q", point.ID, point.Score, text))
	}
	return strings.Join(parts, "; ")
}

func (s *Server) mutationTarget(ctx context.Context, args map[string]interface{}, queryKey string) (qdrant.Point, string) {
	namespace := strings.TrimSpace(strParam(args, "namespace"))
	if id := strings.TrimSpace(strParam(args, "point_id")); id != "" {
		if !validPointIDPattern.MatchString(id) {
			return qdrant.Point{}, "point_id must be a numeric legacy ID or UUID"
		}
		point, found, err := s.qdrant.Get(ctx, id)
		if err != nil {
			return qdrant.Point{}, fmt.Sprintf("exact point lookup failed: %v", err)
		}
		if !found {
			return qdrant.Point{}, fmt.Sprintf("no fact found with point_id %s", id)
		}
		if namespace != "" && stringFromPayload(point.Payload["namespace"]) != namespace {
			return qdrant.Point{}, fmt.Sprintf("point_id %s does not belong to namespace %q", id, namespace)
		}
		if !activeMemoryPayload(point.Payload) {
			return qdrant.Point{}, "fact is quarantined or has invalid maintenance metadata; use the maintenance restore workflow"
		}
		return point, ""
	}

	query := strings.TrimSpace(strParam(args, queryKey))
	if query == "" {
		return qdrant.Point{}, fmt.Sprintf("%s or point_id is required", queryKey)
	}
	vec, err := s.embed.Embed(ctx, query)
	if err != nil {
		return qdrant.Point{}, fmt.Sprintf("embedding failed: %v", err)
	}
	results, err := s.qdrant.Search(ctx, vec, 2, activeMemoryFilters(s.buildFilters(nil, namespace)), nil)
	if err != nil {
		return qdrant.Point{}, fmt.Sprintf("search failed: %v", err)
	}
	results = activeSearchPoints(results)
	if len(results) == 0 {
		return qdrant.Point{}, "no matching fact found"
	}
	if results[0].Score < s.mutationMatchThreshold {
		return qdrant.Point{}, fmt.Sprintf("mutation refused: best score %.3f is below threshold %.3f; candidates: %s", results[0].Score, s.mutationMatchThreshold, mutationCandidates(results))
	}
	if len(results) > 1 && results[0].Score-results[1].Score < mutationAmbiguityMargin {
		return qdrant.Point{}, fmt.Sprintf("mutation refused: ambiguous matches (score delta %.3f is below %.3f); candidates: %s", results[0].Score-results[1].Score, mutationAmbiguityMargin, mutationCandidates(results))
	}
	return results[0], ""
}

// --- Tool implementations ---

type StoreFactResult struct {
	Status       string                 `json:"status"`
	Stored       bool                   `json:"stored"`
	PointID      string                 `json:"point_id,omitempty"`
	Message      string                 `json:"message,omitempty"`
	Duplicate    *RelatedFactCandidate  `json:"duplicate,omitempty"`
	RelatedFacts []RelatedFactCandidate `json:"related_facts"`
}

type ImportFactItemResult struct {
	ItemIndex int                   `json:"item_index"`
	Status    string                `json:"status"`
	PointID   string                `json:"point_id,omitempty"`
	Message   string                `json:"message,omitempty"`
	Duplicate *RelatedFactCandidate `json:"duplicate,omitempty"`
}

type ImportFactsResult struct {
	Imported int                    `json:"imported"`
	Outcomes []ImportFactItemResult `json:"outcomes"`
}

type FindRelatedResult struct {
	RelatedFacts []RelatedFactCandidate `json:"related_facts"`
	Count        int                    `json:"count"`
}

type LifecycleMutationResult struct {
	PointID   string         `json:"point_id"`
	Lifecycle lifecycle.View `json:"lifecycle"`
}

type RecallFact struct {
	PointID       string                        `json:"point_id"`
	Text          string                        `json:"text"`
	Namespace     string                        `json:"namespace"`
	Tags          []string                      `json:"tags"`
	PrimaryTag    string                        `json:"primary_tag,omitempty"`
	RecallCount   int                           `json:"recall_count"`
	SemanticScore float64                       `json:"semantic_score"`
	SemanticRank  int                           `json:"semantic_rank"`
	FinalRank     int                           `json:"final_rank"`
	Lifecycle     lifecycle.View                `json:"lifecycle"`
	Decision      LifecyclePresentationDecision `json:"decision"`
	ReasonCodes   []LifecycleReasonCode         `json:"reason_codes"`
}

type RecallFactsResult struct {
	Count                    int                 `json:"count"`
	LifecycleMode            RecallLifecycleMode `json:"lifecycle_mode"`
	AsOf                     string              `json:"as_of,omitempty"`
	CandidateWindowSaturated bool                `json:"candidate_window_saturated"`
	Facts                    []RecallFact        `json:"facts"`
}

func formatRelatedCandidate(candidate RelatedFactCandidate) string {
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return "- candidate: <unavailable>"
	}
	return "- candidate: " + string(encoded)
}

func formatStoreFactResult(result StoreFactResult) string {
	lines := []string{
		"status: " + result.Status,
		fmt.Sprintf("stored: %t", result.Stored),
	}
	if result.PointID != "" {
		lines = append(lines, "point_id: "+result.PointID)
	}
	if result.Message != "" {
		lines = append(lines, "message: "+result.Message)
	}
	if result.Duplicate != nil {
		lines = append(lines, "duplicate:", formatRelatedCandidate(*result.Duplicate))
	}
	lines = append(lines, fmt.Sprintf("related_facts: %d", len(result.RelatedFacts)))
	for _, candidate := range result.RelatedFacts {
		lines = append(lines, formatRelatedCandidate(candidate))
	}
	return strings.Join(lines, "\n")
}

func formatImportFactsResult(result ImportFactsResult) string {
	lines := []string{fmt.Sprintf("imported: %d", result.Imported), fmt.Sprintf("outcomes: %d", len(result.Outcomes))}
	for _, outcome := range result.Outcomes {
		line := fmt.Sprintf("- item_index: %d status: %s", outcome.ItemIndex, outcome.Status)
		if outcome.PointID != "" {
			line += " point_id: " + outcome.PointID
		}
		if outcome.Message != "" {
			line += " message: " + outcome.Message
		}
		lines = append(lines, line)
		if outcome.Duplicate != nil {
			lines = append(lines, formatRelatedCandidate(*outcome.Duplicate))
		}
	}
	return strings.Join(lines, "\n")
}

func formatFindRelatedResult(result FindRelatedResult) string {
	lines := []string{fmt.Sprintf("count: %d", result.Count)}
	for _, candidate := range result.RelatedFacts {
		lines = append(lines, formatRelatedCandidate(candidate))
	}
	return strings.Join(lines, "\n")
}

func (s *Server) storeFact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	fact := strParam(args, "fact")
	if err := validateBoundedString("fact", fact, maxFactBytes, true); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateCommonMetadata(args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	tags, primaryTag := normalizeFactTags(tagsParam(args), strParam(args, "primary_tag"))
	namespace := NormalizeNamespace(strParam(args, "namespace"))
	pointID := PointID(namespace, fact)
	parsedLifecycle, err := parseLifecycleInput(args, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var targetLifecycle lifecycle.View
	if parsedLifecycle.Present {
		targetLifecycle, err = lifecycle.NormalizeInput(pointID, parsedLifecycle.Input)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := lifecycle.ValidateTransition(pointID, lifecycle.View{State: lifecycle.Current, Valid: true}, targetLifecycle); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	permanent := boolParam(args, "permanent", false)
	validUntil, err := validUntilArgument(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	vec, err := s.embed.Embed(ctx, fact)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embedding failed: %v", err)), nil
	}

	unlockMutation := s.lockFactMutations(namespace)
	defer unlockMutation()

	if check, message := s.checkDeterministicPoint(ctx, pointID); check != deterministicPointAvailable {
		result := StoreFactResult{Status: string(check), Stored: false, PointID: pointID, Message: message, RelatedFacts: []RelatedFactCandidate{}}
		toolResult := mcp.NewToolResultStructured(result, formatStoreFactResult(result))
		toolResult.IsError = true
		return toolResult, nil
	}

	dedupLimit := lifecycleCandidateLimit(relatedFactResultLimit)
	dedupLow := s.dedupThreshold
	dedupCandidates, dedupErr := s.qdrant.Search(ctx, vec, dedupLimit, activeMemoryFilters(s.buildFilters(nil, namespace)), &dedupLow)
	var duplicate *RelatedFactCandidate
	if dedupErr != nil {
		// Preserve the existing fail-open behavior: availability of duplicate
		// preflight must not make the memory write path unavailable.
		slog.Warn("dedup search failed", "error", dedupErr)
	} else {
		dedupCandidates = activeSearchPoints(dedupCandidates)
		duplicate, _ = selectRelatedCandidates(dedupCandidates, s.relatedFactLow, s.dedupThreshold, relatedFactResultLimit)
		if duplicate == nil && len(dedupCandidates) == dedupLimit {
			return mcp.NewToolResultError("duplicate preflight inconclusive; candidate limit reached"), nil
		}
	}

	relatedFacts := []RelatedFactCandidate{}
	relatedLow := s.relatedFactLow
	relatedCandidates, relatedErr := s.qdrant.Search(ctx, vec, lifecycleCandidateLimit(relatedFactResultLimit), activeMemoryFilters(s.buildFilters(nil, namespace)), &relatedLow)
	if relatedErr != nil {
		slog.Warn("related fact search failed", "error", relatedErr)
	} else {
		relatedCandidates = activeSearchPoints(relatedCandidates)
		var relatedDuplicate *RelatedFactCandidate
		relatedDuplicate, relatedFacts = selectRelatedCandidates(relatedCandidates, s.relatedFactLow, s.dedupThreshold, relatedFactResultLimit)
		if duplicate == nil {
			duplicate = relatedDuplicate
		}
	}

	if duplicate != nil {
		result := StoreFactResult{
			Status:       "duplicate",
			Stored:       false,
			Duplicate:    duplicate,
			RelatedFacts: relatedFacts,
		}
		return mcp.NewToolResultStructured(result, formatStoreFactResult(result)), nil
	}

	createdAt := nowISO()
	payload := map[string]interface{}{
		"text":         fact,
		"user":         s.user,
		"namespace":    namespace,
		"tags":         tags,
		"primary_tag":  primaryTag,
		"permanent":    permanent,
		"created_at":   createdAt,
		"recall_count": 0,
	}
	if validUntil != "" {
		payload["valid_until"] = validUntil
	}
	if parsedLifecycle.Present {
		targetLifecycle.TransitionedAt = createdAt
		payload = lifecycle.ApplyToPayload(payload, targetLifecycle)
	}

	writeDispatched := true
	defer func() {
		if writeDispatched {
			s.cache.Invalidate()
		}
	}()
	if err := s.qdrant.Upsert(ctx, qdrant.Point{
		ID:      pointID,
		Vector:  vec,
		Payload: payload,
	}); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("store failed: %v", err)), nil
	}

	result := StoreFactResult{
		Status:       "stored",
		Stored:       true,
		PointID:      pointID,
		RelatedFacts: relatedFacts,
	}
	return mcp.NewToolResultStructured(result, formatStoreFactResult(result)), nil
}

func (s *Server) recallFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	query := strParam(args, "query")
	if err := validateBoundedString("query", query, maxQueryBytes, true); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateCommonMetadata(args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	lifecycleOptions, err := parseLifecycleRecallOptions(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	tags := tagsParam(args)
	namespace := strParam(args, "namespace")
	limit, err := intParam(args, "limit", 5)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if validationError := validatePositiveLimit("limit", limit); validationError != "" {
		return mcp.NewToolResultError(validationError), nil
	}

	cacheKey := recallFactsCacheKey(query, namespace, tags, limit, lifecycleOptions)
	cached, flight, err := s.cache.AcquireRecall(ctx, cacheKey, func(result *RecallFactsResult) error {
		return s.countRecalls(ctx, result)
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("record recall failed: %v", err)), nil
	}
	if flight == nil {
		return mcp.NewToolResultStructured(cached, formatRecallFactsResult(cached)), nil
	}
	defer s.cache.FinishRecall(cacheKey, flight, nil)

	vec, err := s.embed.Embed(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embedding failed: %v", err)), nil
	}

	candidateLimit := lifecycleCandidateLimit(limit)
	results, err := s.qdrant.Search(ctx, vec, candidateLimit, lifecycleRecallFilters(s.buildFilters(tags, namespace), lifecycleOptions.Mode), nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}
	candidateWindowSaturated := len(results) == candidateLimit
	results = activeSearchPoints(results)

	candidates := presentLifecycleRecallCandidates(results, lifecycleOptions, time.Now())
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	result := RecallFactsResult{
		LifecycleMode:            lifecycleOptions.normalizedMode(),
		AsOf:                     lifecycleOptions.AsOf,
		CandidateWindowSaturated: candidateWindowSaturated,
		Facts:                    make([]RecallFact, 0, len(candidates)),
	}
	for _, candidate := range candidates {
		point := candidate.point
		result.Facts = append(result.Facts, RecallFact{
			PointID:       point.ID,
			Text:          stringFromPayload(point.Payload["text"]),
			Namespace:     stringFromPayload(point.Payload["namespace"]),
			Tags:          relatedCandidateTags(point.Payload["tags"]),
			PrimaryTag:    stringFromPayload(point.Payload["primary_tag"]),
			RecallCount:   payloadInt(point.Payload["recall_count"]),
			SemanticScore: point.Score,
			SemanticRank:  candidate.SemanticRank,
			FinalRank:     candidate.FinalRank,
			Lifecycle:     candidate.view,
			Decision:      candidate.Decision,
			ReasonCodes:   append([]LifecycleReasonCode{}, candidate.ReasonCodes...),
		})
	}
	result.Count = len(result.Facts)
	if err := s.countRecalls(ctx, &result); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("record recall failed: %v", err)), nil
	}
	s.cache.FinishRecall(cacheKey, flight, &result)
	return mcp.NewToolResultStructured(result, formatRecallFactsResult(result)), nil
}

func recallFactsCacheKey(query, namespace string, tags []string, limit int, options LifecycleRecallOptions) string {
	canonicalTags := append([]string{}, tags...)
	sort.Strings(canonicalTags)
	if len(canonicalTags) > 1 {
		unique := canonicalTags[:1]
		for _, tag := range canonicalTags[1:] {
			if tag != unique[len(unique)-1] {
				unique = append(unique, tag)
			}
		}
		canonicalTags = unique
	}
	mode := options.normalizedMode()
	asOf := ""
	if mode == RecallLifecycleAsOf {
		asOf = options.AsOf
	}
	identity := struct {
		Version       string              `json:"version"`
		Query         string              `json:"query"`
		Namespace     string              `json:"namespace"`
		Tags          []string            `json:"tags"`
		Limit         int                 `json:"limit"`
		LifecycleMode RecallLifecycleMode `json:"lifecycle_mode"`
		AsOf          string              `json:"as_of"`
	}{
		Version:       lifecycleRecallCacheVersion,
		Query:         query,
		Namespace:     namespace,
		Tags:          canonicalTags,
		Limit:         limit,
		LifecycleMode: mode,
		AsOf:          asOf,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		panic(fmt.Sprintf("encode recall cache identity: %v", err))
	}
	return string(encoded)
}

func (s *Server) updateFact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	newFact := strParam(args, "new_fact")
	if err := validateBoundedString("new_fact", newFact, maxFactBytes, true); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := validateCommonMetadata(args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strParam(args, "point_id") == "" {
		if err := validateBoundedString("old_query", strParam(args, "old_query"), maxQueryBytes, true); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	parsedLifecycle, err := parseLifecycleInput(args, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	oldCandidate, targetError := s.mutationTarget(ctx, args, "old_query")
	if targetError != "" {
		return mcp.NewToolResultError(targetError), nil
	}
	oldNamespace := NormalizeNamespace(stringFromPayload(oldCandidate.Payload["namespace"]))
	namespace := oldNamespace
	if ns := strParam(args, "namespace"); ns != "" {
		namespace = NormalizeNamespace(ns)
	}
	newID := PointID(namespace, newFact)

	validateTarget := func(point qdrant.Point, targetID string) (lifecycle.View, error) {
		if parsedLifecycle.Present {
			target, validationErr := lifecycle.NormalizeInput(targetID, parsedLifecycle.Input)
			if validationErr == nil {
				validationErr = lifecycle.ValidateTransition(targetID, lifecycleView(point.ID, point.Payload), target)
			}
			return target, validationErr
		}
		return lifecycle.View{}, lifecycle.ValidateRelationshipsForPoint(point.Payload, targetID)
	}
	if _, err = validateTarget(oldCandidate, newID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("lifecycle metadata is invalid for updated point_id: %v", err)), nil
	}

	// Embedding does not participate in mutation ordering and stays outside the
	// namespace lock. Lifecycle metadata is revalidated against the exact point
	// after the lock is acquired.
	newVec, err := s.embed.Embed(ctx, newFact)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embedding new fact failed: %v", err)), nil
	}

	unlockMutation := s.lockFactMutations(oldNamespace, namespace)
	defer unlockMutation()

	old, found, err := s.qdrant.Get(ctx, oldCandidate.ID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("exact point lookup failed: %v", err)), nil
	}
	if !found {
		return mcp.NewToolResultError("fact changed during update; retry"), nil
	}
	if !activeMemoryPayload(old.Payload) {
		return mcp.NewToolResultError("fact entered quarantine during update; use the maintenance restore workflow"), nil
	}
	if currentNamespace := NormalizeNamespace(stringFromPayload(old.Payload["namespace"])); currentNamespace != oldNamespace {
		return mcp.NewToolResultError("fact namespace changed during update; retry"), nil
	}
	if newID != old.ID {
		if _, conflictFound, lookupErr := s.qdrant.Get(ctx, newID); lookupErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("target point lookup failed: %v", lookupErr)), nil
		} else if conflictFound {
			return mcp.NewToolResultError("updated text collides with an existing fact; refusing to overwrite"), nil
		}
	}
	currentLifecycle := lifecycleView(old.ID, old.Payload)
	targetLifecycle, err := validateTarget(old, newID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("lifecycle metadata is invalid for updated point_id: %v", err)), nil
	}
	oldText, _ := old.Payload["text"].(string)

	// Preserve metadata from old fact without mutating the retrieved map.
	payload := make(map[string]interface{}, len(old.Payload)+1)
	for key, value := range old.Payload {
		payload[key] = value
	}
	payload["text"] = newFact
	updatedAt := nowISO()
	payload["updated_at"] = updatedAt
	payload["namespace"] = namespace
	if tags := tagsParam(args); tags != nil {
		primary := strParam(args, "primary_tag")
		if primary == "" {
			primary = stringFromPayload(payload["primary_tag"])
		}
		normalizedTags, primaryTag := normalizeFactTags(tags, primary)
		payload["tags"] = normalizedTags
		payload["primary_tag"] = primaryTag
	} else if primary := strParam(args, "primary_tag"); primary != "" {
		normalizedTags, primaryTag := normalizeFactTags(tagsParamFromPayload(payload["tags"]), primary)
		payload["tags"] = normalizedTags
		payload["primary_tag"] = primaryTag
	}
	if v, ok := args["permanent"]; ok && v != nil {
		payload["permanent"] = v
	}
	if parsedLifecycle.Present {
		targetLifecycle.TransitionedAt = lifecycleTransitionedAt(currentLifecycle, targetLifecycle, updatedAt)
		payload = lifecycle.ApplyToPayload(payload, targetLifecycle)
	}

	writeDispatched := false
	defer func() {
		if writeDispatched {
			s.cache.Invalidate()
		}
	}()
	writeDispatched = true
	if err := s.qdrant.Upsert(ctx, qdrant.Point{
		ID:      newID,
		Vector:  newVec,
		Payload: payload,
	}); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("store updated fact failed: %v", err)), nil
	}

	if old.ID != newID {
		writeDispatched = true
		if err := s.qdrant.Delete(ctx, []string{old.ID}); err != nil {
			slog.Warn(
				"delete old point after update failed; duplicate may remain",
				"old_point_id", old.ID,
				"new_point_id", newID,
				"error", err,
			)
			return mcp.NewToolResultError(fmt.Sprintf("delete old failed: %v", err)), nil
		}
	}

	return mcp.NewToolResultText(fmt.Sprintf("Updated: '%s' → '%s'", oldText, newFact)), nil
}

func (s *Server) setFactLifecycle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pointID := strings.TrimSpace(strParam(args, "point_id"))
	if !validPointIDPattern.MatchString(pointID) {
		return mcp.NewToolResultError("point_id must be a numeric legacy ID or UUID"), nil
	}
	parsed, err := parseLifecycleInput(args, true)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target, err := lifecycle.NormalizeInput(pointID, parsed.Input)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	point, found, err := s.qdrant.Get(ctx, pointID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("exact point lookup failed: %v", err)), nil
	}
	if !found {
		return mcp.NewToolResultError(fmt.Sprintf("no fact found with point_id %s", pointID)), nil
	}
	if !activeMemoryPayload(point.Payload) {
		return mcp.NewToolResultError("quarantined facts cannot be changed by lifecycle tools; restore the fact first"), nil
	}
	namespace := NormalizeNamespace(stringFromPayload(point.Payload["namespace"]))
	unlockMutation := s.lockFactMutations(namespace)
	defer unlockMutation()

	point, found, err = s.qdrant.Get(ctx, pointID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("exact point lookup failed: %v", err)), nil
	}
	if !found {
		return mcp.NewToolResultError(fmt.Sprintf("no fact found with point_id %s", pointID)), nil
	}
	if !activeMemoryPayload(point.Payload) {
		return mcp.NewToolResultError("fact entered quarantine during lifecycle update; restore the fact first"), nil
	}
	if NormalizeNamespace(stringFromPayload(point.Payload["namespace"])) != namespace {
		return mcp.NewToolResultError("fact namespace changed during lifecycle update; retry"), nil
	}
	current := lifecycleView(point.ID, point.Payload)
	if err := lifecycle.ValidateTransition(point.ID, current, target); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target.TransitionedAt = lifecycleTransitionedAt(current, target, nowISO())
	set, deleteKeys := lifecycleMutationPayload(target)
	err = s.qdrant.ReplaceLifecyclePayload(ctx, point.ID, set, deleteKeys)
	// Once dispatched, a transport/server error is ambiguous: Qdrant may have
	// applied the first ordered operation. Never leave a stale read cache.
	s.cache.Invalidate()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("set lifecycle failed: %v", err)), nil
	}

	result := LifecycleMutationResult{PointID: point.ID, Lifecycle: target}
	return mcp.NewToolResultStructured(result, fmt.Sprintf("Updated lifecycle for point_id %s to %s", point.ID, target.State)), nil
}

func (s *Server) deleteFact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	if err := validateNamespace(strParam(args, "namespace")); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strParam(args, "point_id") == "" {
		if err := validateBoundedString("query", strParam(args, "query"), maxQueryBytes, true); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	targetCandidate, targetError := s.mutationTarget(ctx, args, "query")
	if targetError != "" {
		return mcp.NewToolResultError(targetError), nil
	}
	namespace := NormalizeNamespace(stringFromPayload(targetCandidate.Payload["namespace"]))
	unlockMutation := s.lockFactMutations(namespace)
	defer unlockMutation()

	target, found, err := s.qdrant.Get(ctx, targetCandidate.ID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("exact point lookup failed: %v", err)), nil
	}
	if !found {
		return mcp.NewToolResultError("fact changed during delete; retry"), nil
	}
	if !activeMemoryPayload(target.Payload) {
		return mcp.NewToolResultError("fact entered quarantine during delete; use the maintenance workflow"), nil
	}
	if NormalizeNamespace(stringFromPayload(target.Payload["namespace"])) != namespace {
		return mcp.NewToolResultError("fact namespace changed during delete; retry"), nil
	}
	// A delete error after dispatch is ambiguous: the server may have applied
	// it before the client lost the response. Invalidate before reporting it.
	writeDispatched := true
	defer func() {
		if writeDispatched {
			s.cache.Invalidate()
		}
	}()
	if err := s.qdrant.Delete(ctx, []string{target.ID}); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete failed: %v", err)), nil
	}
	text, _ := target.Payload["text"].(string)
	return mcp.NewToolResultText(fmt.Sprintf("Deleted: %s (score %.2f)", text, targetCandidate.Score)), nil
}

func (s *Server) forgetOld(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	days, err := intParam(args, "days", 90)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if days <= 0 {
		return mcp.NewToolResultError("days must be greater than zero"), nil
	}
	namespace := strParam(args, "namespace")
	if err := validateNamespace(namespace); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	dryRun := boolParam(args, "dry_run", true)
	if !dryRun {
		return mcp.NewToolResultError("direct age-only deletion is disabled; run maintenance analyze, then explicitly quarantine and snapshot-gated purge"), nil
	}
	now := time.Now().UTC()
	manifest, err := maintenance.Analyze(ctx, s.qdrant, maintenance.Options{
		Collection: s.qdrant.CollectionName(), Namespace: namespace, ReferenceTime: now,
		SupersededRetention: 30 * 24 * time.Hour, StaleAfter: time.Duration(days) * 24 * time.Hour, LowRecallThreshold: 1,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("maintenance analysis failed: %v", err)), nil
	}
	eligible := 0
	for _, finding := range manifest.Findings {
		if finding.EligibleForQuarantine {
			eligible++
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("Deprecated dry-run analysis: scanned=%d findings=%d eligible_for_quarantine=%d. No facts were changed; use cmd/maintenance for a saved content-free manifest.", manifest.Scanned, len(manifest.Findings), eligible)), nil
}

func (s *Server) importFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	factsRaw := strParam(args, "facts")
	if factsRaw == "" {
		return mcp.NewToolResultError("facts is required"), nil
	}
	if len(factsRaw) > maxImportBytes {
		return mcp.NewToolResultError(fmt.Sprintf("facts must be at most %d bytes", maxImportBytes)), nil
	}

	// Parse JSON array of fact objects.
	var facts []map[string]interface{}
	if err := json.Unmarshal([]byte(factsRaw), &facts); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid JSON: %v", err)), nil
	}
	if len(facts) > maxImportFacts {
		return mcp.NewToolResultError(fmt.Sprintf("facts must contain at most %d entries", maxImportFacts)), nil
	}

	type importCandidate struct {
		itemIndex      int
		source         map[string]interface{}
		text           string
		namespace      string
		pointID        string
		lifecycle      lifecycle.View
		hasLifecycle   bool
		vector         []float32
		embeddingReady bool
	}

	candidates := make([]importCandidate, 0, len(facts))
	outcomesByIndex := make(map[int]ImportFactItemResult, len(facts))
	setOutcome := func(index int, status, pointID, message string, duplicate *RelatedFactCandidate) {
		outcomesByIndex[index] = ImportFactItemResult{
			ItemIndex: index,
			Status:    status,
			PointID:   pointID,
			Message:   message,
			Duplicate: duplicate,
		}
	}
	for index, f := range facts {
		text, _ := f["text"].(string)
		if err := validateBoundedString("text", text, maxFactBytes, true); err != nil {
			setOutcome(index, "invalid", "", err.Error(), nil)
			continue
		}
		if err := validateNamespace(stringFromPayload(f["namespace"])); err != nil {
			setOutcome(index, "invalid", "", err.Error(), nil)
			continue
		}
		if err := validateTagsPayload(f["tags"]); err != nil {
			setOutcome(index, "invalid", "", err.Error(), nil)
			continue
		}
		if err := validateBoundedString("primary_tag", stringFromPayload(f["primary_tag"]), maxTagBytes, false); err != nil {
			setOutcome(index, "invalid", "", err.Error(), nil)
			continue
		}
		if _, _, err := validUntilPayload(f); err != nil {
			setOutcome(index, "invalid", "", err.Error(), nil)
			continue
		}

		namespace := NormalizeNamespace(stringFromPayload(f["namespace"]))
		pointID := PointID(namespace, text)
		var importedLifecycle lifecycle.View
		hasLifecycle := false
		for _, key := range lifecycle.PayloadFields() {
			if _, exists := f[key]; exists {
				hasLifecycle = true
				break
			}
		}
		if hasLifecycle {
			var err error
			importedLifecycle, err = lifecycle.Parse(f, pointID)
			if err != nil {
				slog.Warn("import lifecycle validation failed", "item_index", index, "point_id", pointID, "error", err)
				setOutcome(index, "invalid", pointID, "lifecycle metadata is invalid", nil)
				continue
			}
		}

		candidates = append(candidates, importCandidate{
			itemIndex:    index,
			source:       f,
			text:         text,
			namespace:    namespace,
			pointID:      pointID,
			lifecycle:    importedLifecycle,
			hasLifecycle: hasLifecycle,
		})
	}

	texts := make([]string, len(candidates))
	for index := range candidates {
		texts[index] = candidates[index].text
	}
	vectors, batchErr := s.embed.EmbedBatch(ctx, texts)
	if batchErr == nil {
		for index := range candidates {
			candidates[index].vector = vectors[index]
			candidates[index].embeddingReady = true
		}
	} else {
		// Preserve the historical per-item failure behavior if a batch request
		// fails, without logging private text or a possibly echoing TEI body.
		slog.Warn("import batch embed failed; retrying individually", "candidate_count", len(candidates))
		for index := range candidates {
			vec, embedErr := s.embed.Embed(ctx, candidates[index].text)
			if embedErr != nil {
				slog.Warn("import embed failed", "item_index", candidates[index].itemIndex, "point_id", candidates[index].pointID)
				setOutcome(candidates[index].itemIndex, "embedding_failed", candidates[index].pointID, "embedding failed", nil)
				continue
			}
			candidates[index].vector = vec
			candidates[index].embeddingReady = true
		}
	}

	imported := 0
	writesDispatched := false
	for _, candidate := range candidates {
		if !candidate.embeddingReady {
			continue
		}
		outcome := ImportFactItemResult{ItemIndex: candidate.itemIndex, PointID: candidate.pointID, Status: "failed"}
		s.withFactMutationLock(candidate.namespace, func() bool {
			if check, message := s.checkDeterministicPoint(ctx, candidate.pointID); check != deterministicPointAvailable {
				outcome.Status = string(check)
				outcome.Message = message
				return false
			}
			// Deduplication is namespace-scoped and lifecycle-aware, matching
			// store_fact semantics. Search failures preserve the existing
			// fail-open import behavior, while a saturated non-blocking window
			// is inconclusive.
			dedupLimit := lifecycleCandidateLimit(relatedFactResultLimit)
			dedupLow := s.dedupThreshold
			existing, searchErr := s.qdrant.Search(
				ctx,
				candidate.vector,
				dedupLimit,
				activeMemoryFilters(s.buildFilters(nil, candidate.namespace)),
				&dedupLow,
			)
			if searchErr == nil {
				existing = activeSearchPoints(existing)
				duplicate, _ := selectRelatedCandidates(existing, s.relatedFactLow, s.dedupThreshold, relatedFactResultLimit)
				if duplicate != nil {
					outcome.Status = "duplicate"
					outcome.Message = "semantic duplicate blocks import"
					outcome.Duplicate = duplicate
					return false
				}
				if len(existing) == dedupLimit {
					outcome.Status = "inconclusive"
					outcome.Message = "duplicate preflight candidate limit reached"
					return false
				}
			}

			payload := map[string]interface{}{
				"text":         candidate.text,
				"user":         s.user,
				"namespace":    candidate.namespace,
				"tags":         nil,
				"primary_tag":  nil,
				"permanent":    candidate.source["permanent"],
				"created_at":   candidate.source["created_at"],
				"recall_count": 0,
			}
			tags, primaryTag := normalizeFactTags(
				tagsParamFromPayload(candidate.source["tags"]),
				stringFromPayload(candidate.source["primary_tag"]),
			)
			payload["tags"] = tags
			payload["primary_tag"] = primaryTag
			if value, ok := candidate.source["valid_until"]; ok {
				payload["valid_until"] = value
			}
			if payload["created_at"] == nil {
				payload["created_at"] = nowISO()
			}
			if candidate.hasLifecycle {
				if candidate.lifecycle.TransitionedAt == "" {
					candidate.lifecycle.TransitionedAt = nowISO()
				}
				payload = lifecycle.ApplyToPayload(payload, candidate.lifecycle)
			}

			writesDispatched = true
			if err := s.qdrant.Upsert(ctx, qdrant.Point{
				ID:      candidate.pointID,
				Vector:  candidate.vector,
				Payload: payload,
			}); err != nil {
				slog.Warn("import upsert failed", "item_index", candidate.itemIndex, "point_id", candidate.pointID)
				outcome.Status = "write_ambiguous"
				outcome.Message = "write outcome is ambiguous; verify before retrying"
				return false
			}
			outcome.Status = "stored"
			outcome.Message = ""
			return true
		})
		setOutcome(candidate.itemIndex, outcome.Status, outcome.PointID, outcome.Message, outcome.Duplicate)
		if outcome.Status == "stored" {
			imported++
		}
	}

	if writesDispatched {
		s.cache.Invalidate()
	}
	outcomes := make([]ImportFactItemResult, 0, len(facts))
	for index := range facts {
		outcome, ok := outcomesByIndex[index]
		if !ok {
			outcome = ImportFactItemResult{ItemIndex: index, Status: "failed", Message: "item was not processed"}
		}
		outcomes = append(outcomes, outcome)
	}
	result := ImportFactsResult{Imported: imported, Outcomes: outcomes}
	return mcp.NewToolResultStructured(result, formatImportFactsResult(result)), nil
}

func (s *Server) findRelated(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	query := strParam(args, "query")
	if err := validateBoundedString("query", query, maxQueryBytes, true); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	namespace := strParam(args, "namespace")
	if err := validateNamespace(namespace); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	limit, err := intParam(args, "limit", 5)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if validationError := validatePositiveLimit("limit", limit); validationError != "" {
		return mcp.NewToolResultError(validationError), nil
	}

	vec, err := s.embed.Embed(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("embedding failed: %v", err)), nil
	}

	low := s.relatedFactLow
	results, err := s.qdrant.Search(ctx, vec, lifecycleCandidateLimit(limit), activeMemoryFilters(s.buildFilters(nil, namespace)), &low)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}
	results = activeSearchPoints(results)

	_, relatedFacts := selectRelatedCandidates(results, s.relatedFactLow, s.dedupThreshold, limit)
	structured := FindRelatedResult{RelatedFacts: relatedFacts, Count: len(relatedFacts)}
	return mcp.NewToolResultStructured(structured, formatFindRelatedResult(structured)), nil
}

func (s *Server) listFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	if err := validateCommonMetadata(args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	namespace := strParam(args, "namespace")
	tags := tagsParam(args)

	filters := activeMemoryFilters(s.buildFilters(tags, namespace))
	points, err := s.qdrant.ScrollAll(ctx, filters, false)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("scroll failed: %v", err)), nil
	}
	points = activeScrollPoints(points)

	var lines []string
	for _, p := range points {
		text, _ := p.Payload["text"].(string)
		ns, _ := p.Payload["namespace"].(string)
		createdAt, _ := p.Payload["created_at"].(string)
		rc := 0
		if v, ok := p.Payload["recall_count"].(float64); ok {
			rc = int(v)
		}
		perm := ""
		if v, ok := p.Payload["permanent"].(bool); ok && v {
			perm = " [permanent]"
		}
		tagsList := formatTagsList(p.Payload["tags"])
		primary := formatPrimaryTag(p.Payload["primary_tag"])
		lifecycleSummary := formatLifecycleView(lifecycleView(p.ID, p.Payload))
		lines = append(lines, fmt.Sprintf("- [%s] %s%s ns:%s%s recalls:%d %s %s", createdAt, tagsList, primary, ns, perm, rc, lifecycleSummary, text))
	}

	if len(lines) == 0 {
		return mcp.NewToolResultText("No facts found."), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func (s *Server) getStats(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	points, err := s.qdrant.ScrollAll(ctx, activeMemoryFilters(nil), false)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("scroll failed: %v", err)), nil
	}
	points = activeScrollPoints(points)

	total := len(points)
	permanent := 0
	expired := 0
	lifecycleStateCounts, legacyLifecycle, invalidLifecycle := lifecycleCounts(points)
	namespaces := make(map[string]int)
	tags := make(map[string]int)
	primaryTags := make(map[string]int)
	missingPrimary := 0
	var mostRecalled string
	maxRecalls := 0

	for _, p := range points {
		if v, ok := p.Payload["permanent"].(bool); ok && v {
			permanent++
		}
		if isExpired(p.Payload) {
			expired++
		}
		if ns, ok := p.Payload["namespace"].(string); ok {
			namespaces[ns]++
		}
		for _, tag := range tagsParamFromPayload(p.Payload["tags"]) {
			tags[tag]++
		}
		if primary, ok := p.Payload["primary_tag"].(string); ok && strings.TrimSpace(primary) != "" {
			primaryTags[primary]++
		} else {
			missingPrimary++
		}
		if rc, ok := p.Payload["recall_count"].(float64); ok && int(rc) > maxRecalls {
			maxRecalls = int(rc)
			mostRecalled, _ = p.Payload["text"].(string)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Total facts: %d\n", total)
	fmt.Fprintf(&sb, "Permanent: %d\n", permanent)
	fmt.Fprintf(&sb, "Expired: %d\n", expired)
	sb.WriteString("\nLifecycle states:\n")
	for _, line := range sortLifecycleCounts(lifecycleStateCounts) {
		sb.WriteString(line + "\n")
	}
	fmt.Fprintf(&sb, "  legacy (no lifecycle fields): %d\n", legacyLifecycle)
	fmt.Fprintf(&sb, "  invalid explicit metadata: %d\n", invalidLifecycle)

	sb.WriteString("\nNamespaces:\n")
	for ns, count := range namespaces {
		fmt.Fprintf(&sb, "  %s: %d\n", ns, count)
	}

	sb.WriteString("\nTop tags:\n")
	type tagCount struct {
		tag   string
		count int
	}
	var sorted []tagCount
	for t, c := range tags {
		sorted = append(sorted, tagCount{t, c})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
	for i, tc := range sorted {
		if i >= 20 {
			break
		}
		fmt.Fprintf(&sb, "  %s: %d\n", tc.tag, tc.count)
	}

	sb.WriteString("\nPrimary tags:\n")
	sorted = sorted[:0]
	for t, c := range primaryTags {
		sorted = append(sorted, tagCount{t, c})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
	for i, tc := range sorted {
		if i >= 20 {
			break
		}
		fmt.Fprintf(&sb, "  %s: %d\n", tc.tag, tc.count)
	}
	fmt.Fprintf(&sb, "  no primary_tag: %d\n", missingPrimary)

	if mostRecalled != "" {
		fmt.Fprintf(&sb, "\nMost recalled (%d times): %s", maxRecalls, mostRecalled)
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func (s *Server) listTags(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	namespace := strParam(args, "namespace")
	if err := validateNamespace(namespace); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	filters := activeMemoryFilters(s.buildFilters(nil, namespace))
	points, err := s.qdrant.ScrollAll(ctx, filters, false)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("scroll failed: %v", err)), nil
	}
	points = activeScrollPoints(points)

	tags := make(map[string]int)
	for _, p := range points {
		for _, tag := range tagsParamFromPayload(p.Payload["tags"]) {
			tags[tag]++
		}
	}

	if len(tags) == 0 {
		return mcp.NewToolResultText("No tags found."), nil
	}

	var lines []string
	for tag, count := range tags {
		lines = append(lines, fmt.Sprintf("%s: %d", tag, count))
	}
	sort.Strings(lines)
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func (s *Server) exportFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	namespace := strParam(args, "namespace")
	if err := validateNamespace(namespace); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	filters := activeMemoryFilters(s.buildFilters(nil, namespace))
	points, err := s.qdrant.ScrollAll(ctx, filters, false)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("scroll failed: %v", err)), nil
	}
	points = activeScrollPoints(points)

	var facts []map[string]interface{}
	for _, p := range points {
		facts = append(facts, p.Payload)
	}

	b, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal failed: %v", err)), nil
	}

	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) getOperationalFacts(ctx context.Context, namespace string, topRecalled int) ([]lifecycleOperationalPoint, error) {
	filters := activeMemoryFilters(currentLifecycleFilters(s.buildFilters(nil, namespace)))

	points, err := s.qdrant.ScrollAll(ctx, filters, false)
	if err != nil {
		return nil, err
	}
	points = activeScrollPoints(points)

	seen := make(map[string]bool)
	var permanent []lifecycleOperationalPoint
	var nonPermanent []lifecycleOperationalPoint

	for _, p := range points {
		view := lifecycleView(p.ID, p.Payload)
		if !lifecycle.IsCurrentTruth(view, isExpired(p.Payload)) {
			continue
		}
		lifecyclePoint := lifecycleOperationalPoint{point: p, view: view}
		if v, ok := p.Payload["permanent"].(bool); ok && v {
			permanent = append(permanent, lifecyclePoint)
			seen[p.ID] = true
		} else {
			nonPermanent = append(nonPermanent, lifecyclePoint)
		}
	}
	// Rank non-permanent facts by canonical authority, then recall count, and take top N.
	sort.Slice(nonPermanent, func(i, j int) bool {
		left := nonPermanent[i].view
		right := nonPermanent[j].view
		if left.Canonical != right.Canonical {
			return left.Canonical
		}
		ri, _ := nonPermanent[i].point.Payload["recall_count"].(float64)
		rj, _ := nonPermanent[j].point.Payload["recall_count"].(float64)
		if ri != rj {
			return ri > rj
		}
		return nonPermanent[i].point.ID < nonPermanent[j].point.ID
	})

	result := permanent
	added := 0
	for _, p := range nonPermanent {
		if added >= topRecalled {
			break
		}
		rc, _ := p.point.Payload["recall_count"].(float64)
		if rc == 0 {
			continue // never-recalled facts do not consume the bounded selection
		}
		if !seen[p.point.ID] {
			result = append(result, p)
			added++
		}
	}
	sortOperationalPoints(result)

	return result, nil
}

func (s *Server) getOperationalContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	namespace := strParam(args, "namespace")
	if err := validateNamespace(namespace); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	topRecalled, err := intParam(args, "top_recalled", 10)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if validationError := validatePositiveLimit("top_recalled", topRecalled); validationError != "" {
		return mcp.NewToolResultError(validationError), nil
	}

	points, err := s.getOperationalFacts(ctx, namespace, topRecalled)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("scroll failed: %v", err)), nil
	}

	if len(points) == 0 {
		return mcp.NewToolResultText("No operational context found."), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Operational Context (%d facts)\n\n", len(points))
	for _, lifecyclePoint := range points {
		p := lifecyclePoint.point
		text, _ := p.Payload["text"].(string)
		ns, _ := p.Payload["namespace"].(string)
		perm := ""
		if v, ok := p.Payload["permanent"].(bool); ok && v {
			perm = " [permanent]"
		}
		rc := 0
		if v, ok := p.Payload["recall_count"].(float64); ok {
			rc = int(v)
		}
		tagsList := formatTagsList(p.Payload["tags"])
		primary := formatPrimaryTag(p.Payload["primary_tag"])
		lifecycleSummary := formatLifecycleView(lifecyclePoint.view)
		fmt.Fprintf(&sb, "- %s%s ns:%s%s recalls:%d %s %s\n", tagsList, primary, ns, perm, rc, lifecycleSummary, text)
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// OperationalContextHandler returns an HTTP handler for GET /memory/operational.
// Returns operational facts as plain text, suitable for Claude Code hooks.
func (s *Server) OperationalContextHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace := r.URL.Query().Get("namespace")
		if err := validateNamespace(namespace); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		points, err := s.getOperationalFacts(r.Context(), namespace, 10)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(points) == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		var sb strings.Builder
		sb.WriteString("# Operational Context\n\n")
		for _, lifecyclePoint := range points {
			p := lifecyclePoint.point
			text, _ := p.Payload["text"].(string)
			ns, _ := p.Payload["namespace"].(string)
			lifecycleSummary := formatLifecycleView(lifecyclePoint.view)
			fmt.Fprintf(&sb, "- [%s] %s %s\n", ns, lifecycleSummary, text)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(sb.String()))
	}
}

// --- Formatting helpers ---

func formatRecallFactsResult(result RecallFactsResult) string {
	if len(result.Facts) == 0 {
		if result.CandidateWindowSaturated {
			return "No facts found. Candidate window saturated; results may be incomplete."
		}
		return "No facts found."
	}
	lines := make([]string, 0, len(result.Facts))
	for _, fact := range result.Facts {
		tagsList := formatTagsList(fact.Tags)
		primary := formatPrimaryTag(fact.PrimaryTag)
		lifecycleSummary := formatRecallLifecycleView(fact.Lifecycle)
		line := fmt.Sprintf("- [%.3f] %s%s ns:%s %s %s", fact.SemanticScore, tagsList, primary, fact.Namespace, lifecycleSummary, fact.Text)
		if fact.RecallCount > 0 {
			line = fmt.Sprintf("- [%.3f] %s%s ns:%s recalls:%d %s %s", fact.SemanticScore, tagsList, primary, fact.Namespace, fact.RecallCount, lifecycleSummary, fact.Text)
		}
		lines = append(lines, line)
	}
	if result.CandidateWindowSaturated {
		lines = append(lines, "Candidate window saturated; results may be incomplete.")
	}
	return strings.Join(lines, "\n")
}

func formatRecallLifecycleView(view lifecycle.View) string {
	// Relationships remain available in structured content. Their values are
	// point IDs, which recall's backward-compatible human-readable fallback
	// must not expose.
	view.Supersedes = nil
	view.SupersededBy = nil
	return formatLifecycleView(view)
}

func formatTagsList(v interface{}) string {
	if v == nil {
		return "[]"
	}
	switch t := v.(type) {
	case []interface{}:
		tags := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				tags = append(tags, "'"+s+"'")
			}
		}
		return "[" + strings.Join(tags, ", ") + "]"
	case []string:
		tags := make([]string, 0, len(t))
		for _, s := range t {
			tags = append(tags, "'"+s+"'")
		}
		return "[" + strings.Join(tags, ", ") + "]"
	}
	return "[]"
}

func formatPrimaryTag(v interface{}) string {
	primary, _ := v.(string)
	if primary == "" {
		return ""
	}
	return fmt.Sprintf(" primary:%s", primary)
}
