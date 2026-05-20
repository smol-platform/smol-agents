// Package cgroup is the operator-side controller for the eBPF maps
// used by bpf/programs/egress_redirect.bpf.c.
//
// It compiles tenant policy (CIDRs + allow rules + sidecar location)
// into the LPM trie + hash map shapes the BPF programs consume, and
// keeps them in sync as AgentNetwork CRs change.
//
// The controller does NOT load BPF programs itself — that's the
// ebpf-loader DaemonSet's job. It only manipulates already-loaded
// maps via pinned paths under /sys/fs/bpf/smol-agents/.
package cgroup
