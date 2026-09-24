package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"

	stderrors "errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/dinoallo/labring-sigs-harbor/api/v1"
	"github.com/dinoallo/labring-sigs-harbor/internal/harbor"
)

const (
	harborFinalizer   = "harbor.sealos.io/cleanup"
	refreshAnnotation = "harbor.sealos.io/refresh-token"
	projectLabel      = "harbor.sealos.io/project"
)

// HarborProjectReconciler reconciles a HarborProject object
type HarborProjectReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	HarborClient HarborAPIClient
	RegistryHost string // e.g. harbor.sealos.example.com

	// RotationStateNamespace is the namespace where rotation-pending Secrets
	// are stored. If empty, rotation state is not persisted and retries will
	// generate a fresh secret (safe but may briefly invalidate credentials).
	RotationStateNamespace string
}

// +kubebuilder:rbac:groups=harbor.sealos.io,resources=harborprojects,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=harbor.sealos.io,resources=harborprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=harbor.sealos.io,resources=harborprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="coordination.k8s.io",resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles HarborProject changes
func (r *HarborProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	project := &v1.HarborProject{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !project.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, project)
	}

	// Handle token refresh trigger via annotation (must be exactly "true")
	if project.Annotations != nil {
		if v, ok := project.Annotations[refreshAnnotation]; ok && v == "true" && project.Status.Phase == v1.HarborPhaseReady {
			return r.reconcileRefreshToken(ctx, project)
		}
	}

	return r.reconcileCreate(ctx, project)
}

