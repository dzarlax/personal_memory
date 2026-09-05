package qdrant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultHTTPTimeout = 30 * time.Second
const maxResponseBodyBytes int64 = 16 << 20

type Client struct {
	url        string
	collection string
	httpClient *http.Client
}

// CollectionInfo describes the collection properties used to validate that a
// configured embedding model is compatible with data already stored in Qdrant.
type CollectionInfo struct {
	Exists     bool
	Points     uint64
	VectorSize int
	Metadata   map[string]any
}

func NewClient(url, collection string) *Client {
	return &Client{
		url:        url,
		collection: collection,
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// CollectionName returns the collection targeted by this client.
func (c *Client) CollectionName() string {
	return c.collection
}

// Point represents a Qdrant point with vector and payload.
type Point struct {
	ID      string                 `json:"id"`
	Vector  []float32              `json:"vector,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
	Score   float64                `json:"score,omitempty"`
}

// SnapshotIdentity is the typed, opaque identity Qdrant returns for a
// collection snapshot. It deliberately contains no collection URL or data.
type SnapshotIdentity struct {
	Name string `json:"name"`
}

// parsePointID converts a Qdrant point ID (int or string) to string.
func parsePointID(v interface{}) string {
	switch id := v.(type) {
	case string:
		return id
	case exactPointID:
		return string(id)
	case float64:
		return strconv.FormatFloat(id, 'f', -1, 64)
	case json.Number:
		return id.String()
	default:
		return fmt.Sprintf("%v", id)
	}
}

// exactPointID decodes Qdrant's string or unsigned integer IDs without routing
// JSON numbers through float64. It is intentionally used only for ID fields so
// numeric values in point payloads retain their established float64 types.
type exactPointID string

func (id *exactPointID) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty point ID")
	}
	if data[0] == '"' {
		var decoded string
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*id = exactPointID(decoded)
		return nil
	}
	if _, err := strconv.ParseUint(string(data), 10, 64); err != nil {
		return fmt.Errorf("invalid numeric point ID %q: %w", data, err)
	}
	*id = exactPointID(data)
	return nil
}

func qdrantPointID(id string) interface{} {
	if parsed, err := strconv.ParseUint(id, 10, 64); err == nil {
		return parsed
	}
	return id
}

// decodeRequiredResult rejects successful HTTP envelopes that do not contain a
// usable Qdrant result. An empty array/object remains a valid result; missing,
// null, or explicit error envelopes are transport/protocol failures, not an
// empty collection.
func decodeRequiredResult(body []byte, target interface{}) error {
	var envelope struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	result := bytes.TrimSpace(envelope.Result)
	if envelope.Status == "error" || len(result) == 0 || bytes.Equal(result, []byte("null")) {
		return fmt.Errorf("qdrant response has no usable result")
	}
	return json.Unmarshal(result, target)
}

// Get retrieves one point by its exact ID. The bool is false when Qdrant
// reports that the point does not exist.
func (c *Client) Get(ctx context.Context, id string) (Point, bool, error) {
	requestURL := fmt.Sprintf("%s/collections/%s/points/%s?with_vector=true", c.url, c.collection, url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return Point{}, false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Point{}, false, fmt.Errorf("get point: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Point{}, false, nil
	}
	b, err := readLimitedBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return Point{}, false, fmt.Errorf("read point response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return Point{}, false, fmt.Errorf("GET %s failed (status %d): %s", requestURL, resp.StatusCode, string(b))
	}
	var result struct {
		ID      *exactPointID          `json:"id"`
		Vector  []float32              `json:"vector"`
		Payload map[string]interface{} `json:"payload"`
	}
	if err := decodeRequiredResult(b, &result); err != nil {
		return Point{}, false, fmt.Errorf("decode point response: %w", err)
	}
	if result.ID == nil || strings.TrimSpace(string(*result.ID)) == "" {
		return Point{}, false, fmt.Errorf("decode point response: point id is required")
	}
	return Point{ID: parsePointID(*result.ID), Vector: result.Vector, Payload: result.Payload}, true, nil
}

// CollectionInfo returns the collection's point count, vector size, and
// collection metadata. A missing collection is represented by Exists=false.
func (c *Client) CollectionInfo(ctx context.Context) (CollectionInfo, error) {
	requestURL := fmt.Sprintf("%s/collections/%s", c.url, c.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return CollectionInfo{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CollectionInfo{}, fmt.Errorf("get collection info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return CollectionInfo{Exists: false}, nil
	}
	body, err := readLimitedBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return CollectionInfo{}, fmt.Errorf("read collection info response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CollectionInfo{}, fmt.Errorf("GET %s failed (status %d): %s", requestURL, resp.StatusCode, string(body))
	}

	var response struct {
		Result struct {
			Points *uint64 `json:"points_count"`
			Config struct {
				Params struct {
					Vectors json.RawMessage `json:"vectors"`
				} `json:"params"`
				Metadata map[string]any `json:"metadata"`
			} `json:"config"`
			// Accept top-level metadata as well to remain compatible with Qdrant
			// response variants while preferring the canonical config location.
			Metadata map[string]any `json:"metadata"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return CollectionInfo{}, fmt.Errorf("decode collection info response: %w", err)
	}
	if response.Result.Points == nil {
		return CollectionInfo{}, fmt.Errorf("decode collection info response: points_count is required")
	}
	vectorSize, err := collectionVectorSize(response.Result.Config.Params.Vectors)
	if err != nil {
		return CollectionInfo{}, fmt.Errorf("decode collection vector configuration: %w", err)
	}
	metadata := response.Result.Config.Metadata
	if metadata == nil {
		metadata = response.Result.Metadata
	}
	return CollectionInfo{
		Exists:     true,
		Points:     *response.Result.Points,
		VectorSize: vectorSize,
		Metadata:   metadata,
	}, nil
}

// ExactCount returns the exact number of points currently stored in the
// collection. CollectionInfo's points_count may be approximate, so callers
// must use this method for safety decisions that distinguish empty from
// non-empty collections.
func (c *Client) ExactCount(ctx context.Context) (uint64, error) {
	requestURL := fmt.Sprintf("%s/collections/%s/points/count", c.url, c.collection)
	responseBody, err := c.postJSON(ctx, requestURL, map[string]any{"exact": true})
	if err != nil {
		return 0, fmt.Errorf("count collection points: %w", err)
	}
	var response struct {
		Result struct {
			Count *uint64 `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return 0, fmt.Errorf("decode exact count response: %w", err)
	}
	if response.Result.Count == nil {
		return 0, fmt.Errorf("decode exact count response: count is required")
	}
	return *response.Result.Count, nil
}

func collectionVectorSize(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, nil
	}
	var unnamed struct {
		Size int `json:"size"`
	}
	if err := json.Unmarshal(raw, &unnamed); err != nil {
		return 0, err
	}
	if unnamed.Size > 0 {
		return unnamed.Size, nil
	}

	var named map[string]struct {
		Size int `json:"size"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return 0, err
	}
	if len(named) == 1 {
		for _, vector := range named {
			return vector.Size, nil
		}
	}
	return 0, nil
}

// CreateCollection creates the target collection with cosine distance and
// optional collection-level metadata.
func (c *Client) CreateCollection(ctx context.Context, vectorSize int, metadata map[string]any) error {
	requestURL := fmt.Sprintf("%s/collections/%s", c.url, c.collection)
	body := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	}
	if metadata != nil {
		body["metadata"] = metadata
	}
	return c.mutate(ctx, http.MethodPut, requestURL, body, false, false)
}

// DeleteCollection removes the target collection only when its name has the
// caller-supplied non-empty safety prefix.
func (c *Client) DeleteCollection(ctx context.Context, requiredPrefix string) error {
	if requiredPrefix == "" || !strings.HasPrefix(c.collection, requiredPrefix) {
		return fmt.Errorf("collection %q does not have required deletion prefix %q", c.collection, requiredPrefix)
	}
	requestURL := fmt.Sprintf("%s/collections/%s", c.url, c.collection)
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return fmt.Errorf("parse collection deletion URL: %w", err)
	}
	query := parsed.Query()
	query.Set("timeout", "15")
	parsed.RawQuery = query.Encode()
	return c.mutate(ctx, http.MethodDelete, parsed.String(), nil, false, true)
}

