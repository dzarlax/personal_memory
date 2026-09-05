package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/embeddingidentity"
	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/memory/lifecycle"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/Dzarlax-AI/personal-memory/internal/rerank"
	"github.com/Dzarlax-AI/personal-memory/internal/retrieval"
)

// PurposeEmbedder embeds literal inputs using the dataset's declared profile.
type PurposeEmbedder interface {
	EmbedWithPurpose(context.Context, string, embeddings.Purpose, embeddings.InputProfile, string) ([]float32, error)
	EmbedBatchWithPurpose(context.Context, []string, embeddings.Purpose, embeddings.InputProfile, string) ([][]float32, error)
}

// RunOptions selects the evaluation source and external clients.
type RunOptions struct {
	Source         string
	QdrantURL      string
	Embed          func(context.Context, string) ([]float32, error)
	Embedder       PurposeEmbedder
	DocumentsRoot  string
	CleanupTimeout time.Duration
	Now            func() time.Time
	Reranker       rerank.Reranker
}

// Run validates and evaluates a dataset in fixture or read-only live mode.
func Run(ctx context.Context, dataset *Dataset, options RunOptions) (Report, error) {
	if dataset == nil {
		return Report{}, fmt.Errorf("dataset is required")
	}
	if err := dataset.ValidateForSource(options.Source); err != nil {
		return Report{}, err
	}
	if dataset.SchemaVersion == DocumentRoutingSchemaVersion {
		declaredModel := strings.TrimSpace(dataset.Configuration.RerankerModelID)
		if declaredModel != "" {
			if options.Reranker == nil {
				return Report{}, fmt.Errorf("reranker %q is configured but no reranker service was injected", declaredModel)
			}
			identity, ok := options.Reranker.(interface{ ModelID() string })
			if !ok || strings.TrimSpace(identity.ModelID()) != declaredModel {
				return Report{}, fmt.Errorf("injected reranker identity does not match configured reranker_model_id")
			}
		} else if options.Reranker != nil {
			return Report{}, fmt.Errorf("injected reranker requires reranker_model_id in report identity")
		}
	}
	if options.Source == "live" && strings.TrimSpace(options.DocumentsRoot) == "" {
		for _, query := range dataset.Queries {
			if query.Target == "documents" {
				return Report{}, fmt.Errorf("live document evaluation requires DocumentsRoot")
			}
		}
	}
	if strings.TrimSpace(options.QdrantURL) == "" {
		return Report{}, fmt.Errorf("qdrant URL is required")
	}
	switch options.Source {
	case "fixture":
		return runFixtureTimedWithEmbedder(ctx, dataset, options.QdrantURL, "fixture", nil, options.DocumentsRoot, options.CleanupTimeout, nil, options.Reranker)
	case "live":
		return runLive(ctx, dataset, options)
	case "tei-fixture":
		return runTEIFixture(ctx, dataset, options)
	default:
		return Report{}, fmt.Errorf("source must be fixture, live, or tei-fixture")
	}
}

type collections struct {
	facts   *qdrant.Client
	chunks  *qdrant.Client
	folders *qdrant.Client
}

func runFixture(ctx context.Context, dataset *Dataset, qdrantURL, documentsRoot string, cleanupTimeout time.Duration) (report Report, err error) {
	return runFixtureTimed(ctx, dataset, qdrantURL, "fixture", nil, documentsRoot, cleanupTimeout)
}

func runFixtureTimed(ctx context.Context, dataset *Dataset, qdrantURL, mode string, timings *timingCollector, documentsRoot string, cleanupTimeout time.Duration) (report Report, err error) {
	return runFixtureTimedWithEmbedder(ctx, dataset, qdrantURL, mode, timings, documentsRoot, cleanupTimeout, nil, nil)
}

