package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/memory/lifecycle"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

// LifecycleReasonCode is a stable, non-sensitive explanation for a
// presentation or transition decision.
type LifecycleReasonCode string

const (
	ReasonCurrentTruth        LifecycleReasonCode = "current_truth"
	ReasonCanonicalPreference LifecycleReasonCode = "canonical_preference"
	ReasonCurrentContext      LifecycleReasonCode = "current_context"
	ReasonHistorical          LifecycleReasonCode = "historical"
	ReasonHistoricalContext   LifecycleReasonCode = "historical_context"
	ReasonSuperseded          LifecycleReasonCode = "superseded"
	ReasonSupersededContext   LifecycleReasonCode = "superseded_context"
	ReasonDisputed            LifecycleReasonCode = "disputed"
	ReasonInvalidLifecycle    LifecycleReasonCode = "invalid_lifecycle"
	ReasonExpired             LifecycleReasonCode = "expired"

	ReasonTransitionValid   LifecycleReasonCode = "transition_valid"
	ReasonSourceInvalid     LifecycleReasonCode = "source_invalid"
	ReasonTargetInvalid     LifecycleReasonCode = "target_invalid"
	ReasonTransitionInvalid LifecycleReasonCode = "transition_invalid"
)

// LifecycleCandidateReport contains presentation evidence without fact text.
type LifecycleCandidateReport struct {
	ID          string                `json:"id"`
	State       lifecycle.State       `json:"state,omitempty"`
	Canonical   bool                  `json:"canonical"`
	Expired     bool                  `json:"expired"`
	Decision    PresentationDecision  `json:"decision"`
	ReasonCodes []LifecycleReasonCode `json:"reason_codes"`
	Valid       bool                  `json:"valid"`
	present     map[string]bool
}

// QueryLifecycleReport scores declared lifecycle expectations independently
// from semantic relevance metrics. Reason-code expectations use exact matching.
type QueryLifecycleReport struct {
	Intent     QueryIntent                `json:"intent"`
	AsOf       string                     `json:"as_of,omitempty"`
	Candidates []LifecycleCandidateReport `json:"candidates"`
	Checks     int                        `json:"checks"`
	Violations []LifecycleViolation       `json:"violations"`
	present    map[string]bool
}

// LifecycleViolationScope identifies the lifecycle contract being checked.
type LifecycleViolationScope string

const (
	ViolationScopeQuery      LifecycleViolationScope = "query"
	ViolationScopeTransition LifecycleViolationScope = "transition"
)

// LifecycleInvariant is a closed set of safe invariant identifiers.
type LifecycleInvariant string

const (
	InvariantCandidatePresent LifecycleInvariant = "candidate_present"
	InvariantState            LifecycleInvariant = "state"
	InvariantDecision         LifecycleInvariant = "decision"
	InvariantReasonCodes      LifecycleInvariant = "reason_codes"
	InvariantTransitionValid  LifecycleInvariant = "valid"
	InvariantTransitionReason LifecycleInvariant = "reason_code"
)

// LifecycleViolation contains identifiers and enum values only. It never
// carries fact text, query text, or free-form error messages.
type LifecycleViolation struct {
	Scope       LifecycleViolationScope `json:"scope"`
	QueryID     string                  `json:"query_id,omitempty"`
	ScenarioID  string                  `json:"scenario_id,omitempty"`
	CandidateID string                  `json:"candidate_id,omitempty"`
	Invariant   LifecycleInvariant      `json:"invariant"`
	present     map[string]bool
}

func (violation LifecycleViolation) message() string {
	if violation.Scope == ViolationScopeTransition {
		return fmt.Sprintf("scenario %s invariant %s", violation.ScenarioID, violation.Invariant)
	}
	return fmt.Sprintf("query %s candidate %s invariant %s",
		violation.QueryID, violation.CandidateID, violation.Invariant)
}

func (violation LifecycleViolation) key() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s",
		violation.Scope, violation.QueryID, violation.ScenarioID,
		violation.CandidateID, violation.Invariant)
}

// LifecycleAggregateMetrics does not participate in MRR or nDCG aggregation.
type LifecycleAggregateMetrics struct {
	Checks                        int `json:"checks"`
	Violations                    int `json:"violations"`
	CanonicalPreferenceChecks     int `json:"canonical_preference_checks"`
	CanonicalPreferenceViolations int `json:"canonical_preference_violations"`
	present                       map[string]bool
}

