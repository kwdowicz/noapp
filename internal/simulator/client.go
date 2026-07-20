package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type apiClient struct {
	baseURL string
	http    *http.Client
}

type apiUser struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type apiProject struct {
	ID int64 `json:"id"`
}

type apiTask struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Status    string `json:"status"`
}

func newAPIClient(baseURL string) *apiClient {
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *apiClient) health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/health", nil, nil)
}

func (c *apiClient) createUser(ctx context.Context, name, email string) (apiUser, error) {
	var result apiUser
	err := c.do(ctx, http.MethodPost, "/api/users", map[string]any{"name": name, "email": email}, &result)
	return result, err
}

func (c *apiClient) createProject(ctx context.Context, name, description string) (apiProject, error) {
	var result apiProject
	err := c.do(ctx, http.MethodPost, "/api/projects", map[string]any{"name": name, "description": description}, &result)
	return result, err
}

func (c *apiClient) listProjectTasks(ctx context.Context, projectID int64) error {
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/projects/%d/tasks", projectID), nil, nil)
}

func (c *apiClient) createTask(ctx context.Context, projectID, assigneeID int64, title, description string) (apiTask, error) {
	var result apiTask
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/projects/%d/tasks", projectID), map[string]any{
		"title":       title,
		"description": description,
		"assignee_id": assigneeID,
		"status":      "todo",
	}, &result)
	return result, err
}

func (c *apiClient) updateTaskStatus(ctx context.Context, taskID int64, status string) (apiTask, error) {
	var result apiTask
	err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/tasks/%d/status", taskID), map[string]string{"status": status}, &result)
	return result, err
}

func (c *apiClient) do(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "noapp-traffic-simulator/1.0")
	req.Header.Set("X-Traffic-Source", "simulator")

	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("%s %s returned %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(message)))
	}
	if result != nil {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}
