package memory

import (
	"fmt"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/memory/lifecycle"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

const lifecycleRecallCacheVersion = "lifecycle-recall-v2"

// RecallLifecycleMode controls which valid lifecycle states recall may inspect.
type RecallLifecycleMode string

const (
	RecallLifecycleCurrent    RecallLifecycleMode = "current"
	RecallLifecycleHistory    RecallLifecycleMode = "history"
	RecallLifecycleAsOf       RecallLifecycleMode = "as_of"
	RecallLifecycleIncludeAll RecallLifecycleMode = "include_all"
)

// LifecycleRecallOptions is the normalized runtime lifecycle contract for recall.
// AsOf is populated only for RecallLifecycleAsOf and always uses YYYY-MM-DD.
type LifecycleRecallOptions struct {
	Mode RecallLifecycleMode
	AsOf string
}

// LifecyclePresentationDecision is the stable MCP presentation action.
type LifecyclePresentationDecision = lifecycle.PresentationDecision

const (
	LifecycleDecisionInclude   = lifecycle.PresentationInclude
	LifecycleDecisionDemote    = lifecycle.PresentationDemote
	LifecycleDecisionUncertain = lifecycle.PresentationUncertain
)

// LifecycleReasonCode is the stable MCP presentation explanation.
type LifecycleReasonCode = lifecycle.PresentationReasonCode

const (
	LifecycleReasonCurrentTruth        = lifecycle.PresentationReasonCurrentTruth
	LifecycleReasonCanonicalPreference = lifecycle.PresentationReasonCanonicalPreference
	LifecycleReasonCurrentContext      = lifecycle.PresentationReasonCurrentContext
	LifecycleReasonHistoricalContext   = lifecycle.PresentationReasonHistoricalContext
	LifecycleReasonSupersededContext   = lifecycle.PresentationReasonSupersededContext
	LifecycleReasonDisputed            = lifecycle.PresentationReasonDisputed
)

type lifecycleRecallCandidate struct {
	point        qdrant.Point
	view         lifecycle.View
	Decision     LifecyclePresentationDecision
	ReasonCodes  []LifecycleReasonCode
	SemanticRank int
	FinalRank    int
}

func parseLifecycleRecallOptions(args map[string]interface{}) (LifecycleRecallOptions, error) {
	mode := RecallLifecycleCurrent
	if raw, exists := args["lifecycle_mode"]; exists {
		value, ok := raw.(string)
		if !ok {
			return LifecycleRecallOptions{}, fmt.Errorf("lifecycle_mode must be a string")
		}
		mode = RecallLifecycleMode(value)
	}
	if !mode.valid() {
		return LifecycleRecallOptions{}, fmt.Errorf("lifecycle_mode must be current, history, as_of, or include_all")
	}

	rawAsOf, hasAsOf := args["as_of"]
	if mode != RecallLifecycleAsOf {
		if hasAsOf {
			return LifecycleRecallOptions{}, fmt.Errorf("as_of is only valid when lifecycle_mode is as_of")
		}
		return LifecycleRecallOptions{Mode: mode}, nil
	}
	if !hasAsOf {
		return LifecycleRecallOptions{}, fmt.Errorf("as_of is required when lifecycle_mode is as_of")
	}
	asOf, ok := rawAsOf.(string)
	if !ok {
		return LifecycleRecallOptions{}, fmt.Errorf("as_of must be a string")
	}
	parsed, err := parseUTCCalendarDate(asOf)
	if err != nil {
		return LifecycleRecallOptions{}, fmt.Errorf("as_of must use exact YYYY-MM-DD format")
	}
	return LifecycleRecallOptions{Mode: mode, AsOf: parsed.Format("2006-01-02")}, nil
}

func (mode RecallLifecycleMode) valid() bool {
	switch mode {
	case RecallLifecycleCurrent, RecallLifecycleHistory, RecallLifecycleAsOf, RecallLifecycleIncludeAll:
		return true
	default:
		return false
	}
}

func (options LifecycleRecallOptions) normalizedMode() RecallLifecycleMode {
	if options.Mode == "" {
		return RecallLifecycleCurrent
	}
	return options.Mode
}

func lifecycleRecallCacheIdentity(options LifecycleRecallOptions) string {
	mode := options.normalizedMode()
	asOf := "-"
	if mode == RecallLifecycleAsOf {
		asOf = options.AsOf
	}
	return fmt.Sprintf("%s|mode=%s|as_of=%s", lifecycleRecallCacheVersion, mode, asOf)
}

func lifecycleRecallFilters(base map[string]interface{}, mode RecallLifecycleMode) map[string]interface{} {
	filters := make(map[string]interface{}, len(base))
	for key, value := range base {
		filters[key] = cloneLifecycleRecallFilterValue(value)
	}
	if mode == "" || mode == RecallLifecycleCurrent {
		return activeMemoryFilters(currentLifecycleFilters(filters))
	}
	return activeMemoryFilters(filters)
}

func cloneLifecycleRecallFilterValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		cloned := make(map[string]interface{}, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneLifecycleRecallFilterValue(nested)
		}
		return cloned
	case []map[string]interface{}:
		cloned := make([]map[string]interface{}, len(typed))
		for index, nested := range typed {
			cloned[index] = cloneLifecycleRecallFilterValue(nested).(map[string]interface{})
		}
		return cloned
	case []interface{}:
		cloned := make([]interface{}, len(typed))
		for index, nested := range typed {
			cloned[index] = cloneLifecycleRecallFilterValue(nested)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func presentLifecycleRecallCandidates(points []qdrant.Point, options LifecycleRecallOptions, now time.Time) []lifecycleRecallCandidate {
	mode := options.normalizedMode()
	reference := now.UTC()
	if mode == RecallLifecycleAsOf {
		if parsed, err := parseUTCCalendarDate(options.AsOf); err == nil {
			reference = parsed
		}
	}

	byID := make(map[string]lifecycleRecallCandidate, len(points))
	ordered := make([]lifecycle.Candidate, 0, len(points))
	seenIDs := make(map[string]struct{}, len(points))
	hasCanonicalCurrent := false
	for index, point := range points {
		// Qdrant IDs are unique by contract. If a malformed response repeats one,
		// retain only its first (highest semantic rank) occurrence.
		if _, duplicate := seenIDs[point.ID]; duplicate {
			continue
		}
		seenIDs[point.ID] = struct{}{}
		view := lifecycleView(point.ID, point.Payload)
		if !view.Valid || factExpiredAt(point.Payload, reference) || !lifecycleVisibleInRecallMode(view.State, mode) {
			continue
		}
		candidate := lifecycleRecallCandidate{
			point:        point,
			view:         view,
			SemanticRank: index + 1,
		}
		byID[point.ID] = candidate
		ordered = append(ordered, lifecycle.Candidate{PointID: point.ID, Score: point.Score, View: view})
		if view.State == lifecycle.Current && view.Canonical {
			hasCanonicalCurrent = true
		}
	}

	lifecycle.SortCandidates(ordered)
	result := make([]lifecycleRecallCandidate, 0, len(ordered))
	for _, orderedCandidate := range ordered {
		candidate := byID[orderedCandidate.PointID]
		candidate.Decision, candidate.ReasonCodes = lifecycleRecallDecision(mode, candidate.view, hasCanonicalCurrent)
		candidate.FinalRank = len(result) + 1
		result = append(result, candidate)
	}
	return result
}

func lifecycleVisibleInRecallMode(state lifecycle.State, mode RecallLifecycleMode) bool {
	if mode == RecallLifecycleCurrent {
		return state == lifecycle.Current
	}
	switch mode {
	case RecallLifecycleHistory, RecallLifecycleAsOf, RecallLifecycleIncludeAll:
		return state.Valid()
	default:
		return false
	}
}

func lifecycleRecallDecision(mode RecallLifecycleMode, view lifecycle.View, hasCanonicalCurrent bool) (LifecyclePresentationDecision, []LifecycleReasonCode) {
	intent := lifecycle.PresentationIntentBroad
	if mode == RecallLifecycleCurrent {
		intent = lifecycle.PresentationIntentCurrent
	}
	result := lifecycle.DecidePresentation(intent, view, false, hasCanonicalCurrent)
	return result.Decision, result.ReasonCodes
}

func factExpiredAt(payload map[string]interface{}, reference time.Time) bool {
	expiry, present, err := validUntilPayload(payload)
	if !present {
		return false
	}
	if err != nil {
		// A malformed explicit expiry is not safe current context. It remains
		// inspectable through inventory surfaces, where its malformed payload is
		// visible, but no recall mode presents it as valid.
		return true
	}
	utc := reference.UTC()
	referenceDate := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return referenceDate.After(expiry)
}
