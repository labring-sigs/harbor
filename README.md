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

You can deploy the controller using either raw Kubernetes manifests or Helm charts.

### Option A: Raw manifests (quick start)

```bash
kubectl create namespace harbor-system
kubectl apply -f deploy/crds/harbor.sealos.io_harborprojects.yaml
kubectl apply -f deploy/rbac.yaml
kubectl -n harbor-system create secret generic harbor-admin-password \
  --from-literal=password=<your-harbor-admin-password>
kubectl apply -f deploy/deployment.yaml
```

Verify:

```bash
kubectl -n harbor-system get pods -l app.kubernetes.io/name=harbor-controller
```

### Option B: Helm charts (recommended)

Three Helm charts are provided in the `charts/` directory:

| Chart | Path | Description |
|-------|------|-------------|
| `harbor-controller` | `charts/harbor-controller/` | Standalone HarborProject CRD controller (Deployment, RBAC, CRDs) |
| `harbor` | `charts/harbor/` | Minimal Harbor wrapper — disables Portal, Trivy, Notary, ChartMuseum, Exporter |
| `harbor-stack` | `charts/harbor-stack/` | Umbrella chart bundling both Harbor + Controller for a single-command install |

#### Prerequisites

- Helm 3.8+
- Kubernetes 1.22+
- The official Harbor Helm chart repository (required by the `harbor` chart):

```bash
helm repo add harbor https://helm.goharbor.io
helm repo update
```

#### Build chart dependencies

Before installing any chart that depends on the official `harbor-helm` chart, build the
dependency:

```bash
cd charts/harbor
helm dependency build
```

The `harbor-stack` umbrella chart also needs its subchart dependencies resolved:

```bash
cd charts/harbor-stack
helm dependency build
```

#### Scenario 1: Controller with an external Harbor instance (default)

Install the controller alone and point it at an existing Harbor service:

```bash
# Create the admin password Secret (required)
kubectl create namespace harbor-system
kubectl -n harbor-system create secret generic harbor-admin-password \
  --from-literal=password=<your-harbor-admin-password>

# Install the controller
helm install harbor-controller charts/harbor-controller \
  --namespace harbor-system --create-namespace \
  --set harbor.endpoint=https://harbor.example.com \
  --set harbor.registryHost=harbor.example.com
```

#### Scenario 2: Bundled Harbor + Controller (all-in-one)

Deploy a minimal Harbor instance alongside the controller with a single command:

```bash
# Build dependencies first
cd charts/harbor-stack && helm dependency build

# Install the stack with bundled Harbor
helm install harbor-stack . \
  --namespace harbor-system --create-namespace \
  --set harbor.enabled=true \
  --set harbor.adminPassword=<your-admin-password>
```

> **Important:** When `harbor.adminPassword` is omitted or empty, the admin password is randomly generated during install. Retrieve it from the `harbor-admin-password` Secret.

#### Scenario 3: Harbor only

Deploy the minimal Harbor wrapper chart on its own:

```bash
cd charts/harbor && helm dependency build
helm install harbor . --namespace harbor-system --create-namespace
```

#### Configuration reference

##### Harbor controller (`charts/harbor-controller/values.yaml`)

| Parameter | Default | Description |
|-----------|---------|-------------|
| `harbor.endpoint` | `https://core.harbor.svc:8443` | Harbor API endpoint |
| `harbor.adminUsername` | `admin` | Harbor admin username |
| `harbor.adminPasswordSecret.name` | `harbor-admin-password` | Secret name for the admin password |
| `harbor.adminPasswordSecret.key` | `password` | Secret key for the admin password |
| `harbor.registryHost` | `registry.sealos.io` | Registry hostname in dockerconfigjson Secrets |
| `leaderElection.enabled` | `true` | Enable leader election for HA |
| `controllerArgs` | `[]` | Additional controller arguments (managed by `leaderElection`) |
| `metrics.serviceMonitor.create` | `false` | Create a Prometheus ServiceMonitor |