// TransitionReport records read-only validation of one declared transition.
type TransitionReport struct {
	ID         string               `json:"id"`
	Valid      bool                 `json:"valid"`
	ReasonCode LifecycleReasonCode  `json:"reason_code"`
	Passed     bool                 `json:"passed"`
	Violations []LifecycleViolation `json:"violations"`
	present    map[string]bool
}

// LifecycleReport is the dedicated schema-v2 lifecycle report section.
type LifecycleReport struct {
	Aggregate   LifecycleAggregateMetrics `json:"aggregate"`
	Transitions []TransitionReport        `json:"transitions"`
	present     map[string]bool
}

func (candidate *LifecycleCandidateReport) UnmarshalJSON(data []byte) error {
	type wire LifecycleCandidateReport
	var decoded wire
	present, err := decodeLifecycleObject(data, &decoded)
	if err != nil {
		return err
	}
	*candidate = LifecycleCandidateReport(decoded)
	candidate.present = present
	return nil
}

func (report *QueryLifecycleReport) UnmarshalJSON(data []byte) error {
	type wire QueryLifecycleReport
	var decoded wire
	present, err := decodeLifecycleObject(data, &decoded)
	if err != nil {
		return err
	}
	*report = QueryLifecycleReport(decoded)
	report.present = present
	return nil
}

func (violation *LifecycleViolation) UnmarshalJSON(data []byte) error {
	type wire LifecycleViolation
	var decoded wire
	present, err := decodeLifecycleObject(data, &decoded)
	if err != nil {
		return err
	}
	*violation = LifecycleViolation(decoded)
	violation.present = present
	return nil
}

func (metrics *LifecycleAggregateMetrics) UnmarshalJSON(data []byte) error {
	type wire LifecycleAggregateMetrics
	var decoded wire
	present, err := decodeLifecycleObject(data, &decoded)
	if err != nil {
		return err
	}
	*metrics = LifecycleAggregateMetrics(decoded)
	metrics.present = present
	return nil
}

func (report *TransitionReport) UnmarshalJSON(data []byte) error {
	type wire TransitionReport
	var decoded wire
	present, err := decodeLifecycleObject(data, &decoded)
	if err != nil {
		return err
	}
	*report = TransitionReport(decoded)
	report.present = present
	return nil
}

func (report *LifecycleReport) UnmarshalJSON(data []byte) error {
	type wire LifecycleReport
	var decoded wire
	present, err := decodeLifecycleObject(data, &decoded)
	if err != nil {
		return err
	}
	*report = LifecycleReport(decoded)
	report.present = present
	return nil
}

func decodeLifecycleObject(data []byte, target any) (map[string]bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("lifecycle report object contains trailing JSON")
		}
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(fields))
	for field, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("lifecycle report field %q must not be null", field)
		}
		present[field] = true
	}
	return present, nil
}

type presentedFacts struct {
	results   []RetrievedItem
	report    QueryLifecycleReport
	canonical LifecycleAggregateMetrics
}

func presentFactCandidates(query Query, points []qdrant.Point, now time.Time) presentedFacts {
	return presentFactCandidatesWithOrder(query, points, now, false)
}

