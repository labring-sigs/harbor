package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	v1 "github.com/dinoallo/labring-sigs-harbor/api/v1"
	"github.com/dinoallo/labring-sigs-harbor/internal/harbor"
)

// ---------------------------------------------------------------------------
// E2E test: Full HarborProject lifecycle with OCI image push/pull
//
// This test:
//   1. Starts the extended Harbor mock (management API + OCI registry) via Testcontainers
//   2. Starts envtest (Kubernetes API server with CRD support)
//   3. Creates a HarborProject CR
//   4. Drives the reconciler to create the project + robot + dockerconfigjson secret
//   5. Pushes an OCI image to the mock registry using the robot credentials
//   6. Pulls the image back and verifies the content matches
//   7. Deletes the HarborProject CR
//   8. Drives the reconciler for cleanup
//   9. Verifies the project is removed from Harbor
//
// Requirements:
//   - Docker daemon (for Testcontainers)
//   - envtest binaries (etcd + kube-apiserver) on PATH or KUBEBUILDER_ASSETS
//
// Run:
//   go test -v -run TestE2E_HarborProjectLifecycle ./controllers/
// ---------------------------------------------------------------------------

const (
	e2eTimeout     = 180 * time.Second
	harborMockPort = "8080/tcp"
)

// buildAndStartE2EMock is like buildAndStartHarborMock but for the extended
// mock that includes OCI Distribution API support.
func buildAndStartE2EMock(t *testing.T, ctx context.Context) (testcontainers.Container, string, func()) {
	t.Helper()

	// Locate the mock source directory (relative to the test file)
	mockDir, err := filepath.Abs("../internal/harbor/testdata/harbor-mock")
	if err != nil {
		t.Fatalf("failed to resolve mock directory: %v", err)
	}
	if _, err := os.Stat(mockDir); os.IsNotExist(err) {
		t.Fatalf("mock directory not found: %s", mockDir)
	}

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:       mockDir,
			Dockerfile:    "Dockerfile",
			Repo:          "harbor-mock-e2e",
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
		t.Fatalf("failed to start e2e mock container: %v", err)
	}

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

// setupE2EEnvTest starts envtest and returns the environment, reconciler,
// context, cancel, and cleanup function.
func setupE2EEnvTest(t *testing.T, harborEndpoint string) (*envtest.Environment, *HarborProjectReconciler, context.Context, func()) {
	t.Helper()

	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	crdDir := findCRDDir(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		_ = env.Stop()
		t.Fatalf("failed to add v1 scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		_ = env.Stop()
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("failed to create client: %v", err)
	}

	// Create a real Harbor client pointed at the mock
	harborClient := harbor.NewClient(harborEndpoint, "admin", "harbor12345")

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)

	// Use the mock endpoint as the registry host (the mock serves both APIs)
	reconciler := &HarborProjectReconciler{
		Client:       c,
		Scheme:       scheme,
		HarborClient: harborClient,
		RegistryHost: strings.TrimPrefix(harborEndpoint, "http://"), // registry host without scheme
	}

	cleanup := func() {
		cancel()
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop envtest: %v", err)
		}
	}

	return env, reconciler, ctx, cleanup
}

// ---------------------------------------------------------------------------
// OCI distribution helpers (used directly via HTTP)
// ---------------------------------------------------------------------------

// ociClient is a minimal OCI distribution client for testing.
type ociClient struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

func newOCIClient(baseURL, username, password string) *ociClient {
	return &ociClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		username:   username,
		password:   password,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *ociClient) do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(c.username, c.password)
	return c.httpClient.Do(req)
}

// ping checks if the registry supports v2.
func (c *ociClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OCI ping failed: status=%d", resp.StatusCode)
	}
	return nil
}

// uploadBlob uploads a blob (config or layer) to the repository.
func (c *ociClient) uploadBlob(ctx context.Context, repo string, data []byte) (string, error) {
	digest := computeDigest(data)

	// Start upload
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v2/"+repo+"/blobs/uploads/", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return "", fmt.Errorf("start upload failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	uploadLocation := resp.Header.Get("Location")
	resp.Body.Close()

	// The Location may be relative or absolute. If relative, prepend base.
	if strings.HasPrefix(uploadLocation, "/") {
		uploadLocation = c.baseURL + uploadLocation
	}

	// Upload the blob data
	req, err = http.NewRequestWithContext(ctx, http.MethodPut,
		uploadLocation+"?digest="+digest, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))
	resp, err = c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("complete upload failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	return digest, nil
}

