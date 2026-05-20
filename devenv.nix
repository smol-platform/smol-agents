{ pkgs, ... }:

{
  packages = with pkgs; [
    # Go toolchain
    go_1_24
    gopls
    delve
    golangci-lint
    gofumpt

    # eBPF toolchain
    clang
    llvm
    libbpf
    bpftools
    elfutils
    pkg-config

    # Kubernetes toolchain
    kubectl
    kubernetes-helm
    kustomize
    kind
    knative-client

    # SPIFFE/SPIRE
    # spire is brought in via container images for tests

    # Verification tooling
    # quint installed via npm in tasks below

    # Container tooling
    docker
    skopeo

    # Misc
    git
    jq
    yq-go
    just
    protobuf
    grpcurl
  ];

  languages.go.enable = true;
  languages.go.package = pkgs.go_1_24;

  languages.javascript.enable = true;
  languages.javascript.npm.install.enable = true;

  env.CGO_ENABLED = "1";
  env.GOFLAGS = "-mod=mod";

  enterShell = ''
    echo "smol-agents devenv"
    echo "  go:       $(go version)"
    echo "  clang:    $(clang --version | head -n1)"
    echo "  kubectl:  $(kubectl version --client --short 2>/dev/null || kubectl version --client)"
    if ! command -v quint >/dev/null 2>&1; then
      echo "  installing quint..."
      npm install -g @informalsystems/quint || true
    fi
  '';

  tasks = {
    "build:all" = {
      exec = "make build";
      description = "Build all binaries";
    };
    "test:all" = {
      exec = "make test";
      description = "Run unit + integration tests";
    };
    "verify:formal" = {
      exec = "make verify-formal";
      description = "Run Quint model checker";
    };
  };

  pre-commit.hooks = {
    gofmt.enable = true;
    govet.enable = true;
    golangci-lint.enable = true;
  };
}
