package builders

import (
	"fmt"
	"strings"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

// kataVersion is the kata-containers static bundle we ship. Mirrors
// scripts/aws-l2/cloud-init-*.tmpl (kept in sync there for the e2e ring).
const kataVersion = "3.10.0"

// kataArch maps a node arch to the kata-static release suffix.
func kataArch(arch string) string {
	if arch == "amd64" {
		return "x86_64"
	}
	return "arm64"
}

// distroKata captures the per-distro bits of the kata layer: how to install
// the device-mapper userspace tools, and where kata binaries can be
// symlinked so they land on PATH (Flatcar/FCOS have a read-only /usr).
type distroKata struct {
	pkgInstall string
	binDir     string
}

var distroKataMatrix = map[string]distroKata{
	"al2023":        {pkgInstall: "dnf install -y lvm2 device-mapper-persistent-data", binDir: "/usr/local/bin"},
	"ubuntu":        {pkgInstall: "apt-get update && apt-get install -y lvm2 thin-provisioning-tools", binDir: "/usr/local/bin"},
	"flatcar":       {pkgInstall: "true # lvm2/dmsetup ship in the Flatcar base image", binDir: "/opt/bin"},
	"fedora-coreos": {pkgInstall: "true # dm tools in base; prefer PrebakedAMI for FCOS (thin_check + SELinux)", binDir: "/opt/bin"},
}

// BuildKataLayer renders the bash recipe that makes a node kata-capable:
// kata-static → /opt/kata, a devmapper thin-pool (on instance-store NVMe
// when present), and the k0s containerd drop-ins. Derived from the hardened
// scripts/aws-l2/cloud-init-*.tmpl recipe.
//
// When installKata is false (Bootstrap.Mode == PrebakedAMI) the kata
// binaries are already in the image, so only the per-launch thin-pool +
// drop-ins are emitted (instance-store is blank at every launch).
func BuildKataLayer(distro, arch string, tp v1.ThinPoolConfig, installKata bool) string {
	dk, ok := distroKataMatrix[distro]
	if !ok {
		dk = distroKataMatrix["al2023"]
	}
	data := truncateSize(orDefault(tp.DataSize, "50Gi"))
	meta := truncateSize(orDefault(tp.MetaSize, "5Gi"))

	var b strings.Builder
	fmt.Fprintf(&b, "# --- smol-agents kata layer (distro=%s arch=%s) ---\n", distro, arch)
	b.WriteString("set -euo pipefail\n")

	if installKata {
		fmt.Fprintf(&b, "%s\n", dk.pkgInstall)
		b.WriteString("mkdir -p /opt/kata\n")
		fmt.Fprintf(&b,
			"curl -sSL https://github.com/kata-containers/kata-containers/releases/download/%s/kata-static-%s-%s.tar.xz | tar -xJ -C /opt/kata --strip-components=3\n",
			kataVersion, kataVersion, kataArch(arch))
		fmt.Fprintf(&b, "ln -sf /opt/kata/bin/kata-runtime %s/kata-runtime\n", dk.binDir)
		fmt.Fprintf(&b, "ln -sf /opt/kata/bin/containerd-shim-kata-v2 %s/containerd-shim-kata-v2\n", dk.binDir)
	}

	b.WriteString(thinPoolBlock(data, meta))
	b.WriteString(containerdDropins)
	return b.String()
}

// thinPoolBlock builds the devmapper thin-pool. It prefers an ephemeral
// instance-store NVMe (fast, doesn't bloat the root EBS); image files on
// that volume back the loop devices the pool is created from.
func thinPoolBlock(data, meta string) string {
	return fmt.Sprintf(`# devmapper thin-pool for kata-fc (instance-store NVMe when present).
POOL_DIR=/var/lib/containerd/devmapper
mkdir -p "$POOL_DIR"
ISTORE=$(lsblk -dno NAME,MODEL | awk '/Amazon EC2 NVMe Instance Storage/{print "/dev/"$1; exit}')
if [ -n "$ISTORE" ] && ! mountpoint -q "$POOL_DIR"; then
  mkfs.xfs -f "$ISTORE" >/dev/null 2>&1 || true
  mount "$ISTORE" "$POOL_DIR" || true
fi
DATA="$POOL_DIR/data.img"; META="$POOL_DIR/meta.img"
[ -f "$DATA" ] || truncate -s %s "$DATA"
[ -f "$META" ] || truncate -s %s "$META"
modprobe dm_thin_pool
DATA_LOOP=$(losetup --find --show "$DATA")
META_LOOP=$(losetup --find --show "$META")
SECTORS=$(blockdev --getsz "$DATA_LOOP")
dmsetup create kata-thinpool --table "0 $SECTORS thin-pool $META_LOOP $DATA_LOOP 128 32768 1 skip_block_zeroing"
`, data, meta)
}

// containerdDropins registers the devmapper snapshotter and the kata-fc
// runtime with k0s's bundled containerd (which reads /etc/k0s/containerd.d/
// but not the system /etc/containerd/config.toml). Per-runtime snapshotter
// keeps default workloads on overlayfs; only kata-fc pods use devmapper.
const containerdDropins = `mkdir -p /etc/k0s/containerd.d
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
`

// truncateSize converts a k8s-style quantity (50Gi) to the suffix GNU
// truncate expects (50G == 50*1024^3, matching the Gi semantics).
func truncateSize(s string) string {
	return strings.TrimSuffix(s, "i")
}
