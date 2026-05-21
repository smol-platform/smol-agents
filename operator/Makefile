# Operator-local Makefile. The repo-root Makefile builds binaries; this
# one is for the operator's lifecycle (CRDs, manifests, deploy).

GOPATH ?= $(shell go env GOPATH)
PROJECT_ROOT := $(shell git rev-parse --show-toplevel 2>/dev/null || pwd)
KUBECTL ?= kubectl
CONTROLLER_GEN ?= $(GOPATH)/bin/controller-gen

.PHONY: all
all: vet test

.PHONY: vet
vet:
	cd $(PROJECT_ROOT) && go vet ./operator/...

.PHONY: test
test:
	cd $(PROJECT_ROOT) && go test -race -count=1 ./operator/...

.PHONY: install-tools
install-tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

.PHONY: manifests
manifests: install-tools
	cd $(PROJECT_ROOT) && $(CONTROLLER_GEN) crd:crdVersions=v1,allowDangerousTypes=true paths=./operator/api/... output:crd:dir=operator/config/crd
	cd $(PROJECT_ROOT) && $(CONTROLLER_GEN) rbac:roleName=smol-agents-operator paths=./operator/internal/controllers/... output:rbac:dir=operator/config/rbac

.PHONY: deepcopy
deepcopy: install-tools
	cd $(PROJECT_ROOT) && $(CONTROLLER_GEN) \
	    object:headerFile=hack/boilerplate.go.txt \
	    paths=./operator/api/...

.PHONY: deploy
deploy:
	$(KUBECTL) apply -f operator/config/crd
	$(KUBECTL) apply -f operator/config/manager

.PHONY: undeploy
undeploy:
	$(KUBECTL) delete -f operator/config/manager --ignore-not-found
	$(KUBECTL) delete -f operator/config/crd --ignore-not-found

.PHONY: build
build:
	cd $(PROJECT_ROOT) && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o bin/operator ./operator/cmd/manager
