package cgroup

import (
	"net"
	"strings"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestCompile_HappyPath(t *testing.T) {
	spec := v1.EgressPolicy{
		RedirectCIDRs: []string{"10.42.0.0/16", "10.43.0.0/16"},
		Allow: []v1.EgressRule{
			{CIDR: "10.42.5.7/32", Protocol: "tcp", Ports: []int32{443, 5432}},
		},
	}
	r, a, err := Compile(spec, net.ParseIP("127.0.0.1"), 9100, 0xdeadbeef)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 2 {
		t.Errorf("redirect entries = %d", len(r))
	}
	if len(a) != 2 {
		t.Errorf("allow entries = %d (one per port)", len(a))
	}
	if r[0].SidecarPort != 9100 {
		t.Errorf("sidecarPort = %d", r[0].SidecarPort)
	}
}

func TestCompile_RejectsIPv6Sidecar(t *testing.T) {
	_, _, err := Compile(v1.EgressPolicy{}, net.ParseIP("::1"), 1, 0)
	if err == nil {
		t.Error("expected IPv6 rejection")
	}
}

func TestEncodeRedirect_NetworkByteOrder(t *testing.T) {
	k, v, err := EncodeRedirect(RedirectEntry{CIDR: "10.42.0.0/16", SidecarIP: "127.0.0.1", SidecarPort: 9100})
	if err != nil {
		t.Fatal(err)
	}
	if k.PrefixLen != 16 {
		t.Errorf("prefix = %d", k.PrefixLen)
	}
	// 10.42.0.0 in network byte order = bytes [0x0a, 0x2a, 0x00, 0x00]
	if k.Addr != [4]byte{0x0a, 0x2a, 0x00, 0x00} {
		t.Errorf("addr = %v", k.Addr)
	}
	// 127.0.0.1 = bytes [0x7f, 0x00, 0x00, 0x01]
	if v.SidecarIP != [4]byte{0x7f, 0x00, 0x00, 0x01} {
		t.Errorf("sidecarIP = %v", v.SidecarIP)
	}
}

func TestEncodeRedirect_RejectsBadCIDR(t *testing.T) {
	if _, _, err := EncodeRedirect(RedirectEntry{CIDR: "not-cidr"}); err == nil {
		t.Error("expected CIDR error")
	}
}

func TestEncodeAllow_Only32(t *testing.T) {
	if _, err := EncodeAllow(AllowEntry{DstCIDR: "10.42.0.0/16", Port: 443, Proto: "tcp"}); err == nil {
		t.Error("expected /32-only rejection")
	}
	keys, err := EncodeAllow(AllowEntry{DstCIDR: "10.42.5.7/32", Port: 443, Proto: "tcp", CgroupID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 key, got %d", len(keys))
	}
	k := keys[0]
	if k.Proto != 6 {
		t.Errorf("tcp proto = %d", k.Proto)
	}
	if k.DstPort != 443 {
		t.Errorf("port = %d", k.DstPort)
	}
}

func TestEncodeAllow_UDPCode(t *testing.T) {
	keys, _ := EncodeAllow(AllowEntry{DstCIDR: "10.0.0.1/32", Port: 53, Proto: "udp"})
	if keys[0].Proto != 17 {
		t.Errorf("udp proto = %d", keys[0].Proto)
	}
}

func TestEncodeAllow_BadProto(t *testing.T) {
	_, err := EncodeAllow(AllowEntry{DstCIDR: "1.1.1.1/32", Proto: "icmp"})
	if err == nil || !strings.Contains(err.Error(), "proto") {
		t.Errorf("expected proto error: %v", err)
	}
}

func TestFakeDriver_StoresAndCloses(t *testing.T) {
	f := &FakeDriver{}
	must(t, f.UpdateRedirect([]RedirectEntry{{CIDR: "10/8", SidecarIP: "127.0.0.1"}}))
	must(t, f.UpdateAllow([]AllowEntry{{DstCIDR: "1.1.1.1/32", Port: 443, Proto: "tcp"}}))
	if len(f.Redirect) != 1 || len(f.Allow) != 1 {
		t.Error("entries not stored")
	}
	must(t, f.Close())
	if !f.Closed {
		t.Error("Close did not mark closed")
	}
}

func TestFakeDriver_FailNext(t *testing.T) {
	f := &FakeDriver{FailNext: true}
	if err := f.UpdateRedirect(nil); err == nil {
		t.Error("expected forced failure")
	}
	if err := f.UpdateRedirect(nil); err != nil {
		t.Error("FailNext should be one-shot")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
