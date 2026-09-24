# Harbor Registry Integration Design

> Version: v1.0
> Date: 2026-09-23
> Changes: Initial version

---

## 1. Background & Goals

### 1.1 Background

Sealos Cloud currently lacks an in-cluster container image registry. Users cannot upload, manage, or distribute container images within the platform. An enterprise-grade registry is needed to complete the platform capabilities.

### 1.2 Goals

- Deploy Harbor as the container image registry inside the Sealos cluster
- Manage Harbor Project lifecycle via a custom CRD
- Users push/pull images via Docker CLI / containerd / podman without accessing the Harbor Web UI
- Image storage usage is integrated into Sealos' existing metering and billing system

### 1.3 Design Principles

- **Minimal Surface Area**: Harbor Portal is not exposed to end users; they interact only through standard OCI CLI tools
- **Standalone Controller**: A new independent Harbor Controller, no modifications to the existing account controller / NamespaceReconciler
- **CRD-Driven**: Declarative lifecycle management of Harbor Projects via the `HarborProject` custom resource
- **Incremental Expansion**: Phase 1 focuses on create and delete; subsequent phases add features like debt suspension

---

## 2. Architecture

```
+-----------------------------------------------------------------------+
|                           Sealos Cloud                                |
|                                                                       |
|  +---------------------------------------------------------------+   |
|  |                     Kubernetes Cluster                        |   |
|  |                                                               |   |
|  |  +--------------------------+    +--------------------------+  |   |
|  |  |   Harbor Controller       |    |   Harbor (db_auth)      |  |   |
|  |  |   (standalone)            |--->|                          |  |   |
|  |  |                           |    |  +- Core                |  |   |
|  |  |   Watches HarborProject   |    |  +- Registry (OCI)      |  |   |
|  |  |   CR changes              |    |  +- JobService          |  |   |
|  |  |                           |    |  +- Exporter            |  |   |
|  |  +------------+--------------+    +------------+-------------+  |   |
|  |               |                               |                 |   |
|  |               | Create/Delete                 | S3 protocol     |   |
|  |               v                               v                 |   |
|  |  +-------------------------+    +--------------------------+    |   |
|  |  |   User Namespace        |    |   MinIO / S3             |    |   |
|  |  |   +-----------------+   |    |   (image layer storage)  |    |   |
|  |  |   | Secret:         |   |    +--------------------------+    |   |
|  |  |   | harbor-token    |   |                                     |   |
|  |  |   +-----------------+   |                                     |   |
|  |  |   +-----------------+   |                                     |   |
|  |  |   | HarborProject   |   |                                     |   |
|  |  |   | CR              |   |                                     |   |
|  |  |   +-----------------+   |                                     |   |
|  |  +-------------------------+                                     |   |
|  |                                                               |   |
|  +---------------------------------------------------------------+   |
|                                                                       |
|  +---------------------------------------------------------------+   |
|  |   Service/Account (Metering & Billing)                        |   |
|  |   +- Harbor Usage Collector (periodic Harbor Quota API call)  |   |
|  |   +- Registry-Storage billing type extension                  |   |
|  +---------------------------------------------------------------+   |
|                                                                       |
|  User Interaction:                                                    |
|  +---------------------------------------------------------------+   |
|  |  # Admin creates HarborProject -> controller does the rest    |   |
|  |  kubectl apply -f registry-project.yaml                       |   |
|  |                                                                |   |
|  |  # User retrieves token                                       |   |
|  |  kubectl get secret harbor-registry-cred -n ns-xxx \          |   |
|  |    -o jsonpath="{.data.\.dockerconfigjson}" | base64 -d        |   |
|  |                                                                |   |
|  |  # User pushes/pulls images                                   |   |
|  |  docker login harbor.sealos.example.com -u robot$ns-xxx -p .. |   |
|  |  docker push harbor.sealos.example.com/ns-xxx/my-app:v1       |   |
|  |  docker pull harbor.sealos.example.com/ns-xxx/my-app:v1       |   |
|  +---------------------------------------------------------------+   |
+-----------------------------------------------------------------------+
```

### 2.1 Core Flow

```
+-------------+     +-------------+     +---------------+
| Admin/User  |     |   Harbor    |     |   Target      |
| Creates CR  |     | Controller  |     |   Namespace   |
+------+------+     +------+------+     +------+--------+
       |                   |                   |
       | 1. Apply          |                   |
       | HarborProject     |                   |
       |------------------>|                   |
       |                   |                   |
       |                   | 2. Reconcile      |
       |                   |------------------>|
       |                   |   - Add finalizer |
       |                   |   - Create Project|
       |                   |   - Create Robot  |
       |                   |   - Create Secret |
       |                   |<------------------|
       |                   |                   |
       | 3. Status updated |                   |
       |<------------------|                   |
       |                   |                   |
       | 4. kubectl delete |                   |
       | HarborProject     |                   |
       |------------------>|                   |
       |                   | 5. Reconcile      |
       |                   |   Delete          |
       |                   |------------------>|
       |                   |   - Delete Project|
       |                   |     (cascades all |
       |                   |      images+robots)|
       |                   |   - Delete Secret |
       |                   |   - Remove        |
       |                   |     Finalizer     |
       |                   |<------------------|
       | 6. CR deleted     |                   |
       |<------------------|                   |
       +                   +                   +
```

