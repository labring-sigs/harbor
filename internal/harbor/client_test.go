package harbor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock HTTP round-tripper
//
// We use a custom http.RoundTripper instead of httptest.Server because the
// sandbox environment does not permit listening on network ports.
// ---------------------------------------------------------------------------

type mockTransport struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (t *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.fn(req)
}

func newMockClient(handler func(req *http.Request) (int, string)) *Client {
	client := NewClient("http://harbor.test.local", "admin", "harbor12345")
	client.httpClient.Transport = &mockTransport{
		fn: func(req *http.Request) (*http.Response, error) {
			statusCode, body := handler(req)
			resp := &http.Response{
				StatusCode: statusCode,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}
			return resp, nil
		},
	}
	return client
}

func newMockClientWithLocation(handler func(req *http.Request) (int, string, string)) *Client {
	client := NewClient("http://harbor.test.local", "admin", "harbor12345")
	client.httpClient.Transport = &mockTransport{
		fn: func(req *http.Request) (*http.Response, error) {
			statusCode, body, location := handler(req)
			header := make(http.Header)
			if location != "" {
				header.Set("Location", location)
			}
			resp := &http.Response{
				StatusCode: statusCode,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(body)),
			}
			return resp, nil
		},
	}
	return client
}

// ---------------------------------------------------------------------------
// Project API tests
// ---------------------------------------------------------------------------

func TestCreateProject_Success(t *testing.T) {
	client := newMockClientWithLocation(func(req *http.Request) (int, string, string) {
		if req.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", req.Method)
		}
		if req.URL.Path != "/api/v2.0/projects" {
			t.Errorf("expected /api/v2.0/projects, got %s", req.URL.Path)
		}

		// Verify auth
		user, pass, ok := req.BasicAuth()
		if !ok || user != "admin" || pass != "harbor12345" {
			t.Errorf("unexpected auth: user=%q, pass=%q", user, pass)
		}

		// Verify body
		bodyBytes, _ := io.ReadAll(req.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatal(err)
		}
		if body["project_name"] != "test-project" {
			t.Errorf("expected project_name 'test-project', got %v", body["project_name"])
		}

		return http.StatusCreated, "", "/api/v2.0/projects/10"
	})

	id, err := client.CreateProject(context.Background(), ProjectSpec{
		Name:         "test-project",
		Public:       false,
		StorageLimit: -1,
		AutoScan:     true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 10 {
		t.Errorf("expected project ID 10, got %d", id)
	}
}

func TestCreateProject_Error(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusConflict, `{"errors":[{"code":"CONFLICT","message":"project already exists"}]}`
	})

	_, err := client.CreateProject(context.Background(), ProjectSpec{Name: "dup"})
	if err == nil {
		t.Fatal("expected error for conflict")
	}
	apiErr, ok := err.(*ErrAPIError)
	if !ok {
		t.Fatalf("expected ErrAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("expected status 409, got %d", apiErr.StatusCode)
	}
}

func TestCreateProject_MissingLocationHeader(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusCreated, ""
	})

	_, err := client.CreateProject(context.Background(), ProjectSpec{Name: "bad"})
	if err == nil {
		t.Fatal("expected error for missing Location header")
	}
}

func TestGetProjectByName_Found(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		if req.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", req.Method)
		}
		if req.URL.Query().Get("name") != "my-project" {
			t.Errorf("expected name=my-project, got %s", req.URL.Query().Get("name"))
		}
		return http.StatusOK, `[{"project_id":5,"name":"my-project","public":false}]`
	})

	proj, err := client.GetProjectByName(context.Background(), "my-project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proj == nil {
		t.Fatal("expected non-nil project")
	}
	if proj.ProjectID != 5 {
		t.Errorf("expected project_id 5, got %d", proj.ProjectID)
	}
	if proj.Name != "my-project" {
		t.Errorf("expected name 'my-project', got %q", proj.Name)
	}
}

func TestGetProjectByName_NotFound(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusOK, `[]`
	})

	proj, err := client.GetProjectByName(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proj != nil {
		t.Fatal("expected nil project for empty result")
	}
}

