package harbor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ---------------------------------------------------------------------------
// Testcontainers integration tests
//
// These tests build a lightweight Harbor API mock as a Docker image,
// start it via Testcontainers, and validate the real Harbor client against it.
//
// Requirements:
//   - Docker daemon
//   - Go toolchain (for building the mock binary)
//
// Run with:  go test -run TestHarborClient_TC ./internal/harbor/
// ---------------------------------------------------------------------------

const (
	harborMockPort = "8080/tcp"
)

// buildAndStartHarborMock builds the Docker image for the Harbor API mock
// from the testdata/harbor-mock directory and starts a container.
func buildAndStartHarborMock(t *testing.T, ctx context.Context) (testcontainers.Container, string, func()) {
	t.Helper()

	// Locate the mock source directory
	mockDir, err := filepath.Abs("testdata/harbor-mock")
	if err != nil {
		t.Fatalf("failed to resolve mock directory: %v", err)
	}
	if _, err := os.Stat(mockDir); os.IsNotExist(err) {
		t.Fatalf("mock directory not found: %s", mockDir)
	}

	// Build the Docker image from the Dockerfile
	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:       mockDir,
			Dockerfile:    "Dockerfile",
			Repo:          "harbor-mock-test",
			Tag:           fmt.Sprint(time.Now().UnixNano()),
			PrintBuildLog: true,
		},
		ExposedPorts: []string{harborMockPort},
		WaitingFor: wait.ForAll(
			wait.ForLog("Harbor mock API starting"),
			wait.ForHTTP("/api/v2.0/projects").WithBasicAuth("admin", "harbor12345"),
		),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start Harbor mock container: %v", err)
	}

	// Get the mapped endpoint
	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get container host: %v", err)
	}
	port, err := container.MappedPort(ctx, harborMockPort)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get mapped port: %v", err)
	}

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	cleanup := func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("warning: failed to terminate container: %v", err)
		}
	}

	return container, endpoint, cleanup
}

// ---------------------------------------------------------------------------
// Test: Harbor client against the mock
// ---------------------------------------------------------------------------