---

## 3. Custom CRD Design

### 3.1 HarborProject CRD

The core CRD for the Harbor Controller, defining the desired state of a Harbor project.

```yaml
# harbor.sealos.io/v1
# Kind: HarborProject
# Scope: Cluster (created in the user's Namespace)

apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: harborprojects.harbor.sealos.io
spec:
  group: harbor.sealos.io
  names:
    kind: HarborProject
    plural: harborprojects
    singular: harborproject
    shortNames:
    - hp
    - hproj
  scope: Cluster
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              projectName:
                type: string
                description: "Project name in Harbor. Auto-generated as 'hp-{name}' if empty."
              displayName:
                type: string
                description: "Project display name"
              namespaceRefs:
                type: array
                description: "Target Kubernetes namespace names where docker credentials will be distributed"
                items:
                  type: string
              storageLimit:
                type: integer
                format: int64
                default: -1
                description: "Storage quota in bytes. -1 means unlimited."
              autoScan:
                type: boolean
                default: false
                description: "Enable automatic vulnerability scanning"
              public:
                type: boolean
                default: false
                description: "Make project publicly pullable without authentication"
              owner:
                type: string
                description: "Final owner for metering and billing. Typically a user ID, tenant ID, or namespace UID"
              robotPermissions:
                type: array
                default:
                - action: push
                - action: pull
                items:
                  type: object
                  properties:
                    action:
                      type: string
                      enum: ["push", "pull", "scanner-pull"]
                      description: "Robot account permission"
          status:
            type: object
            properties:
              phase:
                type: string
                enum: ["Pending", "Creating", "Ready", "Deleting", "Failed"]
                description: "Current lifecycle phase"
              owner:
                type: string
                description: "Final owner, propagated from spec"
              harborProjectID:
                type: integer
                description: "Numeric project ID in Harbor"
              harborProjectName:
                type: string
                description: "Project name in Harbor"
              robotName:
                type: string
                description: "Robot account name"
              secretName:
                type: string
                description: "K8s Secret name containing dockerconfigjson"
              conditions:
                type: array
                items:
                  type: object
                  properties:
                    type:
                      type: string
                    status:
                      type: string
                      enum: ["True", "False", "Unknown"]
                    reason:
                      type: string
                    message:
                      type: string
                    lastTransitionTime:
                      type: string
                      format: date-time
    subresources:
      status: {}
    additionalPrinterColumns:
    - name: Phase
      type: string
      jsonPath: ".status.phase"
    - name: ProjectID
      type: integer
      jsonPath: ".status.harborProjectID"
    - name: Age
      type: date
      jsonPath: ".metadata.creationTimestamp"
```

### 3.2 Usage Example

An admin or user creates the following CR to request a Harbor Project:

```yaml
apiVersion: harbor.sealos.io/v1
kind: HarborProject
metadata:
  name: my-project
spec:
  owner: "user-abc123"  # Metering/billing owner
  displayName: "My Image Registry"
  namespaceRefs:
    - ns-ff839a27
  storageLimit: 10737418240  # 10GB
  autoScan: true
  robotPermissions:
  - action: push
  - action: pull
```

After applying, the Controller automatically:

1. Creates Project `ns-ff839a27` in Harbor
2. Creates Robot Account `robot$ns-ff839a27+my-project`
3. Creates Secret `harbor-registry-cred-my-project` in Namespace `ns-ff839a27`
4. Updates CR Status to `Ready`

Check status:

```bash
kubectl get harborproject
# NAME         PHASE   PROJECTID   AGE
# my-project   Ready   1           2m

kubectl get harborproject my-project -o yaml
# status:
#   phase: Ready
#   harborProjectID: 1
#   harborProjectName: ns-ff839a27
#   robotName: robot$ns-ff839a27+my-project

#   conditions:
#   - type: ProjectCreated
#     status: "True"
#   - type: RobotCreated
#     status: "True"
#   - type: SecretCreated
#     status: "True"
```

Delete the CR to reclaim all Harbor resources:

```bash
kubectl delete harborproject my-project
# → cascading delete of Harbor Project + all Secrets in target namespaces
```

### 3.3 Finalizer Design

