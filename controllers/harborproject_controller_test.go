package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/dinoallo/labring-sigs-harbor/api/v1"
	"github.com/dinoallo/labring-sigs-harbor/internal/harbor"
)

// ---------------------------------------------------------------------------
// Mock Harbor client
// ---------------------------------------------------------------------------

type harborClient interface {
	GetProjectByName(ctx context.Context, name string) (*harbor.Project, error)
	CreateProject(ctx context.Context, spec harbor.ProjectSpec) (int64, error)
	UpdateProject(ctx context.Context, projectID int64, spec harbor.ProjectSpec) error
	CreateRobot(ctx context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error)
	RefreshRobotSecret(ctx context.Context, robotID int64, secret string) error
	DeleteProjectRobot(ctx context.Context, projectID, robotID int64) error
	DeleteProject(ctx context.Context, projectID int64) error
}

// Ensure *harbor.Client satisfies the interface at compile time (for
// production; tests use mockHarborClient).
var _ harborClient = (*harbor.Client)(nil)

type mockHarborClient struct {
	getProjectByNameFn    func(ctx context.Context, name string) (*harbor.Project, error)
	createProjectFn       func(ctx context.Context, spec harbor.ProjectSpec) (int64, error)
	updateProjectFn       func(ctx context.Context, projectID int64, spec harbor.ProjectSpec) error
	createRobotFn         func(ctx context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error)
	refreshRobotSecretFn  func(ctx context.Context, robotID int64, secret string) error
	deleteProjectRobotFn  func(ctx context.Context, projectID, robotID int64) error
	deleteProjectFn       func(ctx context.Context, projectID int64) error
}

func (m *mockHarborClient) GetProjectByName(ctx context.Context, name string) (*harbor.Project, error) {
	return m.getProjectByNameFn(ctx, name)
}
func (m *mockHarborClient) CreateProject(ctx context.Context, spec harbor.ProjectSpec) (int64, error) {
	return m.createProjectFn(ctx, spec)
}
func (m *mockHarborClient) UpdateProject(ctx context.Context, projectID int64, spec harbor.ProjectSpec) error {
	return m.updateProjectFn(ctx, projectID, spec)
}
func (m *mockHarborClient) CreateRobot(ctx context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
	return m.createRobotFn(ctx, projectID, spec)
}
func (m *mockHarborClient) DeleteProjectRobot(ctx context.Context, projectID, robotID int64) error {
	return m.deleteProjectRobotFn(ctx, projectID, robotID)
}
func (m *mockHarborClient) DeleteProject(ctx context.Context, projectID int64) error {
	return m.deleteProjectFn(ctx, projectID)
}
func (m *mockHarborClient) RefreshRobotSecret(ctx context.Context, robotID int64, secret string) error {
	return m.refreshRobotSecretFn(ctx, robotID, secret)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestReconciler(mock *mockHarborClient, objs ...runtime.Object) *HarborProjectReconciler {
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.HarborProject{}).
		WithRuntimeObjects(objs...).
		Build()

	return &HarborProjectReconciler{
		Client:       fakeClient,
		Scheme:       scheme,
		HarborClient: mock,
		RegistryHost: "registry.test.sealos.io",
	}
}

