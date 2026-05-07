package cgroup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"

	v1 "github.com/stigen/knative-agents/pkg/agentmodel/v1"
)

// RedirectKey is the BPF LPM_TRIE key used by `redirect_cidrs`. The
// layout matches the C struct in egress_redirect.bpf.c — the BPF
// driver expects exactly this byte shape.
type RedirectKey struct {
	PrefixLen uint32
	Addr      uint32 // network byte order (big-endian)
}

// RedirectValue is the LPM_TRIE value: where to redirect to.
type RedirectValue struct {
	SidecarIP   uint32 // network byte order
	SidecarPort uint16 // host byte order; the BPF program htons-es
	Pad         uint16
}

// AllowKey mirrors the C struct in egress_redirect.bpf.c.
type AllowKey struct {
	CgroupID uint64
	DstIP    uint32 // network byte order
	DstPort  uint16 // host byte order
	Proto    uint8  // 6=tcp 17=udp
	Pad      uint8
}

// MapDriver is the abstract eBPF map writer. Production: a
// cilium/ebpf wrapper around the pinned maps. Tests: an in-memory
// stub.
type MapDriver interface {
	UpdateRedirect(entries []RedirectEntry) error
	UpdateAllow(entries []AllowEntry) error
	Close() error
}

// RedirectEntry is the operator-friendly shape; converted to
// (RedirectKey, RedirectValue) during write.
type RedirectEntry struct {
	CIDR        string
	SidecarIP   string
	SidecarPort uint16
}

// AllowEntry is the operator-friendly shape.
type AllowEntry struct {
	CgroupID uint64
	DstCIDR  string
	Port     uint16
	Proto    string // "tcp" | "udp"
}

// Compile turns the CR's egress policy into entries the driver can
// install. The function is pure — same input gives same output —
// which lets the operator hash the entries to detect drift.
func Compile(spec v1.EgressPolicy, sidecarIP net.IP, sidecarPort uint16, cgroupID uint64) (
	redirect []RedirectEntry, allow []AllowEntry, err error,
) {
	if sidecarIP.To4() == nil {
		return nil, nil, errors.New("agentnet/cgroup: only IPv4 sidecar supported in v1")
	}
	for _, c := range spec.RedirectCIDRs {
		redirect = append(redirect, RedirectEntry{
			CIDR:        c,
			SidecarIP:   sidecarIP.To4().String(),
			SidecarPort: sidecarPort,
		})
	}
	for _, rule := range spec.Allow {
		ports := rule.Ports
		if len(ports) == 0 {
			ports = []int32{0} // 0 = any (the BPF program treats as wildcard)
		}
		for _, p := range ports {
			allow = append(allow, AllowEntry{
				CgroupID: cgroupID,
				DstCIDR:  rule.CIDR,
				Port:     uint16(p),
				Proto:    rule.Protocol,
			})
		}
	}
	return redirect, allow, nil
}

// EncodeRedirect produces the wire pair for one entry. Returns the
// LPM trie prefix length + key + value. The BPF map's key type uses
// network-byte-order addresses.
func EncodeRedirect(e RedirectEntry) (RedirectKey, RedirectValue, error) {
	_, ipnet, err := net.ParseCIDR(e.CIDR)
	if err != nil {
		return RedirectKey{}, RedirectValue{}, fmt.Errorf("cidr: %w", err)
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return RedirectKey{}, RedirectValue{}, errors.New("only IPv4")
	}
	mask, _ := ipnet.Mask.Size()
	side := net.ParseIP(e.SidecarIP).To4()
	if side == nil {
		return RedirectKey{}, RedirectValue{}, errors.New("sidecarIP not IPv4")
	}
	return RedirectKey{
			PrefixLen: uint32(mask),
			Addr:      binary.BigEndian.Uint32(ip),
		}, RedirectValue{
			SidecarIP:   binary.BigEndian.Uint32(side),
			SidecarPort: e.SidecarPort,
		}, nil
}

// EncodeAllow expands one CIDR-based AllowEntry into (key, value)
// pairs — one per IP in the CIDR. v1 only supports /32 entries to
// keep the hash table small; larger CIDRs return an error so the
// admission webhook can surface it.
func EncodeAllow(e AllowEntry) ([]AllowKey, error) {
	_, ipnet, err := net.ParseCIDR(e.DstCIDR)
	if err != nil {
		return nil, fmt.Errorf("cidr: %w", err)
	}
	mask, _ := ipnet.Mask.Size()
	if mask != 32 {
		return nil, fmt.Errorf("v1 supports only /32 entries in allow-list (got /%d)", mask)
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return nil, errors.New("only IPv4")
	}
	proto := uint8(0)
	switch e.Proto {
	case "tcp", "":
		proto = 6
	case "udp":
		proto = 17
	default:
		return nil, fmt.Errorf("proto=%q invalid", e.Proto)
	}
	return []AllowKey{{
		CgroupID: e.CgroupID,
		DstIP:    binary.BigEndian.Uint32(ip),
		DstPort:  e.Port,
		Proto:    proto,
	}}, nil
}

// FakeDriver is an in-memory stand-in used by tests + envtest. Stores
// the last-applied entries so assertions can read them.
type FakeDriver struct {
	mu       sync.Mutex
	Redirect []RedirectEntry
	Allow    []AllowEntry
	Closed   bool
	FailNext bool
}

func (f *FakeDriver) UpdateRedirect(entries []RedirectEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailNext {
		f.FailNext = false
		return errors.New("fake: forced failure")
	}
	f.Redirect = append([]RedirectEntry(nil), entries...)
	return nil
}

func (f *FakeDriver) UpdateAllow(entries []AllowEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailNext {
		f.FailNext = false
		return errors.New("fake: forced failure")
	}
	f.Allow = append([]AllowEntry(nil), entries...)
	return nil
}

func (f *FakeDriver) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = true
	return nil
}