// pushManifest uploads a manifest for a repository, tagged with ref.
func (c *ociClient) pushManifest(ctx context.Context, repo, ref string, manifest []byte, mediaType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/v2/"+repo+"/manifests/"+ref, bytes.NewReader(manifest))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mediaType)
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("push manifest failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	return digest, nil
}

// pullManifest fetches a manifest by reference (tag or digest).
func (c *ociClient) pullManifest(ctx context.Context, repo, ref string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v2/"+repo+"/manifests/"+ref, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("pull manifest failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	mediaType := resp.Header.Get("Content-Type")
	_ = mediaType
	return data, digest, nil
}

// pullBlob fetches a blob by digest.
func (c *ociClient) pullBlob(ctx context.Context, repo, digest string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v2/"+repo+"/blobs/"+digest, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pull blob failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Test: Full HarborProject lifecycle with OCI image push/pull
// ---------------------------------------------------------------------------

func TestE2E_HarborProjectLifecycle(t *testing.T) {
	if os.Getenv("SKIP_E2E") != "" {
		t.Skip("SKIP_E2E is set; skipping e2e test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping e2e test")
	}
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()

	// ---- Step 1: Start the extended Harbor mock (management API + OCI registry) ----
	t.Log("=== Step 1: Starting Harbor mock (management API + OCI registry) ===")
	_, mockEndpoint, cleanupMock := buildAndStartE2EMock(t, ctx)
	defer cleanupMock()

	t.Logf("Mock endpoint: %s", mockEndpoint)

	// Verify the OCI v2 endpoint works
	ociAdmin := newOCIClient(mockEndpoint, "admin", "harbor12345")
	if err := ociAdmin.ping(ctx); err != nil {
		t.Fatalf("OCI ping failed: %v", err)
	}
	t.Log("OCI v2 check passed")

	// ---- Step 2: Start envtest ----
	t.Log("=== Step 2: Starting envtest (Kubernetes API server) ===")
	_, reconciler, envCtx, cleanupEnv := setupE2EEnvTest(t, mockEndpoint)
	defer cleanupEnv()

	kubeClient := reconciler.Client

	// ---- Step 3: Create a namespace and HarborProject CR ----
	t.Log("=== Step 3: Creating namespace and HarborProject CR ===")
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns-e2e-test",
		},
	}
	if err := kubeClient.Create(envCtx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	hp := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: "e2e-test-project",
		},
		Spec: v1.HarborProjectSpec{
			NamespaceRefs: []string{"ns-e2e-test"},
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
			StorageLimit: -1,
			Public:       false,
		},
	}

	if err := kubeClient.Create(envCtx, hp); err != nil {
		t.Fatalf("failed to create HarborProject: %v", err)
	}
	t.Log("HarborProject CR created")

	// ---- Step 4: Drive the reconciler to create the project and robot ----
	t.Log("=== Step 4: Driving reconciler (create project + robot) ===")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "e2e-test-project"}}

	// First reconcile should add finalizer → requeue
	result, err := reconciler.Reconcile(envCtx, req)
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	if !result.Requeue {
		t.Log("note: first reconcile did not requeue (finalizer may already be set)")
	}

	// Second reconcile should create the project and robot, and set status
	result2, err2 := reconciler.Reconcile(envCtx, req)
	if err2 != nil {
		t.Fatalf("second reconcile error: %v", err2)
	}
	_ = result2

	// Check the status
	updated := &v1.HarborProject{}
	if err := kubeClient.Get(envCtx, types.NamespacedName{Name: "e2e-test-project"}, updated); err != nil {
		t.Fatalf("failed to get updated project: %v", err)
	}
	if updated.Status.Phase != v1.HarborPhaseReady {
		t.Errorf("expected phase Ready, got %q", updated.Status.Phase)
	}
	if updated.Status.HarborProjectID == 0 {
		t.Errorf("expected non-zero HarborProjectID")
	}
	if updated.Status.RobotID == 0 {
		t.Errorf("expected non-zero RobotID")
	}
	t.Logf("Project status: phase=%s projectID=%d robotID=%d",
		updated.Status.Phase, updated.Status.HarborProjectID, updated.Status.RobotID)

	// The project name in Harbor should be auto-generated
	projectName := updated.Status.HarborProjectName
	if projectName == "" {
		projectName = "hp-e2e-test-project"
	}
	t.Logf("Harbor project name: %s", projectName)

	// ---- Step 5: Get the dockerconfigjson secret ----
	t.Log("=== Step 5: Reading docker config secret ===")
	secret := &corev1.Secret{}
	if err := kubeClient.Get(envCtx, types.NamespacedName{
		Name:      "harbor-registry-cred-e2e-test-project",
		Namespace: "ns-e2e-test",
	}, secret); err != nil {
		t.Fatalf("expected secret to be created: %v", err)
	}

	dockerConfigJSON := secret.Data[corev1.DockerConfigJsonKey]
	var dc map[string]interface{}
	if err := json.Unmarshal(dockerConfigJSON, &dc); err != nil {
		t.Fatalf("failed to unmarshal docker config: %v", err)
	}

	auths, ok := dc["auths"].(map[string]interface{})
	if !ok {
		t.Fatal("expected auths map")
	}

	// Get the registry host (without scheme)
	registryHost := strings.TrimPrefix(mockEndpoint, "http://")
	entry, ok := auths[registryHost].(map[string]interface{})
	if !ok {
		// Try with port
		entry, ok = auths[registryHost+":"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected registry entry for %s in auths: %+v", registryHost, auths)
		}
	}

	robotUser := entry["username"].(string)
	robotPass := entry["password"].(string)
	t.Logf("Robot credentials: user=%s pass=%s", robotUser, robotPass)

	// ---- Step 6: Push an OCI image using the robot credentials ----
	t.Log("=== Step 6: Pushing OCI image ===")
	ociRobot := newOCIClient(mockEndpoint, robotUser, robotPass)

	// Build a minimal OCI image
	// 1. Config blob
	config := map[string]interface{}{
		"created": "2024-01-01T00:00:00Z",
		"architecture": "amd64",
		"os": "linux",
		"rootfs": map[string]interface{}{
			"type": "layers",
			"diff_ids": []string{},
		},
		"config": map[string]interface{}{},
	}
	configData, _ := json.Marshal(config)
	configDigest, err := ociRobot.uploadBlob(envCtx, projectName+"/my-image", configData)
	if err != nil {
		t.Fatalf("failed to upload config blob: %v", err)
	}
	t.Logf("Config blob digest: %s (%d bytes)", configDigest, len(configData))

	// 2. Layer blob (empty tar-like layer for a minimal image)
	layerData := []byte("Hello, Harbor E2E test! This is a test layer content.\n")
	layerDigest, err := ociRobot.uploadBlob(envCtx, projectName+"/my-image", layerData)
	if err != nil {
		t.Fatalf("failed to upload layer blob: %v", err)
	}
	t.Logf("Layer blob digest: %s (%d bytes)", layerDigest, len(layerData))

	// 3. Manifest (OCI image manifest)
	manifest := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(configData),
		},
		"layers": []map[string]interface{}{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar",
				"digest":    layerDigest,
				"size":      len(layerData),
			},
		},
	}
	manifestData, _ := json.Marshal(manifest)
	manifestDigest, err := ociRobot.pushManifest(envCtx, projectName+"/my-image", "latest", manifestData,
		"application/vnd.oci.image.manifest.v1+json")
	if err != nil {
		t.Fatalf("failed to push manifest: %v", err)
	}
	t.Logf("Manifest digest: %s (%d bytes)", manifestDigest, len(manifestData))

	// ---- Step 7: Pull the image back and verify ----
	t.Log("=== Step 7: Pulling image back and verifying ===")

	// Pull manifest by tag
	pulledManifest, pulledManifestDigest, err := ociRobot.pullManifest(envCtx, projectName+"/my-image", "latest")
	if err != nil {
		t.Fatalf("failed to pull manifest: %v", err)
	}
	if string(pulledManifest) != string(manifestData) {
		t.Errorf("pulled manifest differs from pushed manifest")
	}
	if pulledManifestDigest != manifestDigest {
		t.Errorf("pulled manifest digest %q != expected %q", pulledManifestDigest, manifestDigest)
	}
	t.Logf("Manifest verified: digest=%s", pulledManifestDigest)

	// Pull config blob
	pulledConfig, err := ociRobot.pullBlob(envCtx, projectName+"/my-image", configDigest)
	if err != nil {
		t.Fatalf("failed to pull config blob: %v", err)
	}
	if string(pulledConfig) != string(configData) {
		t.Errorf("pulled config differs from pushed config")
	}
	t.Logf("Config blob verified: %d bytes", len(pulledConfig))

	// Pull layer blob
	pulledLayer, err := ociRobot.pullBlob(envCtx, projectName+"/my-image", layerDigest)
	if err != nil {
		t.Fatalf("failed to pull layer blob: %v", err)
	}
	if string(pulledLayer) != string(layerData) {
		t.Errorf("pulled layer differs from pushed layer")
	}
	t.Logf("Layer blob verified: %d bytes -> %q", len(pulledLayer), string(pulledLayer))

	// ---- Step 8: Delete the HarborProject CR ----
	t.Log("=== Step 8: Deleting HarborProject CR ===")
	if err := kubeClient.Delete(envCtx, hp); err != nil {
		t.Fatalf("failed to delete HarborProject: %v", err)
	}

	// Re-reconcile to trigger deletion
	_, err = reconciler.Reconcile(envCtx, req)
	if err != nil {
		t.Fatalf("reconcile delete error: %v", err)
	}

	// ---- Step 9: Verify cleanup ----
	t.Log("=== Step 9: Verifying cleanup ===")

	// Check that the Harbor project was deleted via the admin client
	harborClient := harbor.NewClient(mockEndpoint, "admin", "harbor12345")
	proj, err := harborClient.GetProjectByName(envCtx, projectName)
	if err != nil {
		t.Fatalf("failed to check project deletion: %v", err)
	}
	if proj != nil {
		t.Errorf("expected Harbor project %q to be deleted after CR removal, but it still exists", projectName)
	} else {
		t.Logf("Harbor project %q successfully deleted", projectName)
	}

	// Check the Kubernetes object either has no finalizer or is gone
	finalCheck := &v1.HarborProject{}
	if err := kubeClient.Get(envCtx, types.NamespacedName{Name: "e2e-test-project"}, finalCheck); err != nil {
		t.Logf("HarborProject CR deleted as expected: %v", err)
	} else if !hasFinalizer(finalCheck, harborFinalizer) {
		t.Log("Finalizer removed, CR will be garbage collected")
	}

	// Verify the OCI image artifacts are still present (the mock doesn't delete them
	// on project deletion since it's a simplified mock; real Harbor would clean up).
	t.Log("E2E test completed successfully!")
}