##### Minimal Harbor (`charts/harbor/values.yaml`)

All values under the `harbor:` key are forwarded to the official
[`harbor-helm`](https://github.com/goharbor/harbor-helm) chart.
Key defaults specific to this wrapper:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `harbor.expose.type` | `ingress` | Expose type: `ingress`, `clusterIP`, `nodePort`, or `loadBalancer` |
| `harbor.expose.tls.certSource` | `secret` | TLS certificate source: `auto`, `secret`, or `none` |
| `harbor.externalURL` | `https://core.harbor.domain` | External URL (used for token generation) |
| `harbor.database.type` | `internal` | Database backend: `internal` (bundled PostgreSQL) or `external` |
| `harbor.redis.type` | `internal` | Redis backend: `internal` (bundled Valkey) or `external` |
| `harbor.persistence.enabled` | `true` | Enable persistent volumes |

Components **disabled** by default: Portal, Trivy, Notary, ChartMuseum, Exporter.
Components **enabled**: Core, Registry, JobService, Database, Redis.

##### Harbor stack (`charts/harbor-stack/values.yaml`)

| Parameter | Default | Description |
|-----------|---------|-------------|
| `harbor.enabled` | `false` | Set to `true` to deploy bundled Harbor |
| `harbor.adminPassword` | `""` (auto-generated) | Initial admin password (empty = random; creates the shared Secret) |
| `harbor-controller.harbor.endpoint` | `http://harbor:80` | Controller endpoint (auto-configured for bundled mode) |
| `harbor-controller.harbor.registryHost` | `harbor:80` | Registry host (auto-configured for bundled mode) |

#### Admin password management

- **Standalone controller:** You must create the `harbor-admin-password` Secret manually
  in the target namespace before the controller starts.
- **Bundled stack (`harbor.enabled=true`):** The chart automatically creates the
  shared Secret from `.Values.harbor.adminPassword`. Both Harbor and the controller
  reference the same Secret.
- **Post-deploy password change:** Update the Secret data, then restart the controller
  pod and the Harbor core pod.

#### Ingress and TLS

When using `harbor.expose.type: ingress` (the default), the chart creates an Ingress
resource for the Harbor Core API. By default, TLS is enabled with `certSource: secret`,
meaning you must provide a TLS certificate Secret or set `certSource: auto` for
auto-provisioned certificates (e.g., Let's Encrypt via cert-manager).

To configure a custom domain:

```bash
helm install harbor-stack charts/harbor-stack \
  --set harbor.harbor.expose.ingress.hosts.core=harbor.mycompany.com \
  --set harbor.harbor.externalURL=https://harbor.mycompany.com
```

#### Database and Redis migration

Both `database.type` and `redis.type` default to `internal`, which deploys
containers within the cluster. To migrate to external instances after deployment:

1. Provision your external PostgreSQL / Redis
2. Update the values to use `type: external` with the appropriate connection
   parameters (see the [upstream chart docs](https://github.com/goharbor/harbor-helm))
3. Run `helm upgrade` to apply the changes


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
# The robot username and token are stored in the Secret; extract them:

USER=$(kubectl get secret harbor-registry-cred-my-team-project \
  -n team-ns-alpha \
  -o jsonpath="{.data.\.dockerconfigjson}" | base64 -d \
  | python3 -c "import sys,json; c=json.load(sys.stdin); print(list(c['auths'].values())[0]['username'])")

TOKEN=$(kubectl get secret harbor-registry-cred-my-team-project \
  -n team-ns-alpha \
  -o jsonpath="{.data.\.dockerconfigjson}" | base64 -d \
  | python3 -c "import sys,json; c=json.load(sys.stdin); print(list(c['auths'].values())[0]['password'])")

# Login with robot account credentials
echo "$TOKEN" | docker login harbor.sealos.example.com -u "$USER" --password-stdin

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

- Go 1.26+
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