func runFixtureTimedWithEmbedder(ctx context.Context, dataset *Dataset, qdrantURL, mode string, timings *timingCollector, documentsRoot string, cleanupTimeout time.Duration, queryEmbedder PurposeEmbedder, documentReranker rerank.Reranker) (report Report, err error) {
	suffix, err := randomSuffix()
	if err != nil {
		return Report{}, err
	}
	names := []string{
		"eval_facts_" + suffix,
		"eval_chunks_" + suffix,
		"eval_folders_" + suffix,
	}
	clients := collections{
		facts:   qdrant.NewClient(qdrantURL, names[0]),
		chunks:  qdrant.NewClient(qdrantURL, names[1]),
		folders: qdrant.NewClient(qdrantURL, names[2]),
	}
	allClients := []*qdrant.Client{clients.facts, clients.chunks, clients.folders}
	created := make([]*qdrant.Client, 0, len(allClients))
	if cleanupTimeout <= 0 {
		cleanupTimeout = 15 * time.Second
	}
	defer func() {
		var cleanupErrors []error
		for i := len(created) - 1; i >= 0; i-- {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			if cleanupErr := cleanupTemporaryCollection(cleanupCtx, created[i]); cleanupErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("clean up %s: %w", created[i].CollectionName(), cleanupErr))
			}
			cancel()
		}
		if cleanupErr := errors.Join(cleanupErrors...); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	metadata := map[string]any{
		"evaluation": map[string]any{
			"dataset_version": dataset.DatasetVersion,
			"temporary":       true,
		},
	}
	if dataset.SchemaVersion >= CurrentDatasetSchemaVersion {
		metadata[embeddingidentity.MetadataKey] = embeddingidentity.Record{
			SchemaVersion: 1, Provider: dataset.Embedding.Provider,
			ModelID: dataset.Embedding.ModelID, ModelRevision: dataset.Embedding.ModelRevision,
			ModelDType: dataset.Embedding.DType, Pooling: dataset.Embedding.Pooling,
			VectorSize:   dataset.Embedding.VectorSize,
			InputProfile: embeddings.InputProfile(dataset.Embedding.InputProfile),
		}
	}
	for _, client := range allClients {
		if !strings.HasPrefix(client.CollectionName(), "eval_") {
			return Report{}, fmt.Errorf("temporary collection %q lacks eval_ prefix", client.CollectionName())
		}
		info, infoErr := client.CollectionInfo(ctx)
		if infoErr != nil {
			return Report{}, fmt.Errorf("preflight temporary collection %s: %w", client.CollectionName(), infoErr)
		}
		if info.Exists {
			return Report{}, fmt.Errorf("temporary collection %q already exists", client.CollectionName())
		}
		if createErr := client.CreateCollection(ctx, dataset.Embedding.VectorSize, metadata); createErr != nil {
			resolveCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			postCreateInfo, inspectErr := client.CollectionInfo(resolveCtx)
			cancel()
			if inspectErr != nil {
				created = append(created, client)
				return Report{}, errors.Join(
					fmt.Errorf("create temporary collection %s: %w", client.CollectionName(), createErr),
					fmt.Errorf("resolve ambiguous creation of %s: %w", client.CollectionName(), inspectErr),
				)
			}
			if postCreateInfo.Exists {
				created = append(created, client)
			}
			return Report{}, fmt.Errorf("create temporary collection %s: %w", client.CollectionName(), createErr)
		}
		created = append(created, client)
	}
	if err := upsertFixturePoints(ctx, clients.facts, dataset.Facts); err != nil {
		return Report{}, fmt.Errorf("seed facts: %w", err)
	}
	if err := upsertFixturePoints(ctx, clients.chunks, dataset.Chunks); err != nil {
		return Report{}, fmt.Errorf("seed chunks: %w", err)
	}
	if err := upsertFixturePoints(ctx, clients.folders, dataset.Folders); err != nil {
		return Report{}, fmt.Errorf("seed folders: %w", err)
	}
	return executeQueriesTimed(ctx, dataset, clients, mode, timings, documentsRoot, queryEmbedder, documentReranker)
}

func cleanupTemporaryCollection(ctx context.Context, client *qdrant.Client) error {
	info, err := client.CollectionInfo(ctx)
	if err != nil {
		// Creation response loss can make the existence check unavailable.
		// The eval_ prefix guard plus idempotent delete keeps cleanup safe.
		return client.DeleteCollection(ctx, "eval_")
	}
	if !info.Exists {
		return nil
	}
	return client.DeleteCollection(ctx, "eval_")
}

func runLive(ctx context.Context, dataset *Dataset, options RunOptions) (Report, error) {
	clients := collections{
		facts:   qdrant.NewClient(options.QdrantURL, dataset.Configuration.FactCollection),
		chunks:  qdrant.NewClient(options.QdrantURL, dataset.Configuration.ChunkCollection),
		folders: qdrant.NewClient(options.QdrantURL, dataset.Configuration.FolderCollection),
	}
	copyDataset := *dataset
	copyDataset.Queries = append([]Query(nil), dataset.Queries...)
	var timings *timingCollector
	if dataset.SchemaVersion >= CurrentDatasetSchemaVersion {
		timings = newTimingCollector(options.Now)
		expected := embeddingidentity.Record{
			SchemaVersion: 1, Provider: dataset.Embedding.Provider,
			ModelID: dataset.Embedding.ModelID, ModelRevision: dataset.Embedding.ModelRevision,
			ModelDType: dataset.Embedding.DType, Pooling: dataset.Embedding.Pooling,
			VectorSize:   dataset.Embedding.VectorSize,
			InputProfile: embeddings.InputProfile(dataset.Embedding.InputProfile),
		}
		for _, client := range liveCollectionsUsed(dataset, clients) {
			if err := embeddingidentity.VerifyCollection(ctx, client, expected); err != nil {
				return Report{}, err
			}
		}
	}
	for i := range copyDataset.Queries {
		query := &copyDataset.Queries[i]
		if len(query.Vector) > 0 {
			continue
		}
		if dataset.SchemaVersion >= CurrentDatasetSchemaVersion {
			continue
		}
		var vector []float32
		var err error
		if options.Embed != nil {
			vector, err = options.Embed(ctx, query.Text)
		} else {
			return Report{}, fmt.Errorf("live query %q has no vector and no embedder was configured", query.ID)
		}
		if err != nil {
			return Report{}, fmt.Errorf("embed live query %q: %w", query.ID, err)
		}
		if err := validateVector(vector, copyDataset.Embedding.VectorSize); err != nil {
			return Report{}, fmt.Errorf("live query %q: %w", query.ID, err)
		}
		query.Vector = vector
	}
	return executeQueriesTimed(ctx, &copyDataset, clients, "live", timings, options.DocumentsRoot, options.Embedder, options.Reranker)
}

func liveCollectionsUsed(dataset *Dataset, clients collections) []*qdrant.Client {
	used := make(map[string]*qdrant.Client)
	for _, query := range dataset.Queries {
		if query.Target == "facts" {
			used[clients.facts.CollectionName()] = clients.facts
			continue
		}
		used[clients.chunks.CollectionName()] = clients.chunks
		if query.Mode == "hierarchical" ||
			(dataset.SchemaVersion == DocumentRoutingSchemaVersion &&
				dataset.Configuration.DocumentRoutingStrategy != DocumentRoutingFlat) {
			used[clients.folders.CollectionName()] = clients.folders
		}
	}
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]*qdrant.Client, 0, len(names))
	for _, name := range names {
		result = append(result, used[name])
	}
	return result
}