// UpdateCollectionMetadata merges collection-level metadata. Qdrant treats an
// empty object as a request to clear the metadata.
func (c *Client) UpdateCollectionMetadata(ctx context.Context, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	requestURL := fmt.Sprintf("%s/collections/%s", c.url, c.collection)
	body := map[string]interface{}{"metadata": metadata}
	return c.mutate(ctx, http.MethodPatch, requestURL, body, false, false)
}

// EnsureCollection creates the collection if it doesn't exist.
func (c *Client) EnsureCollection(ctx context.Context, vectorSize int) error {
	info, err := c.CollectionInfo(ctx)
	if err != nil {
		return err
	}
	if info.Exists {
		return nil
	}
	return c.CreateCollection(ctx, vectorSize, nil)
}

// Upsert inserts or updates a point.
func (c *Client) Upsert(ctx context.Context, point Point) error {
	return c.upsert(ctx, point, qdrantPointID(point.ID))
}

// UpsertWithPointID preserves the JSON fixture ID kind. This is primarily
// useful for compatibility tests that need both numeric and string point IDs.
func (c *Client) UpsertWithPointID(ctx context.Context, point Point, numeric bool) error {
	var id any = point.ID
	if numeric {
		parsed, err := strconv.ParseUint(point.ID, 10, 64)
		if err != nil {
			return fmt.Errorf("numeric point ID %q: %w", point.ID, err)
		}
		id = parsed
	}
	return c.upsert(ctx, point, id)
}

