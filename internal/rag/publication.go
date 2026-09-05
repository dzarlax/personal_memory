package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
)

// A seal is deliberately stored in the chunk collection rather than a
// separate pointer collection. A reader can therefore verify the exact data it
// is about to return without trusting a mutable "latest" pointer.
const publicationVersion = "rag-generation-seal-v1"

const (
	maxPublicationValidationPoints = 4096
	maxPublicationValidationPages  = 64
	maxPublicationValidationBytes  = 8 << 20
)

type generationScroller interface {
	ScrollWithPayload(context.Context, int, interface{}, map[string]interface{}, interface{}, bool) (*qdrant.ScrollResult, error)
}

type publicationDescriptor struct {
	Version       string `json:"version"`
	FilePath      string `json:"file_path"`
	Generation    string `json:"generation"`
	Layout        string `json:"layout"`
	FileHash      string `json:"file_hash"`
	SealedAt      string `json:"sealed_at"`
	TotalChunks   int    `json:"total_chunks"`
	OrderedDigest string `json:"ordered_digest"`
}

type validatedGeneration struct {
	points   []qdrant.ScrollPoint
	digest   string
	fileHash string
	sealedAt time.Time
}

type sealedGenerationDiscovery struct {
	generations map[string]validatedGeneration
	pending     bool
}