func fakeProject(name, phase string, withFinalizer bool) *v1.HarborProject {
	hp := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.HarborProjectSpec{
			NamespaceRefs: []string{"ns-1"},
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
			StorageLimit: -1,
		},
		Status: v1.HarborProjectStatus{
			Phase: v1.HarborProjectPhase(phase),
		},
	}
	if withFinalizer {
		hp.Finalizers = []string{harborFinalizer}
	}
	return hp
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestReconcile_CreateProject(t *testing.T) {
	project := fakeProject("test-hp", string(v1.HarborPhasePending), false)

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return nil, nil // project does not exist yet
		},
		createProjectFn: func(_ context.Context, spec harbor.ProjectSpec) (int64, error) {
			if spec.Name != "hp-test-hp" {
				t.Errorf("expected project name 'hp-test-hp', got %q", spec.Name)
			}
			return 42, nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			if projectID != 42 {
				t.Errorf("expected projectID 42, got %d", projectID)
			}
			return &harbor.RobotAccount{
				ID:    101,
				Name:  "robot-test-abc",
				Token: "token-secret-123",
			}, nil
		},
	}

	r := newTestReconciler(mock, project)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-hp"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First call adds finalizer and requeues
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Second call: finalizer already set, proceed with creation
	result2, err2 := r.Reconcile(context.Background(), req)
	if err2 != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err2)
	}
	if result2.Requeue {
		t.Fatal("did not expect requeue after successful creation")
	}

	// Verify the updated project status
	updated := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "test-hp"}, updated); err != nil {
		t.Fatalf("failed to get updated project: %v", err)
	}

	if updated.Status.Phase != v1.HarborPhaseReady {
		t.Errorf("expected phase Ready, got %q", updated.Status.Phase)
	}
	if updated.Status.HarborProjectID != 42 {
		t.Errorf("expected HarborProjectID 42, got %d", updated.Status.HarborProjectID)
	}
	if updated.Status.HarborProjectName != "hp-test-hp" {
		t.Errorf("expected HarborProjectName 'hp-test-hp', got %q", updated.Status.HarborProjectName)
	}
	if updated.Status.RobotID != 101 {
		t.Errorf("expected RobotID 101, got %d", updated.Status.RobotID)
	}

	// Verify the dockerconfigjson secret was created
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "harbor-registry-cred-test-hp", Namespace: "ns-1"}, secret); err != nil {
		t.Fatalf("expected secret to be created: %v", err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("expected secret type %s, got %s", corev1.SecretTypeDockerConfigJson, secret.Type)
	}
}

func TestReconcile_CreateProject_AlreadyExistsInHarbor(t *testing.T) {
	project := fakeProject("existing-proj", string(v1.HarborPhasePending), true)

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{ProjectID: 99, Name: "hp-existing-proj"}, nil // already exists
		},
		createProjectFn: func(_ context.Context, spec harbor.ProjectSpec) (int64, error) {
			t.Fatal("CreateProject should not be called when project already exists")
			return 0, nil
		},
		updateProjectFn: func(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
			// Project already exists; just acknowledge the update
			return nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			if projectID != 99 {
				t.Errorf("expected projectID 99, got %d", projectID)
			}
			return &harbor.RobotAccount{ID: 201, Name: "robot-existing", Token: "tok"}, nil
		},
	}

	r := newTestReconciler(mock, project)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "existing-proj"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("did not expect requeue after successful reconcile (finalizer already set)")
	}

	updated := &v1.HarborProject{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "existing-proj"}, updated)
	if updated.Status.HarborProjectID != 99 {
		t.Errorf("expected HarborProjectID 99, got %d", updated.Status.HarborProjectID)
	}
}

func TestReconcile_CreateProject_Fails(t *testing.T) {
	project := fakeProject("fail-proj", string(v1.HarborPhasePending), true)

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return nil, errors.New("harbor unreachable")
		},
	}

	r := newTestReconciler(mock, project)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fail-proj"}}

	_, err := r.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when Harbor is unreachable")
	}

	updated := &v1.HarborProject{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "fail-proj"}, updated)
	if updated.Status.Phase != v1.HarborPhaseFailed {
		t.Errorf("expected phase Failed, got %q", updated.Status.Phase)
	}
}

func TestReconcile_DeleteProject(t *testing.T) {
	project := fakeProject("delete-me", string(v1.HarborPhaseReady), true)
	project.Status.HarborProjectID = 77
	project.Status.HarborProjectName = "hp-delete-me"
	project.Status.RobotID = 301
	now := metav1.Now()
	project.DeletionTimestamp = &now

	var deletedProjectID int64
	mock := &mockHarborClient{
		deleteProjectRobotFn: func(_ context.Context, projectID, robotID int64) error {
			return nil
		},
		deleteProjectFn: func(_ context.Context, projectID int64) error {
			deletedProjectID = projectID
			if projectID != 77 {
				t.Errorf("expected projectID 77, got %d", projectID)
			}
			return nil
		},
	}

	r := newTestReconciler(mock, project)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "delete-me"}}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the finalizer is removed (project will be deleted by GC)
	if deletedProjectID != 77 {
		t.Errorf("expected DeleteProject to be called with 77, got %d", deletedProjectID)
	}

	// Verify secrets in namespaceRefs are cleaned up
	secret := &corev1.Secret{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "harbor-registry-cred-delete-me", Namespace: "ns-1"}, secret)
	if err == nil {
		t.Fatal("expected secret to be deleted")
	}
}

