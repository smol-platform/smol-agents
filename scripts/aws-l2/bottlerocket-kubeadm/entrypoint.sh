#!/usr/bin/env bash
# Bottlerocket kubeadm bootstrap-container entrypoint.
#
# Runs the EKS-Anywhere "self-managed control plane on Bottlerocket"
# pattern (kubeadm + standalone-mode + apiclient hand-off):
#
#   1. Symlink /var/lib/kubeadm + /etc/kubernetes into the host's
#      writable /.bottlerocket/rootfs/var/lib/kubeadm so kubeadm
#      can write PKI + manifests. (Bottlerocket's /etc + /var/lib
#      are read-only inside the bootstrap-container.)
#   2. Generate /tmp/kubeadm.yaml and run the kubeadm phases:
#      certs all → kubeconfig all → control-plane all → etcd local
#      → bootstrap-token.
#   3. For every static-pod manifest kubeadm wrote to
#      /etc/kubernetes/manifests/, base64-encode and push it via
#      `apiclient set kubernetes.static-pods.<name>.manifest=<b64>
#      kubernetes.static-pods.<name>.enabled=true`. The host's
#      static-pods service materialises them under
#      /etc/kubernetes/static-pods/ where kubelet's staticPodPath
#      points.
#   4. apiclient set the kubernetes.* cascade (api-server, CA cert,
#      bootstrap-token, authentication-mode=tls, standalone-mode=
#      false). This unblocks Bottlerocket's pluto + starts kubelet.
#   5. Wait for kubelet to register the node, then for the static
#      pods to come Ready.
#   6. Apply our manifests (cert-manager + spire + operator) from
#      the L2 manifest tarball + run the same health gate as the
#      other distros.
#   7. Touch /var/log/l2-bootstrap.READY on the host filesystem.
#   8. Disable ourself via `apiclient set host-containers.kubeadm-
#      bootstrap.enabled=false` and exit (so we don't re-run on
#      reboot).
#
# Pattern source: aws/eks-anywhere-build-tooling
#   projects/aws/bottlerocket-bootstrap/pkg/kubeadm/

set -eo pipefail

ROOT=/.bottlerocket/rootfs
HOST_LOG_DIR="$ROOT/var/log"
mkdir -p "$HOST_LOG_DIR" 2>/dev/null || true
log="$HOST_LOG_DIR/l2-bootstrap.log"
: > "$log" 2>/dev/null || log=/tmp/l2-bootstrap.log

fail() {
  echo "BOOTSTRAP FAILED: $*" | tee -a "$log" >&2
  touch "$HOST_LOG_DIR/l2-bootstrap.FAILED" 2>/dev/null || \
    touch /tmp/l2-bootstrap.FAILED
  if [ -n "${ARTIFACT_BUCKET:-}" ] && [ -n "${RUN_ID:-}" ]; then
    tar -czf /tmp/bootstrap-fail.tgz "$log" 2>/dev/null || true
    aws s3 cp /tmp/bootstrap-fail.tgz \
      "s3://${ARTIFACT_BUCKET}/bootstrap-fail/${RUN_ID}.tgz" \
      --region us-east-2 2>/dev/null || true
  fi
  sleep 600  # keep alive for SSM-based debug
}
exec >>"$log" 2>&1
echo "=== L2 bootstrap on bottlerocket-kubeadm $(date -u) ==="

# --- 0. user-data ---------------------------------------------------
# Bottlerocket delivers it base64-decoded at one of these paths.
for c in \
  /.bottlerocket/bootstrap-containers/current/user-data \
  /.bottlerocket/bootstrap-containers/kubeadm/user-data \
  /.bottlerocket/host-containers/current/user-data \
  /.bottlerocket/host-containers/kubeadm/user-data; do
  if [ -f "$c" ]; then
    eval "$(cat "$c")"
    echo "loaded user-data from $c"
    break
  fi