func runTEIFixture(ctx context.Context, dataset *Dataset, options RunOptions) (Report, error) {
	if options.Embedder == nil {
		return Report{}, fmt.Errorf("tei-fixture requires a purpose-aware embedder")
	}
	timings := newTimingCollector(options.Now)
	materializationTimings := &timedMaterializationEmbedder{
		delegate: options.Embedder,
		now:      timings.now,
	}
	materialized, err := cloneDataset(dataset)
	if err != nil {
		return Report{}, fmt.Errorf("clone tei-fixture dataset: %w", err)
	}
	preparation, err := materializeCorpusPurposeBatches(
		ctx, &materialized, materializationTimings,
	)
	if err != nil {
		return Report{}, err
	}
	for i := range materialized.Queries {
		materialized.Queries[i].Vector = nil
	}
	report, err := runFixtureTimedWithEmbedder(
		ctx, &materialized, options.QdrantURL, "tei-fixture", timings,
		options.DocumentsRoot, options.CleanupTimeout, options.Embedder,
		options.Reranker,
	)
	if err == nil {
		report.Diagnostics.Corpus = &CorpusDiagnostics{
			EmbeddingDurationUS: materializationTimings.corpusUS,
			EmbeddingCount:      preparation.Facts + preparation.Chunks + preparation.Folders,
		}
	}
	return report, err
}

func upsertFixturePoints(ctx context.Context, client *qdrant.Client, points []FixturePoint) error {
	for _, point := range points {
		if err := client.UpsertWithPointID(ctx, qdrant.Point{
			ID:      point.ID.String(),
			Vector:  point.Vector,
			Payload: point.Payload,
		}, point.ID.IsNumeric()); err != nil {
			return fmt.Errorf("upsert point %q: %w", point.ID.String(), err)
		}
	}
	return nil
}

func executeQueries(ctx context.Context, dataset *Dataset, clients collections, mode string) (Report, error) {
	return executeQueriesTimed(ctx, dataset, clients, mode, nil, "", nil, nil)
}