To prevent orphaned Harbor Projects when the K8s CR is deleted, a Finalizer is used:

```yaml
# Harbor Controller adds the finalizer automatically during reconcile
metadata:
  finalizers:
  - harbor.sealos.io/cleanup

# Deletion flow:
# 1. kubectl delete → deletionTimestamp is set
# 2. Controller detects deletionTimestamp → runs cleanup
#    2a. Deletes Harbor Project (cascading deletes all images, Robot Accounts)
#    2b. Deletes K8s Secret
# 3. Removes Finalizer
# 4. CR is actually deleted
```

---

## 4. Harbor Controller Design

### 4.1 Controller Architecture

```
+------------------------------------------------------+
|                  Harbor Controller                    |
|                                                      |
|  +----------------------------------------------+   |
|  |  Reconciler (controller-runtime)             |   |
|  |                                              |   |
|  |  watches: HarborProject (OWN)               |   |
|  |  watches: Secret (OWN + delete event)        |   |
|  |                                              |   |
|  |  Reconcile Logic:                            |   |
|  |  +--------------------------------------+   |   |
|  |  |  1. Read HarborProject CR            |   |   |
|  |  |  2. Handle finalizer                  |   |   |
|  |  |  3. Check Harbor-side state           |   |   |
|  |  |  4. Reconcile to desired state        |   |   |
|  |  |  5. Update Status                     |   |   |
|  |  +--------------------------------------+   |   |
|  +----------------------------------------------+   |
|                                                      |
|  +----------------------------------------------+   |
|  |  Harbor Admin Client (internal/harbor)       |   |
|  |  - CreateProject                             |   |
|  |  - DeleteProject                             |   |
|  |  - CreateRobot                               |   |
|  |  - GetProjectByName                          |   |
|  +----------------------------------------------+   |
|                                                      |
|  +----------------------------------------------+   |
|  |  K8s Client                                   |   |
|  |  - Create/Get/Update/Delete Secret           |   |
|  |  - Update HarborProject Status              |   |
|  +----------------------------------------------+   |
+------------------------------------------------------+
```

### 4.2 Project Structure

The project is a standalone Go module, not coupled with Sealos controllers:

```
labring-sigs-harbor/
+-- main.go                    # Entry point
+-- api/
|   +-- v1/
|       +-- harborproject_types.go   # CRD type definitions
|       +-- groupversion_info.go     # GV registration
+-- controllers/
|   +-- harborproject_controller.go  # Reconciler main logic
+-- internal/
|   +-- harbor/
|       +-- client.go                # Harbor Admin Client
|       +-- types.go                 # Harbor API types
|       +-- errors.go                # Custom errors
+-- deploy/
|   +-- crds/
|   |   +-- harbor.sealos.io_harborprojects.yaml  # CRD definition
|   +-- rbac.yaml
|   +-- deployment.yaml
+-- Dockerfile
+-- Makefile
+-- go.mod
+-- go.sum
```

### 4.3 CRD Type Definition (Go)

```go
package v1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HarborProjectSpec defines the desired state of HarborProject
type HarborProjectSpec struct {
    // Owner is the final owner for metering and billing. Typically a user ID, tenant ID, or namespace UID.
    // +optional
    Owner string `json:"owner,omitempty"`

    // ProjectName is the name of the project in Harbor. Auto-generated if empty.
    // +optional
    ProjectName string `json:"projectName,omitempty"`

    // DisplayName is the human-readable display name
    // +optional
    DisplayName string `json:"displayName,omitempty"`

    // NamespaceRefs specifies the target K8s namespaces where Robot Secrets will be distributed
    // +optional
    NamespaceRefs []string `json:"namespaceRefs,omitempty"`

    // StorageLimit is the storage quota in bytes. -1 means unlimited.
    // +kubebuilder:default:=-1
    StorageLimit int64 `json:"storageLimit,omitempty"`

    // AutoScan enables automatic vulnerability scanning on push
    // +kubebuilder:default:=false
    AutoScan bool `json:"autoScan,omitempty"`

    // Public makes the project publicly pullable without authentication
    // +kubebuilder:default:=false
    Public bool `json:"public,omitempty"`

    // RobotPermissions defines the list of robot account permissions
    // +kubebuilder:default:={{action: "push"},{action: "pull"}}
    RobotPermissions []RobotPermission `json:"robotPermissions,omitempty"`
}

type RobotPermission struct {
    Action string `json:"action"`
}

// HarborProjectStatus defines the observed state of HarborProject
type HarborProjectStatus struct {
    Phase              HarborProjectPhase `json:"phase,omitempty"`
    HarborProjectID    int64              `json:"harborProjectID,omitempty"`
    HarborProjectName  string             `json:"harborProjectName,omitempty"`
    RobotName          string             `json:"robotName,omitempty"`
    Owner              string             `json:"owner,omitempty"`
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type HarborProjectPhase string

const (
    HarborPhasePending  HarborProjectPhase = "Pending"
    HarborPhaseCreating HarborProjectPhase = "Creating"
    HarborPhaseReady    HarborProjectPhase = "Ready"
    HarborPhaseDeleting HarborProjectPhase = "Deleting"
    HarborPhaseFailed   HarborProjectPhase = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName={hp,hproj}
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="ProjectID",type="integer",JSONPath=".status.harborProjectID"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".status.owner"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type HarborProject struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   HarborProjectSpec   `json:"spec,omitempty"`
    Status HarborProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HarborProjectList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []HarborProject `json:"items"`
}
```

