.PHONY: default all image deploy
THIS_FILE := $(lastword $(MAKEFILE_LIST))

PLATFORM ?= alpine
REPO ?= github.com/mavryk-network/mvindex
BUILD_TARGET ?= mvindex
BUILD_TAG ?= master
BUILD_VERSION ?= $(shell git describe ${BUILD_TAG} --tags --always)-$(PLATFORM)
BUILD_COMMIT ?= $(shell git rev-parse --short ${BUILD_TAG})
BUILD_IMAGE := mavrykdynamics/$(BUILD_TARGET):$(BUILD_VERSION)
BUILD_LATEST := mavrykdynamics/$(BUILD_TARGET):latest
export BUILD_TAG BUILD_TARGET BUILD_VERSION BUILD_COMMIT BUILD_IMAGE BUILD_LATEST

BUILD_FLAGS := --build-arg BUILD_TARGET=$(BUILD_TARGET) --build-arg BUILD_COMMIT=$(BUILD_COMMIT) --build-arg BUILD_VERSION=$(BUILD_VERSION) --build-arg BUILD_TAG=$(BUILD_TAG)

all: build

build:
	@echo $@
	go clean
	CGO_ENABLED=0 go build -mod=mod -a -o ./ -ldflags "-w -X ${REPO}/main.version=${BUILD_VERSION} -X ${REPO}/main.gitcommit=${BUILD_COMMIT}" ./cmd/...

image:
	@echo $@
	docker build -f docker/Dockerfile.$(PLATFORM) --pull --no-cache --rm --tag $(BUILD_IMAGE) --tag $(BUILD_LATEST) $(BUILD_FLAGS) .
	docker image prune --force --filter "label=autodelete=true"

deploy: image
	@echo $@
	@echo "Publishing image..."
	docker login -u $(DOCKER_REGISTRY_USER) -p $(DOCKER_REGISTRY_PASSPHRASE) $(DOCKER_REGISTRY_ADDR)
	docker push $(BUILD_IMAGE)
	docker push $(BUILD_LATEST)