func (r *HarborProjectReconciler) reconcileCreate(ctx context.Context, project *v1.HarborProject) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(project, harborFinalizer) {
		controllerutil.AddFinalizer(project, harborFinalizer)
		if err := r.Update(ctx, project); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// If Ready and ObservedGeneration matches Generation, spec has not changed
	if project.Status.Phase == v1.HarborPhaseReady && project.Status.ObservedGeneration == project.Generation {
		return ctrl.Result{}, nil
	}

	// Capture whether the project was Ready before we modify the phase below.
	wasReady := project.Status.Phase == v1.HarborPhaseReady
	// Set phase to Creating
	project.Status.Phase = v1.HarborPhaseCreating
	if err := r.Status().Update(ctx, project); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status to Creating: %w", err)
	}

	// Propagate owner to status
	project.Status.Owner = project.Spec.Owner

	// Determine project name
	projectName := project.Spec.ProjectName
	if projectName == "" {
		projectName = "hp-" + project.Name
	}

	// Create or get Harbor project
	hbProject, err := r.HarborClient.GetProjectByName(ctx, projectName)
	if err != nil {
		project.Status.Phase = v1.HarborPhaseFailed
		_ = r.Status().Update(ctx, project)
		return ctrl.Result{}, fmt.Errorf("failed to get harbor project: %w", err)
	}

	if hbProject == nil {
		projectID, err := r.HarborClient.CreateProject(ctx, harbor.ProjectSpec{
			Name:         projectName,
			Public:       project.Spec.Public,
			StorageLimit: project.Spec.StorageLimit,
			AutoScan:     project.Spec.AutoScan,
		})
		if err != nil {
			project.Status.Phase = v1.HarborPhaseFailed
			_ = r.Status().Update(ctx, project)
			return ctrl.Result{}, fmt.Errorf("failed to create harbor project: %w", err)
		}
		project.Status.HarborProjectID = projectID
		project.Status.HarborProjectName = projectName
	} else {
		// Update existing project properties to match spec
		if err := r.HarborClient.UpdateProject(ctx, hbProject.ProjectID, harbor.ProjectSpec{
			Name:         projectName,
			Public:       project.Spec.Public,
			StorageLimit: project.Spec.StorageLimit,
			AutoScan:     project.Spec.AutoScan,
		}); err != nil {
			project.Status.Phase = v1.HarborPhaseFailed
			_ = r.Status().Update(ctx, project)
			return ctrl.Result{}, fmt.Errorf("failed to update harbor project: %w", err)
		}
		// Capture the previous Harbor project ID before overwriting it,
		// so we can detect if Harbor deleted and recreated the project.
		prevHarborProjectID := project.Status.HarborProjectID
		project.Status.HarborProjectID = hbProject.ProjectID
		project.Status.HarborProjectName = hbProject.Name

		// If the project was previously ready, has a robot account, the
		// full-flow fields haven't changed, and the Harbor project identity
		// is unchanged, skip robot/secret recreation. This handles the common
		// case where a user changes public/autoScan/storageLimit on an existing
		// project.
		if wasReady && project.Status.RobotID > 0 && project.Status.LastSpecHash == computeSpecHash(projectName, project.Spec.NamespaceRefs, project.Spec.RobotPermissions) && prevHarborProjectID == hbProject.ProjectID {
			project.Status.Phase = v1.HarborPhaseReady
			project.Status.ObservedGeneration = project.Generation
			logger.Info("HarborProject metadata synced to Harbor",
				"project", projectName,
				"id", project.Status.HarborProjectID)
			return ctrl.Result{}, r.Status().Update(ctx, project)
		}
	}

	// Capture the previous robot ID before overwriting it with the new one.
	oldRobotID := project.Status.RobotID

	// Phase 1: Create a single shared Robot Account for all namespaces.
	// Future: create per-namespace robots with different permissions when needed.
	robot, err := r.HarborClient.CreateRobot(ctx, project.Status.HarborProjectID, harbor.RobotSpec{
		Name:     "robot-" + shortID(project.Name),
		Duration: -1, // never expire
		Permissions: []harbor.RobotPermission{
			{
				Kind:      "project",
				Namespace: projectName,
				Access:    toAccess(project.Spec.RobotPermissions),
			},
		},
	})
	if err != nil {
		project.Status.Phase = v1.HarborPhaseFailed
		_ = r.Status().Update(ctx, project)
		return ctrl.Result{}, fmt.Errorf("failed to create robot account: %w", err)
	}
	project.Status.RobotName = robot.Name
	project.Status.RobotID = robot.ID

	// Distribute Secrets to all target namespaces
	for _, ns := range project.Spec.NamespaceRefs {
		secret := r.buildDockerConfigSecret(project, robot, ns)
		if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
			logger.Error(err, "failed to create secret in namespace", "namespace", ns)
			project.Status.Phase = v1.HarborPhaseFailed
			_ = r.Status().Update(ctx, project)
			return ctrl.Result{}, fmt.Errorf("failed to create secret in namespace %s: %w", ns, err)
		} else if errors.IsAlreadyExists(err) {
			// Secret already exists — update it with correct credentials
			existing := &corev1.Secret{}
			if getErr := r.Get(ctx, client.ObjectKey{Name: secret.Name, Namespace: ns}, existing); getErr == nil {
				existing.Data = secret.Data
				existing.Labels = secret.Labels
				if updateErr := r.Update(ctx, existing); updateErr != nil {
					logger.Error(updateErr, "failed to update existing secret in namespace", "namespace", ns)
					project.Status.Phase = v1.HarborPhaseFailed
					_ = r.Status().Update(ctx, project)
					return ctrl.Result{}, fmt.Errorf("failed to update secret in namespace %s: %w", ns, updateErr)
				}
			} else {
				logger.Error(getErr, "failed to get existing secret in namespace", "namespace", ns)
				project.Status.Phase = v1.HarborPhaseFailed
				_ = r.Status().Update(ctx, project)
				return ctrl.Result{}, fmt.Errorf("failed to get existing secret in namespace %s: %w", ns, getErr)
			}
		}
	}

	// Delete the previous robot account now that the replacement is durable.
	// This follows the create -> update -> delete ordering used by the
	// refresh path to avoid invalidating credentials on transient failures.
	if oldRobotID > 0 {
		logger.V(1).Info("removing previous robot account",
			"robotID", oldRobotID)
		if err := r.HarborClient.DeleteProjectRobot(ctx, project.Status.HarborProjectID, oldRobotID); err != nil {
			// Log but don't fail — the robot might already be gone.
			logger.Error(err, "failed to delete previous robot (continuing)", "robotID", oldRobotID)
		}
	}

	// Compute and store the hash of full-flow fields for fast-path detection
	// on subsequent reconciliations.
	project.Status.LastSpecHash = computeSpecHash(projectName, project.Spec.NamespaceRefs, project.Spec.RobotPermissions)

	// Mark as Ready
	project.Status.Phase = v1.HarborPhaseReady
	project.Status.ObservedGeneration = project.Generation

	logger.Info("HarborProject reconciled successfully",
		"project", projectName,
		"id", project.Status.HarborProjectID,
		"owner", project.Status.Owner)
	return ctrl.Result{}, r.Status().Update(ctx, project)
}