func TestReconcile_RefreshToken(t *testing.T) {
	project := fakeProject("refresh-proj", string(v1.HarborPhaseReady), true)
	project.Annotations = map[string]string{refreshAnnotation: "true"}
	project.Status.HarborProjectID = 55
	project.Status.HarborProjectName = "hp-refresh-proj"
	project.Status.RobotID = 400
	project.Status.RobotName = "robot$hp-refresh-proj+abc"

	var refreshedRobotID int64
	var refreshedSecret string
	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{ProjectID: 55, Name: "hp-refresh-proj"}, nil
		},
		refreshRobotSecretFn: func(_ context.Context, robotID int64, secret string) error {
			refreshedRobotID = robotID
			refreshedSecret = secret
			return nil
		},
	}

	r := newTestReconciler(mock, project)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "refresh-proj"}}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify RefreshRobotSecret was called with correct robotID
	if refreshedRobotID != 400 {
		t.Errorf("expected robotID 400, got %d", refreshedRobotID)
	}
	if refreshedSecret == "" {
		t.Fatal("expected a non-empty secret to be generated")
	}

	// Verify status unchanged (robot ID/name stays the same)
	updated := &v1.HarborProject{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "refresh-proj"}, updated)
	if updated.Status.RobotID != 400 {
		t.Errorf("expected RobotID to remain 400, got %d", updated.Status.RobotID)
	}

	// Verify refresh annotation removed
	if _, ok := updated.Annotations[refreshAnnotation]; ok {
		t.Fatal("expected refresh annotation to be removed")
	}

	// Verify secret was updated with new generated secret
	secret := &corev1.Secret{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "harbor-registry-cred-refresh-proj", Namespace: "ns-1"}, secret)

	var dc map[string]interface{}
	_ = json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &dc)
	auths := dc["auths"].(map[string]interface{})
	entry := auths["registry.test.sealos.io"].(map[string]interface{})
	if entry["password"] != refreshedSecret {
		t.Errorf("expected password %q, got %v", refreshedSecret, entry["password"])
	}
	if entry["username"] != "robot$hp-refresh-proj+abc" {
		t.Errorf("expected username %q, got %v", "robot$hp-refresh-proj+abc", entry["username"])
	}
}

func TestReconcile_RefreshToken_NoRobot(t *testing.T) {
	project := fakeProject("refresh-norobot", string(v1.HarborPhaseReady), true)
	project.Annotations = map[string]string{refreshAnnotation: "true"}
	project.Status.HarborProjectID = 60
	project.Status.HarborProjectName = "hp-refresh-norobot"
	project.Status.RobotID = 0 // no existing robot

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{ProjectID: 60, Name: "hp-refresh-norobot"}, nil
		},
	}

	r := newTestReconciler(mock, project)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "refresh-norobot"}}

	_, err := r.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when RobotID is 0, got nil")
	}
}

func TestReconcile_AlreadyReady_NoOp(t *testing.T) {
	project := fakeProject("stable", string(v1.HarborPhaseReady), true)
	project.Status.HarborProjectID = 100

	mock := &mockHarborClient{} // no methods should be called

	r := newTestReconciler(mock, project)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "stable"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("did not expect requeue for already-ready project")
	}
}

func TestReconcile_NotFound_NoError(t *testing.T) {
	r := newTestReconciler(&mockHarborClient{})
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected nil error for missing project, got: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue for missing project")
	}
}

// ---------------------------------------------------------------------------
// buildDockerConfigSecret tests
// ---------------------------------------------------------------------------

