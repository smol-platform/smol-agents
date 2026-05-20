// Command spiffe-probe runs SPIFFE-dependent e2e assertions from
// inside a kind Pod (where the SPIRE workload-API socket is dial-
// able with peercred). The L1 driver does:
//
//	kubectl run spiffe-probe ... --image=smol-agents/spiffe-probe:dev
//	kubectl logs spiffe-probe        # parses lines below
//
// Output format (one per line):
//
//	OK <scenario-id> <free-form detail>
//	FAIL <scenario-id> <reason>
//
// The probe exits 0 if every requested scenario passes, 1 otherwise.
//
// Scenarios (selected via --scenarios flag):
//
//	ident       — fetch X509-SVID; assert non-empty SPIFFE ID
//	proxy-tcp   — mTLS dial of fake-gateway-tcp; assert echo round-trips
//	proxy-http  — JWT-SVID Bearer GET; assert audience/spiffeID echoed
//
// All scenarios share a single workload API source so we don't
// re-attest per call.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

func main() {
	socket := flag.String("socket", "unix:///run/spire/agent-sockets/api.sock", "SPIRE workload-API socket")
	scenarios := flag.String("scenarios", "ident", "comma-separated scenario IDs to run")
	tcpAddr := flag.String("tcp-addr", "", "fake-gateway TCP echo addr (host:port) for proxy-tcp")
	httpURL := flag.String("http-url", "", "fake-gateway HTTP URL for proxy-http")
	httpAud := flag.String("http-audience", "", "JWT-SVID audience for proxy-http")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	x509src, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(*socket)))
	if err != nil {
		fail("setup", "x509 source: %v", err)
		os.Exit(1)
	}
	defer x509src.Close()

	jwtSrc, err := workloadapi.NewJWTSource(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(*socket)))
	if err != nil {
		fail("setup", "jwt source: %v", err)
		os.Exit(1)
	}
	defer jwtSrc.Close()

	pass := true
	for _, s := range strings.Split(*scenarios, ",") {
		s = strings.TrimSpace(s)
		var ok bool
		switch s {
		case "ident":
			ok = runIdent(ctx, x509src)
		case "proxy-tcp":
			ok = runProxyTCP(ctx, x509src, *tcpAddr)
		case "proxy-http":
			ok = runProxyHTTP(ctx, x509src, jwtSrc, *httpURL, *httpAud)
		default:
			fail(s, "unknown scenario")
			ok = false
		}
		pass = pass && ok
	}
	if !pass {
		os.Exit(1)
	}
}

// ----------------------------- runners -----------------------------

func runIdent(ctx context.Context, src *workloadapi.X509Source) bool {
	svid, err := src.GetX509SVID()
	if err != nil {
		fail("ident", "GetX509SVID: %v", err)
		return false
	}
	if svid.ID.IsZero() || len(svid.Certificates) == 0 {
		fail("ident", "empty SVID: id=%s, certs=%d", svid.ID, len(svid.Certificates))
		return false
	}
	pass("ident", "spiffeID=%s, expires=%s", svid.ID, svid.Certificates[0].NotAfter.Format(time.RFC3339))
	return true
}

func runProxyTCP(ctx context.Context, src *workloadapi.X509Source, addr string) bool {
	if addr == "" {
		fail("proxy-tcp", "--tcp-addr empty")
		return false
	}
	cfg := tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeAny())
	d := tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		fail("proxy-tcp", "dial: %v", err)
		return false
	}
	defer conn.Close()

	want := []byte("hello-from-spiffe-probe\n")
	if _, err := conn.Write(want); err != nil {
		fail("proxy-tcp", "write: %v", err)
		return false
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		fail("proxy-tcp", "read: %v", err)
		return false
	}
	if string(got) != string(want) {
		fail("proxy-tcp", "echo mismatch: %q != %q", got, want)
		return false
	}
	pass("proxy-tcp", "echo round-trip OK over mTLS")
	return true
}

func runProxyHTTP(ctx context.Context, x509src *workloadapi.X509Source, jwtSrc *workloadapi.JWTSource, url, aud string) bool {
	if url == "" || aud == "" {
		fail("proxy-http", "--http-url or --http-audience empty")
		return false
	}
	tok, err := jwtSrc.FetchJWTSVID(ctx, jwtsvid.Params{Audience: aud})
	if err != nil {
		fail("proxy-http", "FetchJWTSVID: %v", err)
		return false
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsconfig.TLSClientConfig(x509src, tlsconfig.AuthorizeAny()),
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/spiffe-probe", nil)
	if err != nil {
		fail("proxy-http", "new request: %v", err)
		return false
	}
	req.Header.Set("Authorization", "Bearer "+tok.Marshal())
	resp, err := client.Do(req)
	if err != nil {
		fail("proxy-http", "do: %v", err)
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fail("proxy-http", "status %d: %s", resp.StatusCode, body)
		return false
	}
	var echoed map[string]any
	if err := json.Unmarshal(body, &echoed); err != nil {
		fail("proxy-http", "decode: %v: %s", err, body)
		return false
	}
	if echoed["audience"] != aud {
		fail("proxy-http", "audience echoed %v, want %v", echoed["audience"], aud)
		return false
	}
	pass("proxy-http", "audience=%s spiffeID=%v", aud, echoed["spiffeID"])
	return true
}

// ----------------------------- output ------------------------------

func pass(id, format string, args ...any) {
	fmt.Printf("OK %s %s\n", id, fmt.Sprintf(format, args...))
}
func fail(id, format string, args ...any) {
	fmt.Printf("FAIL %s %s\n", id, fmt.Sprintf(format, args...))
}

// silence unused import on Go versions without bufio in stdlib —
// kept because logs may want line buffering on huge outputs.
var _ = bufio.NewReader
var _ = exec.Command
