package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForBrokerSocket(t *testing.T) {
	// Absent socket directory => no broker attached => immediate false.
	missing := filepath.Join(t.TempDir(), "nope", "secret-broker.sock")
	start := time.Now()
	if waitForBrokerSocket(missing, 2*time.Second) {
		t.Error("want false when the socket directory is absent")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("absent directory should return immediately, not wait for the timeout")
	}

	// Directory exists and a socket is bound => true. Use a short path: macOS
	// caps unix socket paths (~104 chars) and the default TMPDIR is long.
	dir, err := os.MkdirTemp("/tmp", "bs")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer l.Close()
	if !waitForBrokerSocket(sock, 2*time.Second) {
		t.Error("want true when the socket is bound")
	}

	// Directory exists but no socket ever binds => false after the timeout.
	empty := t.TempDir()
	if waitForBrokerSocket(filepath.Join(empty, "secret-broker.sock"), 300*time.Millisecond) {
		t.Error("want false when the dir exists but no socket binds")
	}
}
