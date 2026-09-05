package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/retrieval"
	"github.com/google/uuid"
)

const maxDatasetBytes int64 = 32 << 20

// Load decodes and source-neutrally validates a bounded dataset document.
func Load(reader io.Reader) (*Dataset, error) {
	data, err := readDataset(reader)
	if err != nil {
		return nil, err
	}
	if err := validateV3DatasetRaw(data); err != nil {
		return nil, err
	}
	dataset, err := decodeDataset(data)
	if err != nil {
		return nil, err
	}
	if err := dataset.Validate(); err != nil {
		return nil, err
	}
	return dataset, nil
}

// LoadForMaterialization decodes a strict schema-v3 dataset whose corpus
// vectors may be omitted or empty. Query vectors remain required even though
// Materialize replaces every corpus and query vector.
func LoadForMaterialization(reader io.Reader) (*Dataset, error) {
	data, err := readDataset(reader)
	if err != nil {
		return nil, err
	}
	if err := validateMaterializationDatasetRaw(data); err != nil {
		return nil, err
	}
	dataset, err := decodeDataset(data)
	if err != nil {
		return nil, err
	}
	if err := dataset.ValidateForMaterialization(); err != nil {
		return nil, err
	}
	return dataset, nil
}

func readDataset(reader io.Reader) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: maxDatasetBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}
	if int64(len(data)) > maxDatasetBytes {
		return nil, fmt.Errorf("fixture exceeds %d bytes", maxDatasetBytes)
	}
	return data, nil
}

func decodeDataset(data []byte) (*Dataset, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var dataset Dataset
	if err := decoder.Decode(&dataset); err != nil {
		return nil, fmt.Errorf("decode fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("fixture contains trailing JSON")
		}
		return nil, fmt.Errorf("decode trailing JSON: %w", err)
	}
	return &dataset, nil
}

// Validate checks schema, vectors, IDs, queries, metrics, and gates without
// requiring live query IDs to exist in fixture point arrays.
func (d *Dataset) Validate() error {
	return d.validate(false, false)
}

