# Harbor 镜像仓库集成方案

> 版本: v1.0
> 日期: 2026-09-23
> 变更: 初始版本

---

## 一、背景与目标

### 1.1 背景

Sealos Cloud 目前缺少集群内镜像仓库产品。用户无法在平台内上传、管理和分发容器镜像。为完善平台能力，需要引入企业级镜像仓库。

### 1.2 目标

- 在 Sealos 集群内部署 Harbor 作为镜像仓库
- 通过自定义 CRD 控制 Harbor Project 的创建与销毁
- 用户通过 Docker CLI / containerd / podman 推送和拉取镜像，无需访问 Harbor Web UI
- 镜像存储用量纳入 Sealos 现有的计量计费体系

### 1.3 设计原则

- **最小暴露面**：不对外暴露 Harbor Portal，用户仅通过标准 OCI CLI 工具交互
- **独立控制器**：新增独立的 Harbor Controller，不修改现有 account controller / NamespaceReconciler
- **CRD 驱动**：通过自定义资源 `HarborProject` 声明式管理 Harbor Project 生命周期
- **渐进式扩展**：Phase 1 聚焦创建和删除，后续按需增加欠费暂停等功能

---

## 二、整体架构

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Sealos Cloud                                 │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │                  Kubernetes Cluster                         │   │
│  │                                                             │   │
│  │  ┌──────────────────────┐    ┌──────────────────────────┐  │   │
│  │  │  Harbor Controller    │    │  Harbor (db_auth)        │  │   │
│  │  │  (standalone)         │───▶│                          │  │   │
│  │  │                       │    │  ├─ Core                 │  │   │
│  │  │  Watches HarborProject │    │  ├─ Registry (OCI)       │  │   │
│  │  │  CR (Cluster scoped)  │    │  ├─ JobService           │  │   │
│  │  │                       │    │  └─ Exporter             │  │   │
│  │  └──────────┬────────────┘    └──────────┬───────────────┘  │   │
│  │             │                            │                   │   │
│  │             │ 创建/删除                    │ S3 协议           │   │
│  │             ▼                            ▼                   │   │
│  │  ┌─────────────────────┐    ┌──────────────────────┐        │   │
│  │  │  User Namespace     │    │  MinIO / S3          │        │   │
│  │  │  ┌───────────────┐  │    │  (镜像层存储)          │        │   │
│  │  │  │ Secret:       │  │    └──────────────────────┘        │   │
│  │  │  │ harbor-token  │  │                                     │   │
│  │  │  └───────────────┘  │                                     │   │
│  │  │  ┌───────────────┐  │                                     │   │
│  │  │  │ HarborProject│  │                                     │   │
│  │  │  │ CR             │  │                                     │   │
│  │  │  └───────────────┘  │                                     │   │
│  │  └─────────────────────┘                                     │   │
│  │                                                             │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  Service/Account (计量计费)                                  │   │
│  │  ├─ Harbor 用量采集器 (定时调用 Harbor Quota API)            │   │
│  │  └─ Registry-Storage 计费类型扩展                           │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  用户交互方式:                                                      │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  # 管理员创建 HarborProject (Cluster) → 控制器自动完成后续操作 │   │
│  │  kubectl apply -f harbor-project.yaml                        │   │
│  │                                                               │   │
│  │  # 用户获取 Token                                             │   │
│  │  kubectl get secret harbor-registry-cred -n ns-xxx \          │   │
│  │    -o jsonpath="{.data.\.dockerconfigjson}" \| base64 -d      │   │
│  │                                                               │   │
│  │  # 用户推送/拉取镜像                                           │   │
│  │  docker login harbor.sealos.example.com -u robot$ns-xxx -p ...      │   │
│  │  docker push harbor.sealos.example.com/ns-xxx/my-app:v1             │   │
│  │  docker pull harbor.sealos.example.com/ns-xxx/my-app:v1             │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

