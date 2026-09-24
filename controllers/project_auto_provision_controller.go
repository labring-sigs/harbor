package controllers

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/dinoallo/labring-sigs-harbor/api/v1"
)

const (
	// AutoProvisionLabel is set on automatically created HarborProject CRs
	// to distinguish them from manually created ones.
	AutoProvisionLabel = "harbor.sealos.io/auto-provision"

	// SourceNamespaceLabel records the namespace from which the HarborProject
	// was auto-provisioned.
	SourceNamespaceLabel = "harbor.sealos.io/source-namespace"

	// SourceNamespaceUIDLabel records the UID of the namespace at the time of
	// auto-provisioning. This is used to detect namespace recreation: if the
	// namespace is deleted and recreated with the same name but a different UID,
	// the controller will fully adopt the new namespace rather than treating the
	// existing HarborProject CR as stale.
	SourceNamespaceUIDLabel = "harbor.sealos.io/source-namespace-uid"

	// StorageLimitAnnotation is an optional annotation on a Namespace that
	// overrides the default storage limit for the auto-provisioned
	// HarborProject. The value is a byte count (e.g. "10737418240" for 10 GB)
	// or "-1" for unlimited. When absent, DefaultStorageLimitBytes is used.
	StorageLimitAnnotation = "harbor.sealos.io/storage-limit"
)

// ProjectAutoProvisionReconciler watches Namespace resources and automatically
// creates or updates a HarborProject CR for every namespace that carries the
// configured owner label key.
//
// This is an OPTIONAL controller. It is only registered with the manager when
// the --enable-project-auto-provision flag is passed to the binary.
type ProjectAutoProvisionReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	OwnerLabelKey string

	// MaxConcurrentReconciles is the maximum number of concurrent reconciles
	// for the ProjectAutoProvision controller. Defaults to 1.
	MaxConcurrentReconciles int

	// DefaultStorageLimitBytes is the storage limit (in bytes) assigned to
	// auto-provisioned HarborProject CRs when the namespace does not carry
	// the StorageLimitAnnotation. Use -1 for unlimited.
	DefaultStorageLimitBytes int64
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=harbor.sealos.io,resources=harborprojects,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=harbor.sealos.io,resources=harborprojects/finalizers,verbs=update

// Reconcile handles Namespace create/update events.
//   - If the namespace has the configured owner label → ensure a HarborProject CR exists
//   - If the namespace does not have the label → no-op (the user opted to not delete on label removal)
func (r *ProjectAutoProvisionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the namespace
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, req.NamespacedName, ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if the namespace is being deleted
	if !ns.DeletionTimestamp.IsZero() {
		logger.V(3).Info("namespace is being deleted, skipping", "namespace", ns.Name)
		return ctrl.Result{}, nil
	}

	// 2. Check for the owner label
	owner, hasLabel := ns.Labels[r.OwnerLabelKey]
	if !hasLabel || owner == "" {
		// Label not present → no-op (user chose to not delete on removal)
		return ctrl.Result{}, nil
	}

	// 3. Determine storage limit: namespace annotation overrides the default
	storageLimit := r.DefaultStorageLimitBytes
	if v, ok := ns.Annotations[StorageLimitAnnotation]; ok {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			logger.Info("invalid storage-limit annotation, falling back to default",
				"namespace", ns.Name,
				"annotation", StorageLimitAnnotation,
				"value", v,
				"error", err,
			)
		} else {
			storageLimit = parsed
		}
	}

	// 4. Construct the desired HarborProject spec
	hpName := "hp-" + ns.Name

	desired := &v1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name: hpName,
			Labels: map[string]string{
				AutoProvisionLabel:      "true",
				SourceNamespaceLabel:    ns.Name,
				SourceNamespaceUIDLabel: string(ns.UID),
			},
		},
		Spec: v1.HarborProjectSpec{
			Owner:         owner,
			ProjectName:   ns.Name,
			NamespaceRefs: []string{ns.Name},
			StorageLimit:  storageLimit,
			Public:        false,
			AutoScan:      false,
			RobotPermissions: []v1.RobotPermission{
				{Action: "push"},
				{Action: "pull"},
			},
		},
	}

	// 5. Try to get the existing HarborProject CR
	existing := &v1.HarborProject{}
	if err := r.Get(ctx, types.NamespacedName{Name: hpName}, existing); err != nil {
		if errors.IsNotFound(err) {
			// 5a. Does not exist → create it
			logger.Info("creating auto-provisioned HarborProject",
				"namespace", ns.Name,
				"hpName", hpName,
				"owner", owner,
				"storageLimit", storageLimit,
			)
			if err := r.Create(ctx, desired); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create HarborProject %s: %w", hpName, err)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get HarborProject %s: %w", hpName, err)
	}

	// 6. Adopt/update existing CR
	if needsUpdate(existing, desired) {
		logger.Info("updating auto-provisioned HarborProject",
			"namespace", ns.Name,
			"hpName", hpName,
			"owner", owner,
			"storageLimit", storageLimit,
		)
		updated := existing.DeepCopy()
		updated.Spec.Owner = desired.Spec.Owner
		updated.Spec.ProjectName = desired.Spec.ProjectName
		updated.Spec.NamespaceRefs = desired.Spec.NamespaceRefs
		updated.Spec.StorageLimit = desired.Spec.StorageLimit
		updated.Spec.Public = desired.Spec.Public
		updated.Spec.AutoScan = desired.Spec.AutoScan
		updated.Spec.RobotPermissions = desired.Spec.RobotPermissions
		// Merge labels, preserving any existing ones but overwriting our managed labels
		if updated.Labels == nil {
			updated.Labels = make(map[string]string)
		}
		for k, v := range desired.Labels {
			updated.Labels[k] = v
		}
		if err := r.Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update HarborProject %s: %w", hpName, err)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the Namespace watch.
func (r *ProjectAutoProvisionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Named("project-auto-provision").
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}

// needsUpdate compares the spec-relevant fields and managed labels of an
// existing HarborProject against the desired one. It returns true if an
// update is needed.
func needsUpdate(existing, desired *v1.HarborProject) bool {
	// Compare spec fields
	if existing.Spec.Owner != desired.Spec.Owner {
		return true
	}
	if existing.Spec.ProjectName != desired.Spec.ProjectName {
		return true
	}
	if existing.Spec.Public != desired.Spec.Public {
		return true
	}
	if existing.Spec.AutoScan != desired.Spec.AutoScan {
		return true
	}
	if existing.Spec.StorageLimit != desired.Spec.StorageLimit {
		return true
	}
	if !stringSlicesEqual(existing.Spec.NamespaceRefs, desired.Spec.NamespaceRefs) {
		return true
	}
	if !robotPermissionsEqual(existing.Spec.RobotPermissions, desired.Spec.RobotPermissions) {
		return true
	}

	// Compare managed labels
	if existing.Labels[AutoProvisionLabel] != desired.Labels[AutoProvisionLabel] {
		return true
	}
	if existing.Labels[SourceNamespaceLabel] != desired.Labels[SourceNamespaceLabel] {
		return true
	}
	if existing.Labels[SourceNamespaceUIDLabel] != desired.Labels[SourceNamespaceUIDLabel] {
		return true
	}

	return false
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func robotPermissionsEqual(a, b []v1.RobotPermission) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Action != b[i].Action {
			return false
		}
	}
	return true
}