func (d *Dataset) validate(allowEmptyCorpusVectors, requireQueryVectors bool) error {
	if d.SchemaVersion != SchemaVersion &&
		d.SchemaVersion != LifecycleSchemaVersion &&
		d.SchemaVersion != CurrentDatasetSchemaVersion &&
		d.SchemaVersion != DocumentRoutingSchemaVersion {
		return fmt.Errorf("schema_version must be %d, %d, %d, or %d",
			SchemaVersion, LifecycleSchemaVersion, CurrentDatasetSchemaVersion, DocumentRoutingSchemaVersion)
	}
	if strings.TrimSpace(d.DatasetVersion) == "" {
		return fmt.Errorf("dataset_version is required")
	}
	identity := d.Embedding
	if err := validateEmbeddingIdentity(identity, d.SchemaVersion >= CurrentDatasetSchemaVersion); err != nil {
		return err
	}
	if d.SchemaVersion < CurrentDatasetSchemaVersion && identity.inputProfilePresent {
		return fmt.Errorf("input_profile requires schema_version %d", CurrentDatasetSchemaVersion)
	}
	cfg := &d.Configuration
	if err := validateBaseConfiguration(*cfg); err != nil {
		return err
	}
	if err := normalizeTopK(&cfg.TopK); err != nil {
		return err
	}
	if err := validateRetrievalConfiguration(*cfg, d.SchemaVersion >= CurrentDatasetSchemaVersion); err != nil {
		return err
	}
	if d.SchemaVersion < CurrentDatasetSchemaVersion &&
		(cfg.present["retrieval_strategy"] ||
			cfg.present["dense_candidate_limit"] ||
			cfg.present["rrf_constant"]) {
		return fmt.Errorf("retrieval strategy fields require schema_version %d", CurrentDatasetSchemaVersion)
	}
	if d.SchemaVersion >= CurrentDatasetSchemaVersion && len(d.Queries) == 0 {
		return fmt.Errorf("schema_version %d dataset requires at least one query",
			CurrentDatasetSchemaVersion)
	}
	if d.SchemaVersion == DocumentRoutingSchemaVersion {
		if err := validateDocumentRoutingConfiguration(*cfg); err != nil {
			return err
		}
	} else if cfg.present["document_routing_strategy"] || cfg.present["routing_candidate_limit"] ||
		cfg.present["routing_rrf_constant"] || cfg.present["reranker_model_id"] ||
		cfg.present["reranker_candidate_cap"] || cfg.present["reranker_timeout_ms"] {
		return fmt.Errorf("document routing fields require schema_version %d", DocumentRoutingSchemaVersion)
	}

	for name, points := range map[string][]FixturePoint{
		"facts": d.Facts, "chunks": d.Chunks, "folders": d.Folders,
	} {
		ids := make(map[string]struct{}, len(points))
		for i, point := range points {
			if point.ID.String() == "" {
				return fmt.Errorf("%s point %d has empty ID", name, i)
			}
			if _, duplicate := ids[point.ID.String()]; duplicate {
				return fmt.Errorf("duplicate %s point ID %q", name, point.ID.String())
			}
			ids[point.ID.String()] = struct{}{}
			if allowEmptyCorpusVectors && len(point.Vector) == 0 {
				continue
			}
			if err := validateVector(point.Vector, identity.VectorSize); err != nil {
				return fmt.Errorf("%s point %q: %w", name, point.ID.String(), err)
			}
		}
	}

	queryIDs := make(map[string]struct{}, len(d.Queries))
	for i := range d.Queries {
		query := &d.Queries[i]
		if strings.TrimSpace(query.ID) == "" {
			return fmt.Errorf("query ID is required")
		}
		if d.SchemaVersion >= LifecycleSchemaVersion && !safeReportIdentifier(query.ID) {
			return fmt.Errorf("query ID must use safe identifier characters")
		}
		if _, duplicate := queryIDs[query.ID]; duplicate {
			return fmt.Errorf("duplicate query ID %q", query.ID)
		}
		queryIDs[query.ID] = struct{}{}
		if query.Target != "facts" && query.Target != "documents" {
			return fmt.Errorf("query %q target must be facts or documents", query.ID)
		}
		if query.Mode != "flat" && query.Mode != "hierarchical" {
			return fmt.Errorf("query %q mode must be flat or hierarchical", query.ID)
		}
		if query.Target == "facts" && query.Mode != "flat" {
			return fmt.Errorf("query %q facts target supports only flat mode", query.ID)
		}
		if d.SchemaVersion == SchemaVersion &&
			(query.intentPresent ||
				query.Intent != "" ||
				query.asOfPresent || query.AsOf != "" ||
				query.lifecycleExpectationsPresent || len(query.LifecycleExpectations) != 0) {
			return fmt.Errorf("query %q lifecycle fields require schema_version %d", query.ID, LifecycleSchemaVersion)
		}
		if d.SchemaVersion < CurrentDatasetSchemaVersion &&
			(query.cohortsPresent || query.Cohorts != nil) {
			return fmt.Errorf("query %q cohorts require schema_version %d", query.ID, CurrentDatasetSchemaVersion)
		}
		if d.SchemaVersion >= CurrentDatasetSchemaVersion {
			if err := validateQueryCohorts(query.ID, query.Cohorts, query.cohortsPresent); err != nil {
				return err
			}
		}
		intent := query.Intent
		if !query.intentPresent {
			intent = query.EffectiveIntent()
		}
		if !intent.valid() {
			return fmt.Errorf("query %q intent must be current, history, as_of, or uncertainty", query.ID)
		}
		if intent == QueryIntentAsOf {
			if !validISODate(query.AsOf) {
				return fmt.Errorf("query %q as_of intent requires an ISO YYYY-MM-DD date", query.ID)
			}
		} else if query.asOfPresent || query.AsOf != "" {
			return fmt.Errorf("query %q as_of is only valid for as_of intent", query.ID)
		}
		if query.Target == "documents" {
			if intent != QueryIntentCurrent {
				return fmt.Errorf("query %q document queries support only current intent", query.ID)
			}
			if len(query.LifecycleExpectations) != 0 {
				return fmt.Errorf("query %q document queries do not support lifecycle expectations", query.ID)
			}
		}
		if strings.TrimSpace(query.Text) == "" {
			return fmt.Errorf("query %q text is required", query.ID)
		}
		if requireQueryVectors && len(query.Vector) == 0 {
			return fmt.Errorf("query %q vector must not be empty", query.ID)
		}
		if len(query.Vector) > 0 {
			if err := validateVector(query.Vector, identity.VectorSize); err != nil {
				return fmt.Errorf("query %q: %w", query.ID, err)
			}
		}
		if len(query.Expected) == 0 {
			return fmt.Errorf("query %q expected items are required", query.ID)
		}
		expectedIDs := make(map[string]struct{}, len(query.Expected))
		for _, expected := range query.Expected {
			if expected.Grade < 1 || expected.Grade > 3 {
				return fmt.Errorf("query %q expected grade for %q must be 1..3", query.ID, expected.ID)
			}
			if err := validateNormalizedPointID(expected.ID); err != nil {
				return fmt.Errorf("query %q expected ID %q: %w", query.ID, expected.ID, err)
			}
			if _, duplicate := expectedIDs[expected.ID]; duplicate {
				return fmt.Errorf("query %q has duplicate expected ID %q", query.ID, expected.ID)
			}
			expectedIDs[expected.ID] = struct{}{}
		}
		forbiddenIDs := make(map[string]struct{}, len(query.ForbiddenIDs))
		for _, forbiddenID := range query.ForbiddenIDs {
			if err := validateNormalizedPointID(forbiddenID); err != nil {
				return fmt.Errorf("query %q forbidden ID %q: %w", query.ID, forbiddenID, err)
			}
			if _, duplicate := forbiddenIDs[forbiddenID]; duplicate {
				return fmt.Errorf("query %q has duplicate forbidden ID %q", query.ID, forbiddenID)
			}
			if _, expected := expectedIDs[forbiddenID]; expected {
				return fmt.Errorf("query %q ID %q is both expected and forbidden", query.ID, forbiddenID)
			}
			forbiddenIDs[forbiddenID] = struct{}{}
		}
		expectationIDs := make(map[string]struct{}, len(query.LifecycleExpectations))
		for _, expectation := range query.LifecycleExpectations {
			if err := validateNormalizedPointID(expectation.ID); err != nil {
				return fmt.Errorf("query %q lifecycle expectation ID %q: %w", query.ID, expectation.ID, err)
			}
			if _, duplicate := expectationIDs[expectation.ID]; duplicate {
				return fmt.Errorf("query %q has duplicate lifecycle expectation ID %q", query.ID, expectation.ID)
			}
			expectationIDs[expectation.ID] = struct{}{}
			if expectation.State != "" && !expectation.State.Valid() {
				return fmt.Errorf("query %q lifecycle expectation state for %q is invalid", query.ID, expectation.ID)
			}
			if expectation.statePresent && expectation.State == "" {
				return fmt.Errorf("query %q lifecycle expectation state for %q must not be empty", query.ID, expectation.ID)
			}
			if !expectation.Decision.valid() {
				return fmt.Errorf("query %q lifecycle expectation decision for %q must be include, suppress, demote, or uncertain", query.ID, expectation.ID)
			}
			reasonCodes := make(map[string]struct{}, len(expectation.ReasonCodes))
			for _, reasonCode := range expectation.ReasonCodes {
				if strings.TrimSpace(reasonCode) == "" || reasonCode != strings.TrimSpace(reasonCode) {
					return fmt.Errorf("query %q lifecycle expectation for %q contains an empty or non-normalized reason code", query.ID, expectation.ID)
				}
				if !LifecycleReasonCode(reasonCode).validPresentation() {
					return fmt.Errorf("query %q lifecycle expectation for %q contains unknown reason code %q", query.ID, expectation.ID, reasonCode)
				}
				if _, duplicate := reasonCodes[reasonCode]; duplicate {
					return fmt.Errorf("query %q lifecycle expectation for %q contains duplicate reason code %q", query.ID, expectation.ID, reasonCode)
				}
				reasonCodes[reasonCode] = struct{}{}
			}
		}
	}
	transitionIDs := make(map[string]struct{}, len(d.TransitionScenarios))
	for i := range d.TransitionScenarios {
		scenario := &d.TransitionScenarios[i]
		if strings.TrimSpace(scenario.ID) == "" || scenario.ID != strings.TrimSpace(scenario.ID) {
			return fmt.Errorf("transition scenario ID must be a non-empty normalized string")
		}
		if !safeReportIdentifier(scenario.ID) {
			return fmt.Errorf("transition scenario ID must use safe identifier characters")
		}
		if _, duplicate := transitionIDs[scenario.ID]; duplicate {
			return fmt.Errorf("duplicate transition scenario ID %q", scenario.ID)
		}
		transitionIDs[scenario.ID] = struct{}{}
		if scenario.PointID.String() == "" {
			return fmt.Errorf("transition scenario %q point_id is required", scenario.ID)
		}
		if !scenario.expectedValidPresent {
			return fmt.Errorf("transition scenario %q expected_valid is required", scenario.ID)
		}
		if scenario.reasonCodePresent && (strings.TrimSpace(scenario.ExpectedReasonCode) == "" ||
			scenario.ExpectedReasonCode != strings.TrimSpace(scenario.ExpectedReasonCode)) {
			return fmt.Errorf("transition scenario %q expected_reason_code must be non-empty and normalized", scenario.ID)
		}
		if scenario.reasonCodePresent &&
			!LifecycleReasonCode(scenario.ExpectedReasonCode).validTransition() {
			return fmt.Errorf("transition scenario %q expected_reason_code is unknown", scenario.ID)
		}
		if err := validateLifecyclePayload(scenario.SourceLifecycle, "source_lifecycle"); err != nil {
			return fmt.Errorf("transition scenario %q: %w", scenario.ID, err)
		}
		if err := validateLifecyclePayload(scenario.TargetLifecycle, "target_lifecycle"); err != nil {
			return fmt.Errorf("transition scenario %q: %w", scenario.ID, err)
		}
	}
	if d.SchemaVersion == SchemaVersion &&
		(d.transitionScenariosPresent || len(d.TransitionScenarios) != 0) {
		return fmt.Errorf("transition_scenarios require schema_version %d", LifecycleSchemaVersion)
	}
	if d.SchemaVersion == SchemaVersion &&
		(d.Gates.forbidLifecycleViolationsPresent || d.Gates.ForbidLifecycleViolations) {
		return fmt.Errorf("forbid_lifecycle_violations requires schema_version %d", LifecycleSchemaVersion)
	}
	if err := validateGateMap("minimum_hit_at", d.Gates.MinimumHitAt, cfg.TopK); err != nil {
		return err
	}
	if err := validateGateMap("minimum_ndcg_at", d.Gates.MinimumNDCGAt, cfg.TopK); err != nil {
		return err
	}
	if d.Gates.MinimumMRR != nil && (*d.Gates.MinimumMRR < 0 || *d.Gates.MinimumMRR > 1 || math.IsNaN(*d.Gates.MinimumMRR)) {
		return fmt.Errorf("minimum_mrr must be between 0 and 1")
	}
	return nil
}

