#!/usr/bin/env bash
# Bottlerocket bootstrap-container entrypoint.
#
# Bottlerocket bind-mounts the host rootfs at /.bottlerocket/rootfs
# and exposes the bootstrap-container's user-data field as the env
# var USER_DATA (base64-decoded). We expect USER_DATA to contain
# `KEY=value` lines for ARTIFACT_BUCKET, ECR_REGISTRY, IMAGE_TAG,
# RUN_ID — sourced below.
#
# All host writes go to /.bottlerocket/rootfs/var (writable layer);
# /etc and /usr on the host are read-only.
set -eo pipefail

# Bottlerocket delivers user-data as a base64-decoded file in the
# bootstrap-container's filesystem at /.bottlerocket/bootstrap-
# containers/<name>/user-data. Try both locations + the legacy env
# var for forward compatibility.
for candidate in \
  /.bottlerocket/bootstrap-containers/current/user-data \
  /.bottlerocket/bootstrap-containers/l2/user-data \
  /usr/share/bootstrap-containers/user-data; do
  if [ -f "$candidate" ]; then
    eval "$(cat "$candidate")"
    echo "loaded user-data from $candidate"
    break
  fi
done
if [ -n "${USER_DATA:-}" ] && [ -z "${ARTIFACT_BUCKET:-}" ]; then
  eval "$(printf '%s' "$USER_DATA" | base64 -d)"
fi
: "${ARTIFACT_BUCKET:?must be set via user-data}"
: "${ECR_REGISTRY:?must be set via user-data}"
: "${IMAGE_TAG:?must be set via user-data}"
: "${RUN_ID:?must be set via user-data}"

ROOT=/.bottlerocket/rootfs
mkdir -p "$ROOT/var/log"
log="$ROOT/var/log/l2-bootstrap.log"
: > "$log"

fail() {
  echo "BOOTSTRAP FAILED: $*" >>"$log"
  touch "$ROOT/var/log/l2-bootstrap.FAILED"
  tar -czf /tmp/bootstrap-fail.tgz \
    "$ROOT/var/log/l2-bootstrap.log" 2>/dev/null || true
  aws s3 cp /tmp/bootstrap-fail.tgz \
    "s3://${ARTIFACT_BUCKET}/bootstrap-fail/${RUN_ID}.tgz" \
    --region us-east-2 || true
  exit 0
}
exec >>"$log" 2>&1
echo "=== L2 bootstrap on bottlerocket $(date -u) ==="

# Wait for IMDS credentials.
for i in $(seq 1 10); do
  if aws sts get-caller-identity --region us-east-2 >/dev/null 2>&1; then break; fi
  echo "waiting for IMDS credentials ($i/10)"
  sleep 6
done

# k0s + everything under it lives in $ROOT/var/lib/k0s.
mkdir -p "$ROOT/etc/k0s" "$ROOT/var/lib/k0s/manifests/cert-manager"
cat >"$ROOT/etc/k0s/k0s.yaml" <<'YAML'
apiVersion: k0s.k0sproject.io/v1beta1
kind: ClusterConfig
spec:
  network:
    provider: kuberouter
YAML