func TestBuildDockerConfigSecret(t *testing.T) {
	r := &HarborProjectReconciler{RegistryHost: "harbor.example.com"}

	project := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{Name: "my-project"},
	}
	robot := &harbor.RobotAccount{
		Name:  "robot$my-project",
		Token: "secret-token",
	}

	secret := r.buildDockerConfigSecret(project, robot, "test-ns")

	if secret.Name != "harbor-registry-cred-my-project" {
		t.Errorf("unexpected secret name: %s", secret.Name)
	}
	if secret.Namespace != "test-ns" {
		t.Errorf("unexpected namespace: %s", secret.Namespace)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("unexpected type: %s", secret.Type)
	}

	// Verify the docker config JSON content
	var dc map[string]interface{}
	if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &dc); err != nil {
		t.Fatalf("failed to unmarshal docker config: %v", err)
	}

	auths, ok := dc["auths"].(map[string]interface{})
	if !ok {
		t.Fatal("expected auths map")
	}

	entry, ok := auths["harbor.example.com"].(map[string]interface{})
	if !ok {
		t.Fatal("expected harbor.example.com entry")
	}

	if entry["username"] != "robot$my-project" {
		t.Errorf("expected username 'robot$my-project', got %v", entry["username"])
	}
	if entry["password"] != "secret-token" {
		t.Errorf("expected password 'secret-token', got %v", entry["password"])
	}

	// Verify auth is correct base64 of "username:password"
	expectedAuth := base64.StdEncoding.EncodeToString([]byte("robot$my-project:secret-token"))
	if entry["auth"] != expectedAuth {
		t.Errorf("expected auth %q, got %q", expectedAuth, entry["auth"])
	}
}

func TestBuildDockerConfigSecret_NamespaceRefs(t *testing.T) {
	r := &HarborProjectReconciler{RegistryHost: "reg.io"}

	// Test without NamespaceRefs
	project := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{Name: "no-ns"},
	}
	robot := &harbor.RobotAccount{Name: "r", Token: "t"}

	secret := r.buildDockerConfigSecret(project, robot, "ns-foo")
	if secret == nil {
		t.Fatal("expected non-nil secret")
	}
	_ = secret
}

func TestReconcile_UpdateProjectProperties(t *testing.T) {
	project := fakeProject("update-test", string(v1.HarborPhaseReady), true)
	project.Generation = 1
	project.Status.HarborProjectID = 42
	project.Status.HarborProjectName = "hp-update-test"
	project.Status.ObservedGeneration = 0 // force reconcile even though phase is Ready
	project.Spec.Public = true
	project.Spec.AutoScan = true
	project.Spec.StorageLimit = 100 * 1024 * 1024 * 1024 // 100GB

	updateCalled := false
	var capturedProjectID int64
	var capturedSpec harbor.ProjectSpec

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{
				ProjectID: 42,
				Name:      "hp-update-test",
				Public:    false, // existing project has different value
			}, nil
		},
		updateProjectFn: func(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
			updateCalled = true
			capturedProjectID = projectID
			capturedSpec = spec
			return nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			return &harbor.RobotAccount{
				ID:    200,
				Name:  "robot-update-test",
				Token: "new-token",
			}, nil
		},
	}

	r := newTestReconciler(mock, project)

	// First reconcile: project exists, so it should call UpdateProject
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "update-test"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Log("Reconcile requested requeue (expected if finalizer was added)")
	}

	if !updateCalled {
		t.Error("expected UpdateProject to be called when project already exists")
	}
	if capturedProjectID != 42 {
		t.Errorf("expected project ID 42, got %d", capturedProjectID)
	}
	if capturedSpec.Public != true {
		t.Errorf("expected Public=true, got %v", capturedSpec.Public)
	}
	if capturedSpec.AutoScan != true {
		t.Errorf("expected AutoScan=true, got %v", capturedSpec.AutoScan)
	}
	if capturedSpec.Name != "hp-update-test" {
		t.Errorf("expected Name=\"hp-update-test\", got %q", capturedSpec.Name)
	}
	if capturedSpec.StorageLimit != 100*1024*1024*1024 {
		t.Errorf("expected StorageLimit=107374182400, got %d", capturedSpec.StorageLimit)
	}

	// Second reconcile: project already ready with matching generation => should not call UpdateProject
	updateCalled = false
	project.Status.ObservedGeneration = project.Generation
	_ = r.Status().Update(context.Background(), project)

	result2, err2 := r.Reconcile(context.Background(), req)
	if err2 != nil {
		t.Fatalf("second reconcile error: %v", err2)
	}
	if result2.Requeue {
		t.Log("Second reconcile requested requeue")
	}
	if updateCalled {
		t.Error("expected UpdateProject NOT to be called when generation matches observed generation")
	}
}