### 4.4 Reconciler Core Logic

```go
package controllers

type HarborProjectReconciler struct {
    client.Client
    Scheme        *runtime.Scheme
    HarborClient  *harbor.Client
    RegistryHost  string  // harbor.sealos.example.com
}

const harborFinalizer = "harbor.sealos.io/cleanup"

func (r *HarborProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // 1. Get CR
    project := &v1.HarborProject{}
    if err := r.Get(ctx, req.NamespacedName, project); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 2. Handle deletion
    if !project.DeletionTimestamp.IsZero() {
        return r.reconcileDelete(ctx, project)
    }

    // 3. Handle create/update
    return r.reconcileCreate(ctx, project)
}

func (r *HarborProjectReconciler) reconcileCreate(ctx context.Context, project *v1.HarborProject) (ctrl.Result, error) {
    // Ensure finalizer is added
    if !controllerutil.ContainsFinalizer(project, harborFinalizer) {
        controllerutil.AddFinalizer(project, harborFinalizer)
        if err := r.Update(ctx, project); err != nil {
            return ctrl.Result{}, err
        }
        return ctrl.Result{Requeue: true}, nil
    }

    // Already Ready, skip
    if project.Status.Phase == v1.HarborPhaseReady {
        return ctrl.Result{}, nil
    }

    project.Status.Phase = v1.HarborPhaseCreating
    _ = r.Status().Update(ctx, project)

    // Get or generate project name
    projectName := project.Spec.ProjectName
    if projectName == "" {
        projectName = "hp-" + project.Name
    }

    // Create Harbor Project
    hbProject, err := r.HarborClient.GetProjectByName(ctx, projectName)
    if err != nil {
        return ctrl.Result{}, err
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
            return ctrl.Result{}, err
        }
        project.Status.HarborProjectID = projectID
        project.Status.HarborProjectName = projectName
    } else {
        project.Status.HarborProjectID = hbProject.ProjectID
        project.Status.HarborProjectName = hbProject.Name
    }

    // Create Robot Account (push + pull permissions)
    robot, err := r.HarborClient.CreateRobot(ctx, project.Status.HarborProjectID, harbor.RobotSpec{
        Name:     "robot-" + shortID(project.Name),
        Duration: -1, // never expire
        Permissions: []harbor.RobotPermission{{
            Kind:      "project",
            Namespace: projectName,
            Access:    toAccess(project.Spec.RobotPermissions),
        }},
    })
    if err != nil {
        project.Status.Phase = v1.HarborPhaseFailed
        _ = r.Status().Update(ctx, project)
        return ctrl.Result{}, err
    }
    project.Status.RobotName = robot.Name

    // Distribute K8s Secrets to all target namespaces
    for _, ns := range project.Spec.NamespaceRefs {
        secret := r.buildDockerConfigSecret(project, robot, ns)
        if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
            project.Status.Phase = v1.HarborPhaseFailed
            _ = r.Status().Update(ctx, project)
            return ctrl.Result{}, err
        }
    }

    // Update status to Ready
    project.Status.Phase = v1.HarborPhaseReady
    setCondition(&project.Status.Conditions, "ProjectCreated", metav1.ConditionTrue, "Success", "Harbor project created")
    setCondition(&project.Status.Conditions, "RobotCreated", metav1.ConditionTrue, "Success", "Robot account created")
    setCondition(&project.Status.Conditions, "SecretCreated", metav1.ConditionTrue, "Success", "K8s secret created")
    return ctrl.Result{}, r.Status().Update(ctx, project)
}

func (r *HarborProjectReconciler) reconcileDelete(ctx context.Context, project *v1.HarborProject) (ctrl.Result, error) {
    // Delete Harbor Project (cascading deletes all images + Robot Accounts)
    if project.Status.HarborProjectID > 0 {
        if err := r.HarborClient.DeleteProject(ctx, project.Status.HarborProjectID); err != nil {
            return ctrl.Result{}, err
        }
    }

    // Delete K8s Secrets from all target namespaces
    for _, ns := range project.Spec.NamespaceRefs {
        secret := &corev1.Secret{
            ObjectMeta: metav1.ObjectMeta{
                Name:      secretNameForProject(project.Name),
                Namespace: ns,
            },
        }
        if err := r.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
            return ctrl.Result{}, err
        }
    }

    // Remove Finalizer
    controllerutil.RemoveFinalizer(project, harborFinalizer)
    return ctrl.Result{}, r.Update(ctx, project)
}
```

