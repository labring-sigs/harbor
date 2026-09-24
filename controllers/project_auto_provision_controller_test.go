package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/dinoallo/labring-sigs-harbor/api/v1"
)

const defaultTestStorageLimit int64 = 5 * 1024 * 1024 * 1024 // 5 GiB

func newAutoProvisionReconciler(ownerLabelKey string, defaultStorageLimit int64, objs ...runtime.Object) *ProjectAutoProvisionReconciler {
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	return &ProjectAutoProvisionReconciler{
		Client:                   fakeClient,
		Scheme:                   scheme,
		OwnerLabelKey:            ownerLabelKey,
		DefaultStorageLimitBytes: defaultStorageLimit,
	}
}

func ns(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
			UID:    types.UID(name + "-uid"),
		},
	}
}

func nsWithAnnotations(name string, labels map[string]string, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: annotations,
			UID:         types.UID(name + "-uid"),
		},
	}
}

func hp(name, owner, namespace string, storageLimit int64) *v1.HarborProject {
	return &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				AutoProvisionLabel:      "true",
				SourceNamespaceLabel:    namespace,
				SourceNamespaceUIDLabel: namespace + "-uid",
			},
		},
		Spec: v1.HarborProjectSpec{
			Owner:         owner,
			ProjectName:   namespace,
			NamespaceRefs: []string{namespace},
			StorageLimit:  storageLimit,
			Public:        false,
			AutoScan:      false,
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestAutoProvision_Namespace_WithLabel_CreatesHarborProject(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-1", map[string]string{ownerLabelKey: "user-abc"})

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-1"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	// Verify HarborProject was created
	hpObj := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-1"}, hpObj); err != nil {
		t.Fatalf("expected HarborProject to be created: %v", err)
	}
	if hpObj.Spec.Owner != "user-abc" {
		t.Errorf("expected owner 'user-abc', got %q", hpObj.Spec.Owner)
	}
	if len(hpObj.Spec.NamespaceRefs) != 1 || hpObj.Spec.NamespaceRefs[0] != "ns-1" {
		t.Errorf("expected namespaceRefs [ns-1], got %v", hpObj.Spec.NamespaceRefs)
	}
	if hpObj.Spec.StorageLimit != defaultTestStorageLimit {
		t.Errorf("expected storageLimit %d, got %d", defaultTestStorageLimit, hpObj.Spec.StorageLimit)
	}
	if hpObj.Spec.Public {
		t.Error("expected public false")
	}
	if hpObj.Spec.AutoScan {
		t.Error("expected autoScan false")
	}
	if hpObj.Labels[AutoProvisionLabel] != "true" {
		t.Errorf("expected label %s=true", AutoProvisionLabel)
	}
	if hpObj.Labels[SourceNamespaceLabel] != "ns-1" {
		t.Errorf("expected label %s=ns-1", SourceNamespaceLabel)
	}
	if hpObj.Labels[SourceNamespaceUIDLabel] == "" {
		t.Errorf("expected label %s to be set", SourceNamespaceUIDLabel)
	}
}

func TestAutoProvision_Namespace_WithoutLabel_DoesNothing(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-2", map[string]string{"some-other-label": "val"})

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-2"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	// Verify no HarborProject was created
	hpObj := &v1.HarborProject{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-2"}, hpObj)
	if err == nil {
		t.Fatal("expected HarborProject not to be created")
	}
}

func TestAutoProvision_Namespace_WithEmptyLabel_DoesNothing(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-3", map[string]string{ownerLabelKey: ""})

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-3"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	hpObj := &v1.HarborProject{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-3"}, hpObj)
	if err == nil {
		t.Fatal("expected HarborProject not to be created for empty owner label")
	}
}

func TestAutoProvision_Namespace_Deleting_DoesNothing(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	now := metav1.Now()
	nsObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ns-4",
			Labels:            map[string]string{ownerLabelKey: "user-abc"},
			UID:               types.UID("ns-4-uid"),
			DeletionTimestamp: &now,
		},
	}

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-4"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	hpObj := &v1.HarborProject{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-4"}, hpObj)
	if err == nil {
		t.Fatal("expected HarborProject not to be created when namespace is deleting")
	}
}

