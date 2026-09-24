package harbor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client is the Harbor admin API client
type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

// NewClient creates a new Harbor admin client
func NewClient(baseURL, username, password string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		username:   username,
		password:   password,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// --- Project API ---

// CreateProject creates a new Harbor project and returns its ID
// NOTE: storage_limit is a top-level field in Harbor v2.x API, not inside metadata.
func (c *Client) CreateProject(ctx context.Context, spec ProjectSpec) (int64, error) {
	body := map[string]interface{}{
		"project_name":  spec.Name,
		"storage_limit": spec.StorageLimit,
		"metadata": map[string]interface{}{
			"public":    strconv.FormatBool(spec.Public),
			"auto_scan": strconv.FormatBool(spec.AutoScan),
		},
	}
	resp, err := c.post(ctx, "/api/v2.0/projects", body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, &ErrAPIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	// From the Location header, get the project ID
	location := resp.Header.Get("Location")
	if location == "" {
		return 0, fmt.Errorf("harbor: no Location header in CreateProject response")
	}
	// /api/v2.0/projects/1
	id, err := strconv.ParseInt(filepath.Base(location), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("harbor: failed to parse project ID from Location header: %w", err)
	}
	return id, nil
}

// GetProjectByName retrieves a project by its name. Returns nil if not found.
func (c *Client) GetProjectByName(ctx context.Context, name string) (*Project, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/api/v2.0/projects?name=%s", url.QueryEscape(name)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &ErrAPIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	var projects []Project
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		return nil, fmt.Errorf("harbor: failed to decode projects list: %w", err)
	}
	if len(projects) == 0 {
		return nil, nil
	}
	return &projects[0], nil
}

// DeleteProject deletes a Harbor project by ID
func (c *Client) DeleteProject(ctx context.Context, projectID int64) error {
	resp, err := c.delete(ctx, fmt.Sprintf("/api/v2.0/projects/%d", projectID))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &ErrAPIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}
	return nil
}

// UpdateProject updates an existing Harbor project's properties (public, auto_scan, storage_limit)
func (c *Client) UpdateProject(ctx context.Context, projectID int64, spec ProjectSpec) error {
	body := map[string]interface{}{
		"project_name":  spec.Name,
		"storage_limit": spec.StorageLimit,
		"metadata": map[string]interface{}{
			"public":    strconv.FormatBool(spec.Public),
			"auto_scan": strconv.FormatBool(spec.AutoScan),
		},
	}
	resp, err := c.put(ctx, fmt.Sprintf("/api/v2.0/projects/%d", projectID), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &ErrAPIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}
	return nil
}

// --- Robot Account API ---

// CreateRobot creates a robot account for a project and returns the full RobotAccount (ID + Name + Token/Secret).
// The token/secret is only returned on creation.
func (c *Client) CreateRobot(ctx context.Context, projectID int64, spec RobotSpec) (*RobotAccount, error) {
	body := map[string]interface{}{
		"name":        spec.Name,
		"duration":    spec.Duration,
		"level":       "project",
		"project_id":  projectID,
		"permissions": spec.Permissions,
	}
	resp, err := c.post(ctx, "/api/v2.0/robots", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &ErrAPIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	var robot RobotAccount
	if err := json.NewDecoder(resp.Body).Decode(&robot); err != nil {
		return nil, fmt.Errorf("harbor: failed to decode robot account: %w", err)
	}
	// If we got a secret but no token, populate token from secret
	if robot.Token == "" && robot.Secret != "" {
		robot.Token = robot.Secret
	}
	return &robot, nil
}

// ListProjectRobots lists all robot accounts for a project
func (c *Client) ListProjectRobots(ctx context.Context, projectID int64) ([]RobotAccount, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/api/v2.0/robots?project_id=%d", projectID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &ErrAPIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	var robots []RobotAccount
	if err := json.NewDecoder(resp.Body).Decode(&robots); err != nil {
		return nil, fmt.Errorf("harbor: failed to decode robot accounts: %w", err)
	}
	return robots, nil
}

// DeleteProjectRobot deletes a specific robot account by ID
func (c *Client) DeleteProjectRobot(ctx context.Context, projectID int64, robotID int64) error {
	resp, err := c.delete(ctx, fmt.Sprintf("/api/v2.0/robots/%d", robotID))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &ErrAPIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}
	return nil
}

// --- HTTP Helpers ---

func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("harbor: failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("harbor: failed to create request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("harbor: request failed: %w", err)
	}

	return resp, nil
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	return c.doRequest(ctx, http.MethodGet, path, nil)
}

func (c *Client) post(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	return c.doRequest(ctx, http.MethodPost, path, body)
}

func (c *Client) delete(ctx context.Context, path string) (*http.Response, error) {
	return c.doRequest(ctx, http.MethodDelete, path, nil)
}

func (c *Client) put(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	return c.doRequest(ctx, http.MethodPut, path, body)
}