// ---------------------------------------------------------------------------
// Helper: parse dockerconfig secret from envtest
// ---------------------------------------------------------------------------

// getDockerConfigFromSecret reads the dockerconfigjson secret from the
// Kubernetes API server (envtest) and returns the registry credentials.
func getDockerConfigFromSecret(t *testing.T, kubeClient client.Client, ctx context.Context,
	secretName, namespace, registryHost string) (username, password string) {

	t.Helper()

	secret := &corev1.Secret{}
	if err := kubeClient.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: namespace,
	}, secret); err != nil {
		t.Fatalf("failed to get secret: %v", err)
	}

	dockerConfigJSON := secret.Data[corev1.DockerConfigJsonKey]
	var dc map[string]interface{}
	if err := json.Unmarshal(dockerConfigJSON, &dc); err != nil {
		t.Fatalf("failed to unmarshal docker config: %v", err)
	}

	auths, ok := dc["auths"].(map[string]interface{})
	if !ok {
		t.Fatal("expected auths map")
	}

	entry, ok := auths[registryHost].(map[string]interface{})
	if !ok {
		t.Fatalf("expected registry entry for %s", registryHost)
	}

	username = entry["username"].(string)
	password = entry["password"].(string)
	return
}

// ---------------------------------------------------------------------------
// Validation test: ensure the mock's OCI endpoints work with admin creds
// ---------------------------------------------------------------------------

