# Build the controller binary
BINARY := manager

# ---------------------------------------------------------------------------
# Image repository – derived from the Git remote so it works in any fork.
# Override with:  make docker-build IMAGE_REPO=ghcr.io/myorg/myrepo
# ---------------------------------------------------------------------------
GIT_OWNER_REPO := $(shell git config --get remote.origin.url 2>/dev/null | sed -E 's|.*github\.com[:\/](.+)\.git|\1|')
IMAGE_REPO     ?= ghcr.io/$(or $(GIT_OWNER_REPO),dinoallo/labring-sigs-harbor)
IMAGE_TAG      ?= latest

.PHONY: all build docker-build docker-push clean run test test-e2e test-integration

all: build

build:
	mkdir -p bin
	go build -o bin/$(BINARY) .

docker-build:
	docker build -t $(IMAGE_REPO):$(IMAGE_TAG) .

docker-push:
	docker push $(IMAGE_REPO):$(IMAGE_TAG)

clean:
	rm -rf bin/

run:
	go run ./main.go

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# Run unit tests (excludes integration and e2e tests)
test:
	SKIP_TC=1 SKIP_E2E=1 go test -v -count=1 -short ./...

# Run integration tests with envtest (requires KUBEBUILDER_ASSETS)
test-integration:
	SKIP_E2E=1 go test -v -count=1 ./controllers/...

# Run e2e tests (requires Docker daemon + KUBEBUILDER_ASSETS)
# E2E tests spin up a Testcontainers container with a mock Harbor server
# that also implements the OCI Distribution API for image push/pull.
#
# Prerequisites:
#   - Docker daemon
#   - envtest binaries (etcd + kube-apiserver) on PATH or KUBEBUILDER_ASSETS
#
# Optional: set SKIP_E2E=1 to skip e2e tests in CI or local runs.
test-e2e:
	go test -v -count=1 -run 'TestE2E_' ./controllers/...

# Generate CRD YAML (requires controller-gen)
controller-gen:
	controller-gen crd:generateEmbedded=true paths="./api/..." output:crd:dir=deploy/crds

# Generate deepcopy (requires controller-gen)
deepcopy-gen:
	controller-gen object:paths="./api/..."
