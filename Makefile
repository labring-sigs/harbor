# Build the controller binary
BINARY := manager

# ---------------------------------------------------------------------------
# Image repository – derived from the Git remote so it works in any fork.
# Override with:  make docker-build IMAGE_REPO=ghcr.io/myorg/myrepo
# ---------------------------------------------------------------------------
GIT_OWNER_REPO := $(shell git config --get remote.origin.url 2>/dev/null | sed -E 's|.*github\.com[:\/](.+)\.git|\1|')
IMAGE_REPO     ?= ghcr.io/$(or $(GIT_OWNER_REPO),dinoallo/labring-sigs-harbor)
IMAGE_TAG      ?= latest

.PHONY: all build docker-build docker-push clean

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

# Generate CRD YAML (requires controller-gen)
controller-gen:
	controller-gen crd:generateEmbedded=true paths="./api/..." output:crd:dir=deploy/crds

# Generate deepcopy (requires controller-gen)
deepcopy-gen:
	controller-gen object:paths="./api/..."