func executeQueriesTimed(ctx context.Context, dataset *Dataset, clients collections, mode string, timings *timingCollector, documentsRoot string, queryEmbedder PurposeEmbedder, documentReranker rerank.Reranker) (Report, error) {
	queries := append([]Query(nil), dataset.Queries...)
	sort.Slice(queries, func(i, j int) bool { return queries[i].ID < queries[j].ID })
	queryReports := make([]QueryReport, 0, len(queries))
	metrics := make([]QueryMetrics, 0, len(queries))
	var lifecycleReport *LifecycleReport
	var lifecycleFailures []string
	if dataset.SchemaVersion >= LifecycleSchemaVersion {
		lifecycleReport = &LifecycleReport{
			Transitions: executeTransitionScenarios(dataset.TransitionScenarios),
		}
		for _, transition := range lifecycleReport.Transitions {
			lifecycleReport.Aggregate.Checks++
			lifecycleReport.Aggregate.Violations += len(transition.Violations)
			lifecycleFailures = append(lifecycleFailures, lifecycleViolationMessages(transition.Violations)...)
		}
	}
	maxK := dataset.Configuration.TopK[len(dataset.Configuration.TopK)-1]
	now := time.Now()
	for _, query := range queries {
		queryStart := time.Now()
		if timings != nil {
			queryStart = timings.now()
		}
		if dataset.SchemaVersion >= CurrentDatasetSchemaVersion && len(query.Vector) == 0 {
			if queryEmbedder == nil {
				return Report{}, fmt.Errorf("query %q has no vector and no purpose-aware embedder was configured", query.ID)
			}
			var embedStart time.Time
			if timings != nil {
				embedStart = timings.now()
			}
			vectors, embedErr := embedQueryPurpose(
				ctx, []string{query.Text}, dataset.Embedding, queryEmbedder, false,
			)
			if embedErr != nil {
				return Report{}, fmt.Errorf("embed query %q: %w", query.ID, embedErr)
			}
			if timings != nil {
				timings.embed = append(timings.embed, elapsedUS(embedStart, timings.now()))
			}
			query.Vector = append(Vector(nil), vectors[0]...)
		}
		searchLimit := maxK
		if mode == "fixture" || mode == "tei-fixture" {
			if query.Target == "facts" && len(dataset.Facts) > searchLimit {
				searchLimit = len(dataset.Facts)
			}
			if query.Target == "documents" && len(dataset.Chunks) > searchLimit {
				searchLimit = len(dataset.Chunks)
			}
		}
		var (
			points       []qdrant.Point
			err          error
			routingTrace *RoutingTrace
		)
		searchStart := queryStart
		if timings != nil {
			searchStart = timings.now()
		}
		switch {
		case query.Target == "facts":
			candidateLimit := max(20, maxK*4)
			if dataset.SchemaVersion >= CurrentDatasetSchemaVersion &&
				dataset.Configuration.RetrievalStrategy == RetrievalHybridRRF {
				candidateLimit = dataset.Configuration.DenseCandidateLimit
			} else if mode == "fixture" || mode == "tei-fixture" {
				candidateLimit = max(candidateLimit, len(dataset.Facts))
			}
			var filter map[string]any
			if query.EffectiveIntent() == QueryIntentCurrent {
				filter = currentLifecycleFilter()
			}
			points, err = clients.facts.Search(ctx, query.Vector, candidateLimit, filter, nil)
			if err == nil {
				if dataset.Configuration.RetrievalStrategy == RetrievalHybridRRF {
					points, err = rerankAllPoints(query.Text, points, dataset.Configuration, "facts", documentsRoot)
				} else {
					points, err = rerankPoints(query.Text, points, maxK, dataset.Configuration, "facts", documentsRoot)
				}
			}
		case query.Target == "documents" && dataset.SchemaVersion == DocumentRoutingSchemaVersion:
			points, routingTrace, err = searchDocumentsWithRouting(ctx, clients, query, searchLimit,
				dataset.Configuration, documentsRoot, documentReranker)
		case query.Mode == "flat":
			candidateLimit := searchLimit
			if dataset.Configuration.RetrievalStrategy == RetrievalHybridRRF {
				candidateLimit = dataset.Configuration.DenseCandidateLimit
			}
			points, err = clients.chunks.Search(ctx, query.Vector, candidateLimit, nil, nil)
			if err == nil {
				points, err = rerankPoints(query.Text, points, searchLimit, dataset.Configuration, "chunks", documentsRoot)
			}
		default:
			points, err = hierarchicalSearchStrategy(ctx, clients, query.Text, query.Vector, searchLimit, dataset.Configuration, documentsRoot)
		}
		if timings != nil {
			timings.search = append(timings.search, elapsedUS(searchStart, timings.now()))
		}
		if err != nil {
			return Report{}, fmt.Errorf("execute query %q: %w", query.ID, err)
		}
		var items []RetrievedItem
		var queryLifecycle *QueryLifecycleReport
		if query.Target == "facts" && dataset.SchemaVersion >= LifecycleSchemaVersion {
			evidencePoints := points
			broadLifecycleSearch := requiresBroadLifecycleSearch(query)
			if broadLifecycleSearch {
				evidencePoints, err = fetchLifecycleEvidence(ctx, clients.facts, query, points)
				if err != nil {
					return Report{}, fmt.Errorf("fetch lifecycle evidence for query %q: %w", query.ID, err)
				}
			}
			hybridOrder := dataset.Configuration.RetrievalStrategy == RetrievalHybridRRF
			presentation := presentFactCandidatesWithOrder(query, evidencePoints, now, hybridOrder)
			if broadLifecycleSearch {
				items = presentFactCandidatesWithOrder(query, points, now, hybridOrder).results
			} else {
				items = presentation.results
			}
			queryLifecycle = &presentation.report
			lifecycleReport.Aggregate.Checks += presentation.canonical.Checks
			lifecycleReport.Aggregate.Violations += presentation.canonical.Violations
			lifecycleReport.Aggregate.CanonicalPreferenceChecks += presentation.canonical.CanonicalPreferenceChecks
			lifecycleReport.Aggregate.CanonicalPreferenceViolations += presentation.canonical.CanonicalPreferenceViolations
			lifecycleFailures = append(lifecycleFailures, lifecycleViolationMessages(presentation.report.Violations)...)
			lifecycleFailures = append(lifecycleFailures, canonicalPreferenceFailureMessages(
				query.ID, presentation.canonical.CanonicalPreferenceViolations,
			)...)
		} else if query.Target == "facts" {
			items = normalizeFactResults(points, now)
		} else {
			if dataset.SchemaVersion == DocumentRoutingSchemaVersion ||
				dataset.Configuration.RetrievalStrategy == RetrievalHybridRRF {
				items = itemsFromPoints(points)
			} else {
				items = normalizeResults(points)
			}
		}
		if items == nil {
			items = []RetrievedItem{}
		}
		if len(items) > maxK {
			items = items[:maxK]
		}
		queryMetrics := ScoreQuery(query, items, dataset.Configuration.TopK)
		queryReports = append(queryReports, QueryReport{
			ID: query.ID, Target: query.Target, Mode: query.Mode, Results: items, Metrics: queryMetrics,
			Lifecycle: queryLifecycle, Cohorts: append([]QueryCohort(nil), query.Cohorts...), Routing: routingTrace,
		})
		metrics = append(metrics, queryMetrics)
		if timings != nil {
			timings.total = append(timings.total, elapsedUS(queryStart, timings.now()))
		}
	}
	aggregate := Aggregate(metrics, dataset.Configuration.TopK)
	failures := EvaluateGates(aggregate, dataset.Gates)
	if dataset.Gates.ForbidLifecycleViolations {
		failures = append(failures, lifecycleFailures...)
		sort.Strings(failures)
	}
	reportSchema := dataset.SchemaVersion
	var cohortMetrics []CohortAggregateMetrics
	if dataset.SchemaVersion >= CurrentDatasetSchemaVersion {
		cohortMetrics = AggregateCohorts(queryReports, dataset.Configuration.TopK)
	}
	report := normalizeReport(Report{
		SchemaVersion:  reportSchema,
		DatasetVersion: dataset.DatasetVersion,
		Mode:           mode,
		Embedding:      dataset.Embedding,
		Configuration:  dataset.Configuration,
		TopK:           dataset.Configuration.TopK,
		Aggregate:      aggregate,
		Cohorts:        cohortMetrics,
		Queries:        queryReports,
		Lifecycle:      lifecycleReport,
		GatesPassed:    len(failures) == 0,
		GateFailures:   failures,
	})
	if timings != nil {
		report.Diagnostics = timings.diagnostics()
	}
	return report, nil
}

