// Package ebpfloader is the host-level eBPF loader used by the
// smol-agents DaemonSet (cmd/ebpf-loader).
//
// Where pkg/ebpf provides an in-process loader for the agent itself,
// pkg/ebpfloader extends it with:
//
//   - Kernel-feature detection (BTF, ring buffer, capability set).
//   - bpffs ("BPF filesystem") setup at /sys/fs/bpf/<root>.
//   - Pinning of programs and maps so they survive loader-pod restarts and
//     can be opened by sandboxed agents via well-known paths.
//   - Optional UDS event fan-out so unprivileged agents can subscribe
//     without their own BPF capabilities.
//
// The package is Linux-only. On other platforms its public API compiles
// but every operation returns ebpf.ErrUnsupportedOS.
package ebpfloader