func (r *HarborProjectReconciler) reconcileDelete(ctx context.Context, project *v1.HarborProject) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Deleting HarborProject", "project", project.Name)

	// Delete Secrets from all target namespaces
	for _, ns := range project.Spec.NamespaceRefs {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretNameForProject(project.Name),
				Namespace: ns,
			},
		}
		if err := r.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "failed to delete secret", "namespace", ns)
			return ctrl.Result{}, fmt.Errorf("failed to delete secret in namespace %s: %w", ns, err)
		}
	}

	// Delete Harbor project (cascades to all images + robot accounts)
	// Handle idempotency: if Harbor returns 404, treat as success
	if project.Status.HarborProjectID > 0 {
		if err := r.HarborClient.DeleteProject(ctx, project.Status.HarborProjectID); err != nil {
			var apiErr *harbor.ErrAPIError
			if stderrors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				logger.Info("Harbor project already deleted, proceeding with cleanup")
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to delete harbor project: %w", err)
			}
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(project, harborFinalizer)
	return ctrl.Result{}, r.Update(ctx, project)
}

// buildDockerConfigSecret creates a dockerconfigjson Secret in the given namespace
func (r *HarborProjectReconciler) buildDockerConfigSecret(project *v1.HarborProject, robot *harbor.RobotAccount, namespace string) *corev1.Secret {
	// Use Token if set, otherwise fall back to Secret field (real Harbor API)
	token := robot.Token
	if token == "" {
		token = robot.Secret
	}

	auth := base64.StdEncoding.EncodeToString([]byte(robot.Name + ":" + token))

	dockerConfig := map[string]interface{}{
		"auths": map[string]interface{}{
			r.RegistryHost: map[string]string{
				"username": robot.Name,
				"password": token,
				"auth":     auth,
			},
		},
	}
	data, _ := json.Marshal(dockerConfig)

	secretName := secretNameForProject(project.Name)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "harbor-controller",
				"app.kubernetes.io/name":       "harbor-registry-cred",
				projectLabel:                   project.Name,
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: data,
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
// Uses label-based watch for Secrets instead of Owns(), because
// cluster-scoped HarborProject cannot be an owner of namespaced Secrets.
func (r *HarborProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.HarborProject{}).
		Watches(
			&corev1.Secret{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapSecretToProject),
		).
		Complete(r)
}

// mapSecretToProject maps a Secret back to its parent HarborProject using labels.
func (r *HarborProjectReconciler) mapSecretToProject(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	projectName, ok2 := secret.Labels[projectLabel]
	if !ok2 || projectName == "" {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: projectName}},
	}
}