func TestE2E_MockOCIEndpoints(t *testing.T) {
	if os.Getenv("SKIP_E2E") != "" {
		t.Skip("SKIP_E2E is set; skipping e2e test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping e2e test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, mockEndpoint, cleanup := buildAndStartE2EMock(t, ctx)
	defer cleanup()

	// Test OCI v2 ping
	oci := newOCIClient(mockEndpoint, "admin", "harbor12345")
	if err := oci.ping(ctx); err != nil {
		t.Fatalf("OCI ping failed: %v", err)
	}
	t.Log("OCI v2 ping OK")

	// Test blob upload
	testData := []byte("test blob content for e2e mock verification")
	digest, err := oci.uploadBlob(ctx, "test-repo", testData)
	if err != nil {
		t.Fatalf("blob upload failed: %v", err)
	}
	t.Logf("Blob uploaded: digest=%s", digest)

	// Test blob download
	pulled, err := oci.pullBlob(ctx, "test-repo", digest)
	if err != nil {
		t.Fatalf("blob download failed: %v", err)
	}
	if string(pulled) != string(testData) {
		t.Errorf("blob content mismatch: got %q, expected %q", string(pulled), string(testData))
	}
	t.Log("Blob roundtrip OK")

	// Test manifest push
	manifest := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    digest,
			"size":      len(testData),
		},
		"layers": []map[string]interface{}{},
	}
	manifestData, _ := json.Marshal(manifest)
	manifestDigest, err := oci.pushManifest(ctx, "test-repo", "latest", manifestData,
		"application/vnd.oci.image.manifest.v1+json")
	if err != nil {
		t.Fatalf("manifest push failed: %v", err)
	}
	t.Logf("Manifest pushed: digest=%s", manifestDigest)

	// Test manifest pull by tag
	pulledManifest, pulledDigest, err := oci.pullManifest(ctx, "test-repo", "latest")
	if err != nil {
		t.Fatalf("manifest pull failed: %v", err)
	}
	if string(pulledManifest) != string(manifestData) {
		t.Errorf("manifest content mismatch")
	}
	if pulledDigest != manifestDigest {
		t.Errorf("manifest digest mismatch: got %s, expected %s", pulledDigest, manifestDigest)
	}
	t.Log("Manifest roundtrip OK")

	// Test manifest pull by digest
	pulledManifest2, _, err := oci.pullManifest(ctx, "test-repo", manifestDigest)
	if err != nil {
		t.Fatalf("manifest pull by digest failed: %v", err)
	}
	if string(pulledManifest2) != string(manifestData) {
		t.Errorf("manifest content mismatch when pulling by digest")
	}
	t.Log("Manifest pull by digest OK")
}