done
: "${ARTIFACT_BUCKET:?must be set via user-data}"
: "${ECR_REGISTRY:?}"
: "${IMAGE_TAG:?}"
: "${RUN_ID:?}"

# Wait for IMDS-backed credentials.
for i in $(seq 1 10); do
  aws sts get-caller-identity --region us-east-2 >/dev/null 2>&1 && break
  echo "waiting for IMDS ($i/10)"; sleep 6
done

# --- 1. Symlink kubeadm directories to host writable space ---------
mkdir -p "$ROOT/var/lib/kubeadm/pki" "$ROOT/var/lib/kubeadm/manifests"
ln -sfn "$ROOT/var/lib/kubeadm" /var/lib/kubeadm
ln -sfn "$ROOT/var/lib/kubeadm" /etc/kubernetes
mkdir -p /etc/kubernetes/manifests

# Get the node's private IP for the apiserver advertise address.
TOKEN=$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
PRIV_IP=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/local-ipv4)
[ -n "$PRIV_IP" ] || fail "no private IP from IMDS"
HOSTNAME=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/local-hostname)

# --- 2. kubeadm config + phases -------------------------------------
cat >/tmp/kubeadm.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta3
kind: InitConfiguration
nodeRegistration:
  name: ${HOSTNAME}
  criSocket: unix:///run/containerd/containerd.sock
  kubeletExtraArgs:
    cloud-provider: ""
localAPIEndpoint:
  advertiseAddress: ${PRIV_IP}
  bindPort: 6443
---
apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
kubernetesVersion: v1.31.5
controlPlaneEndpoint: ${PRIV_IP}:6443
networking:
  serviceSubnet: 10.96.0.0/16
  podSubnet: 10.244.0.0/16
  dnsDomain: cluster.local
apiServer:
  certSANs:
    - ${PRIV_IP}
    - ${HOSTNAME}
    - 127.0.0.1
    - localhost
controllerManager: {}
scheduler: {}
etcd:
  local:
    dataDir: /.bottlerocket/rootfs/var/lib/kubeadm/etcd
EOF

KUBEADM=/opt/bin/kubeadm
$KUBEADM init phase certs all          --config /tmp/kubeadm.yaml || fail "certs"
$KUBEADM init phase kubeconfig all     --config /tmp/kubeadm.yaml || fail "kubeconfig"
$KUBEADM init phase control-plane all  --config /tmp/kubeadm.yaml || fail "control-plane"
$KUBEADM init phase etcd local         --config /tmp/kubeadm.yaml || fail "etcd"
$KUBEADM init phase bootstrap-token    --config /tmp/kubeadm.yaml --skip-token-print 2>&1 || fail "bootstrap-token"

