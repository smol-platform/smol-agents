#!/usr/bin/env bash
# fix-kata-gti.sh — restore kata-fc on the gti k0s node after a reboot dropped
# the loop-backed devmapper thin-pool, and make it survive future reboots.
#
# Run as root ON the node:  sudo bash /tmp/fix-kata-gti.sh
#
# 1. (re)create the thin-pool + drop-ins + restart k0s   (install-kata-k0s.sh)
# 2. install a systemd unit that recreates the loop-backed thin-pool on every
#    boot BEFORE k0s starts (the thin-pool is runtime-only state)
# 3. register the kata-fc RuntimeClass
# 4. verify
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "FATAL: run as root" >&2; exit 1; }

echo "############ 1. install-kata-k0s.sh (recreate thin-pool + restart k0s) ############"
bash /tmp/install-kata-k0s.sh

echo "############ 2. boot persistence (systemd) ############"
install -m 0755 /dev/stdin /usr/local/sbin/kata-thinpool-up.sh <<'SH'
#!/usr/bin/env bash
# Recreate the loop-backed kata devmapper thin-pool from its persisted images.
set -euo pipefail
POOL_DIR=/var/lib/containerd/devmapper
DATA="$POOL_DIR/data.img"; META="$POOL_DIR/meta.img"
[ -f "$DATA" ] && [ -f "$META" ] || { echo "thin-pool images missing in $POOL_DIR" >&2; exit 1; }
modprobe dm_thin_pool
dmsetup info kata-thinpool >/dev/null 2>&1 && exit 0   # already up
DATA_LOOP=$(losetup --find --show "$DATA")
META_LOOP=$(losetup --find --show "$META")
SECTORS=$(blockdev --getsz "$DATA_LOOP")
dmsetup create kata-thinpool --table \
  "0 $SECTORS thin-pool $META_LOOP $DATA_LOOP 128 32768 1 skip_block_zeroing"
SH

cat >/etc/systemd/system/kata-thinpool.service <<'UNIT'
[Unit]
Description=Recreate loop-backed kata devmapper thin-pool before k0s
DefaultDependencies=no
After=local-fs.target systemd-modules-load.service
Before=k0scontroller.service
ConditionPathExists=/var/lib/containerd/devmapper/data.img

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/kata-thinpool-up.sh

[Install]
WantedBy=k0scontroller.service
UNIT
systemctl daemon-reload
systemctl enable kata-thinpool.service
echo "enabled kata-thinpool.service (runs before k0scontroller on boot)"

echo "############ 3. kata-fc RuntimeClass ############"
# install-kata-k0s.sh just restarted k0s; wait for the apiserver before apply.
echo "waiting for k0s apiserver..."
for i in $(seq 1 60); do
  k0s kubectl get --raw /healthz >/dev/null 2>&1 && { echo "apiserver ready"; break; }
  sleep 2
done
k0s kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: kata-fc }
handler: kata-fc
overhead:
  podFixed: { cpu: 250m, memory: 256Mi }
YAML

echo "############ 4. verify ############"
echo "--- dmsetup status kata-thinpool ---"; dmsetup status kata-thinpool || true
echo "--- devmapper snapshotter plugin ---"
k0s ctr -a /run/k0s/containerd.sock -n k8s.io plugins ls 2>/dev/null | grep -E "devmapper" || true
echo "--- kata-runtime check ---"; /opt/kata/bin/kata-runtime check 2>&1 | head -5 || true
echo "--- runtimeclasses ---"; k0s kubectl get runtimeclass
echo "DONE. Smoke test (optional):"
echo "  k0s kubectl run kata-uname --rm -it --restart=Never --image=docker.io/library/busybox:1.36 \\"
echo "    --overrides='{\"spec\":{\"runtimeClassName\":\"kata-fc\"}}' -- uname -r"
