package builders

import (
	"strings"
	"testing"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
)

func defaultThinPool() v1.ThinPoolConfig {
	return v1.ThinPoolConfig{Backing: "instance-store", DataSize: "50Gi", MetaSize: "5Gi"}
}

func TestBuildKataLayer_InstallAL2023(t *testing.T) {
	got := BuildKataLayer("al2023", "arm64", defaultThinPool(), true)
	for _, want := range []string{
		"dnf install -y lvm2 device-mapper-persistent-data",
		"kata-static-3.10.0-arm64.tar.xz",
		"tar -xJ -C /opt/kata --strip-components=3",
		"ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-v2",
		"truncate -s 50G", // 50Gi → 50G
		"truncate -s 5G",  // 5Gi → 5G
		"dmsetup create kata-thinpool",
		"/etc/k0s/containerd.d/devmapper.toml",
		"/etc/k0s/containerd.d/kata-fc.toml",
		`snapshotter = "devmapper"`,
		"Amazon EC2 NVMe Instance Storage", // prefers instance-store
	} {
		if !strings.Contains(got, want) {
			t.Errorf("al2023 kata layer missing %q\n---\n%s", want, got)
		}
	}
}

func TestBuildKataLayer_PrebakedSkipsDownload(t *testing.T) {
	got := BuildKataLayer("al2023", "arm64", defaultThinPool(), false)
	if strings.Contains(got, "kata-static-3.10.0") {
		t.Error("prebaked must not download kata")
	}
	if strings.Contains(got, "dnf install") {
		t.Error("prebaked must not install packages")
	}
	// …but the per-launch thin-pool + drop-ins are still required.
	for _, want := range []string{"dmsetup create kata-thinpool", "/etc/k0s/containerd.d/kata-fc.toml"} {
		if !strings.Contains(got, want) {
			t.Errorf("prebaked still needs %q", want)
		}
	}
}

func TestBuildKataLayer_Ubuntu(t *testing.T) {
	got := BuildKataLayer("ubuntu", "arm64", defaultThinPool(), true)
	if !strings.Contains(got, "apt-get update && apt-get install -y lvm2 thin-provisioning-tools") {
		t.Error("ubuntu should install via apt")
	}
	if !strings.Contains(got, "/usr/local/bin/kata-runtime") {
		t.Error("ubuntu symlinks into /usr/local/bin")
	}
}

func TestBuildKataLayer_FlatcarUsesOptBin(t *testing.T) {
	got := BuildKataLayer("flatcar", "arm64", defaultThinPool(), true)
	if !strings.Contains(got, "/opt/bin/kata-runtime") {
		t.Error("flatcar must symlink into /opt/bin (/usr is read-only)")
	}
	if strings.Contains(got, "/usr/local/bin/kata-runtime") {
		t.Error("flatcar must not use /usr/local/bin")
	}
}

func TestBuildKataLayer_Amd64Arch(t *testing.T) {
	got := BuildKataLayer("al2023", "amd64", defaultThinPool(), true)
	if !strings.Contains(got, "kata-static-3.10.0-x86_64.tar.xz") {
		t.Error("amd64 should pull the x86_64 bundle")
	}
}

func TestBuildKataLayer_UnknownDistroFallsBackToAL2023(t *testing.T) {
	got := BuildKataLayer("plan9", "arm64", defaultThinPool(), true)
	if !strings.Contains(got, "dnf install") {
		t.Error("unknown distro should fall back to the al2023 recipe")
	}
}