### 4.5 Harbor Admin Client

```go
package harbor

type Client struct {
    baseURL    string
    username   string
    password   string
    httpClient *http.Client
}

func NewClient(baseURL, username, password string) *Client {
    return &Client{
        baseURL:    strings.TrimRight(baseURL, "/"),
        username:   username,
        password:   password,
        httpClient: &http.Client{Timeout: 30 * time.Second},
    }
}

// --- Project API ---

func (c *Client) CreateProject(ctx context.Context, spec ProjectSpec) (int64, error) {
    body := map[string]interface{}{
        "project_name": spec.Name,
        "metadata": map[string]interface{}{
            "public":       strconv.FormatBool(spec.Public),
            "auto_scan":    strconv.FormatBool(spec.AutoScan),
            "storage_limit": strconv.FormatInt(spec.StorageLimit, 10),
        },
    }
    resp, err := c.post(ctx, "/api/v2.0/projects", body)
    if err != nil {
        return 0, err
    }
    defer resp.Body.Close()
    // Extract project ID from Location header
    location := resp.Header.Get("Location")
    // /api/v2.0/projects/1
    id, _ := strconv.ParseInt(filepath.Base(location), 10, 64)
    return id, nil
}

func (c *Client) GetProjectByName(ctx context.Context, name string) (*Project, error) {
    resp, err := c.get(ctx, fmt.Sprintf("/api/v2.0/projects?name=%s", url.QueryEscape(name)))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var projects []Project
    if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
        return nil, err
    }
    if len(projects) == 0 {
        return nil, nil
    }
    return &projects[0], nil
}

func (c *Client) DeleteProject(ctx context.Context, projectID int64) error {
    resp, err := c.delete(ctx, fmt.Sprintf("/api/v2.0/projects/%d", projectID))
    if err != nil {
        return err
    }
    return resp.Body.Close()
}

// --- Robot Account API ---

func (c *Client) CreateRobot(ctx context.Context, projectID int64, spec RobotSpec) (*RobotCredential, error) {
    body := map[string]interface{}{
        "name":        spec.Name,
        "duration":    spec.Duration,
        "permissions": spec.Permissions,
    }
    resp, err := c.post(ctx, fmt.Sprintf("/api/v2.0/projects/%d/robots", projectID), body)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var cred RobotCredential
    if err := json.NewDecoder(resp.Body).Decode(&cred); err != nil {
        return nil, err
    }
    return &cred, nil
}

// --- HTTP Helpers ---

func (c *Client) post(ctx context.Context, path string, body interface{}) (*http.Response, error) {
    // Uses Basic Auth, Content-Type: application/json
    // Returns *http.Response
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
    // Uses Basic Auth
    // Returns *http.Response
}

func (c *Client) delete(ctx context.Context, path string) (*http.Response, error) {
    // Uses Basic Auth
    // Returns *http.Response
}
```

### 4.6 Secret Construction

```go
func (r *HarborProjectReconciler) buildDockerConfigSecret(project *v1.HarborProject, robot *harbor.RobotCredential, namespace string) *corev1.Secret {
    // Build dockerconfigjson
    auth := base64.StdEncoding.EncodeToString([]byte(robot.Name + ":" + robot.Token))
    dockerConfig := map[string]interface{}{
        "auths": map[string]interface{}{
            r.RegistryHost: map[string]string{
                "username": robot.Name,
                "password": robot.Token,
                "auth":     auth,
            },
        },
    }
    data, _ := json.Marshal(dockerConfig)

    return &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "harbor-registry-cred-" + project.Name,
            Namespace: project.Namespace,
            OwnerReferences: []metav1.OwnerReference{
                *metav1.NewControllerRef(project, v1.GroupVersion.WithKind("HarborProject")),
            },
            Labels: map[string]string{
                "app.kubernetes.io/managed-by": "harbor-controller",
                "app.kubernetes.io/name":       "harbor-registry-cred",
            },
        },
        Type: corev1.SecretTypeDockerConfigJson,
        Data: map[string][]byte{
            corev1.DockerConfigJsonKey: data,
        },
    }
}
```

### 4.7 RBAC

