## Tool Binaries
TOOLS_DIR := $(abspath tools-bin)
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen

## Paths
CRD_OPTIONS ?= crd:generateEmbeddedObjectMeta=true

.PHONY: all
all: generate manifests fmt build test

.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=typeclaw-operator-manager-role $(CRD_OPTIONS) webhook paths=./api/... paths=./internal/... output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="docs/boilerplate.go.txt" paths=./api/...

.PHONY: fmt
fmt:
	go fmt ./...
	gofmt -s -w .

.PHONY: vet
vet:
	go vet ./...

.PHONY: build
build:
	go build -o bin/manager cmd/main.go

.PHONY: test
test:
	go test ./... -coverprofile=cover.out

.PHONY: install
install: manifests ## Install CRDs and manager RBAC into the configured cluster.
	kubectl apply -k config/crd
	kubectl apply -f config/rbac/role.yaml

.PHONY: uninstall
uninstall:
	kubectl delete -k config/crd 2>/dev/null || true
	kubectl delete -f config/rbac/role.yaml 2>/dev/null || true

.PHONY: run
run: manifests generate ## Run the controller against the configured cluster.
	go run ./cmd

.PHONY: docker-build
docker-build:
	docker build -t ghcr.io/fml09/typeclaw-operator:dev .

.PHONY: docker-push
docker-push:
	docker push ghcr.io/fml09/typeclaw-operator:dev
