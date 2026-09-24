package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory stores
// ---------------------------------------------------------------------------

type project struct {
	ID   int64  `json:"project_id"`
	Name string `json:"name"`
}

type robotAccount struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Secret string `json:"secret,omitempty"`
}

type manifestEntry struct {
	content   []byte
	mediaType string
}

type store struct {
	mu        sync.Mutex
	projects  map[string]*project
	robots    map[int64][]*robotAccount
	nextPID   int64
	nextRID   int64
	now       func() time.Time

	// OCI blob storage
	blobMu    sync.RWMutex
	blobs     map[string][]byte // digest -> content

	// OCI manifest storage
	manMu     sync.RWMutex
	manifests map[string]map[string]manifestEntry // repo -> ref (tag/digest) -> manifest

	// Upload sessions
	uplMu     sync.Mutex
	uploads   map[string][]byte // uploadUUID -> accumulated bytes
}

func newStore() *store {
	return &store{
		projects:  make(map[string]*project),
		robots:    make(map[int64][]*robotAccount),
		nextPID:   1,
		nextRID:   1,
		now:       time.Now,
		blobs:     make(map[string][]byte),
		manifests: make(map[string]map[string]manifestEntry),
		uploads:   make(map[string][]byte),
	}
}

var globalStore = newStore()

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("error encoding response: %v", err)
	}
}

func writeData(w http.ResponseWriter, status int, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		log.Printf("error writing response data: %v", err)
	}
}

func parseID(path string) (int64, bool) {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) == 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	return id, err == nil
}

func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Harbor Management API handlers
// ---------------------------------------------------------------------------

func handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		name := r.URL.Query().Get("name")
		globalStore.mu.Lock()
		if name != "" {
			p, ok := globalStore.projects[name]
			globalStore.mu.Unlock()
			if !ok {
				writeJSON(w, http.StatusOK, []project{})
				return
			}
			writeJSON(w, http.StatusOK, []*project{p})
			return
		}
		all := make([]*project, 0, len(globalStore.projects))
		for _, p := range globalStore.projects {
			all = append(all, p)
		}
		globalStore.mu.Unlock()
		writeJSON(w, http.StatusOK, all)

	case http.MethodPost:
		var req struct {
			Name string `json:"project_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}

		globalStore.mu.Lock()
		if _, exists := globalStore.projects[req.Name]; exists {
			globalStore.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "project already exists"})
			return
		}

		id := globalStore.nextPID
		globalStore.nextPID++
		p := &project{ID: id, Name: req.Name}
		globalStore.projects[req.Name] = p
		globalStore.mu.Unlock()

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
	case http.MethodDelete:
		globalStore.mu.Lock()
		for name, p := range globalStore.projects {
			if p.ID == projectID {
				delete(globalStore.projects, name)
				delete(globalStore.robots, projectID)
				globalStore.mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		globalStore.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func handleRobots(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
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
		var req struct {
			Name      string `json:"name"`
			Duration  int64  `json:"duration"`
			Level     string `json:"level"`
			ProjectID int64  `json:"project_id"`
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
			ID:     id,
			Name:   req.Name,
			Secret: fmt.Sprintf("tc-mock-secret-%d", id),
		}
		globalStore.robots[req.ProjectID] = append(globalStore.robots[req.ProjectID], robot)
		globalStore.mu.Unlock()

		writeJSON(w, http.StatusCreated, robot)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

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

// ---------------------------------------------------------------------------
// OCI Distribution API v2 handlers
// ---------------------------------------------------------------------------

func handleV2Ping(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct{}{})
}

// handleV2Blob handles HEAD/GET/DELETE /v2/<name>/blobs/<digest>
func handleV2Blob(w http.ResponseWriter, r *http.Request, name, digest string) {
	switch r.Method {
	case http.MethodHead:
		globalStore.blobMu.RLock()
		_, ok := globalStore.blobs[digest]
		globalStore.blobMu.RUnlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		globalStore.blobMu.RLock()
		data, ok := globalStore.blobs[digest]
		globalStore.blobMu.RUnlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)

	case http.MethodDelete:
		globalStore.blobMu.Lock()
		delete(globalStore.blobs, digest)
		globalStore.blobMu.Unlock()
		w.WriteHeader(http.StatusAccepted)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleV2BlobUploads handles POST/PATCH/PUT for blob upload sessions.
func handleV2BlobUploads(w http.ResponseWriter, r *http.Request, name string) {
	path := r.URL.Path

	// POST /v2/<name>/blobs/uploads/
	if r.Method == http.MethodPost {
		uploadUUID := fmt.Sprintf("upload-%d", time.Now().UnixNano())
		location := fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadUUID)
		w.Header().Set("Location", location)
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Extract upload UUID from path: .../blobs/uploads/<uuid>
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	uploadUUID := parts[len(parts)-1]

	// PATCH /v2/<name>/blobs/uploads/<uuid>
	if r.Method == http.MethodPatch {
		chunkData, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read body"})
			return
		}

		globalStore.uplMu.Lock()
		globalStore.uploads[uploadUUID] = append(globalStore.uploads[uploadUUID], chunkData...)
		total := len(globalStore.uploads[uploadUUID])
		globalStore.uplMu.Unlock()

		w.Header().Set("Location", r.URL.Path)
		w.Header().Set("Range", fmt.Sprintf("0-%d", total-1))
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// PUT /v2/<name>/blobs/uploads/<uuid>?digest=<digest>
	if r.Method == http.MethodPut {
		digest := r.URL.Query().Get("digest")
		if digest == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing digest"})
			return
		}

		finalData, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read body"})
			return
		}

		globalStore.uplMu.Lock()
		fullData := append(globalStore.uploads[uploadUUID], finalData...)
		delete(globalStore.uploads, uploadUUID)
		globalStore.uplMu.Unlock()

		actualDigest := computeDigest(fullData)
		if actualDigest != digest {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("digest mismatch: expected %s, got %s", digest, actualDigest),
			})
			return
		}

		globalStore.blobMu.Lock()
		globalStore.blobs[digest] = fullData
		globalStore.blobMu.Unlock()

		location := fmt.Sprintf("/v2/%s/blobs/%s", name, digest)
		w.Header().Set("Location", location)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
		return
	}

	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

// handleV2Manifest handles GET/PUT/DELETE /v2/<name>/manifests/<reference>
func handleV2Manifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	switch r.Method {
	case http.MethodGet:
		globalStore.manMu.RLock()
		repo, ok := globalStore.manifests[name]
		if !ok {
			globalStore.manMu.RUnlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		entry, ok := repo[ref]
		if !ok {
			// Try looking up by digest if ref is a tag
			globalStore.manMu.RUnlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		globalStore.manMu.RUnlock()

		digest := computeDigest(entry.content)
		w.Header().Set("Content-Type", entry.mediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(entry.content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(entry.content)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read body"})
			return
		}
		mediaType := r.Header.Get("Content-Type")
		digest := computeDigest(body)

		globalStore.manMu.Lock()
		if globalStore.manifests[name] == nil {
			globalStore.manifests[name] = make(map[string]manifestEntry)
		}
		globalStore.manifests[name][ref] = manifestEntry{content: body, mediaType: mediaType}
		globalStore.manifests[name][digest] = manifestEntry{content: body, mediaType: mediaType}
		globalStore.manMu.Unlock()

		w.Header().Set("Location", r.URL.Path)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)

	case http.MethodDelete:
		globalStore.manMu.Lock()
		repo, ok := globalStore.manifests[name]
		if !ok {
			globalStore.manMu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(repo, ref)
		globalStore.manMu.Unlock()
		w.WriteHeader(http.StatusAccepted)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleV2Catalog handles GET /v2/_catalog
func handleV2Catalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	globalStore.manMu.RLock()
	repos := make([]string, 0, len(globalStore.manifests))
	for name := range globalStore.manifests {
		repos = append(repos, name)
	}
	globalStore.manMu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"repositories": repos,
	})
}

// ---------------------------------------------------------------------------
// Main mux
// ---------------------------------------------------------------------------

func mux(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL.Path)

	// BasicAuth enforcement (matches the test client credentials)
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || pass != "harbor12345" {
		w.Header().Set("WWW-Authenticate", `Basic realm="Harbor"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	path := r.URL.Path

	switch {
	// OCI Distribution API v2
	case path == "/v2/" || path == "/v2":
		handleV2Ping(w, r)
		return

	case path == "/v2/_catalog":
		handleV2Catalog(w, r)
		return

	case strings.HasPrefix(path, "/v2/"):
		// Parse: /v2/<name>/blobs/<digest> or /v2/<name>/blobs/uploads/... or /v2/<name>/manifests/<ref>
		trimmed := strings.TrimPrefix(path, "/v2/")
		if strings.Contains(trimmed, "/blobs/") {
			// Check if it's uploads or specific blob
			if strings.Contains(trimmed, "/blobs/uploads") {
				// Extract name before /blobs/uploads
				parts := strings.SplitN(trimmed, "/blobs/uploads", 2)
				handleV2BlobUploads(w, r, strings.TrimRight(parts[0], "/"))
				return
			}
			// /v2/<name>/blobs/<digest>
			parts := strings.SplitN(trimmed, "/blobs/", 2)
			if len(parts) != 2 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid blob path"})
				return
			}
			handleV2Blob(w, r, strings.TrimRight(parts[0], "/"), parts[1])
			return
		}
		if strings.Contains(trimmed, "/manifests/") {
			parts := strings.SplitN(trimmed, "/manifests/", 2)
			if len(parts) != 2 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid manifest path"})
				return
			}
			handleV2Manifest(w, r, strings.TrimRight(parts[0], "/"), parts[1])
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return

	// Harbor Management API
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