func presentFactCandidatesWithOrder(query Query, points []qdrant.Point, now time.Time, preserveSemanticOrder bool) presentedFacts {
	reference := now.UTC()
	if query.EffectiveIntent() == QueryIntentAsOf {
		if parsed, err := parseUTCCalendarDate(query.AsOf); err == nil {
			reference = parsed
		}
	}

	type parsedCandidate struct {
		point   qdrant.Point
		view    lifecycle.View
		expired bool
	}
	parsed := make(map[string]parsedCandidate, len(points))
	ordered := make([]lifecycle.Candidate, 0, len(points))
	hasCanonicalCurrent := false
	for index, point := range points {
		view, _ := lifecycle.Parse(point.Payload, point.ID)
		expired := factExpiredAt(point.Payload, reference)
		parsed[point.ID] = parsedCandidate{point: point, view: view, expired: expired}
		score := point.Score
		if preserveSemanticOrder {
			score = float64(len(points) - index)
		}
		ordered = append(ordered, lifecycle.Candidate{PointID: point.ID, Score: score, View: view})
		if view.Valid && !expired && view.State == lifecycle.Current && view.Canonical {
			hasCanonicalCurrent = true
		}
	}
	lifecycle.SortCandidates(ordered)

	output := presentedFacts{report: QueryLifecycleReport{
		Intent: query.EffectiveIntent(),
		AsOf:   query.AsOf,
	}}
	for _, candidate := range ordered {
		value := parsed[candidate.PointID]
		evidence := decidePresentation(query.EffectiveIntent(), value.view, value.expired, hasCanonicalCurrent)
		evidence.ID = candidate.PointID
		output.report.Candidates = append(output.report.Candidates, evidence)
		if query.EffectiveIntent() == QueryIntentCurrent &&
			value.view.Valid && !value.expired && value.view.State == lifecycle.Current &&
			!value.view.Canonical && hasCanonicalCurrent {
			output.canonical.CanonicalPreferenceChecks++
			if evidence.Decision != PresentationDemote ||
				!equalReasonCodes(evidence.ReasonCodes, []LifecycleReasonCode{ReasonCanonicalPreference}) {
				output.canonical.CanonicalPreferenceViolations++
			}
		}
		if evidence.Decision == PresentationInclude ||
			evidence.Decision == PresentationDemote ||
			evidence.Decision == PresentationUncertain {
			output.results = append(output.results, itemsFromPoints([]qdrant.Point{value.point})...)
		}
	}
	scoreLifecycleExpectations(query, &output.report)
	output.canonical.Checks = output.report.Checks
	output.canonical.Violations = len(output.report.Violations)
	return output
}

func decidePresentation(intent QueryIntent, view lifecycle.View, expired, hasCanonicalCurrent bool) LifecycleCandidateReport {
	result := LifecycleCandidateReport{
		State: view.State, Canonical: view.Canonical, Expired: expired, Valid: view.Valid,
	}
	sharedIntent := lifecycle.PresentationIntentBroad
	if intent == QueryIntentCurrent {
		sharedIntent = lifecycle.PresentationIntentCurrent
	}
	presentation := lifecycle.DecidePresentation(sharedIntent, view, expired, hasCanonicalCurrent)
	result.Decision = PresentationDecision(presentation.Decision)
	result.ReasonCodes = make([]LifecycleReasonCode, len(presentation.ReasonCodes))
	for index, reason := range presentation.ReasonCodes {
		result.ReasonCodes[index] = LifecycleReasonCode(reason)
	}
	if !view.Valid {
		result.State = ""
	}
	return result
}

func scoreLifecycleExpectations(query Query, report *QueryLifecycleReport) {
	byID := make(map[string]LifecycleCandidateReport, len(report.Candidates))
	for _, candidate := range report.Candidates {
		byID[candidate.ID] = candidate
	}
	for _, expectation := range query.LifecycleExpectations {
		report.Checks++
		candidate, exists := byID[expectation.ID]
		if !exists {
			report.Violations = append(report.Violations,
				queryLifecycleViolation(query.ID, expectation.ID, InvariantCandidatePresent))
			continue
		}
		if expectation.State != "" && candidate.State != expectation.State {
			report.Violations = append(report.Violations,
				queryLifecycleViolation(query.ID, expectation.ID, InvariantState))
		}
		if candidate.Decision != expectation.Decision {
			report.Violations = append(report.Violations,
				queryLifecycleViolation(query.ID, expectation.ID, InvariantDecision))
		}
		if expectation.assertsReasonCodes() {
			expectedReasons := make([]LifecycleReasonCode, len(expectation.ReasonCodes))
			for i, reason := range expectation.ReasonCodes {
				expectedReasons[i] = LifecycleReasonCode(reason)
			}
			if !equalReasonCodes(candidate.ReasonCodes, expectedReasons) {
				report.Violations = append(report.Violations,
					queryLifecycleViolation(query.ID, expectation.ID, InvariantReasonCodes))
			}
		}
	}
	sortLifecycleViolations(report.Violations)
}

func queryLifecycleViolation(queryID, candidateID string, invariant LifecycleInvariant) LifecycleViolation {
	return LifecycleViolation{
		Scope: ViolationScopeQuery, QueryID: queryID,
		CandidateID: candidateID, Invariant: invariant,
	}
}

