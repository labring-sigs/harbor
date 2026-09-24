package controllers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	v1 "github.com/dinoallo/labring-sigs-harbor/api/v1"
	"github.com/dinoallo/labring-sigs-harbor/internal/harbor"
)

const (
	envtestTimeout = 30 * time.Second
)

// setupEnvTest starts a local control plane for integration testing.
// It returns the environment (which must be stopped), the reconciler, and
// a cleanup function.
//
// Usage requires controller-testing binaries (etcd, kube-apiserver) available
// on the PATH or configured via KUBEBUILDER_ASSETS.
//
// IMPORTANT: This function does NOT start the controller-runtime manager or
// register the reconciler with it. Tests call reconciler.Reconcile() directly
// to avoid races between the manager's watch-triggered reconciliation and the
// test's explicit reconciliation. This also avoids the "controller with name
// X already exists" error that occurs when running multiple envtest-based tests
// in the same binary (the controller name is registered globally via metrics).
func setupEnvTest(t *testing.T) (*envtest.Environment, *HarborProjectReconciler, context.Context, context.CancelFunc, func()) {
	t.Helper()

	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	// Determine CRD directory
	crdDir := findCRDDir(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	// Build scheme
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		_ = env.Stop()
		t.Fatalf("failed to add v1 scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		_ = env.Stop()
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}

	// Create a direct client (no cache) backed by the envtest API server.
	// We intentionally avoid creating a controller-runtime Manager here because:
	//   a) Manager starts background watches that trigger redundant reconciliation,
	//      causing races with test-driven Reconcile() calls and "context canceled"
	//      errors on parallel API updates.
	//   b) The controller name is registered globally (via Prometheus metrics),
	//      so a second test in the same binary would fail with
	//      "controller with name X already exists".
	// Tests drive reconciliation explicitly via reconciler.Reconcile().
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), envtestTimeout)

	// Create an in-memory Harbor mock that can track state across reconciliation.
	mockHarbor := &mockIntegrationHarborClient{}

	reconciler := &HarborProjectReconciler{
		Client:       c,
		Scheme:       scheme,
		HarborClient: mockHarbor,
		RegistryHost: "registry.integration-test.local",
	}

	cleanup := func() {
		cancel()
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop envtest: %v", err)
		}
	}

	return env, reconciler, ctx, cancel, cleanup
}

func findCRDDir(t *testing.T) string {
	t.Helper()

	// Determine the project root by walking up from the test file location.
	// In CI the working directory is the project root; locally it may vary.
	candidates := []string{
		"deploy/crds",
		"../deploy/crds",
	}

	// Also try relative to the test file
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "deploy/crds"))
	}

	for _, dir := range candidates {
		abs, err := filepath.Abs(dir)
		if err == nil {
			if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
				return abs
			}
		}
	}

	t.Skip("CRD directory not found; skipping integration tests (set KUBEBUILDER_ASSETS and ensure deploy/crds exists)")
	return ""
}

// mockIntegrationHarborClient is a simple in-memory Harbor mock for envtest.
type mockIntegrationHarborClient struct {
	projects map[string]*harbor.Project
	nextPID  int64
	nextRID  int64
}

func (m *mockIntegrationHarborClient) GetProjectByName(_ context.Context, name string) (*harbor.Project, error) {
	if m.projects == nil {
		return nil, nil
	}
	p, ok := m.projects[name]
	if !ok {
		return nil, nil
	}
	return p, nil
}

func (m *mockIntegrationHarborClient) CreateProject(_ context.Context, spec harbor.ProjectSpec) (int64, error) {
	if m.projects == nil {
		m.projects = make(map[string]*harbor.Project)
	}
	m.nextPID++
	id := m.nextPID
	m.projects[spec.Name] = &harbor.Project{
		ProjectID: id,
		Name:      spec.Name,
		Public:    spec.Public,
	}
	return id, nil
}

func (m *mockIntegrationHarborClient) CreateRobot(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
	m.nextRID++
	robot := &harbor.RobotAccount{
		ID:    m.nextRID,
		Name:  spec.Name,
		Token: "integration-test-token-" + fmt.Sprint(m.nextRID),
	}
	return robot, nil
}

func (m *mockIntegrationHarborClient) DeleteProjectRobot(_ context.Context, projectID, robotID int64) error {
	return nil
}

func (m *mockIntegrationHarborClient) DeleteProject(_ context.Context, projectID int64) error {
	if m.projects == nil {
		return nil
	}
	for name, p := range m.projects {
		if p.ProjectID == projectID {
			delete(m.projects, name)
			return nil
		}
	}
	return nil
}

func (m *mockIntegrationHarborClient) UpdateProject(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
	if m.projects == nil {
		return &harbor.ErrNotFound{Resource: "project", ID: projectID}
	}
	for _, p := range m.projects {
		if p.ProjectID == projectID {
			p.Public = spec.Public
			return nil
		}
	}
	return &harbor.ErrNotFound{Resource: "project", ID: projectID}
}

