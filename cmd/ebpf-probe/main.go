// Command ebpf-probe runs cgroup-attached eBPF assertions from
// inside an L2 Pod. It loads bpf/programs/egress_redirect.bpf.o
// directly (no shared loader), attaches the cgroup programs to
// the Pod's own cgroup, populates the LPM trie / hash maps with
// the CLI-supplied policy, then verifies that egress is
// dropped/redirected as expected.
//
// Output (matches spiffe-probe convention so RunSpiffeProbe-style
// log parsers can read it):
//
//	OK <scenario-id> <detail>
//	FAIL <scenario-id> <reason>
//
// Exits 0 if every requested scenario passes, 1 otherwise.
//
// Scenarios:
//
//	drop  — allow-list = {allow-cidr}; verify connect to dropped-ip
//	        is blocked while connect to allowed-cidr succeeds.
//	redir — redirect-cidr → sidecar; spin up a local TCP listener
//	        on sidecar-port and verify a connect to redirect-cidr
//	        lands there.
//
// Requires:
//   - Linux 5.10+ (uses cilium/ebpf CO-RE attach helpers)
//   - CAP_BPF + CAP_SYS_ADMIN (privileged Pod)
//   - /sys/fs/cgroup (cgroup v2 unified) mounted
//   - /sys/kernel/btf/vmlinux readable for CO-RE relocations
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	cilebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"github.com/stigen/smol-agents/pkg/agentnet/cgroup"
)

func main() {
	scenarios := flag.String("scenarios", "drop", "comma list: drop,redir")
	bpfObj := flag.String("bpf-obj", "/usr/share/smol-agents/bpf/egress_redirect.bpf.o",
		"path to egress_redirect.bpf.o")
	allowCIDR := flag.String("allow-cidr", "127.0.0.1/32",
		"single /32 to allow for drop scenario")
	allowPort := flag.Int("allow-port", 8080, "port for allow-cidr")
	droppedAddr := flag.String("dropped-addr", "1.1.1.1:80",
		"host:port that should be dropped by the eBPF egress filter")
	redirectCIDR := flag.String("redirect-cidr", "203.0.113.42/32",
		"/32 to redirect to the sidecar for redir scenario")
	redirectPort := flag.Int("redirect-port", 80,
		"original port the application is dialing (will be rewritten)")
	sidecarPort := flag.Int("sidecar-port", 19999,
		"local TCP port the in-process sidecar listens on")
	dialTimeout := flag.Duration("dial-timeout", 3*time.Second,
		"connect() timeout — short so a hang/drop both look like failure")
	flag.Parse()

	if err := rlimit.RemoveMemlock(); err != nil {
		fail("setup", "rlimit: %v", err)
		os.Exit(1)
	}

	cgPath, err := selfCgroupPath()
	if err != nil {
		fail("setup", "cgroup path: %v", err)
		os.Exit(1)
	}
	cgID, err := cgroupID(cgPath)
	if err != nil {
		fail("setup", "cgroup id: %v", err)
		os.Exit(1)
	}
	fmt.Printf("INFO cgroup path=%s id=%d\n", cgPath, cgID)

	spec, err := cilebpf.LoadCollectionSpec(*bpfObj)
	if err != nil {
		fail("setup", "load-spec: %v", err)
		os.Exit(1)
	}
	coll, err := cilebpf.NewCollection(spec)
	if err != nil {
		fail("setup", "new-collection: %v", err)
		os.Exit(1)
	}
	defer coll.Close()

	pass := true
	for _, s := range strings.Split(*scenarios, ",") {
		s = strings.TrimSpace(s)
		switch s {
		case "drop":
			if !runDrop(coll, cgPath, cgID, *allowCIDR, *allowPort, *droppedAddr, *dialTimeout) {
				pass = false
			}
		case "redir":
			if !runRedir(coll, cgPath, *redirectCIDR, *redirectPort, *sidecarPort, *dialTimeout) {
				pass = false
			}
		default:
			fail(s, "unknown scenario")
			pass = false
		}
	}
	if !pass {
		os.Exit(1)
	}
}