func TestAutoProvision_ExistingHP_WithMatchingSpec_DoesNotUpdate(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-5", map[string]string{ownerLabelKey: "user-abc"})

	existingHP := hp("hp-ns-5", "user-abc", "ns-5", defaultTestStorageLimit)

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj, existingHP)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-5"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	// The existing HP should be unchanged; we just verify it still exists
	after := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-5"}, after); err != nil {
		t.Fatalf("failed to get HP: %v", err)
	}
	if after.Spec.Owner != "user-abc" {
		t.Errorf("expected owner 'user-abc', got %q", after.Spec.Owner)
	}
	if after.Spec.StorageLimit != defaultTestStorageLimit {
		t.Errorf("expected storageLimit %d, got %d", defaultTestStorageLimit, after.Spec.StorageLimit)
	}
}

func TestAutoProvision_ExistingHP_WithDifferentOwner_Updates(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-6", map[string]string{ownerLabelKey: "user-abc"})

	existingHP := hp("hp-ns-6", "user-old", "ns-6", defaultTestStorageLimit)

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj, existingHP)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-6"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	after := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-6"}, after); err != nil {
		t.Fatalf("failed to get HP: %v", err)
	}
	if after.Spec.Owner != "user-abc" {
		t.Errorf("expected updated owner 'user-abc', got %q", after.Spec.Owner)
	}
}

func TestAutoProvision_Namespace_DeletedAndRecreated_SameName_DifferentUID_Adopts(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-7", map[string]string{ownerLabelKey: "user-abc"})

	// Existing CR with old UID (simulating namespace delete+recreate)
	existingHP := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hp-ns-7",
			Labels: map[string]string{
				AutoProvisionLabel:      "true",
				SourceNamespaceLabel:    "ns-7",
				SourceNamespaceUIDLabel: "old-uid",
			},
		},
		Spec: v1.HarborProjectSpec{
			Owner:         "user-abc",
			ProjectName:   "ns-7",
			NamespaceRefs: []string{"ns-7"},
			StorageLimit:  defaultTestStorageLimit,
			Public:        false,
			AutoScan:      false,
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
		},
	}

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj, existingHP)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-7"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	// The CR should be updated with the new UID
	after := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-7"}, after); err != nil {
		t.Fatalf("failed to get HP: %v", err)
	}
	if after.Labels[SourceNamespaceUIDLabel] != string(nsObj.UID) {
		t.Errorf("expected UID label %q, got %q", string(nsObj.UID), after.Labels[SourceNamespaceUIDLabel])
	}
}

func TestAutoProvision_MissingNamespace_NoError(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error for missing namespace, got: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue for missing namespace")
	}
}

func TestAutoProvision_AdoptsExistingHP_WithWrongSpec(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-8", map[string]string{ownerLabelKey: "user-abc"})

	// Manually-created HP with different spec (different owner, different storageLimit)
	existingHP := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hp-ns-8",
			Labels: map[string]string{
				"some-other-label": "val",
			},
		},
		Spec: v1.HarborProjectSpec{
			Owner:         "user-manual",
			NamespaceRefs: []string{"other-ns"},
			StorageLimit:  -1,
			Public:        true,
			AutoScan:      true,
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
			},
		},
	}

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj, existingHP)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-8"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	// Verify the HP was adopted (spec updated to match desired)
	after := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-8"}, after); err != nil {
		t.Fatalf("failed to get HP: %v", err)
	}
	if after.Spec.Owner != "user-abc" {
		t.Errorf("expected owner 'user-abc', got %q", after.Spec.Owner)
	}
	if len(after.Spec.NamespaceRefs) != 1 || after.Spec.NamespaceRefs[0] != "ns-8" {
		t.Errorf("expected namespaceRefs [ns-8], got %v", after.Spec.NamespaceRefs)
	}
	if after.Spec.StorageLimit != defaultTestStorageLimit {
		t.Errorf("expected storageLimit %d, got %d", defaultTestStorageLimit, after.Spec.StorageLimit)
	}
	if after.Spec.Public {
		t.Error("expected public false")
	}
	// Verify auto-provision labels are added
	if after.Labels[AutoProvisionLabel] != "true" {
		t.Errorf("expected auto-provision label to be added")
	}
	if after.Labels[SourceNamespaceLabel] != "ns-8" {
		t.Errorf("expected label %s=ns-8, got %q", SourceNamespaceLabel, after.Labels[SourceNamespaceLabel])
	}
	if after.Labels[SourceNamespaceUIDLabel] == "" {
		t.Errorf("expected label %s to be set", SourceNamespaceUIDLabel)
	}
}