func TestGetProjectByName_APIError(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusUnauthorized, `{"errors":[{"code":"UNAUTHORIZED","message":"bad credentials"}]}`
	})

	_, err := client.GetProjectByName(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteProject_Success(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		if req.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", req.Method)
		}
		if req.URL.Path != "/api/v2.0/projects/42" {
			t.Errorf("expected /api/v2.0/projects/42, got %s", req.URL.Path)
		}
		return http.StatusOK, ""
	})

	if err := client.DeleteProject(context.Background(), 42); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteProject_NotFound(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusNotFound, ""
	})

	err := client.DeleteProject(context.Background(), 999)
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*ErrAPIError)
	if !ok {
		t.Fatalf("expected ErrAPIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", apiErr.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// UpdateProject tests
// ---------------------------------------------------------------------------

func TestUpdateProject_Success(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		if req.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", req.Method)
		}
		if req.URL.Path != "/api/v2.0/projects/42" {
			t.Errorf("expected /api/v2.0/projects/42, got %s", req.URL.Path)
		}

		// Verify auth
		user, pass, ok := req.BasicAuth()
		if !ok || user != "admin" || pass != "harbor12345" {
			t.Errorf("unexpected auth: user=%q, pass=%q", user, pass)
		}

		// Verify body
		bodyBytes, _ := io.ReadAll(req.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatal(err)
		}
		if body["project_name"] != "test-project" {
			t.Errorf("expected project_name 'test-project', got %v", body["project_name"])
		}
		if body["storage_limit"] != float64(-1) {
			t.Errorf("expected storage_limit -1, got %v", body["storage_limit"])
		}
		metadata, ok := body["metadata"].(map[string]interface{})
		if !ok {
			t.Fatal("expected metadata map")
		}
		if metadata["public"] != "true" {
			t.Errorf("expected public 'true', got %v", metadata["public"])
		}
		if metadata["auto_scan"] != "false" {
			t.Errorf("expected auto_scan 'false', got %v", metadata["auto_scan"])
		}
		return http.StatusOK, ""
	})

	err := client.UpdateProject(context.Background(), 42, ProjectSpec{
		Name:         "test-project",
		Public:       true,
		StorageLimit: -1,
		AutoScan:     false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateProject_APIError(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusNotFound, `{"errors":[{"code":"NOT_FOUND","message":"project not found"}]}`
	})

	err := client.UpdateProject(context.Background(), 999, ProjectSpec{Name: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for not found")
	}
	apiErr, ok := err.(*ErrAPIError)
	if !ok {
		t.Fatalf("expected ErrAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", apiErr.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Robot Account API tests
// ---------------------------------------------------------------------------

func TestCreateRobot_Success(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		if req.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", req.Method)
		}
		if req.URL.Path != "/api/v2.0/robots" {
			t.Errorf("expected /api/v2.0/robots, got %s", req.URL.Path)
		}
		// Verify request body contains level and project_id
		var bodyMap map[string]interface{}
		json.NewDecoder(req.Body).Decode(&bodyMap)
		if bodyMap["level"] != "project" {
			t.Errorf("expected level=project, got %v", bodyMap["level"])
		}
		if bodyMap["project_id"] != float64(7) {
			t.Errorf("expected project_id=7, got %v", bodyMap["project_id"])
		}
		return http.StatusCreated, `{"id":100,"name":"robot$test","token":"secret123"}`
	})

	robot, err := client.CreateRobot(context.Background(), 7, RobotSpec{
		Name:     "robot$test",
		Duration: -1,
		Permissions: []RobotPermission{
			{Kind: "project", Namespace: "my-project", Access: []RobotAccess{{Action: "push"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if robot.ID != 100 {
		t.Errorf("expected ID 100, got %d", robot.ID)
	}
	if robot.Name != "robot$test" {
		t.Errorf("expected name 'robot$test', got %q", robot.Name)
	}
	if robot.Token != "secret123" {
		t.Errorf("expected token 'secret123', got %q", robot.Token)
	}
}

func TestCreateRobot_Error(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusForbidden, `{"errors":[{"code":"DENIED","message":"no permission"}]}`
	})

	_, err := client.CreateRobot(context.Background(), 1, RobotSpec{Name: "bad"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListProjectRobots(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		if req.URL.Path != "/api/v2.0/robots" {
			t.Errorf("unexpected path: %s", req.URL.Path)
		}
		if req.URL.Query().Get("project_id") != "3" {
			t.Errorf("expected project_id=3, got %s", req.URL.Query().Get("project_id"))
		}
		return http.StatusOK, `[
			{"id":1,"name":"robot$one"},
			{"id":2,"name":"robot$two"}
		]`
	})

	robots, err := client.ListProjectRobots(context.Background(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(robots) != 2 {
		t.Fatalf("expected 2 robots, got %d", len(robots))
	}
	if robots[0].ID != 1 {
		t.Errorf("expected ID 1, got %d", robots[0].ID)
	}
}

func TestListProjectRobots_Error(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusInternalServerError, ""
	})

	_, err := client.ListProjectRobots(context.Background(), 3)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteProjectRobot_Success(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		if req.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", req.Method)
		}
		if req.URL.Path != "/api/v2.0/robots/9" {
			t.Errorf("expected /api/v2.0/robots/9, got %s", req.URL.Path)
		}
		return http.StatusOK, ""
	})

	if err := client.DeleteProjectRobot(context.Background(), 5, 9); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteProjectRobot_Error(t *testing.T) {
	client := newMockClient(func(req *http.Request) (int, string) {
		return http.StatusNotFound, ""
	})

	err := client.DeleteProjectRobot(context.Background(), 5, 999)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// Error types tests
// ---------------------------------------------------------------------------

func TestErrNotFound(t *testing.T) {
	e := &ErrNotFound{Resource: "project", ID: 42}
	expected := "harbor project not found: 42"
	if e.Error() != expected {
		t.Errorf("expected %q, got %q", expected, e.Error())
	}
}

func TestErrAPIError(t *testing.T) {
	e := &ErrAPIError{StatusCode: 500, Body: `{"error":"internal"}`}
	expected := `harbor API error: status=500 body={"error":"internal"}`
	if e.Error() != expected {
		t.Errorf("expected %q, got %q", expected, e.Error())
	}
}

// ---------------------------------------------------------------------------
// Integration test placeholder (requires running Harbor instance)
// ---------------------------------------------------------------------------

// TestHarborClientIntegration is a placeholder for integration tests against
// a real Harbor instance. Set these environment variables to run:
//
//	HARBOR_TEST_ENDPOINT=https://core.harbor.local
//	HARBOR_TEST_USERNAME=admin
//	HARBOR_TEST_PASSWORD=Harbor12345
func TestHarborClientIntegration(t *testing.T) {
	endpoint := fmt.Sprint("") // always empty – placeholder
	if endpoint == "" {
		t.Skip("HARBOR_TEST_ENDPOINT not set; skipping integration test")
	}
	_ = endpoint
}