func (c *Client) upsert(ctx context.Context, point Point, id any) error {
	url := fmt.Sprintf("%s/collections/%s/points", c.url, c.collection)
	body := map[string]interface{}{
		"points": []map[string]interface{}{
			{
				"id":      id,
				"vector":  point.Vector,
				"payload": point.Payload,
			},
		},
	}
	return c.mutate(ctx, http.MethodPut, url, body, true, false)
}

// Search performs a vector similarity search with optional filters.
func (c *Client) Search(ctx context.Context, vector []float32, limit int, filters map[string]interface{}, scoreThreshold *float64) ([]Point, error) {
	url := fmt.Sprintf("%s/collections/%s/points/search", c.url, c.collection)
	body := map[string]interface{}{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}
	if filters != nil {
		body["filter"] = filters
	}
	if scoreThreshold != nil {
		body["score_threshold"] = *scoreThreshold
	}

	respBody, err := c.postJSON(ctx, url, body)
	if err != nil {
		return nil, err
	}

	var result []struct {
		ID      exactPointID           `json:"id"`
		Score   float64                `json:"score"`
		Payload map[string]interface{} `json:"payload"`
	}
	if err := decodeRequiredResult(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	points := make([]Point, len(result))
	for i, r := range result {
		points[i] = Point{
			ID:      parsePointID(r.ID),
			Score:   r.Score,
			Payload: r.Payload,
		}
	}
	return points, nil
}

// ScrollResult holds a page of scroll results.
type ScrollResult struct {
	Points    []ScrollPoint `json:"points"`
	RawOffset interface{}   `json:"-"`
}

func (r *ScrollResult) UnmarshalJSON(data []byte) error {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	rawPoints, present := decoded["points"]
	if !present || bytes.Equal(bytes.TrimSpace(rawPoints), []byte("null")) {
		return fmt.Errorf("scroll result points is required")
	}
	if err := json.Unmarshal(rawPoints, &r.Points); err != nil {
		return fmt.Errorf("decode scroll result points: %w", err)
	}
	r.RawOffset = nil
	raw := bytes.TrimSpace(decoded["next_page_offset"])
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var offset string
		if err := json.Unmarshal(raw, &offset); err != nil {
			return err
		}
		r.RawOffset = offset
		return nil
	}
	if _, err := strconv.ParseUint(string(raw), 10, 64); err != nil {
		return fmt.Errorf("invalid numeric scroll offset %q: %w", raw, err)
	}
	r.RawOffset = json.Number(string(raw))
	return nil
}

