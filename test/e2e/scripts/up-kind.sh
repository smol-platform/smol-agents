#!/usr/bin/env bash
# Bring up a kind cluster with Knative Serving and SPIRE.
# This is the canonical "verifiable" e2e harness referenced from the
# Makefile target `make test-e2e`.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-knative-agents}"
KIND="${KIND:-kind}"
HELM="${HELM:-helm}"
KUBECTL="${KUBECTL:-kubectl}"

echo "==> creating kind cluster $CLUSTER_NAME"
"$KIND" create cluster --name "$CLUSTER_NAME" --wait 60s || true

echo "==> installing Knative Serving CRDs"
"$KUBECTL" apply -f https://github.com/knative/serving/releases/download/knative-v1.15.0/serving-crds.yaml
"$KUBECTL" apply -f https://github.com/knative/serving/releases/download/knative-v1.15.0/serving-core.yaml
"$KUBECTL" apply -f https://github.com/knative/net-kourier/releases/download/knative-v1.15.0/kourier.yaml
"$KUBECTL" patch configmap/config-network -n knative-serving --type merge \
  -p '{"data":{"ingress-class":"kourier.ingress.networking.knative.dev"}}'

echo "==> installing SPIRE (CRDs + agent + server)"
"$HELM" repo add spiffe https://spiffe.github.io/helm-charts-hardened || true
"$HELM" repo update
"$HELM" upgrade --install spire-crds spiffe/spire-crds --namespace spire-server --create-namespace
"$HELM" upgrade --install spire spiffe/spire --namespace spire-server \
  --set global.spire.trustDomain=stigen.ai

echo "==> installing knative-agents chart"
"$HELM" upgrade --install agents deploy/helm --namespace knative-agents --create-namespace

echo "==> waiting for Knative Service to be Ready"
"$KUBECTL" wait --for=condition=Ready ksvc/agents-knative-agents -n knative-agents --timeout=300s

echo "==> e2e harness ready"