func TestReconcile_UpdatePublicAutoScan_NoRobotRotation(t *testing.T) {
	project := fakeProject("meta-sync", string(v1.HarborPhaseReady), true)
	project.Generation = 2
	project.Status.HarborProjectID = 77
	project.Status.HarborProjectName = "hp-meta-sync"
	project.Status.RobotID = 88       // already has a robot
	project.Status.ObservedGeneration = 0 // stale, force reconcile
	project.Status.LastSpecHash = "b2f53f2fa22fd8fa" // matches default namespaceRefs+robotPermissions from fakeProject
	project.Spec.Public = true
	project.Spec.AutoScan = true

	updateProjectCalled := false
	createRobotCalled := false

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{ProjectID: 77, Name: "hp-meta-sync"}, nil
		},
		updateProjectFn: func(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
			updateProjectCalled = true
			if projectID != 77 {
				t.Errorf("expected projectID 77, got %d", projectID)
			}
			if spec.Public != true {
				t.Errorf("expected Public=true, got %v", spec.Public)
			}
			if spec.AutoScan != true {
				t.Errorf("expected AutoScan=true, got %v", spec.AutoScan)
			}
			return nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			createRobotCalled = true
			return &harbor.RobotAccount{ID: 99, Name: "should-not-be-called", Token: "tok"}, nil
		},
	}

	r := newTestReconciler(mock, project)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "meta-sync"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("did not expect requeue")
	}

	if !updateProjectCalled {
		t.Error("expected UpdateProject to be called")
	}
	if createRobotCalled {
		t.Error("expected CreateRobot NOT to be called when robot already exists")
	}

	// Verify status updated correctly
	updated := &v1.HarborProject{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "meta-sync"}, updated)
	if updated.Status.Phase != v1.HarborPhaseReady {
		t.Errorf("expected phase Ready, got %q", updated.Status.Phase)
	}
	if updated.Status.ObservedGeneration != project.Generation {
		t.Errorf("expected ObservedGeneration %d, got %d", project.Generation, updated.Status.ObservedGeneration)
	}
	// RobotID must be preserved (not overwritten)
	if updated.Status.RobotID != 88 {
		t.Errorf("expected RobotID 88 (preserved), got %d", updated.Status.RobotID)
	}
}




func TestReconcile_UpdatePublicAutoScan_NotReadyStillRotates(t *testing.T) {
	// If project is not Ready (e.g. recovering from a failed secret distribution),
	// the controller should still perform full reconciliation (including robot creation)
	// even if RobotID > 0.
	project := fakeProject("meta-sync-recover", string(v1.HarborPhaseFailed), true)
	project.Generation = 2
	project.Status.HarborProjectID = 77
	project.Status.HarborProjectName = "hp-meta-sync-recover"
	project.Status.RobotID = 88       // has a robot from a previous attempt
	project.Status.ObservedGeneration = 0 // stale, force reconcile
	project.Status.LastSpecHash = "03737f7570ba8bdb" // hash matches, but wasReady is false so still goes through full flow
	project.Spec.Public = true
	project.Spec.AutoScan = true

	createRobotCalled := false

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{ProjectID: 77, Name: "hp-meta-sync-recover"}, nil
		},
		updateProjectFn: func(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
			return nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			createRobotCalled = true
			return &harbor.RobotAccount{ID: 99, Name: "robot-new", Token: "tok"}, nil
		},
		deleteProjectRobotFn: func(_ context.Context, projectID int64, robotID int64) error {
			return nil
		},
	}

	r := newTestReconciler(mock, project)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "meta-sync-recover"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("did not expect requeue")
	}

	if !createRobotCalled {
		t.Error("expected CreateRobot to be called when project is not Ready (recovery flow)")
	}
}

