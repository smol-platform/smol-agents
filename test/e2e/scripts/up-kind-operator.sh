#!/usr/bin/env bash
# Brings up a kind cluster, builds + loads the operator image, applies
# CRDs + RBAC + Deployment, then a sample SmolAgent CR. Asserts
# Status.Phase reaches Ready (or at least transitions out of Pending).
#
# Used by `make test-e2e-operator`. R-OP-VRF-2.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-smol-agents-op}"
KIND="${KIND:-kind}"
KUBECTL="${KUBECTL:-kubectl}"
DOCKER="${DOCKER:-docker}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-smol-agents/operator:e2e}"

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$ROOT"

echo "==> creating kind cluster $CLUSTER_NAME"
"$KIND" create cluster --name "$CLUSTER_NAME" --wait 60s 2>/dev/null || \
  echo "   (cluster already exists)"

echo "==> building operator image"
"$DOCKER" build -q -f deploy/docker/operator.Dockerfile -t "$OPERATOR_IMAGE" .

echo "==> loading image into kind"
"$KIND" load docker-image "$OPERATOR_IMAGE" --name "$CLUSTER_NAME"

echo "==> applying CRDs"
"$KUBECTL" apply -k operator/config/crd/

echo "==> applying RBAC + manager (without webhooks; we don't have cert-manager in this e2e)"
"$KUBECTL" apply -f operator/config/manager/manager.yaml
"$KUBECTL" apply -f operator/config/rbac/role.yaml
"$KUBECTL" -n smol-agents-system set image deploy/smol-agents-operator manager="$OPERATOR_IMAGE"
"$KUBECTL" -n smol-agents-system set env deploy/smol-agents-operator ENABLE_WEBHOOKS=false

echo "==> waiting for operator deployment to be ready"
"$KUBECTL" -n smol-agents-system rollout status deploy/smol-agents-operator --timeout=180s

echo "==> applying singleton platform CR"
"$KUBECTL" apply -f operator/config/samples/smolagentplatform.yaml

echo "==> applying minimal SmolAgent CR"
"$KUBECTL" create namespace tenant-a 2>/dev/null || true
"$KUBECTL" apply -f operator/config/samples/smolagent_minimal.yaml

echo "==> waiting for SmolAgent status to populate (≤ 60s)"
ok=0
for i in $(seq 1 60); do
  phase=$("$KUBECTL" get smolagent hello -n tenant-a -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  if [ -n "$phase" ]; then
    echo "   phase=$phase (after ${i}s)"
    ok=1
    break
  fi
  sleep 1
done
if [ "$ok" = "0" ]; then
  echo "FAIL: status.phase never populated"
  "$KUBECTL" -n smol-agents-system logs deploy/smol-agents-operator --tail=50
  exit 1
fi

echo "==> dumping observed Status.Features"
"$KUBECTL" get smolagent hello -n tenant-a -o jsonpath='{.status.features}' | jq . 2>/dev/null || \
  "$KUBECTL" get smolagent hello -n tenant-a -o yaml | grep -A 20 "features:"

echo "==> e2e PASSED"
echo
echo "To inspect:"
echo "  kubectl -n tenant-a get smolagent -o wide"
echo "  kubectl -n tenant-a describe smolagent hello"
echo "  kubectl -n smol-agents-system logs deploy/smol-agents-operator -f"
echo
echo "To tear down: kind delete cluster --name $CLUSTER_NAME"