```yaml
# Harbor Controller ServiceAccount
apiVersion: v1
kind: ServiceAccount
metadata:
  name: harbor-controller
  namespace: harbor-system
---
# Permissions required by the controller
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: harbor-controller
rules:
- apiGroups: ["harbor.sealos.io"]
  resources: ["harborprojects", "harborprojects/status"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: harbor-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: harbor-controller
subjects:
- kind: ServiceAccount
  name: harbor-controller
  namespace: harbor-system
```


### 4.8 Project Auto-Provision (Optional Controller)

`ProjectAutoProvision` is an **optional** controller that watches Namespace create/update events and automatically creates corresponding `HarborProject` CRs for Namespaces with a designated owner label, providing an "Namespace creation comes with image repository" automated experience.

#### Enable

Pass the `--enable-project-auto-provision` flag when starting the controller:

```bash
manager --enable-project-auto-provision --owner-label-key="user.sealos.io/owner"
```

#### How It Works

```
┌─────────────────┐     ┌──────────────────────────────┐     ┌──────────────────────┐
│  Namespace       │     │  ProjectAutoProvision         │     │  HarborProject        │
│  Changes         │     │  Reconciler                    │     │  Reconciler           │
│                  │     │                              │     │                      │
│  Create/Update   │────▶│  1. Read Namespace            │────▶│  (Standard Flow)      │
│  Label Change    │     │  2. Check owner label         │     │  Create Project       │
│                  │     │  3. Create/Update HP CR       │     │  Create Robot         │
│                  │     │  4. Label mgmt (record NS UID)│     │  Distribute Secret    │
└─────────────────┘     └──────────────────────────────┘     └──────────────────────┘
```

1. User creates a Namespace with an owner label (e.g. `user.sealos.io/owner: user-abc`)
2. `ProjectAutoProvision` detects the label change and generates a corresponding `HarborProject` CR (name format `hp-{namespace}`)
3. `HarborProjectReconciler` takes over and executes the standard Project/Robot/Secret creation flow

#### Label Conventions

Auto-created `HarborProject` CRs carry the following labels to distinguish them from manually created resources:

| Label | Description |
|-------|-------------|
| `harbor.sealos.io/auto-provision` | Marks as auto-provisioned, value `"true"` |
| `harbor.sealos.io/source-namespace` | Source Namespace name |
| `harbor.sealos.io/source-namespace-uid` | Source Namespace UID, used for rebuild detection |

#### Namespace Rebuild Detection

If a Namespace is deleted and recreated (same name but different UID), the controller detects the UID change via the `source-namespace-uid` label and re-matches and updates the `HarborProject` spec to ensure the new Namespace is correctly adopted.

#### Default Configuration

Auto-created `HarborProject` uses the following defaults:

- **ProjectName**: Namespace name (short name, not `hp-` prefix)
- **StorageLimit**: 5 GB
- **Public**: false
- **AutoScan**: false
- **RobotPermissions**: push + pull
- **NamespaceRefs**: only the source Namespace itself

#### Relationship with Manual Creation

- If a `HarborProject` has already been created manually, `ProjectAutoProvision` will not create a duplicate when it finds an existing CR with the same name. Instead, it will **adopt** the CR: update its spec to match the current Namespace label configuration and supplement the auto-provision labels.
- Deleting an auto-created `HarborProject` CR will **not** trigger automatic recreation (unless the Namespace change event is re-triggered after manual deletion).
- Removing the owner label from a Namespace will **not** trigger deletion of existing CRs — this is intentional to avoid accidental deletion.

---

## 5. Harbor Deployment


### 5.1 Authentication Mode

Uses `db_auth` mode (local database authentication), no OIDC Provider needed.

```yaml
# values.yaml key configuration
expose:
  type: ingress
  tls:
    enabled: true
  ingress:
    hosts:
      core: harbor.sealos.example.com

externalURL: https://harbor.sealos.example.com

auth:
  mode: db_auth

harborAdminPassword: "managed-by-sealos-secret"

persistence:
  imageChartStorage:
    type: s3
    s3:
      region: us-east-1
      bucket: harbor-registry
      accesskey: "{{ .Values.minio.accessKey }}"
      secretkey: "{{ .Values.minio.secretKey }}"
      rootdirectory: /registry
      regionendpoint: https://s3.sealos.io

database:
  type: external
  external:
    host: postgresql-harbor.harbor.svc
    port: 5432
    username: harbor
    password: "{{ .Values.postgres.password }}"
    coreDatabase: harbor
    sslmode: disable

redis:
  type: external
  external:
    addr: redis-harbor.harbor.svc:6379

metrics:
  enabled: true
  exporter:
    path: /metrics
    port: 9103

portal:
  replicas: 1  # Can be reduced, only used for admin debugging
```

### 5.2 Storage Backend

- Recommended: **S3/MinIO** as the Registry storage backend
- Can reuse Sealos' existing MinIO cluster
- Supports horizontal scaling, avoiding PVC performance and capacity bottlenecks

