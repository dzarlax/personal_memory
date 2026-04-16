package todoist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const baseURL = "https://api.todoist.com/api/v1"

type Client struct {
	token      string
	httpClient *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
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
		return nil, fmt.Errorf("todoist %s %s: status %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (c *Client) GetProjects(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/projects", nil)
}

func (c *Client) GetLabels(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/labels", nil)
}

func (c *Client) GetTasks(ctx context.Context, projectID string, limit int) ([]byte, error) {
	path := fmt.Sprintf("/tasks?limit=%d", limit)
	if projectID != "" {
		path += "&project_id=" + projectID
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) GetTasksFiltered(ctx context.Context, filter string, limit int) ([]byte, error) {
	path := fmt.Sprintf("/tasks/filter?query=%s&limit=%d", filter, limit)
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) CreateTask(ctx context.Context, task map[string]interface{}) ([]byte, error) {
	return c.do(ctx, http.MethodPost, "/tasks", task)
}

func (c *Client) UpdateTask(ctx context.Context, taskID string, update map[string]interface{}) ([]byte, error) {
	return c.do(ctx, http.MethodPost, "/tasks/"+taskID, update)
}

func (c *Client) DeleteTask(ctx context.Context, taskID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/tasks/"+taskID, nil)
	return err
}

func (c *Client) CompleteTask(ctx context.Context, taskID string) error {
	_, err := c.do(ctx, http.MethodPost, "/tasks/"+taskID+"/close", nil)
	return err
}
