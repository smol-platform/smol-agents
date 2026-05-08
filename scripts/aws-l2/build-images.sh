#!/usr/bin/env bash
# build-images.sh — build all knative-agents images for linux/arm64
# and push them to ECR under the given tag.
#
# Usage:
#   L2_ECR_REGISTRY=123.dkr.ecr.us-east-2.amazonaws.com \
#   AWS_PROFILE=stigen \
#     scripts/aws-l2/build-images.sh <tag>
set -euo pipefail

TAG=${1:?usage: $0 <tag>}
PROFILE=${AWS_PROFILE:?AWS_PROFILE required}
REGION=${AWS_REGION:-us-east-2}
REGISTRY=${L2_ECR_REGISTRY:?L2_ECR_REGISTRY required}

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

# Each entry: image-name dockerfile build-context
images=(
  "operator     deploy/docker/operator.Dockerfile     ."
  "agent        deploy/docker/agent.Dockerfile        ."
  "ebpf-loader  deploy/docker/ebpf-loader.Dockerfile  ."
  "secret-proxy deploy/docker/secret-proxy.Dockerfile ."
  "fake-llm     deploy/docker/fake-llm.Dockerfile     ."
  "fake-gateway deploy/docker/fake-gateway.Dockerfile ."
)

for entry in "${images[@]}"; do
  read -r name dockerfile ctx <<<"$entry"
  full=$REGISTRY/knative-agents/$name:$TAG

  step "build + push $name → $full"
  docker buildx build \
    --platform linux/arm64 \
    --file "$dockerfile" \
    --tag  "$full" \
    --push \
    "$ctx"
done

echo
echo "DONE. Cloud-init will pull:"
for entry in "${images[@]}"; do
  read -r name _ _ <<<"$entry"
  echo "  $REGISTRY/knative-agents/$name:$TAG"
done