---

## 6. Secret & Authentication Management

### 6.1 Auto-created K8s Secret

After creating the Robot Account, the Harbor Controller automatically creates the following Secret in the target Namespace:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: harbor-registry-cred-my-project
  namespace: ns-ff839a27  # Controller creates one in each namespaceRefs entry
  labels:
    app.kubernetes.io/managed-by: harbor-controller
    app.kubernetes.io/name: harbor-registry-cred
    harbor.sealos.io/project: my-project
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: |
    {
      "auths": {
        "harbor.sealos.example.com": {
          "username": "robot$ns-ff839a27+my-project",
          "password": "{token}",
          "auth": "{base64}"
        }
      }
    }
```

The Controller creates one Secret in each Namespace listed in `namespaceRefs`. On CR deletion, the Controller iterates all target namespaces to delete Secrets, then cleans up Harbor resources, and finally removes the Finalizer.

### 6.2 User Usage

Method 1: Direct Docker CLI

```bash
# Retrieve token from Secret
kubectl get secret harbor-registry-cred-my-project -n ns-xxx \
  -o jsonpath="{.data.\.dockerconfigjson}" | base64 -d

# Login
docker login harbor.sealos.example.com \
  -u robot$ns-xxx+my-project \
  -p <token>

# Push
docker tag my-app:v1 harbor.sealos.example.com/ns-xxx/my-app:v1
docker push harbor.sealos.example.com/ns-xxx/my-app:v1

# Pull
docker pull harbor.sealos.example.com/ns-xxx/my-app:v1
```

Method 2: Use in a Pod

```yaml
apiVersion: v1
kind: Pod
spec:
  imagePullSecrets:
  - name: harbor-registry-cred-my-project
  containers:
  - name: my-app
    image: harbor.sealos.example.com/ns-xxx/my-app:v1
```

### 6.3 Token Security

| Measure | Description |
|---------|-------------|
| **Scoped Access** | Robot Account can only access its own Project |
| **Permanent Token** | Never expires; refreshable on demand via `harbor.sealos.io/refresh-token` annotation |
| **Secret Storage** | Stored as dockerconfigjson Secret in each target namespace |
| **Refreshable** | Annotate `HarborProject` with `harbor.sealos.io/refresh-token: "true"` → controller creates new robot, updates secrets, deletes old robot (zero-image-downtime) |
| **Audit** | Harbor audit log records all operations |



### 6.4 Token Refresh Mechanism

Harbor's API does not provide a dedicated token-rotation endpoint. The controller implements a
**create-new → update-secrets → delete-old** flow that rotates robot tokens without affecting
existing images or requiring project deletion.

**Trigger**: Add the annotation `harbor.sealos.io/refresh-token` to a `HarborProject` CR that is in `Ready` phase.

```
kubectl annotate harborproject my-project harbor.sealos.io/refresh-token=true
```

**Execution Flow**:

```
+------------------+     +------------------+     +------------------+
|  1. Create New   | --> |  2. Update K8s   | --> |  3. Delete Old   |
|  Robot Account   |     |  Secrets (all NS) |     |  Robot Account   |
+------------------+     +------------------+     +------------------+
       |                        |                        |
       | Returns new token      | All namespaces         | Delete by ID
       | + ID                   | updated atomically     | (ID stored in
       |                        |                        | .status.robotID)
       v                        v                        v
   New robot has           Old credentials              Old robot
   push/pull perms         replaced with new one         removed from Harbor
```

**Implementation details**:

1. Controller detects the annotation on a `Ready` CR during reconciliation
2. Calls `CreateRobot` to create a new robot account with a different name (`{name}-refresh`)
3. Iterates all namespaces in `namespaceRefs` and updates the dockerconfigjson Secret with the new credentials (create if missing)
4. Calls `DeleteProjectRobot` with the stored `status.robotID` to remove the old robot from Harbor
5. Updates `status.robotName` and `status.robotID` with the new robot's values
6. Removes the `harbor.sealos.io/refresh-token` annotation

**Key Properties**:

- No image downtime: images remain accessible throughout the process
- The old robot is only deleted after all secrets are updated, minimizing the exposure window
- The `status.robotID` field persists the robot's numeric ID, enabling precise deletion

---

## 7. Metering & Billing

### 7.1 Data Collection

A Harbor usage collector periodically calls the Harbor API to fetch storage usage for each Project:

```go
type HarborUsageCollector struct {
    interval time.Duration // default 1 hour
}