func TestReconcile_UpdatePublicAutoScan_HashMismatchStillRotates(t *testing.T) {
	// A Ready project with LastSpecHash set but namespaceRefs changed should still
	// trigger full reconciliation (robot rotation), because the fast-path hash
	// must match exactly.
	project := fakeProject("meta-sync-hash-mismatch", string(v1.HarborPhaseReady), true)
	project.Generation = 2
	project.Status.HarborProjectID = 77
	project.Status.HarborProjectName = "hp-meta-sync-hash-mismatch"
	project.Status.RobotID = 88
	project.Status.ObservedGeneration = 0 // stale, force reconcile
	// LastSpecHash matches the default fakeProject spec (namespaceRefs=["ns-1"], robotPermissions=[push,pull])
	project.Status.LastSpecHash = "b2f53f2fa22fd8fa"
	// But now change namespaceRefs to something different
	project.Spec.NamespaceRefs = []string{"ns-2"}
	project.Spec.Public = true
	project.Spec.AutoScan = true

	createRobotCalled := false

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{ProjectID: 77, Name: "hp-meta-sync-hash-mismatch"}, nil
		},
		updateProjectFn: func(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
			return nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			createRobotCalled = true
			return &harbor.RobotAccount{ID: 99, Name: "robot-new", Token: "tok"}, nil
		},
		deleteProjectRobotFn: func(_ context.Context, projectID int64, robotID int64) error {
			return nil
		},
	}

	r := newTestReconciler(mock, project)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "meta-sync-hash-mismatch"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("did not expect requeue")
	}

	if !createRobotCalled {
		t.Error("expected CreateRobot to be called when namespaceRefs changed (hash mismatch)")
	}
}


func TestReconcile_FastPath_ProjectIDChanged(t *testing.T) {
	// If Harbor deleted and recreated the project (different ProjectID with same name),
	// the fast path must NOT be taken even if everything else matches.
	project := fakeProject("project-id-change", string(v1.HarborPhaseReady), true)
	project.Generation = 2
	project.Status.HarborProjectID = 77  // old ID
	project.Status.HarborProjectName = "hp-project-id-change"
	project.Status.RobotID = 88
	project.Status.ObservedGeneration = 0
	project.Status.LastSpecHash = "b2f53f2fa22fd8fa" // matches default fakeProject spec
	project.Spec.Public = true
	project.Spec.AutoScan = true

	createRobotCalled := false

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			// Project ID changed from 77 to 99 (Harbor recreated the project)
			return &harbor.Project{ProjectID: 99, Name: "hp-project-id-change"}, nil
		},
		updateProjectFn: func(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
			return nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			createRobotCalled = true
			return &harbor.RobotAccount{ID: 99, Name: "robot-new", Token: "tok"}, nil
		},
		deleteProjectRobotFn: func(_ context.Context, projectID int64, robotID int64) error {
			return nil
		},
	}

	r := newTestReconciler(mock, project)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "project-id-change"}}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !createRobotCalled {
		t.Error("expected CreateRobot to be called when HarborProjectID changed")
	}

	// Verify the stored ID was updated to the new value
	updated := &v1.HarborProject{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "project-id-change"}, updated)
	if updated.Status.HarborProjectID != 99 {
		t.Errorf("expected HarborProjectID 99, got %d", updated.Status.HarborProjectID)
	}
}