func searchDocumentsWithRouting(ctx context.Context, clients collections, query Query, limit int,
	cfg Configuration, documentsRoot string, service rerank.Reranker) ([]qdrant.Point, *RoutingTrace, error) {
	trace := &RoutingTrace{Strategy: cfg.DocumentRoutingStrategy, ReasonCodes: []string{},
		SelectedFolders: []string{}, Results: []RoutingResultTrace{}}
	candidateLimit := cfg.RoutingCandidateLimit
	var lists []retrieval.RankedList
	byID := make(map[string]qdrant.Point)
	add := func(name string, points []qdrant.Point) {
		if len(points) == 0 {
			return
		}
		lists = append(lists, retrieval.RankedList{Name: name, IDs: pointIDsForEval(points)})
		for _, point := range points {
			if _, ok := byID[point.ID]; !ok {
				byID[point.ID] = point
			}
		}
	}
	var flat []qdrant.Point
	if cfg.DocumentRoutingStrategy != DocumentRoutingFlat {
		threshold := cfg.FolderThreshold
		folders, err := clients.folders.Search(ctx, query.Vector, cfg.FolderTopK, nil, &threshold)
		if err != nil {
			return nil, nil, err
		}
		conditions := make([]map[string]any, 0, len(folders))
		for _, folder := range folders {
			stored, _ := folder.Payload["folder_path"].(string)
			if stored == "" {
				continue
			}
			conditions = append(conditions, map[string]any{"key": "folder_path", "match": map[string]any{"value": stored}})
			if relative, ok := relativeLexicalPath(stored, documentsRoot); ok {
				trace.SelectedFolders = append(trace.SelectedFolders, relative)
			}
		}
		if len(conditions) == 0 {
			trace.ReasonCodes = append(trace.ReasonCodes, RoutingReasonNoFolderMatch, RoutingReasonFlatFallback)
		} else {
			filtered, err := clients.chunks.Search(ctx, query.Vector, candidateLimit, map[string]any{"should": conditions}, nil)
			if err != nil {
				return nil, nil, err
			}
			add(RoutingSourceFolderFiltered, filtered)
			if len(filtered) == 0 {
				trace.ReasonCodes = append(trace.ReasonCodes, RoutingReasonEmptyFolderResult, RoutingReasonFlatFallback)
			}
		}
	}
	if cfg.DocumentRoutingStrategy != DocumentRoutingHierarchical || len(lists) == 0 {
		var err error
		flat, err = clients.chunks.Search(ctx, query.Vector, candidateLimit, nil, nil)
		if err != nil {
			return nil, nil, err
		}
		add(RoutingSourceFlat, flat)
	}
	if len(lists) == 0 {
		trace.ReasonCodes = append(trace.ReasonCodes, RoutingReasonEmptyResults)
		return []qdrant.Point{}, trace, nil
	}
	resultLimit := min(limit, retrieval.MaxResults)
	poolLimit := resultLimit
	if service != nil && cfg.RerankerModelID != "" {
		poolLimit = min(max(resultLimit, cfg.RerankerCandidateCap), retrieval.MaxResults)
	}
	options := retrieval.MultiListOptions{RRFConstant: cfg.RoutingRRFConstant, Limit: poolLimit}
	if cfg.DocumentRoutingStrategy == DocumentRoutingBlendedRRF && len(flat) > 0 {
		options.FlatSource = RoutingSourceFlat
	}
	fused, diagnostics, err := retrieval.FuseRankedLists(lists, options)
	if err != nil {
		return nil, nil, err
	}
	if diagnostics.FlatRescueApplied {
		trace.ReasonCodes = append(trace.ReasonCodes, RoutingReasonFlatRescue)
	}
	points := make([]qdrant.Point, len(fused))
	for i, item := range fused {
		points[i] = byID[item.ID]
		sources := make([]RoutingSourceTrace, len(item.Sources))
		for j, source := range item.Sources {
			sources[j] = RoutingSourceTrace{Source: source.Source, Rank: source.Rank}
		}
		trace.Results = append(trace.Results, RoutingResultTrace{ID: item.ID, Sources: sources})
	}
	if service != nil && cfg.RerankerModelID != "" && len(points) > 0 {
		points = applyDocumentReranker(ctx, service, query.Text,
			time.Duration(cfg.RerankerTimeoutMS)*time.Millisecond,
			cfg.RerankerCandidateCap, points, trace)
	}
	points = points[:min(len(points), resultLimit)]
	trace.Results = trace.Results[:min(len(trace.Results), resultLimit)]
	return points, trace, nil
}

func applyDocumentReranker(ctx context.Context, service rerank.Reranker, query string, timeout time.Duration,
	candidateCap int, points []qdrant.Point, trace *RoutingTrace) []qdrant.Point {
	candidateCount := min(len(points), candidateCap)
	candidates := make([]rerank.Candidate, candidateCount)
	for i := range candidates {
		text, _ := points[i].Payload["text"].(string)
		candidates[i] = rerank.Candidate{ID: points[i].ID, Text: text}
	}
	rerankCtx, cancel := context.WithTimeout(ctx, timeout)
	reranked, reason := rerank.ApplyFailOpen(rerankCtx, service, query, candidates)
	cancel()
	trace.RerankerReason = reason
	if reason != rerank.ReasonApplied {
		return points
	}
	pointsByID := make(map[string]qdrant.Point, candidateCount)
	traceByID := make(map[string]RoutingResultTrace, candidateCount)
	for i := 0; i < candidateCount; i++ {
		pointsByID[points[i].ID] = points[i]
		if i < len(trace.Results) {
			traceByID[trace.Results[i].ID] = trace.Results[i]
		}
	}
	for i := 0; i < len(reranked) && i < candidateCount; i++ {
		if point, ok := pointsByID[reranked[i].ID]; ok {
			points[i] = point
		}
		if result, ok := traceByID[reranked[i].ID]; ok && i < len(trace.Results) {
			trace.Results[i] = result
		}
	}
	return points
}

func pointIDsForEval(points []qdrant.Point) []string {
	ids := make([]string, len(points))
	for i := range points {
		ids[i] = points[i].ID
	}
	return ids
}

