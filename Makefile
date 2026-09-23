# Build the controller binary
BINARY := manager
IMAGE_TAG ?= latest
IMAGE_REPO ?= ghcr.io/labring-sigs/harbor

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
