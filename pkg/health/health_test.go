package health

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

type fakeSrc struct{ healthy, ready bool }

func (f fakeSrc) Healthy() bool { return f.healthy }
func (f fakeSrc) Ready() bool   { return f.ready }

func TestServer_HealthyReady(t *testing.T) {
	src := &fakeSrc{healthy: true, ready: true}
	s := pickAddr(t, src)
	must(t, s.Start(context.Background()))
	defer s.Stop(context.Background())
	waitListening(t, s.Addr)

	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
	} {
		resp, err := http.Get("http://" + s.Addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: status %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestServer_NotReady(t *testing.T) {
	src := &fakeSrc{healthy: true, ready: false}
	s := pickAddr(t, src)
	must(t, s.Start(context.Background()))
	defer s.Stop(context.Background())
	waitListening(t, s.Addr)

	resp, err := http.Get("http://" + s.Addr + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func pickAddr(t *testing.T, src Source) *Server {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return New(addr, src)
}

func waitListening(t *testing.T, addr string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for listener")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