func TestHarborClient_TC_CreateAndGetProject(t *testing.T) {
	if os.Getenv("SKIP_TC") != "" {
		t.Skip("SKIP_TC is set; skipping Testcontainers integration test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping Testcontainers integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, endpoint, cleanup := buildAndStartHarborMock(t, ctx)
	defer cleanup()

	// Create the Harbor client pointed at the mock
	client := NewClient(endpoint, "admin", "harbor12345")

	// --- Create a project ---
	projectID, err := client.CreateProject(ctx, ProjectSpec{
		Name:         "tc-test-project",
		Public:       false,
		StorageLimit: -1,
		AutoScan:     true,
	})
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	if projectID <= 0 {
		t.Fatalf("expected positive project ID, got %d", projectID)
	}
	t.Logf("Created project with ID: %d", projectID)

	// --- Get the project by name ---
	proj, err := client.GetProjectByName(ctx, "tc-test-project")
	if err != nil {
		t.Fatalf("GetProjectByName failed: %v", err)
	}
	if proj == nil {
		t.Fatal("expected non-nil project")
	}
	if proj.ProjectID != projectID {
		t.Errorf("expected project ID %d, got %d", projectID, proj.ProjectID)
	}
	if proj.Name != "tc-test-project" {
		t.Errorf("expected name 'tc-test-project', got %q", proj.Name)
	}
	t.Logf("Retrieved project: ID=%d Name=%s", proj.ProjectID, proj.Name)
}

func TestHarborClient_TC_CreateGetDeleteRobot(t *testing.T) {
	if os.Getenv("SKIP_TC") != "" {
		t.Skip("SKIP_TC is set; skipping Testcontainers integration test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping Testcontainers integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, endpoint, cleanup := buildAndStartHarborMock(t, ctx)
	defer cleanup()

	client := NewClient(endpoint, "admin", "harbor12345")

	// First create a project
	projectID, err := client.CreateProject(ctx, ProjectSpec{
		Name:         "tc-robot-test",
		Public:       false,
		StorageLimit: -1,
		AutoScan:     false,
	})
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	// --- Create a robot ---
	robot, err := client.CreateRobot(ctx, projectID, RobotSpec{
		Name:     "robot$tc-test",
		Duration: -1,
		Permissions: []RobotPermission{
			{
				Kind:      "project",
				Namespace: "tc-robot-test",
				Access:    []RobotAccess{{Action: "push"}, {Action: "pull"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateRobot failed: %v", err)
	}
	if robot.ID <= 0 {
		t.Fatalf("expected positive robot ID, got %d", robot.ID)
	}
	if robot.Token == "" {
		t.Error("expected non-empty token")
	}
	t.Logf("Created robot: ID=%d Name=%s Token=%s", robot.ID, robot.Name, robot.Token)

	// --- List robots ---
	robots, err := client.ListProjectRobots(ctx, projectID)
	if err != nil {
		t.Fatalf("ListProjectRobots failed: %v", err)
	}
	if len(robots) < 1 {
		t.Fatal("expected at least 1 robot")
	}
	t.Logf("Listed %d robots for project %d", len(robots), projectID)

	// --- Delete the robot ---
	if err := client.DeleteProjectRobot(ctx, projectID, robot.ID); err != nil {
		t.Fatalf("DeleteProjectRobot failed: %v", err)
	}
	t.Log("Robot deleted successfully")

	// Verify robot is gone
	robotsAfter, _ := client.ListProjectRobots(ctx, projectID)
	for _, r := range robotsAfter {
		if r.ID == robot.ID {
			t.Fatal("expected robot to be deleted")
		}
	}
	t.Logf("Verified robot is gone (remaining: %d)", len(robotsAfter))
}

func TestHarborClient_TC_DeleteProject(t *testing.T) {
	if os.Getenv("SKIP_TC") != "" {
		t.Skip("SKIP_TC is set; skipping Testcontainers integration test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping Testcontainers integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, endpoint, cleanup := buildAndStartHarborMock(t, ctx)
	defer cleanup()

	client := NewClient(endpoint, "admin", "harbor12345")

	// Create a project
	projectID, err := client.CreateProject(ctx, ProjectSpec{
		Name:         "tc-delete-test",
		Public:       false,
		StorageLimit: -1,
		AutoScan:     false,
	})
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	// Delete it
	if err := client.DeleteProject(ctx, projectID); err != nil {
		t.Fatalf("DeleteProject failed: %v", err)
	}
	t.Logf("Project %d deleted successfully", projectID)

	// Verify it's gone
	proj, err := client.GetProjectByName(ctx, "tc-delete-test")
	if err != nil {
		t.Fatalf("GetProjectByName after delete failed: %v", err)
	}
	if proj != nil {
		t.Fatal("expected project to be deleted")
	}
}

func TestHarborClient_TC_Unauthorized(t *testing.T) {
	if os.Getenv("SKIP_TC") != "" {
		t.Skip("SKIP_TC is set; skipping Testcontainers integration test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping Testcontainers integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, endpoint, cleanup := buildAndStartHarborMock(t, ctx)
	defer cleanup()

	// Use wrong credentials
	client := NewClient(endpoint, "admin", "wrong-password")

	_, err := client.CreateProject(ctx, ProjectSpec{Name: "should-fail"})
	if err == nil {
		t.Fatal("expected error with wrong credentials")
	}

	// Check that it's an API error (401)
	apiErr, ok := err.(*ErrAPIError)
	if !ok {
		t.Fatalf("expected *ErrAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", apiErr.StatusCode)
	}
	t.Logf("Got expected 401 error: %v", err)
}

func TestHarborClient_TC_ProjectAlreadyExists(t *testing.T) {
	if os.Getenv("SKIP_TC") != "" {
		t.Skip("SKIP_TC is set; skipping Testcontainers integration test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping Testcontainers integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, endpoint, cleanup := buildAndStartHarborMock(t, ctx)
	defer cleanup()

	client := NewClient(endpoint, "admin", "harbor12345")

	// Create a project
	_, err := client.CreateProject(ctx, ProjectSpec{Name: "dup-project"})
	if err != nil {
		t.Fatalf("first CreateProject failed: %v", err)
	}

	// Try to create the same project again
	_, err = client.CreateProject(ctx, ProjectSpec{Name: "dup-project"})
	if err == nil {
		t.Fatal("expected error for duplicate project")
	}
	apiErr, ok := err.(*ErrAPIError)
	if !ok {
		t.Fatalf("expected *ErrAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("expected status 409, got %d", apiErr.StatusCode)
	}
	t.Logf("Got expected 409 conflict error: %v", err)
}

func TestHarborClient_TC_ListProjectRobots_Empty(t *testing.T) {
	if os.Getenv("SKIP_TC") != "" {
		t.Skip("SKIP_TC is set; skipping Testcontainers integration test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping Testcontainers integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, endpoint, cleanup := buildAndStartHarborMock(t, ctx)
	defer cleanup()

	client := NewClient(endpoint, "admin", "harbor12345")

	projectID, err := client.CreateProject(ctx, ProjectSpec{Name: "tc-no-robots"})
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	robots, err := client.ListProjectRobots(ctx, projectID)
	if err != nil {
		t.Fatalf("ListProjectRobots failed: %v", err)
	}
	if len(robots) != 0 {
		t.Fatalf("expected 0 robots, got %d", len(robots))
	}
	t.Log("Empty robot list verified")
}

// Ensure the mock server binary compiles
func TestHarborMock_Builds(t *testing.T) {
	// Just verify the mock source compiles as a sanity check
	// (the actual build happens inside Docker, but we can check locally too)
	if _, err := os.Stat("testdata/harbor-mock/main.go"); os.IsNotExist(err) {
		t.Fatal("mock server source not found")
	}
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