func validateDocumentRoutingConfiguration(cfg Configuration) error {
	for _, field := range []string{"document_routing_strategy", "routing_candidate_limit", "routing_rrf_constant"} {
		if !cfg.present[field] {
			return fmt.Errorf("configuration field %s is required", field)
		}
	}
	switch cfg.DocumentRoutingStrategy {
	case DocumentRoutingHierarchical, DocumentRoutingFlat, DocumentRoutingBlendedRRF:
	default:
		return fmt.Errorf("document_routing_strategy must be hierarchical-only, flat-only, or blended-rrf")
	}
	const maxRoutingCandidatesPerSource = 100
	if cfg.RoutingCandidateLimit < 1 || cfg.RoutingCandidateLimit > maxRoutingCandidatesPerSource {
		return fmt.Errorf("routing_candidate_limit must be between 1 and %d", maxRoutingCandidatesPerSource)
	}
	if cfg.RoutingCandidateLimit < cfg.TopK[len(cfg.TopK)-1] {
		return fmt.Errorf("routing_candidate_limit must be at least max(top_k)")
	}
	if cfg.RoutingRRFConstant < 1 || cfg.RoutingRRFConstant > 1000 {
		return fmt.Errorf("routing_rrf_constant must be between 1 and 1000")
	}
	hasReranker := strings.TrimSpace(cfg.RerankerModelID) != ""
	if hasReranker != cfg.present["reranker_model_id"] {
		return fmt.Errorf("reranker_model_id must be a non-empty string when present")
	}
	if hasReranker {
		if cfg.RerankerCandidateCap < 1 || cfg.RerankerCandidateCap > 100 {
			return fmt.Errorf("reranker_candidate_cap must be between 1 and 100")
		}
		if cfg.RerankerTimeoutMS < 1 || cfg.RerankerTimeoutMS > 60000 {
			return fmt.Errorf("reranker_timeout_ms must be between 1 and 60000")
		}
	} else if cfg.present["reranker_candidate_cap"] || cfg.present["reranker_timeout_ms"] {
		return fmt.Errorf("reranker cap and timeout require reranker_model_id")
	}
	return nil
}

