package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const collectionName = "memory"

type Client struct {
	url        string
	httpClient *http.Client
}

func NewClient(url string) *Client {
	return &Client{
		url:        url,
		httpClient: &http.Client{},
	}
}

// Point represents a Qdrant point with vector and payload.
type Point struct {
	ID      string                 `json:"id"`
	Vector  []float32              `json:"vector,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
	Score   float64                `json:"score,omitempty"`
}

// EnsureCollection creates the collection if it doesn't exist.
func (c *Client) EnsureCollection(ctx context.Context, vectorSize int) error {
	// Check if collection exists.
	url := fmt.Sprintf("%s/collections/%s", c.url, collectionName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("check collection: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil // already exists
	}

	// Create collection.
	body := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	}
	return c.put(ctx, url, body)
}

// Upsert inserts or updates a point.
func (c *Client) Upsert(ctx context.Context, point Point) error {
	url := fmt.Sprintf("%s/collections/%s/points", c.url, collectionName)
	body := map[string]interface{}{
		"points": []map[string]interface{}{
			{
				"id":      point.ID,
				"vector":  point.Vector,
				"payload": point.Payload,
			},
		},
	}
	return c.putWithMethod(ctx, http.MethodPut, url, body)
}

// Search performs a vector similarity search with optional filters.
func (c *Client) Search(ctx context.Context, vector []float32, limit int, filters map[string]interface{}, scoreThreshold *float64) ([]Point, error) {
	url := fmt.Sprintf("%s/collections/%s/points/search", c.url, collectionName)
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

	var result struct {
		Result []struct {
			ID      string                 `json:"id"`
			Score   float64                `json:"score"`
			Payload map[string]interface{} `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	points := make([]Point, len(result.Result))
	for i, r := range result.Result {
		points[i] = Point{
			ID:      r.ID,
			Score:   r.Score,
			Payload: r.Payload,
		}
	}
	return points, nil
}

// ScrollResult holds a page of scroll results.
type ScrollResult struct {
	Points []ScrollPoint `json:"points"`
	Offset *string       `json:"next_page_offset"`
}

// ScrollPoint is a point returned by scroll (may include vector).
type ScrollPoint struct {
	ID      string                 `json:"id"`
	Vector  []float32              `json:"vector,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// Scroll paginates through all points, optionally with vectors.
func (c *Client) Scroll(ctx context.Context, limit int, offset *string, filters map[string]interface{}, withVector bool) (*ScrollResult, error) {
	url := fmt.Sprintf("%s/collections/%s/points/scroll", c.url, collectionName)
	body := map[string]interface{}{
		"limit":        limit,
		"with_payload": true,
		"with_vector":  withVector,
	}
	if offset != nil {
		body["offset"] = *offset
	}
	if filters != nil {
		body["filter"] = filters
	}

	respBody, err := c.postJSON(ctx, url, body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Result ScrollResult `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode scroll response: %w", err)
	}
	return &result.Result, nil
}

// ScrollAll retrieves all points by paginating through scroll.
func (c *Client) ScrollAll(ctx context.Context, filters map[string]interface{}, withVector bool) ([]ScrollPoint, error) {
	var all []ScrollPoint
	var offset *string
	for {
		result, err := c.Scroll(ctx, 100, offset, filters, withVector)
		if err != nil {
			return nil, err
		}
		all = append(all, result.Points...)
		if result.Offset == nil {
			break
		}
		offset = result.Offset
	}
	return all, nil
}

// Delete removes points by IDs.
func (c *Client) Delete(ctx context.Context, ids []string) error {
	url := fmt.Sprintf("%s/collections/%s/points/delete", c.url, collectionName)
	body := map[string]interface{}{
		"points": ids,
	}
	return c.postDiscard(ctx, url, body)
}

// SetPayload updates payload fields on a point without re-embedding.
func (c *Client) SetPayload(ctx context.Context, id string, payload map[string]interface{}) error {
	url := fmt.Sprintf("%s/collections/%s/points/payload", c.url, collectionName)
	body := map[string]interface{}{
		"payload": payload,
		"points":  []string{id},
	}
	return c.putWithMethod(ctx, http.MethodPut, url, body)
}

// CreateSnapshot triggers a snapshot creation.
func (c *Client) CreateSnapshot(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/collections/%s/snapshots", c.url, collectionName)
	respBody, err := c.postJSON(ctx, url, nil)
	if err != nil {
		return "", err
	}

	var result struct {
		Result struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode snapshot response: %w", err)
	}
	return result.Result.Name, nil
}

// ListSnapshots returns all snapshot names.
func (c *Client) ListSnapshots(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/collections/%s/snapshots", c.url, collectionName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Result []struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, err
	}

	names := make([]string, len(result.Result))
	for i, s := range result.Result {
		names[i] = s.Name
	}
	return names, nil
}

// DeleteSnapshot removes a snapshot by name.
func (c *Client) DeleteSnapshot(ctx context.Context, name string) error {
	url := fmt.Sprintf("%s/collections/%s/snapshots/%s", c.url, collectionName, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// --- HTTP helpers ---

func (c *Client) put(ctx context.Context, url string, body interface{}) error {
	return c.putWithMethod(ctx, http.MethodPut, url, body)
}

func (c *Client) putWithMethod(ctx context.Context, method, url string, body interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s failed (status %d): %s", method, url, resp.StatusCode, string(respBody))
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s failed (status %d): %s", url, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (c *Client) postDiscard(ctx context.Context, url string, body interface{}) error {
	_, err := c.postJSON(ctx, url, body)
	return err
}
