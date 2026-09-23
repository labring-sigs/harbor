# Harbor Controller

A Kubernetes controller that manages [Harbor](https://goharbor.io/) project and robot account lifecycles via a custom CRD (`HarborProject`). Designed for integration with Sealos Cloud — users get push/pull access to their own Harbor project without direct access to the Harbor UI.

## Overview

```
                         ┌──────────────────────┐
                         │   Harbor Controller   │
                         │  (Standalone Pod)     │
                         │                      │
                         │  Watches             │
                         │  HarborProject CR    │
                         └──────┬───────────────┘
                                │ creates / manages
                                ▼
              ┌──────────────────────────────────┐
              │          Harbor (db_auth)         │
              │  ┌──────┐ ┌──────────┐ ┌───────┐ │
              │  │ Core  │ │ Registry │ │ Job   │ │
              │  │       │ │ (OCI)    │ │ Svc   │ │
              │  └──────┘ └──────────┘ └───────┘ │
              └──────────────────────────────────┘
```

- **CRD-Driven**: Define Harbor projects declaratively with `HarborProject` custom resources
- **Automatic Robot Accounts**: Each project gets a robot account with push/pull permissions
- **Secret Distribution**: Docker credentials are distributed as `dockerconfigjson` Secrets to target namespaces
- **Token Refresh**: Rotate robot tokens on demand via a simple annotation (zero-image-downtime)
- **Cluster-Scoped**: `HarborProject` is a cluster-scoped resource, supporting multi-namespace sharing

## CRD: `HarborProject`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `projectName` | `string` | `"hp-{name}"` | Harbor project name (auto-generated if empty) |
| `displayName` | `string` | — | Human-readable display name |
| `namespaceRefs` | `[]string` | — | Target namespaces for credential distribution |
| `owner` | `string` | — | Final owner for metering/billing |
| `storageLimit` | `int64` | `-1` | Storage quota in bytes (`-1` = unlimited) |
| `autoScan` | `bool` | `false` | Auto-scan images on push |
| `public` | `bool` | `false` | Allow anonymous pull |
| `robotPermissions` | `[]RobotPermission` | `[push, pull]` | Robot account permissions |

### Status Fields

| Field | Description |
|-------|-------------|
| `phase` | Current lifecycle phase (`Pending` → `Creating` → `Ready` / `Failed`) |
| `harborProjectID` | Numeric project ID in Harbor |
| `harborProjectName` | Project name in Harbor |
| `robotName` | Robot account name |
| `robotID` | Robot account numeric ID (for precise deletion) |
| `owner` | Owner propagated from spec |
| `conditions` | Standard Kubernetes conditions |

## Prerequisites

- Kubernetes 1.22+
- Harbor instance (deployed in the cluster or externally)
- Admin credentials for Harbor API access

## Installation

### 1. Create namespace and deploy RBAC

```bash
kubectl create namespace harbor-system
kubectl apply -f deploy/rbac.yaml
```

### 2. Set Harbor admin password

```bash
kubectl -n harbor-system create secret generic harbor-admin-password \
  --from-literal=password=<your-harbor-admin-password>
```

### 3. Deploy the controller

```bash
kubectl apply -f deploy/crds/harbor.sealos.io_harborprojects.yaml
kubectl apply -f deploy/deployment.yaml
```

### 4. Verify

```bash
kubectl -n harbor-system get pods -l app.kubernetes.io/name=harbor-controller
```

## Usage

### Create a HarborProject

```yaml
apiVersion: harbor.sealos.io/v1
kind: HarborProject
metadata:
  name: my-team-project
spec:
  displayName: "My Team Project"
  owner: "user-abc123"
  namespaceRefs:
    - team-ns-alpha
    - team-ns-beta
  storageLimit: 10737418240  # 10 GiB
  public: false
  autoScan: true
```

```bash
kubectl apply -f project.yaml
```

### Check status

```bash
kubectl get harborprojects
kubectl get hp     # short name
kubectl get hproj  # short name
```

Example output:

```
NAME              PHASE   PROJECTID   ROBOTID   OWNER        AGE
my-team-project   Ready   5           12        user-abc123  2m
```

### Retrieve credentials

```bash
kubectl get secret harbor-registry-cred-my-team-project \
  -n team-ns-alpha \
  -o jsonpath="{.data.\.dockerconfigjson}" | base64 -d
```

### Push / pull images

```bash
# Login with robot account credentials
docker login harbor.sealos.example.com \
  -u robot$team-ns-alpha+my-team-project \
  -p <token>

# Push
docker tag my-app:v1 harbor.sealos.example.com/my-team-project/my-app:v1
docker push harbor.sealos.example.com/my-team-project/my-app:v1

# Pull
docker pull harbor.sealos.example.com/my-team-project/my-app:v1
```

### Rotate robot token

```bash
# Add the refresh annotation to trigger token rotation
kubectl annotate harborproject my-team-project harbor.sealos.io/refresh-token=true

# The controller will:
# 1. Create a new robot account (fresh token)
# 2. Update dockerconfigjson Secrets in all target namespaces
# 3. Delete the old robot from Harbor
# 4. Remove the annotation
```

Rotation uses a **create-new → update-secrets → delete-old** flow with zero image downtime.

### Delete a HarborProject

```bash
kubectl delete harborproject my-team-project
```

The controller removes all Secrets from target namespaces, deletes the Harbor project (cascading to all images and robot accounts), then removes the finalizer.

## Configuration

The controller reads these environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `HARBOR_ENDPOINT` | `https://core.harbor.svc:8443` | Harbor API endpoint |
| `HARBOR_ADMIN_USERNAME` | `admin` | Harbor admin username |
| `HARBOR_ADMIN_PASSWORD` | — | Harbor admin password (from secret) |
| `REGISTRY_HOST` | `registry.sealos.io` | Registry hostname used in dockerconfigjson |

## Development

### Prerequisites

- Go 1.22+
- Docker (optional, for building images)

### Build

```bash
make build
```

### Run locally

```bash
export HARBOR_ENDPOINT=https://your-harbor.example.com
export HARBOR_ADMIN_USERNAME=admin
export HARBOR_ADMIN_PASSWORD=your-password
export REGISTRY_HOST=registry.example.com

make run
```

### Build Docker image

```bash
make docker-build IMAGE_TAG=v0.1.0
```

### Regenerate CRD YAML

```bash
# Requires controller-gen
make controller-gen
```

## Design Documents

- [Design (English)](DESIGN.en.md)
- [设计文档 (中文)](DESIGN.md)

## License

Apache 2.0