// ValidateForMaterialization retains the strict schema-v3 contract while
// permitting only corpus vectors that Materialize will replace to be empty.
func (d *Dataset) ValidateForMaterialization() error {
	if d == nil {
		return fmt.Errorf("dataset is required")
	}
	if d.SchemaVersion != CurrentDatasetSchemaVersion {
		return fmt.Errorf("materialization requires schema_version %d", CurrentDatasetSchemaVersion)
	}
	if err := d.validate(true, true); err != nil {
		return err
	}
	for _, group := range []struct {
		name   string
		points []FixturePoint
	}{
		{name: "facts", points: d.Facts},
		{name: "chunks", points: d.Chunks},
		{name: "folders", points: d.Folders},
	} {
		for _, point := range group.points {
			if _, ok := corpusText(point.Payload, group.name); !ok {
				return fmt.Errorf("%s point %q has no usable corpus text",
					group.name, point.ID.String())
			}
		}
	}
	return nil
}

func validateBaseConfiguration(configuration Configuration) error {
	if strings.TrimSpace(configuration.Name) == "" ||
		strings.TrimSpace(configuration.FactCollection) == "" ||
		strings.TrimSpace(configuration.ChunkCollection) == "" ||
		strings.TrimSpace(configuration.FolderCollection) == "" {
		return fmt.Errorf("configuration name and logical collection names are required")
	}
	if configuration.FolderTopK < 1 ||
		math.IsNaN(configuration.FolderThreshold) ||
		math.IsInf(configuration.FolderThreshold, 0) {
		return fmt.Errorf("folder_top_k must be positive and folder_threshold must be finite")
	}
	return nil
}

