# smol-agents Makefile
# All targets are idempotent and verifiable.

GO ?= go
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w \
	-X github.com/smol-platform/smol-agents/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev") \
	-X github.com/smol-platform/smol-agents/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none") \
	-X github.com/smol-platform/smol-agents/internal/version.BuildTime=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BIN_DIR := bin
CMDS := agent secret-proxy agentctl ebpf-loader
DOCKER_IMAGES := agent secret-proxy agentctl ebpf-loader operator agentfs-sidecar agentgateway \
	harness-claude-code harness-codex harness-aider harness-goose terminal-sidecar

.PHONY: all
all: tidy fmt vet lint build test

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	gofumpt -w .

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: build
build: $(addprefix $(BIN_DIR)/,$(CMDS))

$(BIN_DIR)/%: cmd/%
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/$*

.PHONY: bpf
bpf:
	$(GO) generate ./pkg/ebpf/...

.PHONY: test
test:
	$(GO) test -race -count=1 ./...

.PHONY: envtest
envtest:
	@command -v $(HOME)/go/bin/setup-envtest >/dev/null 2>&1 || $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@KUBEBUILDER_ASSETS="$$($(HOME)/go/bin/setup-envtest use 1.31 -p path)" \
	  $(GO) test -tags=envtest -count=1 -timeout=300s ./operator/internal/controllers/...

.PHONY: test-e2e-operator
test-e2e-operator:
	bash test/e2e/scripts/up-kind-operator.sh

.PHONY: kind-verify
kind-verify:
	bash scripts/kind-verify.sh

.PHONY: kind-down
kind-down:
	kind delete cluster --name $${CLUSTER:-smol-agents-kind}

.PHONY: test-cover
test-cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

.PHONY: test-integration
test-integration:
	$(GO) test -race -tags=integration -timeout 5m ./test/integration/...

.PHONY: test-e2e
test-e2e:
	$(GO) test -race -tags=e2e -timeout 15m ./test/e2e/...

# fullstack-e2e: layered, runs against real binaries.
# See .spec-workflow/specs/smol-agents-fullstack-e2e/.

.PHONY: e2e-l0
e2e-l0: ## fullstack-e2e L0 ring (docker-compose, ~1 min)
	$(GO) test -tags='e2e_l0 wgnetstack' -timeout 10m ./test/e2e/fullstack/l0/...

.PHONY: e2e-l1
e2e-l1: ## fullstack-e2e L1 ring (kind on Linux, ~5 min)
	$(GO) test -tags=e2e_l1 -timeout 15m ./test/e2e/fullstack/l1/...

# All L2 targets must run against us-east-2 under the smol-agents
# sandbox profile — the IAM role, sweeper Lambda, and budget
# alarm only exist there.
.PHONY: _check-l2-aws
_check-l2-aws:
	@: $${AWS_PROFILE:?must be set to a sandbox AWS profile}
	@: $${AWS_REGION:?must be set, expected us-east-2}
	@if [ "$(AWS_REGION)" != "us-east-2" ]; then \
		echo "AWS_REGION must be us-east-2; got $(AWS_REGION)" >&2; exit 1; \
	fi

.PHONY: e2e-l2
e2e-l2: _check-l2-aws ## fullstack-e2e L2 ring (AWS Spot bare-metal, 12 min, USD 0.22/run)
	$(GO) test -tags=e2e_l2 -timeout 30m -run TestL2$$ ./test/e2e/fullstack/l2/...

# Resolves SG + subnet from terraform output so NodePort scenarios
# can reach in-cluster Services from the host. Without this wiring
# scenarios like AGENTRUN self-skip.
.PHONY: e2e-l2-alldistros
e2e-l2-alldistros: _check-l2-aws ## fullstack-e2e L2 across AL2023+Ubuntu+Flatcar+FCOS with NodePort ingress
	@: $${L2_ARTIFACT_BUCKET:?must be set}
	@: $${L2_ECR_REGISTRY:?must be set}
	@cd infra/terraform/aws-e2e && \
	  sg=$$(terraform output -raw security_group_id) && \
	  subnet=$$(terraform output -raw subnet_id) && \
	  cd ../../.. && \
	  L2_SECURITY_GROUP_ID=$$sg L2_SUBNET_ID=$$subnet \
	    $(GO) test -tags='e2e_l2 wgnetstack' -timeout 60m -run TestL2_AllDistros ./test/e2e/fullstack/l2/...

.PHONY: e2e-l2-smoke
e2e-l2-smoke: _check-l2-aws ## L2 smoke (Provision+Teardown, 6 min, USD 0.10/run)
	$(GO) test -tags=e2e_l2 -timeout 15m -run TestL2_Smoke ./test/e2e/fullstack/l2/...