func fetchLifecycleEvidence(
	ctx context.Context,
	client *qdrant.Client,
	query Query,
	rankingPoints []qdrant.Point,
) ([]qdrant.Point, error) {
	evidence := append([]qdrant.Point(nil), rankingPoints...)
	seen := make(map[string]struct{}, len(rankingPoints)+len(query.LifecycleExpectations))
	for _, point := range rankingPoints {
		seen[point.ID] = struct{}{}
	}
	ids := make([]string, 0, len(query.LifecycleExpectations))
	for _, expectation := range query.LifecycleExpectations {
		if _, exists := seen[expectation.ID]; exists {
			continue
		}
		seen[expectation.ID] = struct{}{}
		ids = append(ids, expectation.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		point, exists, err := client.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get candidate %s: %w", id, err)
		}
		if exists {
			evidence = append(evidence, point)
		}
	}
	return evidence, nil
}

func requiresBroadLifecycleSearch(query Query) bool {
	if query.EffectiveIntent() != QueryIntentCurrent {
		return true
	}
	for _, expectation := range query.LifecycleExpectations {
		if expectation.Decision == PresentationSuppress ||
			expectation.Decision == PresentationUncertain {
			return true
		}
		if expectation.State != "" && expectation.State != lifecycle.Current {
			return true
		}
	}
	return false
}

func lifecycleViolationMessages(violations []LifecycleViolation) []string {
	messages := make([]string, len(violations))
	for i, violation := range violations {
		messages[i] = violation.message()
	}
	return messages
}

func canonicalPreferenceFailureMessages(queryID string, violations int) []string {
	if violations == 0 {
		return nil
	}
	return []string{fmt.Sprintf("query %s invariant %s", queryID, ReasonCanonicalPreference)}
}

func currentLifecycleFilter() map[string]any {
	return map[string]any{"should": []map[string]any{
		{"key": "lifecycle_state", "match": map[string]any{"value": "current"}},
		{"is_empty": map[string]any{"key": "lifecycle_state"}},
	}}
}

func hierarchicalSearch(ctx context.Context, clients collections, vector []float32, limit int, cfg Configuration) ([]qdrant.Point, error) {
	return hierarchicalSearchStrategy(ctx, clients, "", vector, limit, cfg, "")
}

func hierarchicalSearchStrategy(ctx context.Context, clients collections, rawQuery string, vector []float32, limit int, cfg Configuration, documentsRoot string) ([]qdrant.Point, error) {
	threshold := cfg.FolderThreshold
	folderLimit := cfg.FolderTopK
	if cfg.RetrievalStrategy == RetrievalHybridRRF {
		folderLimit = cfg.DenseCandidateLimit
	}
	folders, err := clients.folders.Search(ctx, vector, folderLimit, nil, &threshold)
	if err != nil {
		return nil, err
	}
	folders, err = rerankPoints(rawQuery, folders, cfg.FolderTopK, cfg, "folders", documentsRoot)
	if err != nil {
		return nil, err
	}
	conditions := make([]map[string]any, 0, len(folders))
	for _, folder := range folders {
		if path, ok := folder.Payload["folder_path"].(string); ok && path != "" {
			conditions = append(conditions, map[string]any{
				"key": "folder_path", "match": map[string]any{"value": path},
			})
		}
	}
	if len(conditions) == 0 {
		return searchAndRerankChunks(ctx, clients.chunks, rawQuery, vector, limit, cfg, nil, documentsRoot)
	}
	points, err := searchAndRerankChunks(ctx, clients.chunks, rawQuery, vector, limit, cfg,
		map[string]any{"should": conditions}, documentsRoot)
	if err != nil {
		return nil, err
	}
	if len(points) == 0 {
		return searchAndRerankChunks(ctx, clients.chunks, rawQuery, vector, limit, cfg, nil, documentsRoot)
	}
	return points, nil
}

func searchAndRerankChunks(ctx context.Context, client *qdrant.Client, rawQuery string,
	vector []float32, limit int, cfg Configuration, filter map[string]any, documentsRoot string) ([]qdrant.Point, error) {
	candidateLimit := limit
	if cfg.RetrievalStrategy == RetrievalHybridRRF {
		candidateLimit = cfg.DenseCandidateLimit
	}
	points, err := client.Search(ctx, vector, candidateLimit, filter, nil)
	if err != nil {
		return nil, err
	}
	return rerankPoints(rawQuery, points, limit, cfg, "chunks", documentsRoot)
}

func rerankPoints(rawQuery string, points []qdrant.Point, limit int, cfg Configuration, kind, documentsRoot string) ([]qdrant.Point, error) {
	if cfg.RetrievalStrategy != RetrievalHybridRRF || len(points) == 0 {
		return points, nil
	}
	byID, candidates := retrievalCandidates(points, kind, documentsRoot)
	ranked, err := retrieval.Rank(rawQuery, candidates, retrieval.Options{
		RRFConstant: cfg.RRFConstant, Limit: min(limit, retrieval.MaxResults),
	})
	if err != nil {
		return nil, fmt.Errorf("hybrid rank %s candidates: %w", kind, err)
	}
	result := make([]qdrant.Point, len(ranked))
	for i := range ranked {
		result[i] = byID[ranked[i].Candidate.ID]
	}
	return result, nil
}

func rerankAllPoints(rawQuery string, points []qdrant.Point, cfg Configuration, kind, documentsRoot string) ([]qdrant.Point, error) {
	if cfg.RetrievalStrategy != RetrievalHybridRRF || len(points) == 0 {
		return points, nil
	}
	byID, candidates := retrievalCandidates(points, kind, documentsRoot)
	ranked, err := retrieval.RankAll(rawQuery, candidates, cfg.RRFConstant)
	if err != nil {
		return nil, fmt.Errorf("hybrid rank full %s candidate pool: %w", kind, err)
	}
	result := make([]qdrant.Point, len(ranked))
	for i := range ranked {
		result[i] = byID[ranked[i].Candidate.ID]
	}
	return result, nil
}