func validateEmbeddingIdentity(identity EmbeddingIdentity, requireProfile bool) error {
	if strings.TrimSpace(identity.Provider) == "" || strings.TrimSpace(identity.ModelID) == "" ||
		strings.TrimSpace(identity.ModelRevision) == "" || strings.TrimSpace(identity.DType) == "" ||
		strings.TrimSpace(identity.Pooling) == "" || identity.VectorSize < 1 {
		return fmt.Errorf("complete embedding identity with positive vector_size is required")
	}
	if !requireProfile {
		return nil
	}
	if !identity.inputProfilePresent && identity.InputProfile == "" {
		return fmt.Errorf("embedding input_profile is required")
	}
	switch identity.InputProfile {
	case LegacyRawV1:
		return nil
	case MultilingualE5V1:
		if strings.TrimSpace(identity.ModelID) != multilingualE5SmallModelID {
			return fmt.Errorf("embedding input profile %q does not support model %q",
				identity.InputProfile, identity.ModelID)
		}
		return nil
	default:
		return fmt.Errorf("unknown embedding input profile %q", identity.InputProfile)
	}
}

func validateRetrievalConfiguration(configuration Configuration, requireStrategy bool) error {
	if !requireStrategy {
		return nil
	}
	for _, field := range []string{"retrieval_strategy", "dense_candidate_limit", "rrf_constant"} {
		if configuration.present != nil && !configuration.present[field] {
			return fmt.Errorf("configuration field %s is required", field)
		}
	}
	switch configuration.RetrievalStrategy {
	case RetrievalVectorOnly:
		// Vector-only is canonicalized with explicit zero values so inactive
		// hybrid tuning cannot silently participate in report identity.
		if configuration.DenseCandidateLimit != 0 || configuration.RRFConstant != 0 {
			return fmt.Errorf("vector-only requires dense_candidate_limit=0 and rrf_constant=0")
		}
	case RetrievalHybridRRF:
		maxK := configuration.TopK[len(configuration.TopK)-1]
		if configuration.DenseCandidateLimit < maxK {
			return fmt.Errorf("hybrid-rrf dense_candidate_limit must be at least max(top_k)")
		}
		if configuration.DenseCandidateLimit > retrieval.MaxCandidates {
			return fmt.Errorf("hybrid-rrf dense_candidate_limit must be at most %d", retrieval.MaxCandidates)
		}
		if configuration.RRFConstant < 1 {
			return fmt.Errorf("hybrid-rrf rrf_constant must be positive")
		}
		if configuration.RRFConstant > retrieval.MaxRRFConstant {
			return fmt.Errorf("hybrid-rrf rrf_constant must be at most %d", retrieval.MaxRRFConstant)
		}
	default:
		return fmt.Errorf("retrieval_strategy must be vector-only or hybrid-rrf")
	}
	return nil
}