### 2.1 核心流程

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  管理员      │     │  Harbor      │     │  目标 NS     │
│  创建 CR      │     │  Controller  │     │  (多个)      │
└──────┬──────┘     └──────┬──────┘     └──────┬──────┘
       │                   │                   │
       │ 1. Apply          │                   │
       │ HarborProject   │                   │
       │──────────────────▶│                   │
       │                   │                   │
       │                   │ 2. Reconcile      │
       │                   │   ├─ 读取 CR      │
       │                   │   ├─ 创建 Harbor  │
       │                   │   │  Project      │
       │                   │   ├─ 创建 Robot   │
       │                   │   │  Account      │
       │                   │   ├─ 创建 Secret  │
       │                   │   │──────────────▶│
       │                   │   └─ 更新 Status  │
       │                   │                   │
       │ 3. 检查 Status    │                   │
       │◀──────────────────│                   │
       │                   │                   │
       │ 4. 用户获取 Secret│                   │
       │──────────────────────────────────────▶│
       │                   │                   │
       │ 5. docker push/   │                   │
       │    pull           │                   │
       │──────────────────────────────────────▶│
       │                   │                   │
```

### 2.2 删除流程

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  用户         │     │  Harbor      │     │  Harbor      │
│  删除 CR      │     │  Controller  │     │  API         │
└──────┬──────┘     └──────┬──────┘     └──────┬──────┘
       │                   │                   │
       │ 1. kubectl delete │                   │
       │    HarborProject│                   │
       │──────────────────▶│                   │
       │                   │                   │
       │                   │ 2. Reconcile      │
       │                   │   ├─ 删除 Project │
       │                   │   │──────────────▶│
       │                   │   │  级联删除所有 │
       │                   │   │  镜像+Robot   │
       │                   │   │◀──────────────│
       │                   │   ├─ 删除 Secret  │
       │                   │   └─ 移除 Finalizer
       │                   │                   │
       │ 3. CR 已删除     │                   │
       │◀──────────────────│                   │
```

---

## 三、自定义 CRD 设计

### 3.1 HarborProject CRD

Harbor Controller 的核心 CRD，定义了一个 Harbor Project 的期望状态。

```yaml
# harbor.sealos.io/v1
# Kind: HarborProject
# Scope: Cluster（集群级别，跨 Namespace 共享）
# 字段: owner 用于计量计费归属

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
                description: "Harbor 中的 Project 名称。为空时自动生成 'hp-{name}'"
              displayName:
                type: string
                description: "Project 显示名称"
              namespaceRefs:
                type: array
                description: "目标 K8s Namespace 名称列表，Controller 将在这些 NS 中分发 docker 凭证"
                items:
                  type: string
              storageLimit:
                type: integer
                format: int64
                default: -1
                description: "存储限制（字节），-1 表示不限制"
              autoScan:
                type: boolean
                default: false
                description: "是否启用自动漏洞扫描"
              public:
                type: boolean
                default: false
                description: "是否公开（无需认证即可拉取）"
              owner:
                type: string
                description: "最终归属方，用于计量计费。一般为用户 ID、租户 ID 或 Namespace UID"
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
                      description: "Robot Account 权限"
          status:
            type: object
            properties:
              phase:
                type: string
                enum: ["Pending", "Creating", "Ready", "Deleting", "Failed"]
                description: "当前阶段"
              owner:
                type: string
                description: "最终归属方，从 spec 同步"
              harborProjectID:
                type: integer
                description: "Harbor 中的 Project ID"
              harborProjectName:
                type: string
                description: "Harbor 中的 Project 名称"
              robotName:
                type: string
                description: "Robot Account 名称"
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

### 3.2 使用示例

管理员或用户创建以下 CR 来申请 Harbor Project：

```yaml
apiVersion: harbor.sealos.io/v1
kind: HarborProject
metadata:
  name: my-project
spec:
  owner: "user-abc123"  # 计量计费归属方
  displayName: "我的镜像仓库"
  namespaceRefs:
    - ns-ff839a27
  storageLimit: 10737418240  # 10GB
  autoScan: true
  robotPermissions:
  - action: push
  - action: pull