# --- 3. Stage static pods via apiclient -----------------------------
APICLIENT=/.bottlerocket/rootfs/usr/local/bin/apiclient
[ -x "$APICLIENT" ] || APICLIENT=apiclient
for m in /etc/kubernetes/manifests/*.yaml; do
  name=$(basename "$m" .yaml)
  b64=$(base64 -w0 "$m")
  "$APICLIENT" set \
    "kubernetes.static-pods.${name}.manifest=${b64}" \
    "kubernetes.static-pods.${name}.enabled=true" \
    || fail "apiclient set static-pod $name"
  echo "staged static pod $name"
done

# --- 4. apiclient cascade — wires kubelet to the new control plane --
APISERVER="https://${PRIV_IP}:6443"
B64_CA=$(base64 -w0 /etc/kubernetes/pki/ca.crt)
TOKEN=$($KUBEADM token list -o jsonpath='{.token}' 2>/dev/null \
  || $KUBEADM token create --description=l2-smoke 2>/dev/null)
[ -n "$TOKEN" ] || fail "no bootstrap token"

"$APICLIENT" set \
  "kubernetes.api-server=${APISERVER}" \
  "kubernetes.cluster-certificate=${B64_CA}" \
  "kubernetes.bootstrap-token=${TOKEN}" \
  "kubernetes.authentication-mode=tls" \
  "kubernetes.standalone-mode=false" \
  || fail "apiclient kubernetes.* cascade"

# --- 5. Wait for kubelet + apiserver ready --------------------------
echo "waiting for apiserver readiness..."
for i in $(seq 1 60); do
  curl -fsk "${APISERVER}/healthz" >/dev/null 2>&1 && break
  sleep 5
done
curl -fsk "${APISERVER}/healthz" >/dev/null 2>&1 || fail "apiserver healthz never returned 200"

# Register node + finalize init (uploads config, applies addons,
# creates kubelet bootstrap RBAC).
$KUBEADM init \
  --config /tmp/kubeadm.yaml \
  --skip-phases=preflight,kubelet-start,certs,kubeconfig,bootstrap-token,control-plane,etcd \
  --ignore-preflight-errors=all \
  || echo "warn: kubeadm init finalize had issues, continuing"

# --- 6. Apply our manifests + health gate ---------------------------
KCTL="/opt/bin/kubectl --kubeconfig=/etc/kubernetes/admin.conf"

mkdir -p /tmp/l2-manifests
aws s3 cp "s3://${ARTIFACT_BUCKET}/manifests-${IMAGE_TAG}.tar.gz" \
  /tmp/manifests.tar.gz --region us-east-2 || fail "manifest s3 cp"
tar -xzf /tmp/manifests.tar.gz -C /tmp/l2-manifests/ || fail "manifest extract"
find /tmp/l2-manifests -name '._*' -delete 2>/dev/null || true

curl -sSL \
  https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml \
  -o /tmp/cert-manager.yaml || fail "cert-manager download"

$KCTL apply -f /tmp/cert-manager.yaml || fail "apply cert-manager"
for d in spire operator samples; do
  for f in /tmp/l2-manifests/$d/*.yaml; do
    [ -f "$f" ] && $KCTL apply -f "$f" 2>&1 | tail -5
  done
done

wait_resource() {
  local kind=$1 ns=$2 sel=$3 timeout=${4:-240}
  local end=$(( $(date +%s) + timeout ))
  while [ $(date +%s) -lt $end ]; do
    n=$($KCTL -n "$ns" get "$kind" $sel -o name 2>/dev/null | wc -l)
    [ "$n" -gt 0 ] && return 0
    sleep 3
  done
  return 1
}
wait_resource deployment cert-manager "" 300 || fail "cert-manager not appearing"
$KCTL wait --for=condition=Available --timeout=240s \
  deployment -n cert-manager --all || fail "cert-manager deploys"
wait_resource pod spire-system "-l app=spire-server" 240 || fail "spire-server not appearing"
$KCTL wait --for=condition=Ready --timeout=240s \
  pod -n spire-system -l app=spire-server || fail "spire-server pod"
wait_resource pod spire-system "-l app=spire-agent" 240 || fail "spire-agent not appearing"
$KCTL wait --for=condition=Ready --timeout=240s \
  pod -n spire-system -l app=spire-agent || fail "spire-agent pods"
wait_resource deployment smol-agents-system "" 240 || fail "operator not appearing"
$KCTL wait --for=condition=Available --timeout=240s \
  deployment -n smol-agents-system --all || fail "operator deploy"
$KCTL wait --for=condition=Established --timeout=60s \
  crd/smolagents.agents.smol-agents.ai \
  crd/agentnetworks.runtime.agents.smol-agents.ai \
  crd/agentruns.runtime.agents.smol-agents.ai || fail "CRDs established"

# --- 7. Sentinel + 8. Disable self ---------------------------------
touch "$HOST_LOG_DIR/l2-bootstrap.READY"
echo "=== bootstrap complete; disabling self + exiting ==="
"$APICLIENT" set "host-containers.kubeadm.enabled=false" 2>/dev/null \
  || "$APICLIENT" set "bootstrap-containers.kubeadm.enabled=false" 2>/dev/null \
  || true
exit 0