// reconcileRefreshToken performs a token rotation for the robot account:
//  1. Reuse or generate a new secret and persist it in a rotation-state Secret
//  2. Update K8s Secrets in all target namespaces (new credential available)
//  3. Refresh the robot in-place via Harbor v2.2+ API (PATCH)
//  4. Remove the refresh annotation and clean up rotation state
//
// Unlike the old create-new-delete approach, the robot ID/name remain unchanged.
//
// # Outage window
// Harbor rotates the secret immediately with no dual-credential grace period.
// This means there is a brief window where pods still using the old cached
// credential fail to authenticate until they reload the updated K8s Secret.
// The window runs from step 2 (K8s update) through step 3 (Harbor refresh).
// Step 3 is a single API call, so the window is minimised.
//
// # Retry safety
// The pending secret is persisted in a K8s Secret (in RotationStateNamespace)
// so that on retry the same secret is reused. This prevents a retry from
// generating a fresh secret that would invalidate credentials already written
// to K8s in a previous attempt. The rotation state is cleaned up after the
// annotation is successfully removed.
func (r *HarborProjectReconciler) reconcileRefreshToken(ctx context.Context, project *v1.HarborProject) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Refreshing robot token", "project", project.Name)

	robotID := project.Status.RobotID
	if robotID == 0 {
		return ctrl.Result{}, fmt.Errorf("cannot refresh token: no existing robot account (robotID=0)")
	}

	// 1. Reuse pending secret if available (retry after partial failure),
	//    otherwise generate a new cryptographically random secret.
	newSecret, err := r.getRotationState(ctx, project.Name)
	if err != nil {
		logger.Error(err, "failed to read rotation state")
		return ctrl.Result{}, fmt.Errorf("failed to read rotation state: %w", err)
	}
	if newSecret == "" {
		newSecret, err = generateSecret()
		if err != nil {
			logger.Error(err, "failed to generate new secret")
			return ctrl.Result{}, fmt.Errorf("failed to generate new secret: %w", err)
		}
		// Persist so retries reuse the same secret.
		if err := r.setRotationState(ctx, project.Name, newSecret); err != nil {
			logger.Error(err, "failed to persist rotation state")
			return ctrl.Result{}, fmt.Errorf("failed to persist rotation state: %w", err)
		}
	}

	// Build a RobotAccount with the existing name and new secret for secret distribution
	robot := &harbor.RobotAccount{
		ID:     robotID,
		Name:   project.Status.RobotName,
		Secret: newSecret,
		Token:  newSecret,
	}

	// 2. Update secrets in all target namespaces FIRST with the new credential.
	//    At this point the old credential still works in Harbor, so pods
	//    continue to authenticate until they reload and pick up the new secret.
	//    Once updated, pods that reload will get the new credential which
	//    will be valid after step 3 completes.
	for _, ns := range project.Spec.NamespaceRefs {
		secret := r.buildDockerConfigSecret(project, robot, ns)
		oldSecret := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Name: secret.Name, Namespace: ns}, oldSecret)
		if err == nil {
			// Secret exists → update it
			oldSecret.Data = secret.Data
			oldSecret.Labels = secret.Labels
			if err := r.Update(ctx, oldSecret); err != nil {
				logger.Error(err, "failed to update secret in namespace", "namespace", ns)
				return ctrl.Result{}, fmt.Errorf("failed to update secret in namespace %s: %w", ns, err)
			}
		} else if client.IgnoreNotFound(err) == nil {
			// Secret doesn't exist → create it
			if err := r.Create(ctx, secret); err != nil {
				logger.Error(err, "failed to create secret in namespace", "namespace", ns)
				return ctrl.Result{}, fmt.Errorf("failed to create secret in namespace %s: %w", ns, err)
			}
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to get secret in namespace %s: %w", ns, err)
		}
	}

	// 3. Refresh the robot secret in Harbor via PATCH.
	//    Harbor rotates immediately with no grace period. Pods that reloaded
	//    the K8s Secret in step 2 will find the new credential valid here.
	//    Pods still using the old cached credential will fail auth until they
	//    reload.
	if err := r.HarborClient.RefreshRobotSecret(ctx, robotID, newSecret); err != nil {
		logger.Error(err, "failed to refresh robot secret", "robotID", robotID)
		return ctrl.Result{}, fmt.Errorf("failed to refresh robot secret: %w", err)
	}

	// 4. Clean up rotation state before removing the annotation so that if
	//    cleanup fails the annotation persists and reconciliation retries.
	if err := r.clearRotationState(ctx, project.Name); err != nil {
		logger.Error(err, "failed to clear rotation state")
		return ctrl.Result{}, fmt.Errorf("failed to clear rotation state: %w", err)
	}

	delete(project.Annotations, refreshAnnotation)
	if err := r.Update(ctx, project); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove refresh annotation: %w", err)
	}

	logger.Info("Robot token refreshed successfully",
		"project", project.Status.HarborProjectName,
		"robotID", robotID)

	return ctrl.Result{}, nil
}

// generateSecret generates a cryptographically random 32-byte hex-encoded secret.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// rotationStateSecretName returns the name of the Secret used to persist
// pending rotation state for the given HarborProject.
func rotationStateSecretName(projectName string) string {
	return "harbor-rotation-" + projectName
}