func transitionLifecycleViolation(scenarioID string, invariant LifecycleInvariant) LifecycleViolation {
	return LifecycleViolation{
		Scope: ViolationScopeTransition, ScenarioID: scenarioID, Invariant: invariant,
	}
}

func sortLifecycleViolations(violations []LifecycleViolation) {
	sort.Slice(violations, func(i, j int) bool {
		left, right := violations[i], violations[j]
		if left.Scope != right.Scope {
			return left.Scope < right.Scope
		}
		if left.QueryID != right.QueryID {
			return left.QueryID < right.QueryID
		}
		if left.ScenarioID != right.ScenarioID {
			return left.ScenarioID < right.ScenarioID
		}
		if left.CandidateID != right.CandidateID {
			return left.CandidateID < right.CandidateID
		}
		return left.Invariant < right.Invariant
	})
}

func equalReasonCodes(left, right []LifecycleReasonCode) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]LifecycleReasonCode(nil), left...)
	rightCopy := append([]LifecycleReasonCode(nil), right...)
	sort.Slice(leftCopy, func(i, j int) bool { return leftCopy[i] < leftCopy[j] })
	sort.Slice(rightCopy, func(i, j int) bool { return rightCopy[i] < rightCopy[j] })
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func executeTransitionScenarios(scenarios []TransitionScenario) []TransitionReport {
	reports := make([]TransitionReport, 0, len(scenarios))
	for _, scenario := range scenarios {
		valid, reason := executeTransitionScenario(scenario)
		report := TransitionReport{
			ID: scenario.ID, Valid: valid, ReasonCode: reason,
			Passed: valid == scenario.ExpectedValid &&
				(scenario.ExpectedReasonCode == "" || reason == LifecycleReasonCode(scenario.ExpectedReasonCode)),
		}
		if valid != scenario.ExpectedValid {
			report.Violations = append(report.Violations,
				transitionLifecycleViolation(scenario.ID, InvariantTransitionValid))
		}
		if scenario.ExpectedReasonCode != "" && reason != LifecycleReasonCode(scenario.ExpectedReasonCode) {
			report.Violations = append(report.Violations,
				transitionLifecycleViolation(scenario.ID, InvariantTransitionReason))
		}
		sortLifecycleViolations(report.Violations)
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].ID < reports[j].ID })
	return reports
}

func executeTransitionScenario(scenario TransitionScenario) (bool, LifecycleReasonCode) {
	pointID := scenario.PointID.String()
	source, err := lifecycle.NormalizeInput(pointID, lifecycleInput(scenario.SourceLifecycle))
	if err != nil {
		return false, ReasonSourceInvalid
	}
	target, err := lifecycle.NormalizeInput(pointID, lifecycleInput(scenario.TargetLifecycle))
	if err != nil {
		return false, ReasonTargetInvalid
	}
	if err := lifecycle.ValidateTransition(pointID, source, target); err != nil {
		return false, ReasonTransitionInvalid
	}
	return true, ReasonTransitionValid
}

func lifecycleInput(payload LifecyclePayload) lifecycle.Input {
	return lifecycle.Input{
		State: payload.State, Canonical: payload.Canonical, Provenance: payload.Provenance,
		VerifiedAt:   payload.VerifiedAt,
		Supersedes:   append([]string(nil), payload.Supersedes...),
		SupersededBy: append([]string(nil), payload.SupersededBy...),
	}
}

func (code LifecycleReasonCode) validPresentation() bool {
	switch code {
	case ReasonCurrentTruth, ReasonCanonicalPreference, ReasonCurrentContext,
		ReasonHistorical, ReasonHistoricalContext, ReasonSuperseded,
		ReasonSupersededContext, ReasonDisputed, ReasonInvalidLifecycle, ReasonExpired:
		return true
	default:
		return false
	}
}

func (code LifecycleReasonCode) validTransition() bool {
	switch code {
	case ReasonTransitionValid, ReasonSourceInvalid, ReasonTargetInvalid, ReasonTransitionInvalid:
		return true
	default:
		return false
	}
}

func safeReasonCode(value LifecycleReasonCode) bool {
	return value.validPresentation() || value.validTransition()
}