func validateQueryCohorts(queryID string, cohorts []QueryCohort, present bool) error {
	if !present && cohorts == nil {
		return fmt.Errorf("query %q cohorts are required", queryID)
	}
	if len(cohorts) == 0 {
		return fmt.Errorf("query %q cohorts must be a non-empty array", queryID)
	}
	var previous QueryCohort
	for i, cohort := range cohorts {
		value := string(cohort)
		if !safeCohortIdentifier(value) {
			return fmt.Errorf("query %q cohort must use safe identifier characters", queryID)
		}
		if i > 0 {
			if cohort == previous {
				return fmt.Errorf("query %q contains duplicate cohort %q", queryID, cohort)
			}
			if cohort < previous {
				return fmt.Errorf("query %q cohorts must be sorted", queryID)
			}
		}
		previous = cohort
	}
	return nil
}

func safeCohortIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return false
	}
	return true
}

// ValidateForSource adds execution-mode constraints to the source-neutral
// validation performed while loading a dataset.
func (d *Dataset) ValidateForSource(source string) error {
	if err := d.Validate(); err != nil {
		return err
	}
	switch source {
	case "live":
		return nil
	case "tei-fixture":
		if d.SchemaVersion != CurrentDatasetSchemaVersion {
			return fmt.Errorf("tei-fixture requires schema_version %d", CurrentDatasetSchemaVersion)
		}
	case "fixture":
	default:
		return fmt.Errorf("source must be fixture, live, or tei-fixture")
	}

	sets := map[string]map[string]struct{}{
		"facts":   pointIDSet(d.Facts),
		"chunks":  pointIDSet(d.Chunks),
		"folders": pointIDSet(d.Folders),
	}
	for _, query := range d.Queries {
		if source == "fixture" && len(query.Vector) == 0 {
			return fmt.Errorf("fixture query %q must include a precomputed vector", query.ID)
		}
		targetSet := sets["facts"]
		if query.Target == "documents" {
			targetSet = sets["chunks"]
		}
		for _, expected := range query.Expected {
			if _, exists := targetSet[expected.ID]; !exists {
				return fmt.Errorf("query %q references unknown expected ID %q", query.ID, expected.ID)
			}
		}
		for _, forbiddenID := range query.ForbiddenIDs {
			if _, exists := targetSet[forbiddenID]; !exists {
				return fmt.Errorf("query %q references unknown forbidden ID %q", query.ID, forbiddenID)
			}
		}
		for _, expectation := range query.LifecycleExpectations {
			if _, exists := targetSet[expectation.ID]; !exists {
				return fmt.Errorf("query %q references unknown lifecycle expectation ID %q", query.ID, expectation.ID)
			}
		}
	}
	factIDs := sets["facts"]
	for _, scenario := range d.TransitionScenarios {
		if _, exists := factIDs[scenario.PointID.String()]; !exists {
			return fmt.Errorf("transition scenario %q references unknown point ID %q", scenario.ID, scenario.PointID.String())
		}
	}
	return nil
}