```

应用后，Controller 自动完成：

1. 在 Harbor 创建 Project `ns-ff839a27`
2. 创建 Robot Account `robot$ns-ff839a27+my-project`
3. 在 Namespace `ns-ff839a27` 创建 Secret `harbor-registry-cred-my-project`
4. 更新 CR Status 为 `Ready`

查看状态：

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

删除 CR 即可回收所有 Harbor 资源：

```bash
kubectl delete harborproject my-project
# → 级联删除 Harbor Project + 所有目标 NS 中的 Secret
```

### 3.3 Finalizer 设计

为了防止 K8s CR 删除时 Harbor 侧的 Project 未被清理，使用 Finalizer：

```yaml
# Harbor Controller 在 Reconcile 时自动添加 Finalizer
metadata:
  finalizers:
  - harbor.sealos.io/cleanup

# 删除流程：
# 1. kubectl delete → deletionTimestamp 设置
# 2. Controller 检测到 deletionTimestamp → 执行清理
#    2a. 删除 Harbor Project（级联删除所有镜像、Robot Account）
#    2b. 删除 K8s Secret
# 3. 移除 Finalizer
# 4. CR 被真正删除
```

---

## 四、Harbor Controller 设计

### 4.1 控制器架构

```
┌──────────────────────────────────────────────────────┐
│                 Harbor Controller                      │
│                                                       │
│  ┌──────────────────────────────────────────────┐    │
│  │  Reconciler (controller-runtime)              │    │
│  │                                               │    │
│  │  watches: HarborProject (OWN)               │    │
│  │  watches: Secret (OWN + delete event)         │    │
│  │                                               │    │
│  │  Reconcile 逻辑:                              │    │
│  │  ┌──────────────────────────────────────┐    │    │
│  │  │  1. 读取 HarborProject CR           │    │    │
│  │  │  2. finalizer 处理                    │    │    │
│  │  │  3. 检查 Harbor 端状态                 │    │    │
│  │  │  4. 遍历 namespaceRefs 分发 Secret    │    │    │
│  │  │  5. 更新 Status                      │    │    │
│  │  └──────────────────────────────────────┘    │    │
│  └──────────────────────────────────────────────┘    │
│                                                       │
│  ┌──────────────────────────────────────────────┐    │
│  │  Harbor Admin Client (pkg/harbor)             │    │
│  │  - CreateProject                              │    │
│  │  - DeleteProject                              │    │
│  │  - CreateRobot                                │    │
│  │  - UpdateRobotStatus                          │    │
│  │  - GetProjectByName                           │    │
│  │  - GetProjectQuota                            │    │
│  └──────────────────────────────────────────────┘    │
│                                                       │
│  ┌──────────────────────────────────────────────┐    │
│  │  K8s Client                                    │    │
│  │  - Create/Get/Update/Delete Secret            │    │
│  │  - Update HarborProject Status              │    │
│  └──────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────┘
```

### 4.2 项目结构

项目是一个独立的 Go 模块，不耦合到 Sealos controllers：

```
labring-sigs-harbor/
├── main.go                    # 入口
├── api/
│   └── v1/
│       ├── harborproject_types.go   # CRD 类型定义
│       └── groupversion_info.go     # GV 注册
├── controllers/
│   └── harborproject_controller.go  # Reconciler 主逻辑
├── internal/
│   └── harbor/
│       ├── client.go                # Harbor Admin Client
│       ├── types.go                 # Harbor API 类型
│       └── errors.go                # 自定义错误
├── deploy/
│   ├── crds/
│   │   └── harbor.sealos.io_harborprojects.yaml  # CRD 定义
│   ├── rbac.yaml
│   └── deployment.yaml
├── Dockerfile
├── Makefile
├── go.mod
└── go.sum
```

### 4.3 CRD 类型定义 (Go)

```go
package v1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HarborProjectSpec 定义期望状态
type HarborProjectSpec struct {
    // ProjectName Harbor 中的 Project 名称，为空时自动生成
    // +optional
    ProjectName string `json:"projectName,omitempty"`

    // DisplayName 显示名称
    // +optional
    DisplayName string `json:"displayName,omitempty"`

    // StorageLimit 存储限制（字节），-1 表示不限制
    // +kubebuilder:default:=-1
    StorageLimit int64 `json:"storageLimit,omitempty"`

    // AutoScan 是否启用自动漏洞扫描
    // +kubebuilder:default:=false
    AutoScan bool `json:"autoScan,omitempty"`

    // Public 是否公开（无需认证即可拉取）
    // +kubebuilder:default:=false
    Public bool `json:"public,omitempty"`

    // RobotPermissions Robot Account 权限列表
    // +kubebuilder:default:={{action: "push"},{action: "pull"}}
    RobotPermissions []RobotPermission `json:"robotPermissions,omitempty"`
}