func retrievalCandidates(points []qdrant.Point, kind, documentsRoot string) (map[string]qdrant.Point, []retrieval.Candidate) {
	byID := make(map[string]qdrant.Point, len(points))
	candidates := make([]retrieval.Candidate, 0, len(points))
	for _, point := range points {
		fields := lexicalFields(point.Payload, kind, documentsRoot)
		byID[point.ID] = point
		candidates = append(candidates, retrieval.Candidate{
			ID: point.ID, DenseScore: point.Score, Fields: fields, DenseOnly: len(fields) == 0,
		})
	}
	return byID, candidates
}

func lexicalFields(payload map[string]any, kind, documentsRoot string) []retrieval.Field {
	names := []string{"text"}
	switch kind {
	case "chunks":
		names = append(names, "heading", "file_path")
	case "folders":
		names = append(names, "summary", "folder_path")
	}
	values := make([]struct{ name, value string }, 0, len(names))
	for _, name := range names {
		value, ok := payload[name].(string)
		if ok && strings.TrimSpace(value) != "" {
			if name == "file_path" || name == "folder_path" {
				value, ok = relativeLexicalPath(value, documentsRoot)
				if !ok {
					continue
				}
			}
			values = append(values, struct{ name, value string }{name, value})
		}
	}
	if len(values) == 0 {
		return nil
	}
	// retrieval requires one canonical text field. If a legacy folder has only
	// summary/path, promote the first valid value without exposing it.
	fields := []retrieval.Field{{Name: "text", Value: values[0].value}}
	for _, value := range values {
		if value.name != "text" {
			fields = append(fields, retrieval.Field{Name: value.name, Value: value.value})
		}
	}
	return fields
}

