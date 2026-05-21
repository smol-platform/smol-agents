#!/usr/bin/env bash
# install-kata-k0s.sh — install the kata-fc (Kata Containers + Firecracker)
# runtime on an EXISTING k0s node. Run as root ON the node.
#
# Mirrors the hardened L2 cloud-init recipe (see operator/internal/builders/
# kata_recipe.go and scripts/aws-l2/cloud-init-al2023.yaml.tmpl):
#   1. kata-static bundle  -> /opt/kata
#   2. devmapper thin-pool (kata-fc needs a block-device rootfs; k0s's
#      bundled containerd ships overlayfs only)
#   3. k0s containerd drop-ins registering the snapshotter + kata-fc runtime
#   4. restart k0s so containerd reloads the drop-ins
#   5. (optional) register the kata-fc RuntimeClass via kubectl
#
# REQUIREMENTS: /dev/kvm (bare metal or nested virt), k0s, root.
# DISRUPTION:   step 4 restarts k0s containerd — pods on this node bounce.
#
# NOTE: the thin-pool is loop-backed runtime state and does NOT survive a
# reboot; re-run this script after reboot, or install the optional systemd
# unit printed at the end for persistence.
set -euo pipefail

KATA_VERSION=${KATA_VERSION:-3.10.0}
POOL_DIR=${POOL_DIR:-/var/lib/containerd/devmapper}
DATA_SIZE=${DATA_SIZE:-50G}
META_SIZE=${META_SIZE:-5G}
RESTART=${RESTART:-1} # set 0 to skip the k0s restart (apply drop-ins manually)

[ "$(id -u)" = 0 ] || { echo "FATAL: run as root on the node" >&2; exit 1; }

case "$(uname -m)" in
x86_64) KARCH=x86_64 ;;
aarch64 | arm64) KARCH=arm64 ;;
*) echo "FATAL: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

[ -e /dev/kvm ] || {
	echo "FATAL: /dev/kvm missing — kata-fc needs KVM (bare metal or nested virt)." >&2
	echo "       Use the gvisor runtimeClass instead on hosts without KVM." >&2
	exit 1
}

echo "=== device-mapper userspace tools (thin_check) ==="
if command -v apt-get >/dev/null; then
	apt-get update && apt-get install -y lvm2 thin-provisioning-tools
elif command -v dnf >/dev/null; then
	dnf install -y lvm2 device-mapper-persistent-data
else
	echo "WARN: no apt-get/dnf; ensure lvm2 + thin_check are present" >&2
fi

echo "=== 1. kata-static ${KATA_VERSION} (${KARCH}) -> /opt/kata ==="
mkdir -p /opt/kata
curl -fsSL "https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/kata-static-${KATA_VERSION}-${KARCH}.tar.xz" |
	tar -xJ -C /opt/kata --strip-components=3
ln -sf /opt/kata/bin/kata-runtime /usr/local/bin/kata-runtime
ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-v2

echo "=== 2. devmapper thin-pool (data=${DATA_SIZE} meta=${META_SIZE}) ==="
mkdir -p "$POOL_DIR"
DATA="$POOL_DIR/data.img"
META="$POOL_DIR/meta.img"
[ -f "$DATA" ] || truncate -s "$DATA_SIZE" "$DATA"
[ -f "$META" ] || truncate -s "$META_SIZE" "$META"
modprobe dm_thin_pool
DATA_LOOP=$(losetup --find --show "$DATA")
META_LOOP=$(losetup --find --show "$META")
if ! dmsetup info kata-thinpool >/dev/null 2>&1; then
	SECTORS=$(blockdev --getsz "$DATA_LOOP")
	dmsetup create kata-thinpool --table \
		"0 $SECTORS thin-pool $META_LOOP $DATA_LOOP 128 32768 1 skip_block_zeroing"
fi

echo "=== 3. k0s containerd drop-ins ==="
mkdir -p /etc/k0s/containerd.d
cat >/etc/k0s/containerd.d/devmapper.toml <<'TOML'
[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "kata-thinpool"
  root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.devmapper"
  base_image_size = "8GB"
  async_remove = true
TOML
cat >/etc/k0s/containerd.d/kata-fc.toml <<'TOML'
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc]
  runtime_type = "io.containerd.kata.v2"
  snapshotter = "devmapper"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-fc.toml"
TOML

if [ "$RESTART" = 1 ]; then
	echo "=== 4. restart k0s (containerd reloads drop-ins; pods bounce) ==="
	systemctl restart k0scontroller 2>/dev/null ||
		systemctl restart k0sworker 2>/dev/null ||
		echo "WARN: could not restart k0s; restart it manually to load the drop-ins" >&2
else
	echo "=== 4. SKIPPED k0s restart (RESTART=0); restart k0s to load drop-ins ==="
fi

cat <<'EOF'

=== DONE ===
Verify on the node:
  /opt/kata/bin/kata-runtime check
  dmsetup status kata-thinpool

Register the RuntimeClass (or let the smol-agents operator create it):
  kubectl apply -f - <<'YAML'
  apiVersion: node.k8s.io/v1
  kind: RuntimeClass
  metadata: { name: kata-fc }
  handler: kata-fc
  overhead:
    podFixed: { cpu: 250m, memory: 256Mi }
  YAML

Smoke test:
  kubectl run kata-uname --rm -it --restart=Never \
    --image=docker.io/library/busybox:1.36 \
    --overrides='{"spec":{"runtimeClassName":"kata-fc"}}' -- uname -r
  # The kernel should differ from the host (microVM guest kernel).
EOF