// ScrollPoint is a point returned by scroll (may include vector).
type ScrollPoint struct {
	ID      string                 `json:"-"`
	RawID   exactPointID           `json:"id"`
	Vector  []float32              `json:"vector,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// Scroll paginates through points with full payload.
func (c *Client) Scroll(ctx context.Context, limit int, offset interface{}, filters map[string]interface{}, withVector bool) (*ScrollResult, error) {
	return c.ScrollWithPayload(ctx, limit, offset, filters, true, withVector)
}

// ScrollWithPayload is like Scroll but accepts an explicit payload selector.
// withPayload may be: true (all fields), false (none), or []string (specific fields).
func (c *Client) ScrollWithPayload(ctx context.Context, limit int, offset interface{}, filters map[string]interface{}, withPayload interface{}, withVector bool) (*ScrollResult, error) {
	url := fmt.Sprintf("%s/collections/%s/points/scroll", c.url, c.collection)
	body := map[string]interface{}{
		"limit":        limit,
		"with_payload": withPayload,
		"with_vector":  withVector,
	}
	if offset != nil {
		body["offset"] = offset
	}
	if filters != nil {
		body["filter"] = filters
	}

	respBody, err := c.postJSON(ctx, url, body)
	if err != nil {
		return nil, err
	}

	var result ScrollResult
	if err := decodeRequiredResult(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode scroll response: %w", err)
	}

	for i := range result.Points {
		result.Points[i].ID = parsePointID(result.Points[i].RawID)
	}

	return &result, nil
}

// ScrollAll retrieves all points with full payload.
func (c *Client) ScrollAll(ctx context.Context, filters map[string]interface{}, withVector bool) ([]ScrollPoint, error) {
	return c.ScrollAllWithPayload(ctx, filters, true, withVector)
}

// ScrollAllWithPayload paginates through all points with an explicit payload selector.
func (c *Client) ScrollAllWithPayload(ctx context.Context, filters map[string]interface{}, withPayload interface{}, withVector bool) ([]ScrollPoint, error) {
	var all []ScrollPoint
	var offset interface{}
	for {
		result, err := c.ScrollWithPayload(ctx, 100, offset, filters, withPayload, withVector)
		if err != nil {
			return nil, err
		}
		all = append(all, result.Points...)
		if result.RawOffset == nil {
			break
		}
		offset = result.RawOffset
	}
	return all, nil
}

// Delete removes points by IDs.
func (c *Client) Delete(ctx context.Context, ids []string) error {
	url := fmt.Sprintf("%s/collections/%s/points/delete", c.url, c.collection)
	points := make([]interface{}, len(ids))
	for i, id := range ids {
		points[i] = qdrantPointID(id)
	}
	body := map[string]interface{}{
		"points": points,
	}
	return c.mutate(ctx, http.MethodPost, url, body, true, false)
}

// DeleteExactStrong removes exactly one named point. It is intentionally
// separate from Delete so destructive maintenance flows cannot accidentally
// broaden their target set or weaken ordering/wait semantics.
func (c *Client) DeleteExactStrong(ctx context.Context, id string) error {
	if err := validateMaintenanceTarget(id); err != nil {
		return err
	}
	requestURL := fmt.Sprintf("%s/collections/%s/points/delete", c.url, c.collection)
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return fmt.Errorf("parse exact delete URL: %w", err)
	}
	query := parsed.Query()
	query.Set("wait", "true")
	query.Set("ordering", "strong")
	parsed.RawQuery = query.Encode()
	return c.mutate(ctx, http.MethodPost, parsed.String(), map[string]interface{}{
		"points": []interface{}{qdrantPointID(id)},
	}, true, false)
}

// DeleteByFilter removes all points matching the filter in a single request.
func (c *Client) DeleteByFilter(ctx context.Context, filter map[string]interface{}) error {
	url := fmt.Sprintf("%s/collections/%s/points/delete", c.url, c.collection)
	body := map[string]interface{}{
		"filter": filter,
	}
	return c.mutate(ctx, http.MethodPost, url, body, true, false)
}

// CreateFieldIndex creates a payload field index for fast filtering.
func (c *Client) CreateFieldIndex(ctx context.Context, fieldName, fieldSchema string) error {
	url := fmt.Sprintf("%s/collections/%s/index", c.url, c.collection)
	body := map[string]interface{}{
		"field_name":   fieldName,
		"field_schema": fieldSchema,
	}
	return c.mutate(ctx, http.MethodPut, url, body, true, false)
}

// SetPayload updates payload fields on a point without re-embedding.
func (c *Client) SetPayload(ctx context.Context, id string, payload map[string]interface{}) error {
	url := fmt.Sprintf("%s/collections/%s/points/payload", c.url, c.collection)
	body := map[string]interface{}{
		"payload": payload,
		"points":  []interface{}{qdrantPointID(id)},
	}
	return c.mutate(ctx, http.MethodPost, url, body, true, false)
}

var lifecyclePayloadKeys = map[string]struct{}{
	"lifecycle_state":           {},
	"lifecycle_transitioned_at": {},
	"canonical":                 {},
	"provenance":                {},
	"verified_at":               {},
	"supersedes":                {},
	"superseded_by":             {},
}

type MaintenanceReason string

const (
	MaintenanceReasonExpired             MaintenanceReason = "expired"
	MaintenanceReasonSupersededRetention MaintenanceReason = "superseded_retention"
)

// QuarantineMaintenance applies the only valid quarantined maintenance shape.
// It intentionally does not accept arbitrary payload fields, avoiding invalid
// active/quarantined combinations at the Qdrant boundary.
func (c *Client) QuarantineMaintenance(ctx context.Context, id string, at time.Time, reason MaintenanceReason, batchID string) error {
	if err := validateMaintenanceTarget(id); err != nil {
		return err
	}
	if at.IsZero() {
		return fmt.Errorf("quarantine time is required")
	}
	atText := at.UTC().Format(time.RFC3339)
	if _, err := time.Parse(time.RFC3339, atText); err != nil {
		return fmt.Errorf("quarantine time must use RFC3339 format")
	}
	if reason != MaintenanceReasonExpired && reason != MaintenanceReasonSupersededRetention {
		return fmt.Errorf("quarantine reason is not supported")
	}
	if strings.TrimSpace(batchID) == "" || strings.TrimSpace(batchID) != batchID {
		return fmt.Errorf("quarantine batch ID is required")
	}
	return c.replacePayloadBatch(ctx, id, map[string]interface{}{
		"maintenance_status":  "quarantined",
		"quarantined_at":      atText,
		"quarantine_reason":   string(reason),
		"quarantine_batch_id": batchID,
	}, nil, "maintenance")
}

// RestoreMaintenance applies the only valid active maintenance shape. It
// removes every quarantine-only key in the same strong ordered batch.
func (c *Client) RestoreMaintenance(ctx context.Context, id string) error {
	if err := validateMaintenanceTarget(id); err != nil {
		return err
	}
	return c.replacePayloadBatch(ctx, id, map[string]interface{}{
		"maintenance_status": "active",
	}, []string{"quarantined_at", "quarantine_reason", "quarantine_batch_id"}, "maintenance")
}

func validateMaintenanceTarget(id string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("point ID is required")
	}
	return nil
}

// ReplaceLifecyclePayload applies a complete lifecycle target without
// rewriting unrelated payload fields or vectors. Qdrant executes batch
// operations in order; callers can safely retry the same target.
func (c *Client) ReplaceLifecyclePayload(ctx context.Context, id string, set map[string]interface{}, deleteKeys []string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("point ID is required")
	}
	if len(set) == 0 && len(deleteKeys) == 0 {
		return fmt.Errorf("lifecycle payload mutation must not be empty")
	}
	for key := range set {
		if _, allowed := lifecyclePayloadKeys[key]; !allowed {
			return fmt.Errorf("payload key %q is not lifecycle metadata", key)
		}
	}
	for _, key := range deleteKeys {
		if _, allowed := lifecyclePayloadKeys[key]; !allowed {
			return fmt.Errorf("payload key %q is not lifecycle metadata", key)
		}
	}

	return c.replacePayloadBatch(ctx, id, set, deleteKeys, "lifecycle")
}

func (c *Client) replacePayloadBatch(ctx context.Context, id string, set map[string]interface{}, deleteKeys []string, kind string) error {
	pointID := qdrantPointID(id)
	operations := make([]map[string]interface{}, 0, 2)
	if len(set) > 0 {
		operations = append(operations, map[string]interface{}{
			"set_payload": map[string]interface{}{
				"payload": set,
				"points":  []interface{}{pointID},
			},
		})
	}
	if len(deleteKeys) > 0 {
		operations = append(operations, map[string]interface{}{
			"delete_payload": map[string]interface{}{
				"keys":   append([]string(nil), deleteKeys...),
				"points": []interface{}{pointID},
			},
		})
	}

	requestURL := fmt.Sprintf("%s/collections/%s/points/batch", c.url, c.collection)
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return fmt.Errorf("parse %s batch URL: %w", kind, err)
	}
	query := parsed.Query()
	query.Set("ordering", "strong")
	parsed.RawQuery = query.Encode()
	return c.mutate(ctx, http.MethodPost, parsed.String(), map[string]interface{}{"operations": operations}, true, false)
}

// CreateSnapshot triggers a snapshot creation. New maintenance flows should
// use CreateSnapshotIdentity so snapshot identity cannot be accidentally
// reduced to an untyped string before it is journaled.
func (c *Client) CreateSnapshot(ctx context.Context) (string, error) {
	snapshot, err := c.CreateSnapshotIdentity(ctx)
	return snapshot.Name, err
}

// CreateSnapshotIdentity triggers snapshot creation and returns its opaque
// typed identity.
func (c *Client) CreateSnapshotIdentity(ctx context.Context) (SnapshotIdentity, error) {
	url := fmt.Sprintf("%s/collections/%s/snapshots", c.url, c.collection)
	respBody, err := c.postJSON(ctx, url, nil)
	if err != nil {
		return SnapshotIdentity{}, err
	}

	var result struct {
		Result struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return SnapshotIdentity{}, fmt.Errorf("decode snapshot response: %w", err)
	}
	if err := validateMutationResponse(respBody, false); err != nil {
		return SnapshotIdentity{}, fmt.Errorf("create snapshot: %w", err)
	}
	if result.Result.Name == "" {
		return SnapshotIdentity{}, fmt.Errorf("create snapshot: qdrant response did not include a snapshot name")
	}
	return SnapshotIdentity{Name: result.Result.Name}, nil
}

// ListSnapshots returns all snapshot names.
func (c *Client) ListSnapshots(ctx context.Context) ([]string, error) {
	snapshots, err := c.ListSnapshotIdentities(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(snapshots))
	for i, snapshot := range snapshots {
		names[i] = snapshot.Name
	}
	return names, nil
}

// ListSnapshotIdentities returns typed opaque snapshot identities.
func (c *Client) ListSnapshotIdentities(ctx context.Context) ([]SnapshotIdentity, error) {
	url := fmt.Sprintf("%s/collections/%s/snapshots", c.url, c.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := readLimitedBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s failed (status %d): %s", url, resp.StatusCode, string(b))
	}

	var result struct {
		Result []struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, err
	}

	snapshots := make([]SnapshotIdentity, len(result.Result))
	for i, s := range result.Result {
		if s.Name == "" {
			return nil, fmt.Errorf("snapshot list included an empty snapshot name")
		}
		snapshots[i] = SnapshotIdentity{Name: s.Name}
	}
	return snapshots, nil
}

// EnsureSnapshotArchive downloads a snapshot to an operator-controlled path
// outside Qdrant's ordinary retention set. On retries, expectedSHA256 makes the
// existing archive immutable and proves that the same recovery artifact is
// still available before deletion continues.
func (c *Client) EnsureSnapshotArchive(ctx context.Context, snapshot SnapshotIdentity, destination, expectedSHA256 string) (string, error) {
	if strings.TrimSpace(snapshot.Name) == "" || strings.TrimSpace(snapshot.Name) != snapshot.Name {
		return "", fmt.Errorf("snapshot name is required")
	}
	archive, err := openSnapshotArchiveDestination(destination)
	if err != nil {
		return "", err
	}
	defer archive.close()
	if expectedSHA256 != "" {
		return verifySnapshotArchive(archive, expectedSHA256)
	}
	existingDigest, exists, err := archive.digest()
	if err != nil {
		return "", err
	}

	requestURL := fmt.Sprintf("%s/collections/%s/snapshots/%s", c.url, c.collection, url.PathEscape(snapshot.Name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBodyBytes))
		return "", fmt.Errorf("download snapshot failed (status %d)", resp.StatusCode)
	}

	tmp, tmpName, err := archive.createTemp()
	if err != nil {
		return "", err
	}
	defer archive.removeTemp(tmpName)
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), resp.Body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("download snapshot archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync snapshot archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close snapshot archive: %w", err)
	}
	downloadedDigest := fmt.Sprintf("%x", hash.Sum(nil))
	if exists {
		if existingDigest != downloadedDigest {
			return "", fmt.Errorf("existing snapshot archive does not match downloaded snapshot")
		}
		return downloadedDigest, nil
	}
	if err := archive.publish(tmpName); err != nil {
		return "", fmt.Errorf("publish snapshot archive: %w", err)
	}
	return downloadedDigest, nil
}

func verifySnapshotArchive(archive *snapshotArchiveDestination, expectedSHA256 string) (string, error) {
	if len(expectedSHA256) != sha256.Size*2 || strings.ToLower(expectedSHA256) != expectedSHA256 {
		return "", fmt.Errorf("snapshot archive checksum is invalid")
	}
	actual, exists, err := archive.digest()
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("snapshot archive does not exist")
	}
	if actual != expectedSHA256 {
		return "", fmt.Errorf("snapshot archive checksum mismatch")
	}
	return actual, nil
}

func snapshotArchiveFileDigest(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect snapshot archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("snapshot archive is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("snapshot archive permissions must be 0600")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("read snapshot archive: %w", err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// DeleteSnapshot removes a snapshot by name.
func (c *Client) DeleteSnapshot(ctx context.Context, name string) error {
	requestURL := fmt.Sprintf("%s/collections/%s/snapshots/%s", c.url, c.collection, url.PathEscape(name))
	return c.mutate(ctx, http.MethodDelete, requestURL, nil, false, false)
}

// --- HTTP helpers ---

func (c *Client) mutate(ctx context.Context, method, requestURL string, body interface{}, wait, allowNotFound bool) error {
	if wait {
		parsed, err := url.Parse(requestURL)
		if err != nil {
			return fmt.Errorf("parse mutation URL: %w", err)
		}
		query := parsed.Query()
		query.Set("wait", "true")
		parsed.RawQuery = query.Encode()
		requestURL = parsed.String()
	}

	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, requestURL, err)
	}
	defer resp.Body.Close()
	respBody, err := readLimitedBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, requestURL, err)
	}
	if allowNotFound && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s failed (status %d): %s", method, requestURL, resp.StatusCode, string(respBody))
	}
	if err := validateMutationResponse(respBody, wait); err != nil {
		return fmt.Errorf("%s %s: %w", method, requestURL, err)
	}
	return nil
}

func validateMutationResponse(body []byte, requireCompleted bool) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("empty qdrant mutation response")
	}
	var response struct {
		Status interface{} `json:"status"`
		Result interface{} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("decode qdrant mutation response: %w", err)
	}

	validated := false
	if response.Status != nil {
		status, ok := response.Status.(string)
		if !ok || status != "ok" {
			return fmt.Errorf("qdrant mutation status is %v, want ok", response.Status)
		}
		validated = true
	}
	completed := false
	if result, ok := response.Result.(map[string]interface{}); ok {
		if rawStatus, exists := result["status"]; exists {
			status, ok := rawStatus.(string)
			if !ok || status != "completed" {
				return fmt.Errorf("qdrant operation status is %v, want completed", rawStatus)
			}
			completed = true
			validated = true
		}
	}
	if results, ok := response.Result.([]interface{}); ok && len(results) > 0 {
		completed = true
		for _, rawResult := range results {
			result, ok := rawResult.(map[string]interface{})
			if !ok {
				return fmt.Errorf("qdrant batch result is %T, want object", rawResult)
			}
			status, ok := result["status"].(string)
			if !ok || status != "completed" {
				return fmt.Errorf("qdrant batch operation status is %v, want completed", result["status"])
			}
		}
		validated = true
	}
	if requireCompleted && !completed {
		return fmt.Errorf("qdrant mutation response does not confirm a completed operation")
	}
	if !validated {
		return fmt.Errorf("qdrant mutation response contains no verifiable status")
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, url string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reqBody)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := readLimitedBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s failed (status %d): %s", url, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func readLimitedBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return body, nil
}

func (c *Client) postDiscard(ctx context.Context, url string, body interface{}) error {
	_, err := c.postJSON(ctx, url, body)
	return err
}
