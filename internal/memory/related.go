package memory

import (
	"math"

	"github.com/Dzarlax-AI/personal-memory/internal/memory/lifecycle"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

// RelatedFactCandidate is the normalized fact contract shared by duplicate
// detection and related-fact feedback. Lifecycle remains the single source of
// truth for lifecycle metadata exposed by this package.
type RelatedFactCandidate struct {
	PointID    string         `json:"point_id"`
	Text       string         `json:"text"`
	Score      float64        `json:"score"`
	Namespace  string         `json:"namespace"`
	Tags       []string       `json:"tags"`
	PrimaryTag string         `json:"primary_tag,omitempty"`
	ValidUntil string         `json:"valid_until,omitempty"`
	Lifecycle  lifecycle.View `json:"lifecycle"`
}

// selectRelatedCandidates applies the common duplicate/related boundary.
// A valid superseded fact is the only high-scoring lifecycle classification
// that does not block a write. It remains related feedback only when it also
// meets related visibility rules, including not being expired.
func selectRelatedCandidates(points []qdrant.Point, relatedLow, dedupThreshold float64, limit int) (*RelatedFactCandidate, []RelatedFactCandidate) {
	var duplicate *RelatedFactCandidate
	relatedPoints := make([]qdrant.Point, 0, len(points))

	for _, point := range points {
		if math.IsNaN(point.Score) || math.IsInf(point.Score, 0) {
			continue
		}
		view := lifecycleView(point.ID, point.Payload)
		if point.Score >= dedupThreshold && (!view.Valid || view.State != lifecycle.Superseded) {
			if duplicate == nil || point.Score > duplicate.Score {
				candidate := projectRelatedFactCandidate(point, view)
				duplicate = &candidate
			}
			continue
		}
		if !(point.Score >= relatedLow) || isExpired(point.Payload) {
			continue
		}
		relatedPoints = append(relatedPoints, point)
	}

	if limit <= 0 || len(relatedPoints) == 0 {
		return duplicate, []RelatedFactCandidate{}
	}

	sorted := relatedSearchPoints(relatedPoints)
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	related := make([]RelatedFactCandidate, 0, len(sorted))
	for _, candidate := range sorted {
		related = append(related, projectRelatedFactCandidate(candidate.point, candidate.view))
	}
	return duplicate, related
}

func projectRelatedFactCandidate(point qdrant.Point, view lifecycle.View) RelatedFactCandidate {
	validUntil := ""
	if _, present, err := validUntilPayload(point.Payload); present && err == nil {
		validUntil = relatedCandidateString(point.Payload, "valid_until")
	}
	return RelatedFactCandidate{
		PointID:    point.ID,
		Text:       relatedCandidateString(point.Payload, "text"),
		Score:      point.Score,
		Namespace:  relatedCandidateString(point.Payload, "namespace"),
		Tags:       relatedCandidateTags(point.Payload["tags"]),
		PrimaryTag: relatedCandidateString(point.Payload, "primary_tag"),
		ValidUntil: validUntil,
		Lifecycle:  view,
	}
}

func relatedCandidateString(payload map[string]interface{}, key string) string {
	value, _ := payload[key].(string)
	return value
}

func relatedCandidateTags(raw interface{}) []string {
	tags := []string{}
	switch values := raw.(type) {
	case []string:
		tags = append(tags, values...)
	case []interface{}:
		for _, value := range values {
			if tag, ok := value.(string); ok {
				tags = append(tags, tag)
			}
		}
	}
	return tags
}