func validISODate(value string) bool {
	_, err := parseUTCCalendarDate(value)
	return err == nil
}

func validateLifecyclePayload(payload LifecyclePayload, field string) error {
	for _, required := range []string{"state", "canonical", "supersedes", "superseded_by"} {
		if !payload.present[required] {
			return fmt.Errorf("%s.%s is required", field, required)
		}
	}
	if !payload.State.Valid() {
		return fmt.Errorf("%s.state must be current, historical, superseded, or disputed", field)
	}
	if payload.Supersedes == nil {
		return fmt.Errorf("%s.supersedes must be an array", field)
	}
	if payload.SupersededBy == nil {
		return fmt.Errorf("%s.superseded_by must be an array", field)
	}
	if payload.Provenance != nil && strings.TrimSpace(payload.Provenance.Source) == "" {
		return fmt.Errorf("%s.provenance.source must be a non-empty string", field)
	}
	if payload.VerifiedAt != "" {
		if _, err := time.Parse(time.RFC3339, payload.VerifiedAt); err != nil {
			return fmt.Errorf("%s.verified_at must use RFC3339 format", field)
		}
	}
	for relationshipField, ids := range map[string][]string{
		"supersedes": payload.Supersedes, "superseded_by": payload.SupersededBy,
	} {
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if err := validateNormalizedPointID(id); err != nil {
				return fmt.Errorf("%s.%s ID %q: %w", field, relationshipField, id, err)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("%s.%s contains duplicate point ID %q", field, relationshipField, id)
			}
			seen[id] = struct{}{}
		}
	}
	return nil
}

func pointIDSet(points []FixturePoint) map[string]struct{} {
	ids := make(map[string]struct{}, len(points))
	for _, point := range points {
		ids[point.ID.String()] = struct{}{}
	}
	return ids
}

func validateNormalizedPointID(id string) error {
	if _, err := strconv.ParseUint(id, 10, 64); err == nil {
		return nil
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("must be a Qdrant point ID (unsigned integer or UUID)")
	}
	if id != parsed.String() {
		return fmt.Errorf("must use canonical lowercase UUID format")
	}
	return nil
}

func validateVector(vector []float32, size int) error {
	if len(vector) != size {
		return fmt.Errorf("vector length is %d, want %d", len(vector), size)
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("vector values must be finite")
		}
	}
	return nil
}

func normalizeTopK(values *[]int) error {
	if len(*values) == 0 {
		return fmt.Errorf("top_k must not be empty")
	}
	sort.Ints(*values)
	normalized := (*values)[:0]
	last := 0
	for _, value := range *values {
		if value < 1 || value > 100 {
			return fmt.Errorf("top_k values must be between 1 and 100")
		}
		if value != last {
			normalized = append(normalized, value)
			last = value
		}
	}
	*values = normalized
	return nil
}

func validateGateMap(name string, values map[string]float64, topK []int) error {
	allowed := make(map[int]struct{}, len(topK))
	for _, k := range topK {
		allowed[k] = struct{}{}
	}
	for rawK, value := range values {
		k, err := strconv.Atoi(rawK)
		if err != nil {
			return fmt.Errorf("%s key %q must be an integer", name, rawK)
		}
		if _, exists := allowed[k]; !exists {
			return fmt.Errorf("%s key %d is not present in top_k", name, k)
		}
		if value < 0 || value > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s[%d] must be between 0 and 1", name, k)
		}
	}
	return nil
}