type RobotPermission struct {
    Action string `json:"action"`
}

// HarborProjectStatus 定义实际状态
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

### 4.4 Reconciler 核心逻辑

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

    // 1. 获取 CR
    project := &v1.HarborProject{}
    if err := r.Get(ctx, req.NamespacedName, project); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 2. 处理删除
    if !project.DeletionTimestamp.IsZero() {
        return r.reconcileDelete(ctx, project)
    }

    // 3. 处理创建/更新
    return r.reconcileCreate(ctx, project)
}

func (r *HarborProjectReconciler) reconcileCreate(ctx context.Context, project *v1.HarborProject) (ctrl.Result, error) {
    // 确保 Finalizer 已添加
    if !controllerutil.ContainsFinalizer(project, harborFinalizer) {
        controllerutil.AddFinalizer(project, harborFinalizer)
        if err := r.Update(ctx, project); err != nil {
            return ctrl.Result{}, err
        }
        return ctrl.Result{Requeue: true}, nil
    }

    // 已 Ready，无需处理
    if project.Status.Phase == v1.HarborPhaseReady {
        return ctrl.Result{}, nil
    }

    project.Status.Phase = v1.HarborPhaseCreating
    _ = r.Status().Update(ctx, project)

    // 获取或生成 Project 名称
    projectName := project.Spec.ProjectName
    if projectName == "" {
        projectName = "hp-" + project.Name
    }

    // 创建 Harbor Project
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

    // 创建 Robot Account（push + pull 权限）
    robot, err := r.HarborClient.CreateRobot(ctx, project.Status.HarborProjectID, harbor.RobotSpec{
        Name:     "robot-" + shortID(project.Name),
        Duration: -1, // 永不过期
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

    // 遍历 namespaceRefs 分发 Secret
    for _, ns := range project.Spec.NamespaceRefs {
        secret := r.buildDockerConfigSecret(project, robot, ns)
        if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
            project.Status.Phase = v1.HarborPhaseFailed
            _ = r.Status().Update(ctx, project)
            return ctrl.Result{}, err
        }
    }

    // 更新状态为 Ready
    project.Status.Phase = v1.HarborPhaseReady
    setCondition(&project.Status.Conditions, "ProjectCreated", metav1.ConditionTrue, "Success", "Harbor project created")
    setCondition(&project.Status.Conditions, "RobotCreated", metav1.ConditionTrue, "Success", "Robot account created")
    setCondition(&project.Status.Conditions, "SecretCreated", metav1.ConditionTrue, "Success", "K8s secret created")
    return ctrl.Result{}, r.Status().Update(ctx, project)
}

func (r *HarborProjectReconciler) reconcileDelete(ctx context.Context, project *v1.HarborProject) (ctrl.Result, error) {
    // 删除 Harbor Project（级联删除所有镜像 + Robot Accounts）
    if project.Status.HarborProjectID > 0 {
        if err := r.HarborClient.DeleteProject(ctx, project.Status.HarborProjectID); err != nil {
            return ctrl.Result{}, err
        }
    }

    // 遍历 namespaceRefs 删除 K8s Secret
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

    // 移除 Finalizer
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
    // 从 Location header 获取 project ID
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
    // 使用 Basic Auth，设置 Content-Type: application/json
    // 返回 *http.Response
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
    // 使用 Basic Auth
    // 返回 *http.Response
}

func (c *Client) delete(ctx context.Context, path string) (*http.Response, error) {
    // 使用 Basic Auth
    // 返回 *http.Response
}
```

### 4.6 Secret 构建

```go
func (r *HarborProjectReconciler) buildDockerConfigSecret(project *v1.HarborProject, robot *harbor.RobotCredential, namespace string) *corev1.Secret {
    // 构建 dockerconfigjson
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
# 控制器需要的权限
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
### 4.8 Project Auto-Provision（可选控制器）

`ProjectAutoProvision` 是一个**可选**控制器，监听 Namespace 的创建/更新事件，自动为带有指定 owner 标签的 Namespace 创建对应的 `HarborProject` CR，实现"Namespace 创建即自带镜像仓库"的自动化体验。

#### 启用方式

在 controller 启动时传入 `--enable-project-auto-provision` 标志：

```bash
manager --enable-project-auto-provision --owner-label-key="user.sealos.io/owner"
```

#### 工作原理

```
┌─────────────────┐     ┌──────────────────────────────┐     ┌──────────────────────┐
│  Namespace 变化   │     │  ProjectAutoProvision         │     │  HarborProject        │
│                  │     │  Reconciler                    │     │  Reconciler           │
│  创建/更新        │────▶│                               │────▶│  (标准创建流程)        │
│  label 变更       │     │  1. 读取 Namespace            │     │                      │
│                  │     │  2. 检查 owner 标签             │     │  创建 Project         │
│                  │     │  3. 创建/更新 HarborProject CR  │     │  创建 Robot           │
│                  │     │  4. 标签管理（记录 NS UID）      │     │  分发 Secret          │
└─────────────────┘     └──────────────────────────────┘     └──────────────────────┘
```

1. 用户创建 Namespace，并打上 owner 标签（如 `user.sealos.io/owner: user-abc`）
2. `ProjectAutoProvision` 检测到标签变更，生成对应的 `HarborProject` CR（名称格式 `hp-{namespace}`）
3. `HarborProjectReconciler` 接管，执行标准的 Project/ Robot/ Secret 创建流程

#### 标签约定

自动创建的 `HarborProject` CR 上会附加以下标签，用于区分自动创建与手动创建的资源：

| 标签 | 说明 |
|------|------|
| `harbor.sealos.io/auto-provision` | 标记为自动创建，值为 `"true"` |
| `harbor.sealos.io/source-namespace` | 来源 Namespace 名称 |
| `harbor.sealos.io/source-namespace-uid` | 来源 Namespace 的 UID，用于检测重建 |

#### Namespace 重建检测

如果 Namespace 被删除后重建（同名但不同 UID），控制器通过 `source-namespace-uid` 标签检测到 UID 变化，会重新匹配并更新 `HarborProject` 的 spec，确保新 Namespace 被正确采纳。

#### 默认配置

自动创建的 `HarborProject` 使用以下默认值：

- **ProjectName**: Namespace 名称（短名称，非 `hp-` 前缀）
- **StorageLimit**: 5 GB
- **Public**: false
- **AutoScan**: false
- **RobotPermissions**: push + pull
- **NamespaceRefs**: 仅包含来源 Namespace 自身

#### 与手动创建的关系

- 如果某个 `HarborProject` 已经被手动创建，`ProjectAutoProvision` 发现同名 CR 存在时不会重复创建，而是会**采纳**该 CR：将其 spec 更新为匹配当前 Namespace 的标签配置，并补全自动创建标签
- 删除自动创建的 `HarborProject` CR 后，`ProjectAutoProvision` 不会自动重建（除非手动删除后重新触发 Namespace 变更事件）
- 移除 Namespace 上的 owner 标签**不会**触发删除已有 CR——这是有意设计，避免误删

---

## 五、Harbor 部署

### 5.1 认证模式

使用 `db_auth` 模式（本地数据库认证），不需要 OIDC Provider。

```yaml
# values.yaml 关键配置
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
  replicas: 1  # 可缩减，仅管理员调试使用
```

### 5.2 存储后端

- 推荐使用 **S3/MinIO** 作为 Registry 存储后端
- 可复用 Sealos 已有的 MinIO 集群
- 支持水平扩展，避免 PVC 的性能和容量瓶颈

---

## 六、Secret 与认证管理

### 6.1 自动创建的 K8s Secret

Harbor Controller 创建 Robot Account 后，自动在目标 Namespace 创建以下 Secret：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: harbor-registry-cred-my-project
  namespace: ns-ff839a27  # Controller 在每个 namespaceRefs 创建的 NS 中各创建一个
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

Controller 在 `namespaceRefs` 指定的每个 Namespace 中创建一个 Secret。删除 CR 时，Controller 先遍历所有目标 NS 删除 Secret，再清理 Harbor 端资源，最后移除 Finalizer。

### 6.2 用户使用方式

方式一：直接使用 Docker CLI

```bash
# 从 Secret 获取 Token
# 注意：Secret 创建在目标 Namespace 中
kubectl get secret harbor-registry-cred-my-project -n ns-xxx \
  -o jsonpath="{.data.\.dockerconfigjson}" | base64 -d

# 登录
docker login harbor.sealos.example.com \
  -u robot$ns-xxx+my-project \
  -p <token>

# 推送
docker tag my-app:v1 harbor.sealos.example.com/ns-xxx/my-app:v1
docker push harbor.sealos.example.com/ns-xxx/my-app:v1

# 拉取
docker pull harbor.sealos.example.com/ns-xxx/my-app:v1
```

方式二：在 Pod 中使用

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

### 6.3 Token 安全性

| 措施 | 说明 |
|------|------|
| **限定作用域** | Robot Account 仅能访问自己的 Project |
| **永久 Token** | 永不过期，可通过 `harbor.sealos.io/refresh-token` 注解按需刷新 |
| **Secret 存储** | Token 以 dockerconfigjson 形式存储在目标 NS 的 K8s Secret 中 |
| **可刷新** | 对 `HarborProject` 添加注解 `harbor.sealos.io/refresh-token: "true"` → Controller 创建新 Robot、更新 Secret、删除旧 Robot（不影响已有镜像） |
| **审计** | Harbor 审计日志记录所有操作 |



### 6.4 Token 刷新机制

Harbor API 没有提供专门的 Token 轮转端点。Controller 通过 **创建新 Robot → 更新 Secret → 删除旧 Robot** 的流程来轮转 Robot Token，无需删除项目或影响已有镜像。

**触发方式**：对 `Ready` 阶段的 `HarborProject` CR 添加注解 `harbor.sealos.io/refresh-token`。

```
kubectl annotate harborproject my-project harbor.sealos.io/refresh-token=true
```

**执行流程**：

```
+------------------+     +------------------+     +------------------+
|  1. 创建新 Robot  | --> |  2. 更新 K8s      | --> |  3. 删除旧 Robot  |
|   Account         |     |  Secret（所有 NS） |     |   Account         |
+------------------+     +------------------+     +------------------+
       |                        |                        |
       | 返回新 Token + ID       | 所有 namespace         | 按 ID 删除 |
       |                        | 原子更新                | (ID 保存在 |
       |                        |                        | .status.robotID)
       v                        v                        v
   新 Robot 具备             旧凭据被替换              旧 Robot 从
   push/pull 权限             为新凭据                  Harbor 移除
```

**实现细节**：

1. Controller 在 Reconcile 时检测 `Ready` CR 上的注解
2. 调用 `CreateRobot` 创建新 Robot（名称添加 `-refresh` 后缀）
3. 遍历 `namespaceRefs` 中所有 namespace，更新 dockerconfigjson Secret（如不存在则创建）
4. 调用 `DeleteProjectRobot` 使用 `status.robotID` 删除旧 Robot
5. 更新 `status.robotName` 和 `status.robotID` 为新 Robot 的值
6. 移除 `harbor.sealos.io/refresh-token` 注解

**关键特性**：

- 无镜像停机：整个过程中镜像持续可访问
- 旧 Robot 在所有 Secret 更新完成后才被删除，最小化暴露窗口
- `status.robotID` 持久化 Robot 数字 ID，支持精确删除

---

## 七、计量与计费

### 7.1 数据采集

Harbor 用量采集器定时调用 Harbor API，获取各 Project 的存储用量：

```go
type HarborUsageCollector struct {
    interval time.Duration // 默认 1 小时
}

func (c *HarborUsageCollector) Collect(ctx context.Context) ([]HarborUsage, error) {
    // 1. 列出所有 Project
    projects, err := c.listProjects(ctx)
    if err != nil {
        return nil, err
    }

    // 2. 查询每个 Project 的 Quota
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

### 7.2 计价与扣费

在 `controllers/pkg/resources/resources.go` 中新增 Registry 计价类型：

```go
// | property          | Price | Detail          |
// | ----------------- | ----- | --------------- |
// | Registry-Storage  | 2     | Mebibytes unit  | ← 新增

// 计价单位: 2 units/MiB（与现有 Disk 定价一致）
// 即 1GB = 1024 MiB * 2 = 2048 单位/小时
```

用量采集器通过 CR 的 `spec.owner` 或 `status.owner` 确定扣费归属方，计费流水复用现有的 `Monitor → ActiveBilling → Billing` 管道。

### 7.3 Prometheus 监控

Harbor Exporter 指标可直接接入 Sealos 现有 Prometheus：

```prometheus
# Harbor 存储用量
harbor_project_quota_used{project="ns-xxx", type="storage"}
harbor_project_quota_total{project="ns-xxx", type="storage"}

# Harbor Controller 自定义指标
harbor_controller_harborproject_total{phase="Ready"}
harbor_controller_reconcile_duration_seconds
harbor_controller_operation_total{operation="create_project", status="success"}
```

---

## 八、TODO LIST

### Phase 1: Basic Integration

- [ ] Harbor Helm Chart 部署（配置 db_auth + S3 存储）
- [x] CRD 定义 + 代码生成（HarborProject CRD，deepcopy）
- [x] Harbor Admin Client（`internal/harbor/client.go`）
- [x] Reconciler 核心逻辑（创建 Project → Robot → 遍历 namespaceRefs 分发 Secret；删除时反向清理）
- [x] Robot Token 刷新：通过 `harbor.sealos.io/refresh-token` 注解触发（创建新 → 更新 Secret → 删除旧）
- [x] RBAC + 部署配置（ServiceAccount、ClusterRole、Deployment）
- [x] 端到端测试（创建→推送→拉取→删除全流程）
- [x] Project Auto-Provision 控制器（Namespace 自动创建 HarborProject CR）

### Phase 2: Metering & Billing

- [ ] Harbor usage collector (periodic Quota API data collection)
- [ ] Registry-Storage pricing type (new property in resources.go)
- [ ] Billing pipeline integration (write to Monitor → ActiveBilling pipeline)
- [ ] Grafana dashboard (storage usage visualization)

### Phase 3: Enhanced Features

- [ ] Debt suspension/resume (watch Namespace debt annotation, auto-disable/enable Robot Account)

- [ ] Multi-region replication (Harbor Replication policy)
- [ ] Webhook event notification (image push/scan completion callback)
- [ ] Harbor Portal SSO (optional, after OIDC Provider integration)

---

## 九、K8s 资源清单

### 9.1 部署资源

| 资源 | Namespace | 说明 |
|------|-----------|------|
| `harbor-controller` | `harbor-system` | Deployment（1 副本） |
| `harbor-controller` | `harbor-system` | ServiceAccount |
| `harbor-controller` | ClusterRole | 读取 HarborProject + 管理 Secret |
| `harbor-controller` | ClusterRoleBinding | 绑定 SA 到 ClusterRole |
| `harborprojects.harbor.sealos.io` | Cluster | CRD 定义（Cluster 级别）|

### 9.2 配置

| 环境变量 | 说明 | 示例 |
|----------|------|------|
| `HARBOR_ENDPOINT` | Harbor Core 服务地址 | `https://core.harbor.svc:8443` |
| `HARBOR_ADMIN_USERNAME` | 管理员用户名 | `admin` |
| `HARBOR_ADMIN_PASSWORD` | 管理员密码 | 从 Secret 读取 |
| `REGISTRY_HOST` | Registry 对外域名 | `harbor.sealos.example.com` |

---

## 十、风险评估

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| Harbor 服务不可用 | 推拉镜像失败，Reconcile 重试 | 部署多副本 + 外部 PostgreSQL/Redis |
| Robot Token 泄露 | 非法用户推送镜像 | Robot Account 限定 Project 作用域；可通过 `harbor.sealos.io/refresh-token` 注解在线刷新（创建新 → 更新 Secret → 删旧的零停机流程） |
| Finalizer 残留 | CR 删除后 Harbor 侧资源未清理 | 监控 + 手动清理脚本；Controller 日志告警 |
| CRD 变更 | 已有 CR 兼容性 | 使用 apiextensions.k8s.io/v1，谨慎修改字段 |
| Harbor 升级 | API 版本差异 | 跟踪 Harbor Release Notes，Controller 适配 |
