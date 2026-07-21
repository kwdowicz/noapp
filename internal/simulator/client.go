package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type apiClient struct {
	baseURL string
	http    *http.Client
	oauth   OAuthConfig
	mu      sync.Mutex
	token   string
	expires time.Time
}

type OAuthConfig struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
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

func newAPIClient(baseURL string, oauth OAuthConfig) *apiClient {
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
		oauth:   oauth,
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
	if path != "/api/health" && c.oauth.TokenURL != "" {
		token, err := c.accessToken(ctx)
		if err != nil {
			return fmt.Errorf("get API access token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

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

func (c *apiClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > 30*time.Second {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "noapp-traffic-simulator/1.0")
	req.SetBasicAuth(c.oauth.ClientID, c.oauth.ClientSecret)
	response, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&tokenResponse); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if response.StatusCode != http.StatusOK || tokenResponse.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned %d: %s", response.StatusCode, tokenResponse.Error)
	}
	c.token = tokenResponse.AccessToken
	c.expires = time.Now().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	return c.token, nil
}
