package ebpfloader

import (
	"fmt"
	"os"
	"strings"
)

// KernelFeatures summarises what the host kernel supports. The loader
// uses this both to decide which programs to attach and to log a
// machine-readable diagnostic at startup.
type KernelFeatures struct {
	UnameRelease  string
	BTFAvailable  bool   // /sys/kernel/btf/vmlinux exists and is readable
	BPFFSMounted  bool   // /sys/fs/bpf is a bpffs mount
	BPFFSPath     string // detected bpffs path (default /sys/fs/bpf)
	HasRingBuffer bool   // best-effort guess (kernel ≥ 5.8)
}

// String returns a one-line summary suitable for logs.
func (k KernelFeatures) String() string {
	return fmt.Sprintf("kernel=%s btf=%v ringbuf=%v bpffs=%s",
		k.UnameRelease, k.BTFAvailable, k.HasRingBuffer, k.BPFFSPath)
}

// detectFeatures inspects the host environment for BPF support.
// Pure file probes; safe to run on any OS (returns conservative defaults
// on non-Linux).
func detectFeatures(bpffsPath string) KernelFeatures {
	if bpffsPath == "" {
		bpffsPath = "/sys/fs/bpf"
	}
	out := KernelFeatures{BPFFSPath: bpffsPath}

	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		out.UnameRelease = strings.TrimSpace(string(data))
	}

	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err == nil {
		out.BTFAvailable = true
	}

	if mounts, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		// Each line of mountinfo includes the fstype after a hyphen.
		for _, line := range strings.Split(string(mounts), "\n") {
			parts := strings.SplitN(line, " - ", 2)
			if len(parts) != 2 {
				continue
			}
			fields := strings.Fields(parts[1])
			if len(fields) < 1 {
				continue
			}
			fstype := fields[0]
			head := strings.Fields(parts[0])
			if len(head) < 5 {
				continue
			}
			mountPoint := head[4]
			if fstype == "bpf" && mountPoint == bpffsPath {
				out.BPFFSMounted = true
				break
			}
		}
	}

	out.HasRingBuffer = detectRingBufferSupport(out.UnameRelease)
	return out
}

// detectRingBufferSupport returns true if the kernel version is ≥ 5.8.
// We do not load a probe program just to check; the BPF loader itself
// will fail with a clear error if unsupported.
func detectRingBufferSupport(release string) bool {
	major, minor := parseKernelVersion(release)
	if major > 5 {
		return true
	}
	if major == 5 && minor >= 8 {
		return true
	}
	return false
}

func parseKernelVersion(s string) (major, minor int) {
	cut := s
	for i, r := range s {
		if r == '-' || r == '+' {
			cut = s[:i]
			break
		}
	}
	parts := strings.SplitN(cut, ".", 3)
	if len(parts) < 2 {
		return 0, 0
	}
	major = atoi(parts[0])
	minor = atoi(parts[1])
	return
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
