#!/usr/bin/env bash
# quickstart.sh — the 5-minute demo: kind cluster + operator + a keyless demo
# agent (deterministic fake-llm backend), finishing with a real AgentRun driven
# by `agentctl run -follow` that prints the folded output.
#
#   make quickstart            # everything below
#   make quickstart-down       # delete the cluster
#
# Needs: docker, kind, kubectl, go. No API keys — the demo agent talks to the
# in-cluster fake-llm, so the full operator datapath (broker, run pod, status
# fold) runs for real with a deterministic answer.
#
# Idempotent: re-running reuses the cluster and rebuilds/reloads images.
set -euo pipefail

CLUSTER=${CLUSTER:-smol-agents-quickstart}
IMAGE=${IMAGE:-smol-agents/operator:quickstart}
FAKELLM_IMAGE=${FAKELLM_IMAGE:-smol-agents/fake-llm:quickstart}
NS=demo
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

step() { printf "\n=== %s ===\n" "$*"; }

for bin in docker kind kubectl go; do
  command -v "$bin" >/dev/null || { echo "quickstart: $bin is required" >&2; exit 1; }
done

step "ensure kind cluster $CLUSTER"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 60s
else
  echo "cluster already exists"
fi
KCTL="kubectl --context kind-$CLUSTER"

step "build + load operator and fake-llm images"
docker build -f deploy/docker/operator.Dockerfile -t "$IMAGE" .
docker build -f deploy/docker/fake-llm.Dockerfile -t "$FAKELLM_IMAGE" .
kind load docker-image "$IMAGE" "$FAKELLM_IMAGE" --name "$CLUSTER"

step "install CRDs + operator (kind overlay: webhooks off, runc dev posture)"
kubectl --context "kind-$CLUSTER" apply -k operator/config/kind
$KCTL -n smol-agents-system set image deployment/smol-agents-operator "*=$IMAGE"
$KCTL -n smol-agents-system patch deployment smol-agents-operator --type=json \
  -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
$KCTL -n smol-agents-system rollout status deployment/smol-agents-operator --timeout=180s

step "deploy the keyless demo backend (fake-llm) into namespace $NS"
$KCTL create namespace "$NS" --dry-run=client -o yaml | $KCTL apply -f -
sed -e "s/namespace: tenant-a/namespace: $NS/" \
    -e "s/tenant-a.svc/$NS.svc/" \
    -e "s|image: smol-agents/fake-llm:dev|image: $FAKELLM_IMAGE|" \
    test/e2e/manifests/fake-llm.yaml | $KCTL apply -f -
$KCTL -n "$NS" rollout status deployment/fake-llm --timeout=120s

step "create the demo ModelProvider + Agent"
# The broker resolves the provider's source Secret during run prep even though
# fake-llm ignores the key — a single-key dummy satisfies it.
$KCTL -n "$NS" create secret generic demo-llm-key \
  --from-literal=api-key=quickstart-demo --dry-run=client -o yaml | $KCTL apply -f -
# Tenant boundary (5vr): the operator refuses to read a CR-referenced Secret
# without this label.
$KCTL -n "$NS" label secret demo-llm-key agents.smol-agents.ai/tenant-secret=true --overwrite
cat <<EOF | $KCTL apply -f -
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: { name: demo-llm, namespace: $NS }
spec:
  kind: openai
  endpoint: http://fake-llm.$NS.svc.cluster.local:8080
  secretRef: { secretName: demo-llm-key }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: demo-agent, namespace: $NS }
spec:
  model: { providerRef: demo-llm, name: demo-model }
  instructions: "You are the smol-agents quickstart demo agent."
  budget: { maxSteps: 5, maxTokens: 10000, maxWallClockSeconds: 120, maxToolCalls: 5 }
EOF

step "wait for the Agent to be Ready"
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  phase=$($KCTL -n "$NS" get agent demo-agent -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$phase" == "Ready" ]] && break
  sleep 2
done
[[ "${phase:-}" == "Ready" ]] || { echo "FAIL: agent demo-agent phase=${phase:-<none>} (want Ready)" >&2; exit 1; }

step "run it: agentctl run demo-agent -p \"hello\" -follow"
go build -o bin/agentctl ./cmd/agentctl
bin/agentctl run demo-agent -n "$NS" -p "hello from the quickstart" \
  -follow -timeout 3m -context "kind-$CLUSTER"

step "SUCCESS"
echo "The full chain is live: ModelProvider → Agent → AgentRun → run pod → folded output."
echo "Try:  bin/agentctl run demo-agent -n $NS -p \"another prompt\" -follow -context kind-$CLUSTER"
echo "Tear down with:  make quickstart-down"