func TestAutoProvision_ExistingHP_MissingLabels_TriggersUpdate(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	nsObj := ns("ns-9", map[string]string{ownerLabelKey: "user-abc"})

	// Existing CR that has matching spec but is missing auto-provision labels
	existingHP := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hp-ns-9",
			Labels: map[string]string{
				"some-other-label": "val",
			},
		},
		Spec: v1.HarborProjectSpec{
			Owner:         "user-abc",
			NamespaceRefs: []string{"ns-9"},
			StorageLimit:  defaultTestStorageLimit,
			Public:        false,
			AutoScan:      false,
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
		},
	}

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj, existingHP)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-9"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	// Verify the HP was updated with labels
	after := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-9"}, after); err != nil {
		t.Fatalf("failed to get HP: %v", err)
	}
	if after.Labels[AutoProvisionLabel] != "true" {
		t.Errorf("expected label %s=true, got %q", AutoProvisionLabel, after.Labels[AutoProvisionLabel])
	}
	if after.Labels[SourceNamespaceLabel] != "ns-9" {
		t.Errorf("expected label %s=ns-9, got %q", SourceNamespaceLabel, after.Labels[SourceNamespaceLabel])
	}
	if after.Labels[SourceNamespaceUIDLabel] == "" {
		t.Errorf("expected label %s to be set", SourceNamespaceUIDLabel)
	}
}

// ---------------------------------------------------------------------------
// Namespace annotation override tests
// ---------------------------------------------------------------------------

func TestAutoProvision_Namespace_StorageLimitAnnotation_OverridesDefault(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	// Annotation value in bytes: 10 GiB
	annotations := map[string]string{StorageLimitAnnotation: "10737418240"}
	nsObj := nsWithAnnotations("ns-ann-1", map[string]string{ownerLabelKey: "user-abc"}, annotations)

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-ann-1"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	hpObj := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-ann-1"}, hpObj); err != nil {
		t.Fatalf("expected HarborProject to be created: %v", err)
	}
	if hpObj.Spec.StorageLimit != 10737418240 {
		t.Errorf("expected storageLimit 10737418240 (10 GiB), got %d", hpObj.Spec.StorageLimit)
	}
}

func TestAutoProvision_Namespace_StorageLimitAnnotation_Unlimited(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	annotations := map[string]string{StorageLimitAnnotation: "-1"}
	nsObj := nsWithAnnotations("ns-ann-2", map[string]string{ownerLabelKey: "user-abc"}, annotations)

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-ann-2"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	hpObj := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-ann-2"}, hpObj); err != nil {
		t.Fatalf("expected HarborProject to be created: %v", err)
	}
	if hpObj.Spec.StorageLimit != -1 {
		t.Errorf("expected storageLimit -1 (unlimited), got %d", hpObj.Spec.StorageLimit)
	}
}

func TestAutoProvision_Namespace_StorageLimitAnnotation_Invalid_FallsBack(t *testing.T) {
	ownerLabelKey := "user.sealos.io/owner"
	// Invalid annotation value (not a valid integer)
	annotations := map[string]string{StorageLimitAnnotation: "not-a-number"}
	nsObj := nsWithAnnotations("ns-ann-3", map[string]string{ownerLabelKey: "user-abc"}, annotations)

	r := newAutoProvisionReconciler(ownerLabelKey, defaultTestStorageLimit, nsObj)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-ann-3"}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("expected no requeue")
	}

	// Should fall back to the default
	hpObj := &v1.HarborProject{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "hp-ns-ann-3"}, hpObj); err != nil {
		t.Fatalf("expected HarborProject to be created: %v", err)
	}
	if hpObj.Spec.StorageLimit != defaultTestStorageLimit {
		t.Errorf("expected storageLimit %d (fallback), got %d", defaultTestStorageLimit, hpObj.Spec.StorageLimit)
	}
}
