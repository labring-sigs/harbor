package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// In-memory store simulating Harbor resources
type project struct {
	ID   int64  `json:"project_id"`
	Name string `json:"name"`
}

type robotAccount struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Secret string `json:"secret,omitempty"`
}

type store struct {
	mu       sync.Mutex
	projects map[string]*project
	robots   map[int64][]*robotAccount
	nextPID  int64
	nextRID  int64
	now      func() time.Time
}

func newStore() *store {
	return &store{
		projects: make(map[string]*project),
		robots:   make(map[int64][]*robotAccount),
		nextPID:  1,
		nextRID:  1,
		now:      time.Now,
	}
}

var globalStore = newStore()

// writeJSON writes v as JSON and sets Content-Type.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("error encoding response: %v", err)
	}
}

// parseID extracts an integer ID from the last segment of the URL path.
// For example, "/api/v2.0/projects/42" -> 42.
func parseID(path string) (int64, bool) {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) == 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	return id, err == nil
}

func handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// GET /api/v2.0/projects?name=xxx
		name := r.URL.Query().Get("name")
		globalStore.mu.Lock()
		defer globalStore.mu.Unlock()

		if name != "" {
			p, ok := globalStore.projects[name]
			if !ok {
				writeJSON(w, http.StatusOK, []project{})
				return
			}
			writeJSON(w, http.StatusOK, []*project{p})
			return
		}
		// Return all projects
		all := make([]*project, 0, len(globalStore.projects))
		for _, p := range globalStore.projects {
			all = append(all, p)
		}
		writeJSON(w, http.StatusOK, all)

	case http.MethodPost:
		// POST /api/v2.0/projects
		var req struct {
			Name string `json:"project_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}

		globalStore.mu.Lock()
		defer globalStore.mu.Unlock()

		if _, exists := globalStore.projects[req.Name]; exists {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "project already exists"})
			return
		}

		id := globalStore.nextPID
		globalStore.nextPID++
		p := &project{ID: id, Name: req.Name}
		globalStore.projects[req.Name] = p

		w.Header().Set("Location", fmt.Sprintf("/api/v2.0/projects/%d", id))
		w.WriteHeader(http.StatusCreated)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func handleProjectByID(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseID(r.URL.Path)
	if !ok || projectID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid project ID"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		// PUT /api/v2.0/projects/{id} — update project metadata
		var req struct {
			StorageLimit int64                  `json:"storage_limit"`
			Metadata     map[string]interface{} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}

		globalStore.mu.Lock()
		defer globalStore.mu.Unlock()

		for _, p := range globalStore.projects {
			if p.ID == projectID {
				// In the mock we don't store metadata beyond the project name/ID,
				// but we acknowledge the update as successful.
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})

	case http.MethodDelete:
		globalStore.mu.Lock()
		defer globalStore.mu.Unlock()

		// Find and delete the project
		for name, p := range globalStore.projects {
			if p.ID == projectID {
				delete(globalStore.projects, name)
				delete(globalStore.robots, projectID)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleRobots handles the unified robot API endpoints:
//   - GET  /api/v2.0/robots?project_id=<id>  — list robots for a project
//   - POST /api/v2.0/robots                   — create a robot (level + project_id in body)
func handleRobots(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// GET /api/v2.0/robots?project_id=<id>
		projectIDStr := r.URL.Query().Get("project_id")
		projectID, err := strconv.ParseInt(projectIDStr, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or missing project_id"})
			return
		}
		globalStore.mu.Lock()
		robots := globalStore.robots[projectID]
		if robots == nil {
			robots = []*robotAccount{}
		}
		globalStore.mu.Unlock()
		writeJSON(w, http.StatusOK, robots)

	case http.MethodPost:
		// POST /api/v2.0/robots
		var req struct {
			Name      string        `json:"name"`
			Duration  int64         `json:"duration"`
			Level     string        `json:"level"`
			ProjectID int64         `json:"project_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if req.Level != "project" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "level must be 'project'"})
			return
		}

		globalStore.mu.Lock()
		id := globalStore.nextRID
		globalStore.nextRID++
		robot := &robotAccount{
			ID:    id,
			Name:  req.Name,
			Secret: fmt.Sprintf("tc-mock-secret-%d", id),
		}
		globalStore.robots[req.ProjectID] = append(globalStore.robots[req.ProjectID], robot)
		globalStore.mu.Unlock()

		writeJSON(w, http.StatusCreated, robot)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleRobotByID handles DELETE /api/v2.0/robots/<robotID>
func handleRobotByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	robotID, ok := parseID(r.URL.Path)
	if !ok || robotID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid robot ID"})
		return
	}

	globalStore.mu.Lock()
	defer globalStore.mu.Unlock()

	// Search across all projects for the robot
	for projectID, robots := range globalStore.robots {
		for i, rbt := range robots {
			if rbt.ID == robotID {
				globalStore.robots[projectID] = append(robots[:i], robots[i+1:]...)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "robot not found"})
}

// mux routes requests to the appropriate handler.
func mux(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL.Path)

	// BasicAuth enforcement (matches the test client credentials)
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || pass != "harbor12345" {
		w.Header().Set("WWW-Authenticate", `Basic realm="Harbor"`)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	path := r.URL.Path

	switch {
	case path == "/api/v2.0/projects":
		handleProjects(w, r)

	case strings.Count(path, "/") == 4 && strings.HasPrefix(path, "/api/v2.0/projects/") && !strings.Contains(path, "/robots"):
		handleProjectByID(w, r)

	case path == "/api/v2.0/robots":
		handleRobots(w, r)

	case strings.HasPrefix(path, "/api/v2.0/robots/"):
		handleRobotByID(w, r)

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := fmt.Sprintf("0.0.0.0:%s", port)
	log.Printf("Harbor mock API starting on %s", addr)
	log.Fatal(http.ListenAndServe(addr, http.HandlerFunc(mux)))
}