.PHONY: e2e-clean-aws
e2e-clean-aws: ## terminate every stranded smol-agents-e2e instance
	@: $${AWS_PROFILE:?must be set to a sandbox AWS profile}
	bash scripts/aws-l2/sweep.sh

L2_IMAGE_TAG ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: l2-bundle
l2-bundle: l2-manifests l2-images ## build + upload manifest tarball + ECR images

.PHONY: l2-manifests
l2-manifests: ## kustomize-render manifests, upload tarball to S3
	@: $${L2_ARTIFACT_BUCKET:?must be set (S3 bucket from terraform output)}
	@: $${AWS_PROFILE:?must be set to a sandbox AWS profile}
	bash scripts/aws-l2/build-manifests.sh $(L2_IMAGE_TAG)

.PHONY: l2-images
l2-images: ## build linux/arm64 images, push to ECR
	@: $${L2_ECR_REGISTRY:?must be set (e.g. 123.dkr.ecr.us-east-2.amazonaws.com)}
	@: $${AWS_PROFILE:?must be set to a sandbox AWS profile}
	bash scripts/aws-l2/build-images.sh $(L2_IMAGE_TAG)

.PHONY: verify-formal
verify-formal:
	@command -v quint >/dev/null || { echo "quint not installed"; exit 1; }
	@for f in spec/quint/*.qnt; do \
		echo "=== typecheck $$f ==="; \
		quint typecheck "$$f" || exit 1; \
		echo "=== invariant Safety: $$f ==="; \
		quint run --invariant=Safety --max-steps=30 --max-samples=2000 "$$f" || exit 1; \
	done
	@echo "=== formal verification PASSED ==="

.PHONY: verify
verify: vet lint test verify-formal

# Local single-arch build (host arch) for kind/compose dev. Multiarch
# manifest lists can't be `docker load`ed, so this path stays single-arch;
# published images MUST be multiarch — use `images-push` (or build-images.sh).
.PHONY: docker
docker:
	@for cmd in $(DOCKER_IMAGES); do \
		docker build -f deploy/docker/$$cmd.Dockerfile -t smol-agents/$$cmd:dev .; \
	done

# Multiarch (amd64+arm64) build + push to a registry. This is the standing
# default for any published image.
#   make images-push REGISTRY=ghcr.io/you IMAGE_TAG=v0
PLATFORMS ?= linux/amd64,linux/arm64
REGISTRY  ?=
IMAGE_TAG ?= dev
.PHONY: images-push
images-push: ## buildx multiarch (amd64+arm64) build+push of core images to $REGISTRY
	@: $${REGISTRY:?set REGISTRY=<host>/<org>, e.g. ghcr.io/you}
	mkdir -p bpf/build
	docker buildx build --file deploy/docker/bpf-builder.Dockerfile \
	  --target export --output type=local,dest=. .
	@for cmd in $(DOCKER_IMAGES); do \
		echo "=== $$cmd -> $(REGISTRY)/smol-agents/$$cmd:$(IMAGE_TAG) ($(PLATFORMS)) ==="; \
		docker buildx build --platform $(PLATFORMS) \
			--file deploy/docker/$$cmd.Dockerfile \
			--tag $(REGISTRY)/smol-agents/$$cmd:$(IMAGE_TAG) --push . || exit 1; \
	done

.PHONY: build-operator
build-operator:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/operator ./operator/cmd/manager

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html
	find . -name '*_bpfel*' -delete -o -name '*_bpfeb*' -delete

.PHONY: help
help: ## list documented targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# --- agentbench: benchmarking + verification platform (operator module) ---
.PHONY: bench-lint
bench-lint: ## validate the bench plans offline (hermetic, no cluster)
	cd operator && $(GO) run ./cmd/agentbench lint --plan $(CURDIR)/bench/plans/core

.PHONY: bench-l1
bench-l1: ## run correctness+scale bench tiers on cftest (runc); needs KUBECONFIG
	@: $${KUBECONFIG:?set KUBECONFIG to the cftest cluster}
	cd operator && $(GO) run ./cmd/agentbench run --plan $(CURDIR)/bench/plans/core --tier correctness --kubeconfig $(KUBECONFIG) --out $(CURDIR)/bench/results

.PHONY: bench-l2
bench-l2: ## run the kata isolation bench tier; needs KUBECONFIG to a KVM-capable (AWS Graviton metal) cluster
	@: $${KUBECONFIG:?set KUBECONFIG to a kata-capable cluster}
	cd operator && $(GO) run ./cmd/agentbench run --plan $(CURDIR)/bench/plans/isolation --tier isolation --kubeconfig $(KUBECONFIG) --out $(CURDIR)/bench/results
