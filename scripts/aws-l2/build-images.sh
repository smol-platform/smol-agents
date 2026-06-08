#!/usr/bin/env bash
# build-images.sh — build all smol-agents images multiarch
# (linux/amd64 + linux/arm64) and push them to ECR under the given tag.
#
# Usage:
#   L2_ECR_REGISTRY=123.dkr.ecr.us-east-2.amazonaws.com \
#   AWS_PROFILE=smol-agents \
#     scripts/aws-l2/build-images.sh <tag>
set -euo pipefail

TAG=${1:?usage: $0 <tag>}
PROFILE=${AWS_PROFILE:?AWS_PROFILE required}
REGION=${AWS_REGION:-us-east-2}
REGISTRY=${L2_ECR_REGISTRY:?L2_ECR_REGISTRY required}
# L2 metal is arm64 (Graviton), so an arm64-only build is enough for the ring and
# avoids slow amd64 emulation on arm64 build hosts. Defaults to multiarch so the
# published-image invariant (amd64+arm64) still holds for non-L2 callers.
PLATFORMS=${L2_PLATFORMS:-linux/amd64,linux/arm64}

if [[ "$REGION" != "us-east-2" ]]; then
  echo "REGION must be us-east-2 (got $REGION)" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

step() { printf "\n=== %s ===\n" "$*"; }

step "ECR login"
aws --profile "$PROFILE" --region "$REGION" ecr get-login-password \
  | docker login --username AWS --password-stdin "$REGISTRY"

# Compile CO-RE BPF objects FIRST. ebpf-loader.Dockerfile copies
# bpf/build/*.bpf.o into the image; without this step those files
# are 0-byte placeholders and the loader attaches nothing.
step "compile bpf/build/*.bpf.o (CO-RE; bpfel objects are arch-portable)"
mkdir -p bpf/build
docker buildx build \
  --platform linux/arm64 \
  --file deploy/docker/bpf-builder.Dockerfile \
  --target export \
  --output type=local,dest=. \
  .
ls -la bpf/build/*.bpf.o

# Each entry: image-name dockerfile build-context
images=(
  "operator     deploy/docker/operator.Dockerfile        ."
  "agent        deploy/docker/agent.Dockerfile           ."
  "ebpf-loader  deploy/docker/ebpf-loader.Dockerfile     ."
  "secret-proxy deploy/docker/secret-proxy.Dockerfile    ."
  "agentfs-sidecar deploy/docker/agentfs-sidecar.Dockerfile ."
  "fake-llm     deploy/docker/fake-llm.Dockerfile        ."
  "fake-gateway deploy/docker/fake-gateway.Dockerfile    ."
  "fake-github  deploy/docker/fake-github.Dockerfile     ."
  "fake-tts      deploy/docker/fake-tts.Dockerfile         ."
  "memory-mcp    deploy/docker/memory-mcp.Dockerfile       ."
  "memory-worker deploy/docker/memory-worker.Dockerfile    ."
  "spire-shell   scripts/e2e/spire/Dockerfile.spire-shell  scripts/e2e/spire"
  "spiffe-probe deploy/docker/spiffe-probe.Dockerfile    ."
  "ebpf-probe   deploy/docker/ebpf-probe.Dockerfile      ."
  "bottlerocket-bootstrap scripts/aws-l2/bottlerocket-bootstrap/Dockerfile scripts/aws-l2/bottlerocket-bootstrap"
  "harness-claude-code deploy/docker/harness-claude-code.Dockerfile ."
  "harness-codex deploy/docker/harness-codex.Dockerfile ."
)

for entry in "${images[@]}"; do
  read -r name dockerfile ctx <<<"$entry"
  full=$REGISTRY/smol-agents/$name:$TAG

  step "build + push $name → $full ($PLATFORMS)"
  # buildx injects TARGETOS/TARGETARCH per platform, so each Go
  # Dockerfile cross-compiles correctly; the manifest pushed under the
  # tag covers $PLATFORMS.
  docker buildx build \
    --platform "$PLATFORMS" \
    --file "$dockerfile" \
    --tag  "$full" \
    --push \
    "$ctx"
done

echo
echo "DONE. Cloud-init will pull:"
for entry in "${images[@]}"; do
  read -r name _ _ <<<"$entry"
  echo "  $REGISTRY/smol-agents/$name:$TAG"
done