func (c *HarborUsageCollector) Collect(ctx context.Context) ([]HarborUsage, error) {
    // 1. List all Projects
    projects, err := c.listProjects(ctx)
    if err != nil {
        return nil, err
    }

    // 2. Query each Project's Quota
    var usages []HarborUsage
    for _, p := range projects {
        quota, err := c.getProjectQuota(ctx, p.ProjectID)
        if err != nil {
            continue
        }
        usages = append(usages, HarborUsage{
            Namespace:   projectToNamespace(p.Name),
            StorageUsed: quota.Used.Storage,
            Time:        time.Now(),
        })
    }
    return usages, nil
}
```

### 7.2 Pricing & Deduction

Add a new Registry billing type in `controllers/pkg/resources/resources.go`:

```go
// | property          | Price | Detail          |
// | ----------------- | ----- | --------------- |
// | Registry-Storage  | 2     | Mebibytes unit  | ← New

// Pricing unit: 2 units/MiB (consistent with existing Disk pricing)
// i.e., 1GB = 1024 MiB * 2 = 2048 units/hour
```

The usage collector reads `spec.owner` or `status.owner` from each CR to determine billing attribution. Billing flow reuses the existing `Monitor → ActiveBilling → Billing` pipeline.

### 7.3 Prometheus Monitoring

Harbor Exporter metrics can be directly integrated with Sealos' existing Prometheus:

```prometheus
# Harbor storage usage
harbor_project_quota_used{project="ns-xxx", type="storage"}
harbor_project_quota_total{project="ns-xxx", type="storage"}

# Harbor Controller custom metrics
harbor_controller_harborproject_total{phase="Ready"}
harbor_controller_reconcile_duration_seconds
harbor_controller_operation_total{operation="create_project", status="success"}
```

---

## 8. TODO List

### Phase 1: Basic Integration

- [ ] Harbor Helm Chart deployment (configure db_auth + S3 storage)
- [x] CRD definition + code generation (HarborProject CRD, deepcopy)
- [x] Harbor Admin Client (`internal/harbor/client.go`)
- [x] Reconciler core logic (create Project → Robot → distribute Secrets via namespaceRefs; reverse cleanup on delete)
- [x] Robot Token refresh via `harbor.sealos.io/refresh-token` annotation (create new → update secrets → delete old)
- [x] RBAC + deployment configuration (ServiceAccount, ClusterRole, Deployment)
- [x] End-to-end testing (create → push → pull → delete full workflow)
- [x] Project Auto-Provision controller (auto-create HarborProject CR from Namespace)

### Phase 2: Metering & Billing

- [ ] Harbor usage collector (periodic Quota API data collection)
- [ ] Registry-Storage pricing type (new property in `resources.go`)
- [ ] Billing pipeline integration (write to Monitor → ActiveBilling pipeline)
- [ ] Grafana dashboard (storage usage visualization)

### Phase 3: Enhanced Features

- [ ] Debt suspension/resume (watch Namespace debt annotation, auto-disable/enable Robot Account)

- [ ] Multi-region replication (Harbor Replication policy)
- [ ] Webhook event notification (image push/scan completion callback)
- [ ] Harbor Portal SSO (optional, after OIDC Provider integration)

---

## 9. K8s Resource Inventory

### 9.1 Deployment Resources

| Resource | Namespace | Description |
|----------|-----------|-------------|
| `harbor-controller` | `harbor-system` | Deployment (1 replica) |
| `harbor-controller` | `harbor-system` | ServiceAccount |
| `harbor-controller` | ClusterRole | Read HarborProject + manage Secrets |
| `harbor-controller` | ClusterRoleBinding | Bind SA to ClusterRole |
| `harborprojects.harbor.sealos.io` | Cluster | CRD definition (Cluster scope) |

### 9.2 Configuration

| Environment Variable | Description | Example |
|---------------------|-------------|---------|
| `HARBOR_ENDPOINT` | Harbor Core service URL | `https://core.harbor.svc:8443` |
| `HARBOR_ADMIN_USERNAME` | Admin username | `admin` |
| `HARBOR_ADMIN_PASSWORD` | Admin password | Read from Secret |
| `REGISTRY_HOST` | Registry external domain | `harbor.sealos.example.com` |

---

## 10. Risk Assessment

| Risk | Impact | Mitigation |
|------|--------|------------|
| Harbor service unavailable | Push/pull failures, reconcile retry | Deploy multiple replicas + external PostgreSQL/Redis |
| Robot Token leak | Unauthorized image push | Robot Account scoped to Project; token refreshable via `harbor.sealos.io/refresh-token` annotation (create new → update secrets → delete old, zero-image-downtime) |
| Finalizer stuck | Harbor resources orphaned after CR deletion | Monitoring + manual cleanup script; Controller log alerting |
| CRD changes | Existing CR compatibility | Use apiextensions.k8s.io/v1, modify fields carefully |
| Harbor upgrade | API version differences | Track Harbor Release Notes, adapt Controller accordingly |