# Wire k0s containerd ECR auth (same approach as AL2023/Flatcar).
mkdir -p "$ROOT/etc/k0s/containerd.d" "$ROOT/etc/containerd/certs.d/${ECR_REGISTRY}"
pw=$(aws ecr get-login-password --region us-east-2)
auth=$(printf 'AWS:%s' "$pw" | base64 -w0)
cat >"$ROOT/etc/k0s/containerd.d/registry-config-path.toml" <<TOML
[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"
TOML
cat >"$ROOT/etc/containerd/certs.d/${ECR_REGISTRY}/hosts.toml" <<TOML
server = "https://${ECR_REGISTRY}"
[host."https://${ECR_REGISTRY}"]
  capabilities = ["pull", "resolve"]
  [host."https://${ECR_REGISTRY}".header]
    authorization = "Basic ${auth}"
TOML

# Fetch the manifest tarball from S3.
aws s3 cp "s3://${ARTIFACT_BUCKET}/manifests-${IMAGE_TAG}.tar.gz" \
  /tmp/manifests.tar.gz --region us-east-2 || fail "manifest s3 cp"
tar -xzf /tmp/manifests.tar.gz -C "$ROOT/var/lib/k0s/manifests/" \
  || fail "manifest extract"
chown -R 0:0 "$ROOT/var/lib/k0s/manifests/"{spire,operator,samples}
find "$ROOT/var/lib/k0s/manifests/"{spire,operator,samples} \
  -name '._*' -delete 2>/dev/null || true

# cert-manager.
curl -sSL \
  https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml \
  > "$ROOT/var/lib/k0s/manifests/cert-manager/00-cert-manager.yaml" \
  || fail "cert-manager download"

# Install + start k0s on the host. We use systemd-run to escape into
# pid 1 (Bottlerocket's systemd) so the unit persists after this
# bootstrap-container exits.
chroot "$ROOT" /bin/sh -c "
  cp /usr/local/bin/k0s /var/k0s.bin
  chmod +x /var/k0s.bin
" 2>/dev/null || cp /usr/local/bin/k0s "$ROOT/var/k0s.bin"
chmod +x "$ROOT/var/k0s.bin"

# Bottlerocket has no systemctl + no /etc/systemd/system writable.
# Fork k0s as a daemon via nohup; its sub-processes (etcd, kube-
# apiserver, kubelet) are managed by k0s itself. Bottlerocket's pid
# 1 is separate; if k0s dies the bootstrap-container dies too.
nohup "$ROOT/var/k0s.bin" controller --single \
  -c "$ROOT/etc/k0s/k0s.yaml" \
  --data-dir="$ROOT/var/lib/k0s" \
  >>"$log" 2>&1 &
K0S_PID=$!
echo "k0s started as pid $K0S_PID"

KCTL="$ROOT/var/k0s.bin kubectl --kubeconfig=$ROOT/var/lib/k0s/pki/admin.conf"

# Wait for kubeconfig.
for i in $(seq 1 60); do
  if [ -f "$ROOT/var/lib/k0s/pki/admin.conf" ]; then break; fi
  sleep 5
done
[ -f "$ROOT/var/lib/k0s/pki/admin.conf" ] || fail "k0s admin.conf"

# Wait for k0s to be reachable.
for i in $(seq 1 60); do
  if $KCTL get ns >/dev/null 2>&1; then break; fi
  sleep 3
done
$KCTL get ns >/dev/null 2>&1 || fail "k0s api"

# Health gate (same as AL2023/Ubuntu/Flatcar).
wait_resource() {
  local kind=$1 ns=$2 sel=$3 timeout=${4:-180}
  local end=$(( $(date +%s) + timeout ))
  while [ $(date +%s) -lt $end ]; do
    n=$($KCTL -n "$ns" get "$kind" $sel -o name 2>/dev/null | wc -l)
    if [ "$n" -gt 0 ]; then return 0; fi
    sleep 3
  done
  return 1
}
wait_resource deployment cert-manager "" 180 || fail "cert-manager not appearing"
$KCTL wait --for=condition=Available --timeout=180s \
  deployment -n cert-manager --all || fail "cert-manager deploys"
wait_resource pod spire-system "-l app=spire-server" 180 || fail "spire-server not appearing"
$KCTL wait --for=condition=Ready --timeout=180s \
  pod -n spire-system -l app=spire-server || fail "spire-server pod"
wait_resource pod spire-system "-l app=spire-agent" 180 || fail "spire-agent not appearing"
$KCTL wait --for=condition=Ready --timeout=180s \
  pod -n spire-system -l app=spire-agent || fail "spire-agent pods"
wait_resource deployment knative-agents-system "" 180 || fail "operator not appearing"
$KCTL wait --for=condition=Available --timeout=180s \
  deployment -n knative-agents-system --all || fail "operator deploy"
$KCTL wait --for=condition=Established --timeout=60s \
  crd/knativeagents.agents.stigen.ai \
  crd/agentnetworks.runtime.agents.stigen.ai \
  crd/agentruns.runtime.agents.stigen.ai || fail "CRDs established"

touch "$ROOT/var/log/l2-bootstrap.READY"
echo "=== bootstrap complete; keeping container alive to keep k0s running ==="
# Bootstrap-container must stay alive to keep k0s alive (k0s is a
# child process of this container).
wait "$K0S_PID"
