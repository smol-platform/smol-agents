package shared

import (
	"fmt"
	"sort"
	"strings"
)

// Caps is a set of capability flags. We use a bitset rather than a
// map so capability sets are cheap to compare, intersect, and print.
type Caps uint32

const (
	// CapKubernetes — a real apiserver is reachable; manifests apply
	// and watches work. L1 + L2.
	CapKubernetes Caps = 1 << iota

	// CapEBPF — bpf() syscall available; cgroup v2 mounted; ringbuf
	// readable from userspace. L1 + L2.
	CapEBPF

	// CapKata — runtimeClass `kata-fc` registered with containerd
	// and `/dev/kvm` exposed (host is bare-metal). L2 only.
	CapKata

	// CapWebhook — operator's validating webhook is wired with
	// cert-manager. L2 only (L1 disables webhooks for simplicity).
	CapWebhook

	// CapWireGuard — the WireGuard userspace adapter can bind a
	// netstack TUN. All rings.
	CapWireGuard

	// CapSPIRE — a real SPIRE server + agent is running. All rings.
	CapSPIRE

	// CapNetworkEgress — outbound IP traffic to the public internet
	// is reachable (for the "1.1.1.1 is dropped by eBPF" test).
	// L1 + L2.
	CapNetworkEgress

	// CapInClusterProbe — the env can launch one-shot Pods running
	// cmd/spiffe-probe and parse their logs. L1 + L2 (anything
	// with kubectl + a working node).
	CapInClusterProbe

	// CapLiveLLM — the driver has injected real provider API keys so
	// harness scenarios can hit live LLM endpoints (api.z.ai /
	// api.openai.com). Opt-in via L2_LIVE_LLM=1. L2 only.
	CapLiveLLM
)

var capNames = map[Caps]string{
	CapKubernetes:     "kubernetes",
	CapEBPF:           "ebpf",
	CapKata:           "kata",
	CapWebhook:        "webhook",
	CapWireGuard:      "wireguard",
	CapSPIRE:          "spire",
	CapNetworkEgress:  "network-egress",
	CapInClusterProbe: "in-cluster-probe",
	CapLiveLLM:        "live-llm",
}

// Has reports whether `c` covers every capability in `need`.
func (c Caps) Has(need Caps) bool { return c&need == need }

// String renders a stable, sorted, comma-separated label.
func (c Caps) String() string {
	if c == 0 {
		return "none"
	}
	var on []string
	for bit, name := range capNames {
		if c&bit != 0 {
			on = append(on, name)
		}
	}
	sort.Strings(on)
	return strings.Join(on, ",")
}

// MustParse parses a comma-separated capability label, panicking on
// unknown names. Used in test setup; not a runtime path.
func MustParse(s string) Caps {
	if s == "" || s == "none" {
		return 0
	}
	want := strings.Split(s, ",")
	var out Caps
	for _, w := range want {
		w = strings.TrimSpace(w)
		var match Caps
		for bit, name := range capNames {
			if name == w {
				match = bit
				break
			}
		}
		if match == 0 {
			panic(fmt.Sprintf("unknown capability %q", w))
		}
		out |= match
	}
	return out
}