func relativeLexicalPath(value, documentsRoot string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	if normalized == "" {
		return "", false
	}
	valueKind := lexicalAbsoluteKind(normalized)
	if valueKind == "" {
		if len(normalized) >= 2 && normalized[1] == ':' {
			return "", false
		}
		return safeRelativeLexicalPath(normalized)
	}
	root := strings.ReplaceAll(strings.TrimSpace(documentsRoot), `\`, "/")
	if root == "" || lexicalAbsoluteKind(root) != valueKind {
		return "", false
	}
	cleanedValue := path.Clean(normalized)
	cleanedRoot := strings.TrimSuffix(path.Clean(root), "/")
	valueForCompare, rootForCompare := cleanedValue, cleanedRoot
	if valueKind == "windows-drive" || valueKind == "unc" {
		valueForCompare = strings.ToLower(valueForCompare)
		rootForCompare = strings.ToLower(rootForCompare)
	}
	prefix := rootForCompare + "/"
	if !strings.HasPrefix(valueForCompare, prefix) {
		return "", false
	}
	return safeRelativeLexicalPath(cleanedValue[len(cleanedRoot)+1:])
}

func lexicalAbsoluteKind(value string) string {
	if strings.HasPrefix(value, "//") {
		return "unc"
	}
	if len(value) >= 3 &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':' && value[2] == '/' {
		return "windows-drive"
	}
	if strings.HasPrefix(value, "/") {
		return "posix"
	}
	return ""
}

func safeRelativeLexicalPath(value string) (string, bool) {
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") ||
		strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}

func normalizeResults(points []qdrant.Point) []RetrievedItem {
	sort.SliceStable(points, func(i, j int) bool {
		if points[i].Score == points[j].Score {
			return points[i].ID < points[j].ID
		}
		return points[i].Score > points[j].Score
	})
	return itemsFromPoints(points)
}

func itemsFromPoints(points []qdrant.Point) []RetrievedItem {
	items := make([]RetrievedItem, len(points))
	for i, point := range points {
		text, _ := point.Payload["text"].(string)
		items[i] = RetrievedItem{
			ID: point.ID, Score: point.Score, MissingText: strings.TrimSpace(text) == "",
		}
	}
	return items
}

func normalizeFactResults(points []qdrant.Point, now time.Time) []RetrievedItem {
	byID := make(map[string]qdrant.Point, len(points))
	candidates := make([]lifecycle.Candidate, 0, len(points))
	for _, point := range points {
		view, _ := lifecycle.Parse(point.Payload, point.ID)
		if !lifecycle.IsCurrentTruth(view, factExpiredAt(point.Payload, now)) {
			continue
		}
		byID[point.ID] = point
		candidates = append(candidates, lifecycle.Candidate{PointID: point.ID, Score: point.Score, View: view})
	}
	lifecycle.SortCandidates(candidates)
	sorted := make([]qdrant.Point, 0, len(candidates))
	for _, candidate := range candidates {
		sorted = append(sorted, byID[candidate.PointID])
	}
	return itemsFromPoints(sorted)
}

func factExpiredAt(payload map[string]any, now time.Time) bool {
	raw, exists := payload["valid_until"]
	if !exists || raw == nil {
		return false
	}
	value, ok := raw.(string)
	if !ok {
		return true
	}
	expiry, err := parseUTCCalendarDate(value)
	if err != nil {
		return true
	}
	utc := now.UTC()
	referenceDate := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return referenceDate.After(expiry)
}

// parseUTCCalendarDate keeps eval's expiry semantics aligned with the runtime
// without importing internal/memory (which would create a package cycle).
func parseUTCCalendarDate(value string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || len(value) != len("2006-01-02") || parsed.Format("2006-01-02") != value {
		return time.Time{}, fmt.Errorf("must use exact YYYY-MM-DD format")
	}
	return parsed.UTC(), nil
}

func randomSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate temporary collection suffix: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

type timingCollector struct {
	now    func() time.Time
	embed  []int64
	total  []int64
	search []int64
}

type timedMaterializationEmbedder struct {
	delegate PurposeEmbedder
	now      func() time.Time
	corpusUS int64
}

func (embedder *timedMaterializationEmbedder) EmbedWithPurpose(
	ctx context.Context,
	text string,
	purpose embeddings.Purpose,
	profile embeddings.InputProfile,
	modelID string,
) ([]float32, error) {
	return embedder.delegate.EmbedWithPurpose(ctx, text, purpose, profile, modelID)
}

func (embedder *timedMaterializationEmbedder) EmbedBatchWithPurpose(
	ctx context.Context,
	texts []string,
	purpose embeddings.Purpose,
	profile embeddings.InputProfile,
	modelID string,
) ([][]float32, error) {
	start := embedder.now()
	vectors, err := embedder.delegate.EmbedBatchWithPurpose(
		ctx, texts, purpose, profile, modelID,
	)
	duration := elapsedUS(start, embedder.now())
	embedder.corpusUS += duration
	return vectors, err
}

func newTimingCollector(now func() time.Time) *timingCollector {
	if now == nil {
		now = time.Now
	}
	return &timingCollector{now: now}
}

func elapsedUS(start, end time.Time) int64 {
	value := end.Sub(start).Microseconds()
	if value < 0 {
		return 0
	}
	return value
}

func summarizeDurations(values []int64) DurationSummary {
	if len(values) == 0 {
		return DurationSummary{}
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	percentile := func(p int) int64 {
		index := (p*len(sorted) + 99) / 100
		if index < 1 {
			index = 1
		}
		return sorted[index-1]
	}
	return DurationSummary{
		Count: len(sorted), Min: sorted[0], P50: percentile(50),
		P95: percentile(95), Max: sorted[len(sorted)-1],
	}
}

func (collector *timingCollector) diagnostics() *Diagnostics {
	return &Diagnostics{Query: QueryDiagnostics{
		Total: summarizeDurations(collector.total), Embed: summarizeDurations(collector.embed),
		Search: summarizeDurations(collector.search),
	}}
}

func cloneDataset(source *Dataset) (Dataset, error) {
	cloned := *source
	cloned.Configuration.TopK = append([]int(nil), source.Configuration.TopK...)
	cloned.Configuration.present = clonePresence(source.Configuration.present)
	cloned.Gates.MinimumHitAt = cloneStringMetricMap(source.Gates.MinimumHitAt)
	cloned.Gates.MinimumNDCGAt = cloneStringMetricMap(source.Gates.MinimumNDCGAt)
	if source.Gates.MinimumMRR != nil {
		minimumMRR := *source.Gates.MinimumMRR
		cloned.Gates.MinimumMRR = &minimumMRR
	}
	var err error
	cloned.Facts, err = cloneFixturePoints(source.Facts)
	if err != nil {
		return Dataset{}, fmt.Errorf("clone facts: %w", err)
	}
	cloned.Chunks, err = cloneFixturePoints(source.Chunks)
	if err != nil {
		return Dataset{}, fmt.Errorf("clone chunks: %w", err)
	}
	cloned.Folders, err = cloneFixturePoints(source.Folders)
	if err != nil {
		return Dataset{}, fmt.Errorf("clone folders: %w", err)
	}
	cloned.Queries = append([]Query(nil), source.Queries...)
	for i := range cloned.Queries {
		cloned.Queries[i].Vector = append(Vector(nil), source.Queries[i].Vector...)
		cloned.Queries[i].Expected = append([]ExpectedItem(nil), source.Queries[i].Expected...)
		cloned.Queries[i].ForbiddenIDs = append([]string(nil), source.Queries[i].ForbiddenIDs...)
		cloned.Queries[i].Cohorts = append([]QueryCohort(nil), source.Queries[i].Cohorts...)
		cloned.Queries[i].LifecycleExpectations =
			append([]LifecycleExpectation(nil), source.Queries[i].LifecycleExpectations...)
		for j := range cloned.Queries[i].LifecycleExpectations {
			cloned.Queries[i].LifecycleExpectations[j].ReasonCodes = append(
				[]string(nil), source.Queries[i].LifecycleExpectations[j].ReasonCodes...)
		}
	}
	cloned.TransitionScenarios = append(
		[]TransitionScenario(nil), source.TransitionScenarios...,
	)
	for i := range cloned.TransitionScenarios {
		cloned.TransitionScenarios[i].SourceLifecycle =
			cloneLifecyclePayload(source.TransitionScenarios[i].SourceLifecycle)
		cloned.TransitionScenarios[i].TargetLifecycle =
			cloneLifecyclePayload(source.TransitionScenarios[i].TargetLifecycle)
	}
	return cloned, nil
}

func cloneLifecyclePayload(source LifecyclePayload) LifecyclePayload {
	cloned := source
	cloned.Supersedes = cloneStrings(source.Supersedes)
	cloned.SupersededBy = cloneStrings(source.SupersededBy)
	cloned.present = clonePresence(source.present)
	if source.Provenance != nil {
		provenance := *source.Provenance
		cloned.Provenance = &provenance
	}
	return cloned
}

func cloneStrings(source []string) []string {
	if source == nil {
		return nil
	}
	return append([]string{}, source...)
}

func cloneStringMetricMap(source map[string]float64) map[string]float64 {
	if source == nil {
		return nil
	}
	cloned := make(map[string]float64, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func corpusText(payload map[string]any, group string) (string, bool) {
	value, ok := payload["text"].(string)
	if ok && strings.TrimSpace(value) != "" {
		return value, true
	}
	if group == "folders" {
		summary, ok := payload["summary"].(string)
		if ok && strings.TrimSpace(summary) != "" {
			return summary, true
		}
	}
	return "", false
}