// validateGeneration reads only one (file_path, generation) pair and proves
// its complete layout before a caller can publish or format it. The point
// count, pages, and decoded payload bytes are all bounded: a continuation past
// any bound is incompleteness, never an implicit success.
func validateGeneration(ctx context.Context, source generationScroller, filePath, generation string, requireSeal bool) (validatedGeneration, error) {
	if source == nil {
		return validatedGeneration{}, fmt.Errorf("generation validation unavailable")
	}
	if filePath == "" || generation == "" {
		return validatedGeneration{}, fmt.Errorf("missing file path or generation")
	}
	filter := map[string]interface{}{"must": []map[string]interface{}{
		{"key": "file_path", "match": map[string]interface{}{"value": filePath}},
		{"key": "generation", "match": map[string]interface{}{"value": generation}},
	}}

	var points []qdrant.ScrollPoint
	var offset interface{}
	bytesRead := 0
	for page := 0; ; page++ {
		if page >= maxPublicationValidationPages {
			return validatedGeneration{}, fmt.Errorf("generation validation page cap exhausted")
		}
		remaining := maxPublicationValidationPoints - len(points)
		if remaining <= 0 {
			return validatedGeneration{}, fmt.Errorf("generation validation point cap exhausted")
		}
		pageSize := min(100, remaining)
		result, err := source.ScrollWithPayload(ctx, pageSize, offset, filter, true, false)
		if err != nil {
			return validatedGeneration{}, fmt.Errorf("scroll generation: %w", err)
		}
		if result == nil {
			return validatedGeneration{}, fmt.Errorf("malformed generation scroll response")
		}
		for _, point := range result.Points {
			encoded, err := json.Marshal(point.Payload)
			if err != nil {
				return validatedGeneration{}, fmt.Errorf("encode generation payload: %w", err)
			}
			bytesRead += len(point.ID) + len(encoded)
			if bytesRead > maxPublicationValidationBytes {
				return validatedGeneration{}, fmt.Errorf("generation validation byte cap exhausted")
			}
			points = append(points, point)
			if len(points) > maxPublicationValidationPoints {
				return validatedGeneration{}, fmt.Errorf("generation validation point cap exhausted")
			}
		}
		if result.RawOffset == nil {
			break
		}
		if len(points) == maxPublicationValidationPoints {
			return validatedGeneration{}, fmt.Errorf("generation validation nonterminal at point cap")
		}
		offset = result.RawOffset
	}
	if len(points) == 0 {
		return validatedGeneration{}, fmt.Errorf("generation has no chunks")
	}

	byIndex := make(map[int]qdrant.ScrollPoint, len(points))
	var layout string
	var fileHash string
	var total int
	var sealedAt time.Time
	sealCount := 0
	for _, point := range points {
		payload := point.Payload
		fp, ok := payload["file_path"].(string)
		if !ok || fp != filePath {
			return validatedGeneration{}, fmt.Errorf("generation file path mismatch")
		}
		g, ok := payload["generation"].(string)
		if !ok || g != generation {
			return validatedGeneration{}, fmt.Errorf("generation identity mismatch")
		}
		candidateLayout, ok := payload["layout"].(string)
		if !ok || candidateLayout == "" {
			return validatedGeneration{}, fmt.Errorf("generation layout missing")
		}
		candidateTotal, ok := payloadInt(payload["total_chunks"])
		if !ok || candidateTotal < 1 || candidateTotal > maxPublicationValidationPoints {
			return validatedGeneration{}, fmt.Errorf("generation total chunks invalid")
		}
		index, ok := payloadInt(payload["chunk_index"])
		if !ok || index < 0 || index >= candidateTotal {
			return validatedGeneration{}, fmt.Errorf("generation chunk index invalid")
		}
		if layout == "" {
			layout, total = candidateLayout, candidateTotal
		}
		candidateHash, ok := payload["file_hash"].(string)
		if !ok || candidateHash == "" {
			return validatedGeneration{}, fmt.Errorf("generation file hash missing")
		}
		if fileHash == "" {
			fileHash = candidateHash
		}
		if layout != candidateLayout || total != candidateTotal {
			return validatedGeneration{}, fmt.Errorf("generation layout or total mismatch")
		}
		if fileHash != candidateHash {
			return validatedGeneration{}, fmt.Errorf("generation file hash mismatch")
		}
		if _, exists := byIndex[index]; exists {
			return validatedGeneration{}, fmt.Errorf("generation has duplicate chunk index")
		}
		if _, hasSeal := payload["publication"]; hasSeal {
			sealCount++
			if index != 0 {
				return validatedGeneration{}, fmt.Errorf("generation seal is not on chunk zero")
			}
		}
		byIndex[index] = point
	}
	if len(byIndex) != total {
		return validatedGeneration{}, fmt.Errorf("generation has %d chunks, want %d", len(byIndex), total)
	}
	ordered := make([]qdrant.ScrollPoint, total)
	for index := 0; index < total; index++ {
		point, ok := byIndex[index]
		if !ok {
			return validatedGeneration{}, fmt.Errorf("generation missing chunk %d", index)
		}
		ordered[index] = point
	}
	digest, err := generationDigest(filePath, generation, layout, fileHash, ordered)
	if err != nil {
		return validatedGeneration{}, err
	}
	if requireSeal {
		if sealCount != 1 {
			return validatedGeneration{}, fmt.Errorf("generation has %d seals, want one", sealCount)
		}
		descriptor, err := parsePublicationDescriptor(ordered[0].Payload["publication"])
		if err != nil || descriptor.Version != publicationVersion || descriptor.FilePath != filePath || descriptor.Generation != generation || descriptor.Layout != layout || descriptor.FileHash != fileHash || descriptor.TotalChunks != total || descriptor.OrderedDigest != digest {
			return validatedGeneration{}, fmt.Errorf("generation seal invalid")
		}
		sealedAt, err = time.Parse(time.RFC3339Nano, descriptor.SealedAt)
		if err != nil {
			return validatedGeneration{}, fmt.Errorf("generation seal timestamp invalid")
		}
	} else if sealCount != 0 {
		return validatedGeneration{}, fmt.Errorf("unsealed generation already has a seal")
	}
	return validatedGeneration{points: ordered, digest: digest, fileHash: fileHash, sealedAt: sealedAt}, nil
}

