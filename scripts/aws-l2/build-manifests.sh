#!/usr/bin/env bash
# build-manifests.sh — kustomize-render the L2 manifest set and
# upload as a tarball to S3 for cloud-init to pull at instance-start.
#
# Usage:
#   L2_ARTIFACT_BUCKET=knative-agents-e2e-artifacts-us-east-2 \
#   AWS_PROFILE=stigen \
#     scripts/aws-l2/build-manifests.sh <tag>
#
# Outputs s3://${L2_ARTIFACT_BUCKET}/manifests-${tag}.tar.gz
set -euo pipefail

TAG=${1:?usage: $0 <tag>}
PROFILE=${AWS_PROFILE:?AWS_PROFILE required}
REGION=${AWS_REGION:-us-east-2}
BUCKET=${L2_ARTIFACT_BUCKET:?L2_ARTIFACT_BUCKET required}

if [[ "$REGION" != "us-east-2" ]]; then
  echo "REGION must be us-east-2 (got $REGION)" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK=$(mktemp -d -t knative-agents-l2-manifests.XXXXXX)
trap "rm -rf $WORK" EXIT

step() { printf "\n=== %s ===\n" "$*"; }

step "render manifests for tag=$TAG"

# Each subdir under $WORK becomes one piece k0s manifest watcher
# applies. We render kustomize bases into flat YAML files.
mkdir -p "$WORK/spire" "$WORK/operator" "$WORK/samples"

# 1. SPIRE (server + agent + RBAC + bootstrap sidecar).
kustomize build "$ROOT/test/e2e/manifests/spire" > "$WORK/spire/00-spire.yaml"

# 2. Operator (CRDs + RBAC + manager + webhook).
#    Use the kind-webhook overlay so the L2 cluster gets the full
#    webhook surface — same fidelity as the L1 ring.
kustomize build "$ROOT/operator/config/kind-webhook" > "$WORK/operator/00-operator.yaml"

# 3. Sample CRs (Platform + KnativeAgent + ModelProvider + Tool +
#    Agent + AgentRun + AgentNetwork). The Platform CR must apply
#    BEFORE any KnativeAgent CR, so we prefix-order them.
mkdir -p "$WORK/samples"
cp "$ROOT/operator/config/samples/knativeagentplatform.yaml"   "$WORK/samples/00-platform.yaml"
cp "$ROOT/operator/config/samples/knativeagent_minimal.yaml"   "$WORK/samples/10-knativeagent.yaml"
cp "$ROOT/operator/config/samples/agent_full.yaml"             "$WORK/samples/20-agent-chain.yaml"
cp "$ROOT/operator/config/samples/agentnetwork_proxy.yaml"     "$WORK/samples/30-agentnetwork-proxy.yaml"
cp "$ROOT/operator/config/samples/agentnetwork_wg_client.yaml" "$WORK/samples/31-agentnetwork-wg.yaml"

step "package as tarball"
TARBALL=$WORK/manifests-$TAG.tar.gz
tar -czf "$TARBALL" -C "$WORK" spire operator samples
ls -lh "$TARBALL"

step "upload to s3://$BUCKET/manifests-$TAG.tar.gz"
aws --profile "$PROFILE" --region "$REGION" \
    s3 cp "$TARBALL" "s3://$BUCKET/manifests-$TAG.tar.gz"

echo
echo "DONE. Cloud-init will fetch:"
echo "  s3://$BUCKET/manifests-$TAG.tar.gz"
