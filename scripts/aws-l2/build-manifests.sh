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

# 1. SPIRE (server + agent + RBAC + bootstrap sidecar). The
#    sidecar registers workload entries via spire-server's UDS;
#    without it, scenarios that probe via spiffe-probe pods fail
#    with "setup x509 source: context deadline exceeded" because
#    no workload entries match their SPIFFE-ID parent. Keep the
#    sidecar at L2 too — ECR auth via containerd hosts.toml means
#    kubelet can pull the spire-shell image without racing.
kustomize build "$ROOT/test/e2e/manifests/spire" > "$WORK/spire/00-spire.yaml"

# Rewrite the spire-shell image reference (kind uses
# `knative-agents/spire-shell:dev` with imagePullPolicy: Never;
# L2 pulls from ECR with IfNotPresent).
if [[ -n "${L2_ECR_REGISTRY:-}" ]]; then
  sed -i.bak \
    -e "s|knative-agents/spire-shell:[A-Za-z0-9._-]*|${L2_ECR_REGISTRY}/knative-agents/spire-shell:${TAG}|g" \
    -e "s|imagePullPolicy: Never|imagePullPolicy: IfNotPresent|g" \
    "$WORK/spire/00-spire.yaml"
  rm "$WORK/spire/00-spire.yaml.bak"
fi

# 2. Operator (CRDs + RBAC + manager + webhook).
#    Use the kind-webhook overlay so the L2 cluster gets the full
#    webhook surface — same fidelity as the L1 ring.
kustomize build "$ROOT/operator/config/kind-webhook" > "$WORK/operator/00-operator.yaml"

# Rewrite the operator image reference: kind uses
# `knative-agents/operator:0.1.0` (loaded via `kind load`); L2 pulls
# from ECR. Empty L2_ECR_REGISTRY leaves the dev tag intact.
if [[ -n "${L2_ECR_REGISTRY:-}" ]]; then
  sed -i.bak \
    -e "s|knative-agents/operator:[A-Za-z0-9._-]*|${L2_ECR_REGISTRY}/knative-agents/operator:${TAG}|g" \
    "$WORK/operator/00-operator.yaml"
  rm "$WORK/operator/00-operator.yaml.bak"
fi

# 3. Tenant namespace + fake services + researcher-agent SA. L1's
#    deployFakes() handles this via separate kubectl commands; we
#    pre-stage all of it into the manifest tarball so the k0s
#    manifest watcher applies it at boot.
mkdir -p "$WORK/tenant"
cat >"$WORK/tenant/00-namespace.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: tenant-a
  labels:
    knative-agents.stigen.ai/tenant: a
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: researcher-agent
  namespace: tenant-a
YAML

# Fake services (kustomize-render with the same namespace transform
# kind uses, then rewrite the image references to ECR URLs).
kustomize build "$ROOT/test/e2e/manifests" > "$WORK/tenant/10-fakes.yaml"
if [[ -n "${L2_ECR_REGISTRY:-}" ]]; then
  sed -i.bak \
    -e "s|knative-agents/fake-llm:[A-Za-z0-9._-]*|${L2_ECR_REGISTRY}/knative-agents/fake-llm:${TAG}|g" \
    -e "s|knative-agents/fake-gateway:[A-Za-z0-9._-]*|${L2_ECR_REGISTRY}/knative-agents/fake-gateway:${TAG}|g" \
    -e "s|knative-agents/spiffe-probe:[A-Za-z0-9._-]*|${L2_ECR_REGISTRY}/knative-agents/spiffe-probe:${TAG}|g" \
    -e "s|imagePullPolicy: Never|imagePullPolicy: IfNotPresent|g" \
    "$WORK/tenant/10-fakes.yaml"
  rm "$WORK/tenant/10-fakes.yaml.bak"
fi

# 4. Sample CRs (Platform + KnativeAgent + ModelProvider + Tool +
#    Agent + AgentRun + AgentNetwork). The Platform CR must apply
#    BEFORE any KnativeAgent CR, so we prefix-order them. Tenant
#    namespace must exist first (it's in tenant/ which the manifest
#    watcher applies alphabetically before samples/).
mkdir -p "$WORK/samples"
cp "$ROOT/operator/config/samples/knativeagentplatform.yaml"   "$WORK/samples/00-platform.yaml"
cp "$ROOT/operator/config/samples/knativeagent_minimal.yaml"   "$WORK/samples/10-knativeagent.yaml"
cp "$ROOT/operator/config/samples/agent_full.yaml"             "$WORK/samples/20-agent-chain.yaml"
cp "$ROOT/operator/config/samples/agentnetwork_proxy.yaml"     "$WORK/samples/30-agentnetwork-proxy.yaml"
cp "$ROOT/operator/config/samples/agentnetwork_wg_client.yaml" "$WORK/samples/31-agentnetwork-wg.yaml"

step "package as tarball"
TARBALL=$WORK/manifests-$TAG.tar.gz
# COPYFILE_DISABLE=1 + --no-xattrs strips macOS AppleDouble (._*)
# files; if any leak in, k0s manifest watcher rejects the whole
# directory ("error converting YAML to JSON: yaml: control
# characters are not allowed").
COPYFILE_DISABLE=1 tar --no-xattrs --exclude='._*' \
  -czf "$TARBALL" -C "$WORK" spire operator tenant samples
ls -lh "$TARBALL"

step "upload to s3://$BUCKET/manifests-$TAG.tar.gz"
aws --profile "$PROFILE" --region "$REGION" \
    s3 cp "$TARBALL" "s3://$BUCKET/manifests-$TAG.tar.gz"

echo
echo "DONE. Cloud-init will fetch:"
echo "  s3://$BUCKET/manifests-$TAG.tar.gz"
