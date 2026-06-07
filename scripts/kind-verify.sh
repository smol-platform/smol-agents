#!/usr/bin/env bash
# kind-verify.sh — bring up a kind cluster, build/load the operator
# image, deploy via the kind overlay, apply sample CRs, and confirm
# reconcilers reach Ready.
#
# Idempotent: re-running reuses the cluster + image cache.
set -euo pipefail

CLUSTER=${CLUSTER:-smol-agents-kind}
IMAGE=${IMAGE:-smol-agents/operator:0.1.0}
OVERLAY=${OVERLAY:-operator/config/kind}
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

cd "$ROOT"

step() { printf "\n=== %s ===\n" "$*"; }

step "ensure kind cluster $CLUSTER"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 60s
else
  echo "cluster already exists"
fi

step "build operator image $IMAGE"
docker build -f deploy/docker/operator.Dockerfile -t "$IMAGE" .

step "load image into kind"
kind load docker-image "$IMAGE" --name "$CLUSTER"

step "apply manifests via $OVERLAY"
kubectl --context "kind-$CLUSTER" apply -k "$OVERLAY"

step "wait for deployment Available"
kubectl --context "kind-$CLUSTER" -n smol-agents-system rollout status \
  deployment/smol-agents-operator --timeout=120s

KCTL="kubectl --context kind-$CLUSTER"

step "apply tenant namespace + AgentNetwork samples"
$KCTL create namespace tenant-a --dry-run=client -o yaml | $KCTL apply -f -
$KCTL apply -f operator/config/samples/agentnetwork_proxy.yaml
$KCTL apply -f operator/config/samples/agentnetwork_wg_client.yaml

step "apply runtime CR chain (Provider → Tool → Agent → AgentRun)"
# BuildAgentRunPod sets ServiceAccountName to "<agent>-agent". On a real
# cluster the SmolAgent reconciler creates that SA via the identity
# feature. For the kind-only chain check we pre-create it so the
# AgentRun Pod admits.
$KCTL -n tenant-a create serviceaccount researcher-agent \
  --dry-run=client -o yaml | $KCTL apply -f -
# The openai-prod ModelProvider in agent_full.yaml references secret "openai-key";
# the broker reads it during run prep (else the run stays Pending/RunPrepPending
# and never schedules a pod). kind has no real provider key, but the chain-check
# only verifies the pod is CREATED (not that the LLM call succeeds), so a dummy
# value suffices for the operator-path e2e.
$KCTL -n tenant-a create secret generic openai-key \
  --from-literal=api-key=dummy-kind-e2e --dry-run=client -o yaml | $KCTL apply -f -
# The "search" Tool in agent_full.yaml is kind=http with auth.secretName "tavily-key".
# Loop-mode run prep (gatherRunSecrets, M2 tool-auth-lease) resolves every referenced
# tool's auth secret fail-closed so the in-pod invoker can lease it — a missing
# secret holds the run Pending/RunPrepPending and no pod is ever created. Same as
# openai-key: a dummy single-key value is enough for the pod-CREATED chain check.
$KCTL -n tenant-a create secret generic tavily-key \
  --from-literal=token=dummy-kind-e2e --dry-run=client -o yaml | $KCTL apply -f -
$KCTL apply -f operator/config/samples/agent_full.yaml

step "apply control-plane samples (Platform + SmolAgent)"
$KCTL apply -f operator/config/samples/smolagentplatform.yaml
$KCTL apply -f operator/config/samples/smolagent_minimal.yaml

step "wait for reconcilers to populate status"
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  proxy_phase=$($KCTL  -n tenant-a get agentnetwork prod-net    -o jsonpath='{.status.phase}' 2>/dev/null || true)
  vpn_phase=$(  $KCTL  -n tenant-a get agentnetwork corp-vpn    -o jsonpath='{.status.phase}' 2>/dev/null || true)
  agent_phase=$($KCTL  -n tenant-a get agent       researcher   -o jsonpath='{.status.phase}' 2>/dev/null || true)
  run_state=$(  $KCTL  -n tenant-a get agentrun    researcher-001 -o jsonpath='{.status.state}' 2>/dev/null || true)
  pod_exists=$( $KCTL  -n tenant-a get pod         researcher-001 --ignore-not-found -o name 2>/dev/null || true)
  echo "agentnetwork[prod-net=$proxy_phase corp-vpn=$vpn_phase] agent[researcher=$agent_phase] run[researcher-001=$run_state pod=${pod_exists:-no}]"
  if [[ "$proxy_phase" == "Ready" \
     && ( "$vpn_phase" == "Pending" || "$vpn_phase" == "Ready" ) \
     && "$agent_phase" == "Ready" \
     && -n "$pod_exists" ]]; then
    break
  fi
  sleep 3
done

step "final state"
$KCTL -n tenant-a get agentnetworks,agents,agentruns,modelproviders,tools -o wide
echo "--- run pod ---"
$KCTL -n tenant-a get pod researcher-001 -o wide --ignore-not-found
echo "--- operator logs (last 30) ---"
$KCTL -n smol-agents-system logs deploy/smol-agents-operator --tail=30

# Success criteria — fail loudly with the observed value if any check fails.
require() { local what="$1" got="$2" want="$3"; [[ "$got" == "$want" ]] || \
  { echo "FAIL: $what=$got (want $want)" >&2; exit 1; }; }
inset()   { local what="$1" got="$2" set="$3"; [[ " $set " == *" $got "* ]] || \
  { echo "FAIL: $what=$got (want one of: $set)" >&2; exit 1; }; }

proxy_phase=$($KCTL -n tenant-a get agentnetwork prod-net    -o jsonpath='{.status.phase}')
vpn_phase=$(  $KCTL -n tenant-a get agentnetwork corp-vpn    -o jsonpath='{.status.phase}')
agent_phase=$($KCTL -n tenant-a get agent       researcher   -o jsonpath='{.status.phase}')
pod_exists=$( $KCTL -n tenant-a get pod         researcher-001 --ignore-not-found -o name)

require "agentnetwork prod-net.phase"  "$proxy_phase" "Ready"
inset   "agentnetwork corp-vpn.phase"  "$vpn_phase"   "Pending Ready"
require "agent researcher.phase"        "$agent_phase" "Ready"
[[ -n "$pod_exists" ]] || { echo "FAIL: agentrun researcher-001 pod missing" >&2; exit 1; }

echo "SUCCESS: kind verification passed (AgentNetwork + Agent + AgentRun chain)"