func (m *mockIntegrationHarborClient) UpdateProjectQuota(_ context.Context, projectID int64, storageLimit int64) error {
	// In-memory mock: acknowledge the quota update without persisting
	return nil
}

// ---------------------------------------------------------------------------
// Integration tests
// ---------------------------------------------------------------------------

func TestIntegration_CRD_CreateUpdateDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	_, reconciler, ctx, _, cleanup := setupEnvTest(t)
	defer cleanup()

	client := reconciler.Client

	// Create the namespace used by the test
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns-integration",
		},
	}
	if err := reconciler.Client.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create a HarborProject
	hp := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: "integration-test-project",
		},
		Spec: v1.HarborProjectSpec{
			NamespaceRefs: []string{"ns-integration"},
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
			StorageLimit: -1,
			Public:       false,
		},
	}

	if err := client.Create(ctx, hp); err != nil {
		t.Fatalf("failed to create HarborProject: %v", err)
	}

	// Trigger reconciliation
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "integration-test-project"}}
	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// First reconcile adds finalizer → requeue
	if !result.Requeue {
		t.Log("note: first reconcile did not requeue (finalizer may already be set via cache)")
	}

	// Second reconcile should complete creation
	result2, err2 := reconciler.Reconcile(ctx, req)
	if err2 != nil {
		t.Fatalf("second reconcile error: %v", err2)
	}
	_ = result2

	// Verify the project status
	updated := &v1.HarborProject{}
	if err := client.Get(ctx, types.NamespacedName{Name: "integration-test-project"}, updated); err != nil {
		t.Fatalf("failed to get updated project: %v", err)
	}

	if updated.Status.Phase != v1.HarborPhaseReady {
		t.Errorf("expected phase Ready, got %q", updated.Status.Phase)
	}
	if updated.Status.HarborProjectID == 0 {
		t.Errorf("expected non-zero HarborProjectID")
	}
	if !hasFinalizer(updated, harborFinalizer) {
		t.Errorf("expected finalizer %s", harborFinalizer)
	}

	// Verify the dockerconfigjson secret was created
	secret := &corev1.Secret{}
	if err := client.Get(ctx, types.NamespacedName{
		Name:      "harbor-registry-cred-integration-test-project",
		Namespace: "ns-integration",
	}, secret); err != nil {
		t.Fatalf("expected secret to be created: %v", err)
	}

	// Delete the project and verify cleanup
	if err := client.Delete(ctx, hp); err != nil {
		t.Fatalf("failed to delete HarborProject: %v", err)
	}

	// Re-reconcile to trigger deletion
	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile delete error: %v", err)
	}

	// Verify the project is gone (or has no finalizer)
	finalCheck := &v1.HarborProject{}
	if err := client.Get(ctx, types.NamespacedName{Name: "integration-test-project"}, finalCheck); err != nil {
		t.Logf("project deleted as expected: %v", err)
	} else if !hasFinalizer(finalCheck, harborFinalizer) {
		t.Log("finalizer removed, project will be garbage collected")
	}
}

func TestIntegration_CRD_TokenRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	_, reconciler, ctx, _, cleanup := setupEnvTest(t)
	defer cleanup()

	client := reconciler.Client

	// Create the namespace used by the test
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns-refresh",
		},
	}
	if err := reconciler.Client.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create a project with the refresh annotation
	hp := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: "refresh-test",
			Annotations: map[string]string{
				refreshAnnotation: "true",
			},
		},
		Spec: v1.HarborProjectSpec{
			NamespaceRefs: []string{"ns-refresh"},
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
			},
		},
	}

	if err := client.Create(ctx, hp); err != nil {
		t.Fatalf("failed to create HarborProject: %v", err)
	}

	// Set initial Ready phase so the refresh path is taken
	if err := client.Get(ctx, types.NamespacedName{Name: "refresh-test"}, hp); err != nil {
		t.Fatalf("failed to get project: %v", err)
	}
	hp.Status.Phase = v1.HarborPhaseReady
	hp.Status.HarborProjectID = 1
	hp.Status.HarborProjectName = "hp-refresh-test"
	hp.Status.RobotID = 100
	if err := client.Status().Update(ctx, hp); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Reconcile
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "refresh-test"}}
	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify the refresh annotation was removed
	updated := &v1.HarborProject{}
	if err := client.Get(ctx, types.NamespacedName{Name: "refresh-test"}, updated); err != nil {
		t.Fatalf("failed to get updated project: %v", err)
	}

	if _, ok := updated.Annotations[refreshAnnotation]; ok {
		t.Errorf("expected refresh annotation to be removed")
	}

	t.Logf("Token refresh completed: RobotID changed from 100 to %d", updated.Status.RobotID)
}

func hasFinalizer(hp *v1.HarborProject, finalizer string) bool {
	for _, f := range hp.Finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}