// discoverSealedGenerations establishes freshness for one file independently
// of the bounded semantic result set. If pagination or a discovered seal is
// malformed, callers must fail that file closed rather than return an older
// generation just because it scored higher.
func discoverSealedGenerations(ctx context.Context, source generationScroller, filePath string) (sealedGenerationDiscovery, error) {
	if source == nil || filePath == "" {
		return sealedGenerationDiscovery{}, fmt.Errorf("file generation discovery unavailable")
	}
	filter := map[string]interface{}{"must": []map[string]interface{}{
		{"key": "file_path", "match": map[string]interface{}{"value": filePath}},
	}}
	sealed := map[string]bool{}
	pendingGenerations := map[string]bool{}
	var offset interface{}
	pointsRead, bytesRead := 0, 0
	for page := 0; ; page++ {
		if page >= maxPublicationValidationPages {
			return sealedGenerationDiscovery{}, fmt.Errorf("file generation discovery page cap exhausted")
		}
		remaining := maxPublicationValidationPoints - pointsRead
		if remaining <= 0 {
			return sealedGenerationDiscovery{}, fmt.Errorf("file generation discovery point cap exhausted")
		}
		result, err := source.ScrollWithPayload(ctx, min(100, remaining), offset, filter, true, false)
		if err != nil {
			return sealedGenerationDiscovery{}, fmt.Errorf("scroll file generations: %w", err)
		}
		if result == nil {
			return sealedGenerationDiscovery{}, fmt.Errorf("malformed file generation discovery response")
		}
		for _, point := range result.Points {
			encoded, err := json.Marshal(point.Payload)
			if err != nil {
				return sealedGenerationDiscovery{}, fmt.Errorf("encode file generation payload: %w", err)
			}
			bytesRead += len(point.ID) + len(encoded)
			pointsRead++
			if bytesRead > maxPublicationValidationBytes || pointsRead > maxPublicationValidationPoints {
				return sealedGenerationDiscovery{}, fmt.Errorf("file generation discovery cap exhausted")
			}
			fp, ok := point.Payload["file_path"].(string)
			if !ok || fp != filePath {
				return sealedGenerationDiscovery{}, fmt.Errorf("file generation discovery path mismatch")
			}
			generation, _ := point.Payload["generation"].(string)
			if generation == "" {
				continue // legacy data is unverified, never a seal candidate.
			}
			layout, hasLayout := point.Payload["layout"].(string)
			if layout == "" || !hasLayout {
				continue // legacy content-hash generation, never a seal candidate.
			}
			if _, hasSeal := point.Payload["publication"]; hasSeal {
				sealed[generation] = true
			} else {
				pendingGenerations[generation] = true
			}
		}
		if result.RawOffset == nil {
			break
		}
		if pointsRead == maxPublicationValidationPoints {
			return sealedGenerationDiscovery{}, fmt.Errorf("file generation discovery nonterminal at point cap")
		}
		offset = result.RawOffset
	}

	generations := make([]string, 0, len(sealed))
	for generation := range sealed {
		generations = append(generations, generation)
	}
	sort.Strings(generations)
	validated := make(map[string]validatedGeneration, len(generations))
	for _, generation := range generations {
		generationValidation, err := validateGeneration(ctx, source, filePath, generation, true)
		if err != nil {
			return sealedGenerationDiscovery{}, fmt.Errorf("validate discovered generation %s: %w", generation, err)
		}
		validated[generation] = generationValidation
	}
	for generation := range sealed {
		delete(pendingGenerations, generation)
	}
	return sealedGenerationDiscovery{generations: validated, pending: len(pendingGenerations) > 0}, nil
}

func generationDigest(filePath, generation, layout, fileHash string, points []qdrant.ScrollPoint) (string, error) {
	h := sha256.New()
	for index, point := range points {
		payload := point.Payload
		text, ok := payload["text"].(string)
		if !ok {
			return "", fmt.Errorf("generation chunk %d text missing", index)
		}
		heading, _ := payload["heading"].(string)
		pointHash, ok := payload["file_hash"].(string)
		if !ok || pointHash != fileHash {
			return "", fmt.Errorf("generation chunk %d file hash mismatch", index)
		}
		// Length delimiters make this unambiguous without depending on Go map
		// iteration or Qdrant's JSON field ordering.
		for _, field := range []string{filePath, generation, layout, fileHash, strconv.Itoa(index), text, heading} {
			_, _ = h.Write([]byte(strconv.Itoa(len(field))))
			_, _ = h.Write([]byte{':'})
			_, _ = h.Write([]byte(field))
			_, _ = h.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func parsePublicationDescriptor(raw interface{}) (publicationDescriptor, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return publicationDescriptor{}, err
	}
	var descriptor publicationDescriptor
	if err := json.Unmarshal(encoded, &descriptor); err != nil {
		return publicationDescriptor{}, err
	}
	if descriptor.Version == "" || descriptor.FilePath == "" || descriptor.Generation == "" || descriptor.Layout == "" || descriptor.FileHash == "" || descriptor.SealedAt == "" || descriptor.TotalChunks < 1 || descriptor.OrderedDigest == "" {
		return publicationDescriptor{}, fmt.Errorf("incomplete publication descriptor")
	}
	return descriptor, nil
}

func sealPayload(payload map[string]interface{}, descriptor publicationDescriptor) map[string]interface{} {
	sealed := make(map[string]interface{}, len(payload)+1)
	for key, value := range payload {
		sealed[key] = value
	}
	sealed["publication"] = descriptor
	return sealed
}
