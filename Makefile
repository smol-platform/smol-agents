# knative-agents Makefile
# All targets are idempotent and verifiable.

GO ?= go
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w \
	-X github.com/stigen/knative-agents/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev") \
	-X github.com/stigen/knative-agents/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none") \
	-X github.com/stigen/knative-agents/internal/version.BuildTime=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BIN_DIR := bin
CMDS := agent secret-proxy agentctl ebpf-loader
DOCKER_IMAGES := agent secret-proxy agentctl ebpf-loader operator

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
	kind delete cluster --name $${CLUSTER:-knative-agents-kind}

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
# See .spec-workflow/specs/knative-agents-fullstack-e2e/.

.PHONY: e2e-l0
e2e-l0:
	$(GO) test -tags=e2e_l0 -timeout 10m ./test/e2e/fullstack/l0/...

.PHONY: e2e-l1
e2e-l1:
	$(GO) test -tags=e2e_l1 -timeout 15m ./test/e2e/fullstack/l1/...

.PHONY: e2e-l2
e2e-l2:
	@: $${AWS_PROFILE:?must be set, expected stigen}
	@: $${AWS_REGION:?must be set, expected us-east-2}
	@if [ "$(AWS_REGION)" != "us-east-2" ]; then \
		echo "AWS_REGION must be us-east-2; got $(AWS_REGION)" >&2; exit 1; \
	fi
	$(GO) test -tags=e2e_l2 -timeout 30m ./test/e2e/fullstack/l2/...

# e2e-clean-aws: manual escape hatch — terminate every L2 instance
# tagged knative-agents-e2e in us-east-2 stigen account. Used when
# the sweeper Lambda or budget alarm both failed (R-E2E-VRF-3).
.PHONY: e2e-clean-aws
e2e-clean-aws:
	@: $${AWS_PROFILE:?must be set, expected stigen}
	bash scripts/aws-l2/sweep.sh

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

.PHONY: docker
docker:
	@for cmd in $(DOCKER_IMAGES); do \
		docker build -f deploy/docker/$$cmd.Dockerfile -t knative-agents/$$cmd:dev .; \
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
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