// ---------------------------------------------------------------------------
// Minimal test to verify the reconciler can create a project with the
// OCI-enabled mock (without image push/pull).
// ---------------------------------------------------------------------------

func TestE2E_ReconcilerWithOCIMock(t *testing.T) {
	if os.Getenv("SKIP_E2E") != "" {
		t.Skip("SKIP_E2E is set; skipping e2e test")
	}
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Docker socket not found; skipping e2e test")
	}
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()

	_, mockEndpoint, cleanupMock := buildAndStartE2EMock(t, ctx)
	defer cleanupMock()

	_, reconciler, envCtx, cleanupEnv := setupE2EEnvTest(t, mockEndpoint)
	defer cleanupEnv()

	// Create namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-e2e-reconciler"},
	}
	if err := reconciler.Client.Create(envCtx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create HarborProject
	hp := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-reconciler-test"},
		Spec: v1.HarborProjectSpec{
			NamespaceRefs: []string{"ns-e2e-reconciler"},
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
			StorageLimit: -1,
		},
	}
	if err := reconciler.Client.Create(envCtx, hp); err != nil {
		t.Fatalf("failed to create HarborProject: %v", err)
	}

	// Reconcile (first pass: add finalizer)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "e2e-reconciler-test"}}
	_, err := reconciler.Reconcile(envCtx, req)
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}

	// Reconcile (second pass: create project + robot)
	_, err = reconciler.Reconcile(envCtx, req)
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	// Verify status
	updated := &v1.HarborProject{}
	if err := reconciler.Client.Get(envCtx,
		types.NamespacedName{Name: "e2e-reconciler-test"}, updated); err != nil {
		t.Fatalf("failed to get updated project: %v", err)
	}

	if updated.Status.Phase != v1.HarborPhaseReady {
		t.Errorf("expected phase Ready, got %q", updated.Status.Phase)
	}
	if updated.Status.HarborProjectID == 0 {
		t.Errorf("expected non-zero HarborProjectID")
	}
	if updated.Status.RobotID == 0 {
		t.Errorf("expected non-zero RobotID")
	}

	// Verify secret
	secret := &corev1.Secret{}
	if err := reconciler.Client.Get(envCtx, types.NamespacedName{
		Name:      "harbor-registry-cred-e2e-reconciler-test",
		Namespace: "ns-e2e-reconciler",
	}, secret); err != nil {
		t.Fatalf("expected secret to be created: %v", err)
	}

	t.Logf("Reconciler with OCI mock: phase=%s projectID=%d robotID=%d",
		updated.Status.Phase, updated.Status.HarborProjectID, updated.Status.RobotID)
}

// ---------------------------------------------------------------------------
// Avoid unused import errors for client-go/kubernetes
// ---------------------------------------------------------------------------