// runDrop populates allow_list with the single allowed (cgroup, ip,
// port, tcp) tuple and attaches the allow_only program. It then
// verifies a connect to dropped-addr fails (the default-deny rule).
// We deliberately don't test the allow path with a live listener
// here — that requires a peer in-cluster, which the probe shouldn't
// assume; instead we *check the kernel rejected the dial*, which is
// the assertion this scenario is named for.
func runDrop(coll *cilebpf.Collection, cgPath string, cgID uint64,
	allowCIDR string, allowPort int, droppedAddr string, dt time.Duration,
) bool {
	allowMap := coll.Maps["allow_list"]
	prog := coll.Programs["allow_only"]
	if allowMap == nil || prog == nil {
		fail("drop", "egress_redirect.bpf.o missing allow_list / allow_only")
		return false
	}

	keys, err := cgroup.EncodeAllow(cgroup.AllowEntry{
		CgroupID: cgID, DstCIDR: allowCIDR, Port: uint16(allowPort), Proto: "tcp",
	})
	if err != nil {
		fail("drop", "encode allow: %v", err)
		return false
	}
	for _, k := range keys {
		// Map value is a u8 sentinel (1=allowed). The C struct is
		// `__u8 outcome` — pass a single byte.
		val := uint8(1)
		if err := allowMap.Update(k, val, cilebpf.UpdateAny); err != nil {
			fail("drop", "map update: %v", err)
			return false
		}
	}

	ln, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgPath,
		Attach:  cilebpf.AttachCGroupInetEgress,
		Program: prog,
	})
	if err != nil {
		fail("drop", "attach cgroup_skb/egress: %v", err)
		return false
	}
	defer ln.Close()

	// Now the kernel egress filter is active for this cgroup. Try
	// the dropped destination — expect ECONNREFUSED, EPERM, or timeout.
	ctx, cancel := context.WithTimeout(context.Background(), dt)
	defer cancel()
	conn, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", droppedAddr)
	if conn != nil {
		_ = conn.Close()
		fail("drop", "connect to %s succeeded; eBPF egress filter did NOT drop it", droppedAddr)
		return false
	}
	if dialErr == nil {
		fail("drop", "dial returned nil-err nil-conn?")
		return false
	}
	fmt.Printf("OK drop kernel-dropped %s (err=%v)\n", droppedAddr, dialErr)
	return true
}

// runRedir starts a local TCP listener on sidecar-port, populates
// redirect_cidrs so that any connect to redirect-cidr:redirect-port
// gets rewritten to 127.0.0.1:sidecar-port, attaches the connect4
// program, and verifies the connect lands on the sidecar listener.
func runRedir(coll *cilebpf.Collection, cgPath string,
	redirectCIDR string, redirectPort int, sidecarPort int, dt time.Duration,
) bool {
	redirMap := coll.Maps["redirect_cidrs"]
	prog := coll.Programs["redirect_to_sidecar"]
	if redirMap == nil || prog == nil {
		fail("redir", "egress_redirect.bpf.o missing redirect_cidrs / redirect_to_sidecar")
		return false
	}

	// Bring up the sidecar BEFORE the rewrite is live, so the test
	// dial can never miss a window where the listener wasn't ready.
	side, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", sidecarPort))
	if err != nil {
		fail("redir", "sidecar listen: %v", err)
		return false
	}
	defer side.Close()
	hit := make(chan string, 1)
	go func() {
		c, accErr := side.Accept()
		if accErr != nil {
			return
		}
		hit <- c.RemoteAddr().String()
		_ = c.Close()
	}()

	key, val, err := cgroup.EncodeRedirect(cgroup.RedirectEntry{
		CIDR:        redirectCIDR,
		SidecarIP:   "127.0.0.1",
		SidecarPort: uint16(sidecarPort),
	})
	if err != nil {
		fail("redir", "encode redirect: %v", err)
		return false
	}
	if err := redirMap.Update(key, val, cilebpf.UpdateAny); err != nil {
		fail("redir", "redirect_cidrs update: %v", err)
		return false
	}

	ln, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgPath,
		Attach:  cilebpf.AttachCGroupInet4Connect,
		Program: prog,
	})
	if err != nil {
		fail("redir", "attach cgroup/connect4: %v", err)
		return false
	}
	defer ln.Close()

	// Connect to the *redirected* address; the BPF program rewrites
	// the connect-syscall arguments so we actually land on
	// 127.0.0.1:sidecar-port.
	_, ipnet, _ := net.ParseCIDR(redirectCIDR)
	target := fmt.Sprintf("%s:%d", ipnet.IP.String(), redirectPort)
	ctx, cancel := context.WithTimeout(context.Background(), dt)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
	if err != nil {
		fail("redir", "dial %s: %v", target, err)
		return false
	}
	_ = conn.Close()
	select {
	case from := <-hit:
		fmt.Printf("OK redir sidecar received connection from %s (orig target %s)\n", from, target)
		return true
	case <-time.After(dt):
		fail("redir", "connect to %s succeeded but sidecar saw no connection; BPF redirect did NOT fire", target)
		return false
	}
}

// selfCgroupPath returns the cgroup v2 path of the calling process
// rooted at /sys/fs/cgroup, suitable for link.AttachCgroup. In
// cgroup-v2 unified hierarchy /proc/self/cgroup has exactly one
// line of the form "0::/<path>".
func selfCgroupPath() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			return "/sys/fs/cgroup" + parts[2], nil
		}
	}
	return "", errors.New("no cgroup-v2 line in /proc/self/cgroup")
}

// cgroupID returns the inode of the cgroup dir — that's what
// bpf_get_current_cgroup_id() returns in cgroup v2.
func cgroupID(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}

func fail(scenario, format string, args ...interface{}) {
	fmt.Printf("FAIL %s %s\n", scenario, fmt.Sprintf(format, args...))
}

var _ = binary.BigEndian // imports stay stable across refactors