// getRotationState reads the pending rotation secret from a K8s Secret.
// Returns empty string if no state exists.
func (r *HarborProjectReconciler) getRotationState(ctx context.Context, projectName string) (string, error) {
	if r.RotationStateNamespace == "" {
		return "", nil
	}
	name := rotationStateSecretName(projectName)
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: r.RotationStateNamespace}, &secret); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return string(secret.Data["secret"]), nil
}

// setRotationState persists the pending rotation secret in a K8s Secret.
func (r *HarborProjectReconciler) setRotationState(ctx context.Context, projectName, secret string) error {
	if r.RotationStateNamespace == "" {
		return nil
	}
	name := rotationStateSecretName(projectName)
	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.RotationStateNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "harbor-controller",
				projectLabel:                   projectName,
			},
		},
		Data: map[string][]byte{
			"secret": []byte(secret),
		},
	}
	// Try create first; if it already exists, update in-place
	if err := r.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			var existing corev1.Secret
			if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: r.RotationStateNamespace}, &existing); err != nil {
				return err
			}
			existing.Data = obj.Data
			existing.Labels = obj.Labels
			return r.Update(ctx, &existing)
		}
		return err
	}
	return nil
}

// clearRotationState removes the pending rotation state Secret.
func (r *HarborProjectReconciler) clearRotationState(ctx context.Context, projectName string) error {
	if r.RotationStateNamespace == "" {
		return nil
	}
	name := rotationStateSecretName(projectName)
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: r.RotationStateNamespace}, &secret); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return r.Delete(ctx, &secret)
}

// --- helpers ---

func secretNameForProject(projectName string) string {
	return "harbor-registry-cred-" + projectName
}

func toAccess(permissions []v1.RobotPermission) []harbor.RobotAccess {
	access := make([]harbor.RobotAccess, 0, len(permissions))
	for _, p := range permissions {
		access = append(access, harbor.RobotAccess{
			Action:   p.Action,
			Resource: "repository",
		})
	}
	return access
}

func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) {
	if conditions == nil {
		return
	}
	found := false
	for i, c := range *conditions {
		if c.Type == condType {
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			(*conditions)[i].LastTransitionTime = metav1.Now()
			found = true
			break
		}
	}
	if !found {
		*conditions = append(*conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			LastTransitionTime: metav1.Now(),
			Reason:             reason,
			Message:            message,
		})
	}
}

// computeSpecHash returns a deterministic hash of the fields that require full
// reconciliation (namespaceRefs, robotPermissions). When this hash matches the
// stored value in project.Status.LastSpecHash, only metadata fields
// (public/autoScan/storageLimit) have changed and the fast path can be taken.
func computeSpecHash(projectName string, namespaceRefs []string, robotPermissions []v1.RobotPermission) string {
	h := fnv.New64a()
	buf := make([]byte, 4)
	// Write projectName with length prefix so that the empty string and
	// missing are distinct from any other input.
	binary.LittleEndian.PutUint32(buf, uint32(len(projectName)))
	h.Write(buf)
	h.Write([]byte(projectName))
	// Write namespaceRefs with length prefix, each item also length-prefixed.
	sorted := append([]string{}, namespaceRefs...)
	sort.Strings(sorted)
	binary.LittleEndian.PutUint32(buf, uint32(len(sorted)))
	h.Write(buf)
	for _, ns := range sorted {
		binary.LittleEndian.PutUint32(buf, uint32(len(ns)))
		h.Write(buf)
		h.Write([]byte(ns))
	}
	// Write robotPermissions with length prefix, each action also length-prefixed.
	perms := append([]v1.RobotPermission{}, robotPermissions...)
	sort.Slice(perms, func(i, j int) bool { return perms[i].Action < perms[j].Action })
	binary.LittleEndian.PutUint32(buf, uint32(len(perms)))
	h.Write(buf)
	for _, p := range perms {
		binary.LittleEndian.PutUint32(buf, uint32(len(p.Action)))
		h.Write(buf)
		h.Write([]byte(p.Action))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// shortID generates a short random ID for robot account naming
func shortID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}
