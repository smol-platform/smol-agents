#!/usr/bin/env bash
# Bottlerocket k0s host-container entrypoint.
#
# Runs as PID 1 (tini reaps zombies + forwards signals). The k0s
# controller is started as a child process; we wait on it so the
# host-container stays alive. Bottlerocket's host-containerd will
# Restart=always us if we exit, but that resets state — we want to
# stay up.
set -eo pipefail

# Bottlerocket exposes the host root (read-only) at /.bottlerocket/rootfs.
ROOT=/.bottlerocket/rootfs
HOST_LOG_DIR="$ROOT/var/log"
mkdir -p "$HOST_LOG_DIR" 2>/dev/null || true   # may already exist
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
  # Don't exit — keep container alive so kubelet/admin can probe.
  sleep infinity
}
exec >>"$log" 2>&1
echo "=== L2 bootstrap on bottlerocket host-container $(date -u) ==="

# user-data delivered as a file. Bottlerocket creates symlink
# .../current → .../<run-id> for us.
for candidate in \
  /.bottlerocket/host-containers/current/user-data \
  /.bottlerocket/host-containers/k0s/user-data \
  /local/host-containers/current/user-data \
  /local/host-containers/k0s/user-data; do
  if [ -f "$candidate" ]; then
    eval "$(cat "$candidate")"
    echo "loaded user-data from $candidate"
    break
  fi
done
: "${ARTIFACT_BUCKET:?must be set via host-container user-data}"
: "${ECR_REGISTRY:?must be set}"
: "${IMAGE_TAG:?must be set}"
: "${RUN_ID:?must be set}"

# Wait for IMDS-backed credentials.
for i in $(seq 1 10); do
  if aws sts get-caller-identity --region us-east-2 >/dev/null 2>&1; then
    break
  fi
  echo "waiting for IMDS credentials ($i/10)"
  sleep 6
done

# k0s data dir requires SELinux setxattr support to extract its
# bundled containerd-shim/runc binaries. Bottlerocket's host-
# container overlayfs and persistent mount both reject security.*
# xattrs (operation not supported), and the on-host containerd
# extraction step then fails 1 minute in. tmpfs supports security
# xattrs unconditionally, so mount one over /var/lib/k0s.
# For smoke purposes ephemeral state is fine — the instance is
# per-run anyway.
DATA_DIR=/var/lib/k0s
mkdir -p "$DATA_DIR" /etc/k0s
mount -t tmpfs -o size=4G,mode=0755 tmpfs "$DATA_DIR" \
  || echo "WARN: tmpfs mount failed; continuing on overlayfs"

# Bottlerocket's host-ctr mounts /sys/fs/cgroup as cgroup v1.
# K0s 1.33 tolerates v1 (with deprecation warning); 1.34+ refuses.
echo "=== cgroup state ==="
cat /proc/self/cgroup 2>&1 | head -3

# Bottlerocket host-containers' /dev is minimal; kubelet needs
# /dev/kmsg to read kernel messages. Create the device node
# (major 1 minor 11) — superpowered grants CAP_MKNOD.
[ -c /dev/kmsg ] || mknod -m 600 /dev/kmsg c 1 11 \
  || echo "WARN: mknod /dev/kmsg failed; kubelet will refuse to start"
cat >/etc/k0s/k0s.yaml <<'YAML'
apiVersion: k0s.k0sproject.io/v1beta1
kind: ClusterConfig
spec:
  network:
    provider: kuberouter
YAML

# k0s containerd ECR auth (config_path + hosts.toml) — same pattern
# as AL2023/Ubuntu/Flatcar.
mkdir -p /etc/k0s/containerd.d "/etc/containerd/certs.d/${ECR_REGISTRY}"
pw=$(aws ecr get-login-password --region us-east-2)
auth=$(printf 'AWS:%s' "$pw" | base64 -w0)
cat >/etc/k0s/containerd.d/registry-config-path.toml <<TOML
[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"
TOML
cat >"/etc/containerd/certs.d/${ECR_REGISTRY}/hosts.toml" <<TOML
server = "https://${ECR_REGISTRY}"
[host."https://${ECR_REGISTRY}"]
  capabilities = ["pull", "resolve"]
  [host."https://${ECR_REGISTRY}".header]
    authorization = "Basic ${auth}"
TOML

# Drop manifests into k0s manifest watcher dir BEFORE k0s starts so
# manifest-watcher applies them once api comes up.
MANIFEST_DIR="$DATA_DIR/manifests"
mkdir -p "$MANIFEST_DIR/cert-manager" "$MANIFEST_DIR/spire" \
  "$MANIFEST_DIR/operator" "$MANIFEST_DIR/samples"

aws s3 cp "s3://${ARTIFACT_BUCKET}/manifests-${IMAGE_TAG}.tar.gz" \
  /tmp/manifests.tar.gz --region us-east-2 || fail "manifest s3 cp"
tar -xzf /tmp/manifests.tar.gz -C "$MANIFEST_DIR/" || fail "manifest extract"
chown -R 0:0 "$MANIFEST_DIR/"{spire,operator,samples}
find "$MANIFEST_DIR/"{spire,operator,samples} -name '._*' -delete 2>/dev/null || true

curl -sSL \
  https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml \
  > "$MANIFEST_DIR/cert-manager/00-cert-manager.yaml" \
  || fail "cert-manager download"

# k0s controller as background process.
echo "starting k0s controller --single (data-dir=$DATA_DIR)"
k0s controller --single \
  --config=/etc/k0s/k0s.yaml \
  --data-dir="$DATA_DIR" >>"$log" 2>&1 &
K0S_PID=$!
echo "k0s pid=$K0S_PID"

# Wait for kubeconfig + API.
KCTL="k0s kubectl --kubeconfig=$DATA_DIR/pki/admin.conf"
for i in $(seq 1 60); do
  [ -f "$DATA_DIR/pki/admin.conf" ] && break
  sleep 5
done
[ -f "$DATA_DIR/pki/admin.conf" ] || fail "k0s admin.conf"
for i in $(seq 1 60); do
  $KCTL get ns >/dev/null 2>&1 && break
  sleep 3
done
$KCTL get ns >/dev/null 2>&1 || fail "k0s api"

# Health gate (cert-manager + spire + operator + CRDs).
wait_resource() {
  local kind=$1 ns=$2 sel=$3 timeout=${4:-180}
  local end=$(( $(date +%s) + timeout ))
  while [ $(date +%s) -lt $end ]; do
    n=$($KCTL -n "$ns" get "$kind" $sel -o name 2>/dev/null | wc -l)
    [ "$n" -gt 0 ] && return 0
    sleep 3
  done
  return 1
}
wait_resource deployment cert-manager "" 240 || fail "cert-manager not appearing"
$KCTL wait --for=condition=Available --timeout=180s \
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

# Sentinel on the host filesystem so the L2 driver's SSM-poll sees it.
touch "$HOST_LOG_DIR/l2-bootstrap.READY"
echo "=== bootstrap complete; staying alive to keep k0s running ==="
wait "$K0S_PID"