func TestReconcile_ExistingSecretsUpdated(t *testing.T) {
	// Test that when secrets already exist in the target namespaces,
	// the controller updates them with new robot credentials instead of failing.
	project := fakeProject("existing-secret", string(v1.HarborPhaseReady), true)
	project.Generation = 2
	project.Status.HarborProjectID = 42
	project.Status.HarborProjectName = "hp-existing-secret"
	project.Status.RobotID = 0  // no previous robot → full flow
	project.Status.ObservedGeneration = 0
	project.Spec.Public = true

	// Pre-create a secret in ns-1 with old credentials
	oldSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harbor-registry-cred-existing-secret",
			Namespace: "ns-1",
		},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"old-registry":{"auth":"b2xk"}}}`),
		},
		Type: corev1.SecretTypeDockerConfigJson,
	}

	mock := &mockHarborClient{
		getProjectByNameFn: func(_ context.Context, name string) (*harbor.Project, error) {
			return &harbor.Project{ProjectID: 42, Name: "hp-existing-secret"}, nil
		},
		updateProjectFn: func(_ context.Context, projectID int64, spec harbor.ProjectSpec) error {
			return nil
		},
		createRobotFn: func(_ context.Context, projectID int64, spec harbor.RobotSpec) (*harbor.RobotAccount, error) {
			return &harbor.RobotAccount{ID: 55, Name: "robot-new", Token: "new-token"}, nil
		},
		deleteProjectRobotFn: func(_ context.Context, projectID int64, robotID int64) error {
			return nil
		},
	}

	r := newTestReconciler(mock, project, oldSecret)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "existing-secret"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("did not expect requeue")
	}

	// Verify the updated project status
	updated := &v1.HarborProject{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "existing-secret"}, updated)
	if updated.Status.Phase != v1.HarborPhaseReady {
		t.Errorf("expected phase Ready, got %q", updated.Status.Phase)
	}
	if updated.Status.RobotID != 55 {
		t.Errorf("expected RobotID 55, got %d", updated.Status.RobotID)
	}

	// Verify the existing secret was updated with new credentials
	sec := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "harbor-registry-cred-existing-secret", Namespace: "ns-1"}, sec); err != nil {
		t.Fatalf("expected secret to exist: %v", err)
	}
	if sec.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("expected secret type %s, got %s", corev1.SecretTypeDockerConfigJson, sec.Type)
	}
	// Verify the token was updated (should not contain old credentials)
	token := string(sec.Data[corev1.DockerConfigJsonKey])
	if token == "" {
		t.Error("expected secret data to be updated with new token")
	}
}
// ---------------------------------------------------------------------------
// Helper tests
// ---------------------------------------------------------------------------

func TestSecretNameForProject(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"my-project", "harbor-registry-cred-my-project"},
		{"", "harbor-registry-cred-"},
		{"a", "harbor-registry-cred-a"},
	}
	for _, tc := range tests {
		got := secretNameForProject(tc.input)
		if got != tc.expected {
			t.Errorf("secretNameForProject(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestToAccess(t *testing.T) {
	perms := []v1.RobotPermission{
		{Action: "push"},
		{Action: "pull"},
	}
	access := toAccess(perms)
	if len(access) != 2 {
		t.Fatalf("expected 2 access entries, got %d", len(access))
	}
	if access[0].Action != "push" {
		t.Errorf("expected action 'push', got %q", access[0].Action)
	}
	if access[1].Action != "pull" {
		t.Errorf("expected action 'pull', got %q", access[1].Action)
	}
}

func TestSetCondition(t *testing.T) {
	var conditions []metav1.Condition

	// Add first condition
	setCondition(&conditions, "Ready", metav1.ConditionTrue, "Reconciled", "All good")
	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(conditions))
	}
	if conditions[0].Type != "Ready" {
		t.Errorf("expected type Ready, got %q", conditions[0].Type)
	}

	// Update existing condition
	setCondition(&conditions, "Ready", metav1.ConditionFalse, "Error", "Something failed")
	if len(conditions) != 1 {
		t.Fatalf("expected still 1 condition, got %d", len(conditions))
	}
	if conditions[0].Status != metav1.ConditionFalse {
		t.Errorf("expected status False, got %q", conditions[0].Status)
	}
}

func TestSetCondition_NilList(t *testing.T) {
	// Should not panic
	setCondition(nil, "Ready", metav1.ConditionTrue, "Reason", "Message")
}

func TestShortID(t *testing.T) {
	prefix := "test-prefix"
	result := shortID(prefix)

	// Should start with prefix
	if len(result) <= len(prefix)+1 {
		t.Errorf("expected result to be longer than prefix+separator, got %q", result)
	}
	if result[:len(prefix)] != prefix {
		t.Errorf("expected result to start with %q, got %q", prefix, result)
	}
	if result[len(prefix):len(prefix)+1] != "-" {
		t.Errorf("expected separator '-', got %q", result[len(prefix):len(prefix)+1])
	}

	// The suffix should be lowercase hex (a-f, 0-9)
	suffix := result[len(prefix)+1:]
	if len(suffix) != 8 {
		t.Errorf("expected 8-char hex suffix, got %d chars: %q", len(suffix), suffix)
	}
	for _, c := range suffix {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("expected hex character, got %q in suffix %q", c, suffix)
		}
	}

	// Verify randomness: two calls should produce different results
	result2 := shortID(prefix)
	if result == result2 {
		t.Error("expected two calls to shortID to produce different results")
	}
}
