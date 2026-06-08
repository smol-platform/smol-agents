package shared

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/wireguard"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/fakellm"
)

// All returns every cross-ring scenario in registration order. Each
// ring's test file calls RunAll(t, env, All()).
//
// New scenarios get appended here AND get an entry in
// `test/e2e/fullstack/coverage.go` mapping their R-E2E-SCN-* ID to
// the scenario name. The coverage gate fails if either is missing.
func All() []Scenario {
	return []Scenario{
		identityRotation,
		proxyTCP,
		proxyHTTP,
		ebpfDrop,
		ebpfRedirect,
		wgClient,
		agentRun,
		cancel,
		approvalGate,
		approvalReject,
		approvalTimeout,
		egressFloor,
		policyGate,
		policyReconcileGate,
		toolKindGuard,
		stdioMCPGate,
		webhook,
		kataIsolation,
		smolAgentPhase,
		secretlessEgress,
		memoryAccess,
		agentSession,
		agentSessionRunning,
		a2aComposition,
		claudeHarnessLive,
		codexHarnessLive,
	}
}

// ---------------------------- Scenarios ----------------------------

var identityRotation = Scenario{
	ID:       "R-E2E-SCN-IDENT-1",
	Name:     "agent-svid-rotates",
	Requires: CapSPIRE,
	Run:      runIdentityRotation,
}

func runIdentityRotation(t *testing.T, env Env) {
	t.Helper()

	// L1+ rings: route through the in-cluster probe Pod.
	if env.Capabilities().Has(CapInClusterProbe) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		lines, err := env.RunSpiffeProbe(ctx, []string{"ident"})
		if err != nil {
			t.Fatalf("RunSpiffeProbe: %v", err)
		}
		assertProbeOK(t, lines, "ident")
		return
	}

	// L0 (Linux dev only): in-process direct dial. macOS-OrbStack
	// gates CapSPIRE off so this path doesn't run where it can't.
	socket := env.SPIFFEWorkloadAPI()
	if socket == "" {
		t.Skip("env exposes no SPIFFE socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src, err := workloadapi.NewX509Source(ctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	if err != nil {
		t.Fatalf("NewX509Source: %v", err)
	}
	defer src.Close()

	svid, err := src.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID: %v", err)
	}
	if svid.ID.IsZero() || len(svid.Certificates) == 0 {
		t.Errorf("empty SVID: id=%s certs=%d", svid.ID, len(svid.Certificates))
	}
	if _, err := src.GetX509SVID(); err != nil {
		t.Errorf("re-fetch SVID failed: %v", err)
	}
}

// assertProbeOK fails the test if any of the named scenarios is
// missing or returned FAIL.
func assertProbeOK(t *testing.T, lines []ProbeLine, want ...string) {
	t.Helper()
	got := map[string]ProbeLine{}
	for _, l := range lines {
		got[l.Scenario] = l
	}
	for _, name := range want {
		l, ok := got[name]
		if !ok {
			t.Errorf("probe missing scenario %q in output", name)
			continue
		}
		if !l.OK {
			t.Errorf("probe scenario %q FAIL: %s", name, l.Detail)
			continue
		}
		t.Logf("probe %q OK: %s", name, l.Detail)
	}
}

var proxyTCP = Scenario{
	ID:       "R-E2E-SCN-PROXY-TCP",
	Name:     "tcp-proxy-mtls-svid",
	Requires: CapSPIRE,
	Run:      runProxyTCP,
}

// retryUntil calls fn every `every` until it returns nil or ctx expires.
// The proxy scenarios need it because the fake-gateway's own SVID may not
// have propagated from SPIRE when scenarios start (notably slower on CI
// runners): until the gateway can present a server cert it resets the mTLS
// handshake ("connection reset by peer"). Retrying the dial absorbs that
// warmup without masking a genuinely-broken gateway (it still fails on
// timeout, reporting the last error).
func retryUntil(ctx context.Context, every time.Duration, fn func() error) error {
	var last error
	for {
		if last = fn(); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last attempt: %v)", ctx.Err(), last)
		case <-time.After(every):
		}
	}
}

func runProxyTCP(t *testing.T, env Env) {
	t.Helper()
	tcpAddr, ok := env.Endpoint("fake-gateway-tcp")
	if !ok {
		t.Skip("env has no fake-gateway-tcp endpoint")
	}

	if env.Capabilities().Has(CapInClusterProbe) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		lines, err := env.RunSpiffeProbe(ctx,
			[]string{"proxy-tcp"},
			"--tcp-addr=fake-gateway.tenant-a.svc.cluster.local:8443")
		if err != nil {
			t.Fatalf("RunSpiffeProbe: %v", err)
		}
		assertProbeOK(t, lines, "proxy-tcp")
		return
	}

	socket := env.SPIFFEWorkloadAPI()
	if socket == "" {
		t.Skip("env exposes no SPIFFE socket")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	src, err := workloadapi.NewX509Source(ctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	if err != nil {
		t.Fatalf("x509 source: %v", err)
	}
	defer src.Close()

	cfg := tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeAny())
	// Retry the mTLS dial+handshake until the gateway's SVID is ready.
	var conn *tls.Conn
	if err := retryUntil(ctx, 500*time.Millisecond, func() error {
		c, err := tls.Dial("tcp", tcpAddr, cfg)
		if err != nil {
			return err
		}
		if err := c.HandshakeContext(ctx); err != nil {
			_ = c.Close()
			return err
		}
		conn = c
		return nil
	}); err != nil {
		t.Fatalf("dial fake-gateway-tcp: %v", err)
	}
	defer conn.Close()

	// Echo: write a payload, expect it back verbatim.
	want := []byte("hello-from-l0-test-driver\n")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("echo mismatch: got %q, want %q", got, want)
	}
}

var proxyHTTP = Scenario{
	ID:       "R-E2E-SCN-PROXY-HTTP",
	Name:     "http-proxy-jwt-audience",
	Requires: CapSPIRE,
	Run:      runProxyHTTP,
}

func runProxyHTTP(t *testing.T, env Env) {
	t.Helper()
	gwURL, ok := env.Endpoint("fake-gateway-http")
	if !ok {
		t.Skip("env has no fake-gateway-http endpoint")
	}

	if env.Capabilities().Has(CapInClusterProbe) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		audience := "spiffe://smol-agents.ai/ns/tenant-a/sa/fake-gateway"
		lines, err := env.RunSpiffeProbe(ctx,
			[]string{"proxy-http"},
			"--http-url=http://fake-gateway.tenant-a.svc.cluster.local:8080",
			"--http-audience="+audience)
		if err != nil {
			t.Fatalf("RunSpiffeProbe: %v", err)
		}
		assertProbeOK(t, lines, "proxy-http")
		return
	}

	socket := env.SPIFFEWorkloadAPI()
	if socket == "" {
		t.Skip("env exposes no SPIFFE socket")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	jwtSrc, err := workloadapi.NewJWTSource(ctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	if err != nil {
		t.Fatalf("jwt source: %v", err)
	}
	defer jwtSrc.Close()

	audience := "spiffe://smol-agents.ai/ns/tenant-a/sa/fake-gateway"
	tok, err := jwtSrc.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
	if err != nil {
		t.Fatalf("FetchJWTSVID: %v", err)
	}

	// The fake-gateway HTTP listener serves plain HTTP and authorizes by
	// validating the JWT-SVID in the Authorization header (the in-cluster
	// probe path dials http:// too) — no server TLS, so no X509 source here.
	httpClient := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gwURL+"/billing/charge", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Marshal())

	// Retry until the gateway's SVID is ready (see runProxyTCP).
	var resp *http.Response
	if err := retryUntil(ctx, 500*time.Millisecond, func() error {
		r, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		resp = r
		return nil
	}); err != nil {
		t.Fatalf("http GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	var echoed map[string]any
	if err := json.Unmarshal(body, &echoed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, want := echoed["audience"], audience; got != want {
		t.Errorf("audience echoed back = %v, want %v", got, want)
	}
	if id, _ := echoed["spiffeID"].(string); !strings.HasPrefix(id, "spiffe://smol-agents.ai/") {
		t.Errorf("spiffeID echoed back = %v, expected smol-agents.ai trust domain", id)
	}
}

var ebpfDrop = Scenario{
	ID:       "R-E2E-SCN-EBPF-DROP",
	Name:     "ebpf-blocks-disallowed-cidr",
	Requires: CapEBPF | CapNetworkEgress | CapKubernetes,
	Run:      runEBPFDrop,
}

func runEBPFDrop(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	lines, err := env.RunEBPFProbe(ctx, []string{"drop"},
		"--allow-cidr=127.0.0.1/32", "--allow-port=8080",
		"--dropped-addr=1.1.1.1:80")
	if err != nil {
		t.Fatalf("RunEBPFProbe: %v", err)
	}
	assertProbeOK(t, lines, "drop")
}

var ebpfRedirect = Scenario{
	ID:       "R-E2E-SCN-EBPF-REDIR",
	Name:     "ebpf-redirects-to-sidecar",
	Requires: CapEBPF | CapKubernetes,
	Run:      runEBPFRedirect,
}

func runEBPFRedirect(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// 203.0.113.0/24 is in the TEST-NET-3 documentation range —
	// nothing real routes there, so any "successful" connect to
	// 203.0.113.42 can only have come from the BPF redirect.
	lines, err := env.RunEBPFProbe(ctx, []string{"redir"},
		"--redirect-cidr=203.0.113.42/32", "--redirect-port=80",
		"--sidecar-port=19999")
	if err != nil {
		t.Fatalf("RunEBPFProbe: %v", err)
	}
	assertProbeOK(t, lines, "redir")
}

var wgClient = Scenario{
	ID:       "R-E2E-SCN-WG-CLIENT",
	Name:     "wireguard-client-handshake",
	Requires: CapWireGuard,
	Run:      runWGClient,
}

func runWGClient(t *testing.T, env Env) {
	t.Helper()
	hub, ok := env.Endpoint("wg-hub")
	if !ok {
		t.Skip("env has no wg-hub endpoint")
	}

	// Connectivity smoke first — bail with a clearer error if wg-hub
	// isn't even listening.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "udp", hub)
		if err != nil {
			t.Fatalf("dial wg-hub udp %s: %v", hub, err)
		}
		_ = c.Close()
	}

	// Full handshake requires the netstack-backed userspace adapter,
	// which only links under -tags=wgnetstack. Without the tag, the
	// device's startFn is nil and we can't drive a real test — skip
	// with a clear message.
	dev := wireguard.NewUserspaceDevice()
	driverPriv, _ := base64.StdEncoding.DecodeString(wireguard.TestDriverPrivKey)
	hubPub, _ := base64.StdEncoding.DecodeString(wireguard.TestHubPubKey)

	cfg := wireguard.Config{
		Mode:       wireguard.ModeClient,
		PrivateKey: driverPriv,
		Addresses:  []string{"10.99.0.5/32"},
		MTU:        1420,
		Peers: []wireguard.Peer{{
			Name:                       "hub",
			PublicKey:                  hubPub,
			Endpoint:                   hub,
			AllowedIPs:                 []string{"10.99.0.0/24"},
			PersistentKeepaliveSeconds: 1,
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dev.Start(ctx, cfg); err != nil {
		if errors.Is(err, wireguard.ErrNotWired) {
			t.Skipf("wgnetstack tag not built; UDP smoke OK at %s", hub)
		}
		t.Fatalf("Start: %v", err)
	}
	defer dev.Stop()

	// Poll for handshake completion (peer.State flips to "connected"
	// once wg-hub responds). Allow up to 10s.
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		var connected bool
		for _, p := range dev.Peers() {
			if p.State == "connected" {
				connected = true
				break
			}
		}
		if connected {
			t.Logf("wg-hub handshake completed; userspace tunnel up at %s", hub)
			return
		}
		select {
		case <-deadline:
			t.Errorf("WireGuard handshake never completed; peers=%v", dev.Peers())
			return
		case <-tick.C:
		}
	}
}

var agentRun = Scenario{
	ID:   "R-E2E-SCN-AGENTRUN",
	Name: "agentrun-plan-act-observe",
	// No CapSPIRE: scenario drives the executor in-process and dials
	// fake-llm directly. Self-skips if the env can't serve fake-llm.
	Requires: 0,
	Run:      runAgentRun,
}

func runAgentRun(t *testing.T, env Env) {
	t.Helper()
	llmURL, ok := env.Endpoint("fake-llm")
	if !ok {
		t.Skip("env has no fake-llm endpoint")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Register an "echo" tool so the LLM's first ToolCall in the
	// scripted plans.json sequence has somewhere to land. The
	// invoker just returns the args as the observation — that's
	// enough to exercise plan→tool→observation→FinalAnswer.
	echoTool := v1.Tool{
		Name: "echo",
		Spec: v1.ToolSpec{
			Kind:         v1.ToolFunction,
			Description:  "echo the args verbatim",
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Function:     &v1.FunctionSpec{Name: "echo"},
		},
	}
	executor := &agentruntime.Executor{
		LLM:   fakellm.New(llmURL),
		Tools: map[string]v1.Tool{"echo": echoTool},
		Invokers: map[v1.ToolKind]agentruntime.ToolInvoker{
			v1.ToolFunction: &agentruntime.InProcessInvoker{
				Handlers: map[string]func(json.RawMessage) (json.RawMessage, error){
					"echo": func(args json.RawMessage) (json.RawMessage, error) {
						return args, nil
					},
				},
			},
		},
		Clock: agentruntime.SystemClock(),
	}

	agent := v1.Agent{
		Spec: v1.AgentSpec{
			Mode:         v1.ModeLoop,
			Model:        v1.ModelRef{ProviderRef: "fake", Name: "fake-1"},
			Instructions: "Reply with the answer json.",
			Tools:        []v1.ToolRef{{Name: "echo"}},
			Budget: v1.Budget{
				MaxSteps:            5,
				MaxTokens:           1000,
				MaxWallClockSeconds: 30,
				MaxToolCalls:        2,
			},
		},
	}

	res, err := executor.Run(ctx, agent, json.RawMessage(`{"q":"hi"}`), 42)
	if err != nil {
		t.Fatalf("executor.Run: %v", err)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Errorf("phase = %s, want Completed (steps=%d, reason=%s)",
			res.Phase, len(res.Steps), res.TerminationReason)
	}
	if len(res.Output) == 0 {
		t.Error("Output is empty")
	}
	t.Logf("AgentRun completed: %d steps, output=%s", len(res.Steps), res.Output)
}

var cancel = Scenario{
	ID:   "R-E2E-SCN-CANCEL",
	Name: "agentrun-cancel-stops-pod",
	// Purely in-process (slowLLM stub, no fake-llm); runs anywhere.
	Requires: 0,
	Run:      runCancel,
}

func runCancel(t *testing.T, env Env) {
	t.Helper()
	// We use a slow LLM stub (in-process) rather than fake-llm here:
	// the HTTP fake-llm returns its fallback FinalAnswer in <1ms, so
	// the executor terminates before the cancel goroutine fires.
	// Cancel semantics work the same regardless of LLM source — this
	// scenario asserts the executor honors ctx.Done(), not the
	// network path.
	executor := &agentruntime.Executor{
		LLM:   slowLLM{delay: 5 * time.Second},
		Tools: map[string]v1.Tool{},
		Clock: agentruntime.SystemClock(),
	}
	agent := v1.Agent{
		Spec: v1.AgentSpec{
			Mode:         v1.ModeLoop,
			Model:        v1.ModelRef{ProviderRef: "fake", Name: "fake-1"},
			Instructions: "block until cancelled",
			Budget: v1.Budget{
				MaxSteps: 100, MaxTokens: 10000, MaxWallClockSeconds: 60, MaxToolCalls: 100,
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := executor.Run(ctx, agent, json.RawMessage(`{}`), 1)
	elapsed := time.Since(start)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	// Executor reports either PhaseCancelled (when ctx.Err is observed
	// at the top of the loop) or PhaseFailed (when ctx.Cancel races
	// the LLM.Chat call and surfaces as an LLM error). Both signal
	// the cancel was honored — what matters is the executor unwound
	// quickly. Normalizing to PhaseCancelled in both paths is a
	// follow-up improvement to the executor; tracked separately.
	switch res.Phase {
	case v1.PhaseCancelled:
		// happy path
	case v1.PhaseFailed:
		if !strings.Contains(res.TerminationReason, "context canceled") {
			t.Errorf("phase=Failed but reason=%q, expected context canceled",
				res.TerminationReason)
		}
	default:
		t.Errorf("phase = %s, want Cancelled or Failed-with-cancel (reason=%s)",
			res.Phase, res.TerminationReason)
	}
	if elapsed > 5*time.Second {
		t.Errorf("cancellation took %s, expected < 5s", elapsed)
	}
}

// slowLLM is a minimal LLM that blocks on Chat for `delay` or until
// ctx is cancelled, whichever comes first. Used by S-CANCEL.
type slowLLM struct{ delay time.Duration }

func (s slowLLM) Chat(ctx context.Context, _ agentruntime.ChatRequest) (rt.LLMDecision, error) {
	select {
	case <-ctx.Done():
		return rt.LLMDecision{}, ctx.Err()
	case <-time.After(s.delay):
		return rt.LLMDecision{
			FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"never"}`)},
		}, nil
	}
}

var approvalGate = Scenario{
	ID:       "R-E2E-SCN-APPROVAL",
	Name:     "prerun-approval-gate",
	Requires: CapKubernetes,
	Run:      runApprovalGate,
}

// runApprovalGate exercises the M5 pre-run human-approval gate end-to-end via the
// real operator: an AgentRun with requireApprovalBeforeRun=true is held in
// RequiresAction (a token minted, NO pod) until a matching spec.decision approves
// it, after which it proceeds. Reuses the researcher Agent kind-verify.sh applied.
func runApprovalGate(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const run = "approval-test"
	// Idempotent re-run: drop any prior approval-test (a terminal run from a
	// reused cluster won't re-enter RequiresAction on re-apply).
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentrun", run, "-n", "tenant-a", "--ignore-not-found")
	manifest := []byte(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentRun
metadata:
  name: approval-test
  namespace: tenant-a
spec:
  agentRef: researcher
  requireApprovalBeforeRun: true
  input:
    q: "approve me"
`)
	if err := env.Apply(ctx, manifest); err != nil {
		t.Fatalf("apply approval-test run: %v", err)
	}

	// 1. Gate holds the run in RequiresAction with a minted token + no pod.
	var token string
	err := env.WaitFor(ctx, "run-requires-action", 60*time.Second, func(ctx context.Context) bool {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "agentrun", run, "-o", "jsonpath={.status.state}")
		if strings.TrimSpace(string(st)) != "RequiresAction" {
			return false
		}
		tk, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "agentrun", run, "-o", "jsonpath={.status.pendingAction.token}")
		token = strings.TrimSpace(string(tk))
		return token != ""
	})
	if err != nil {
		t.Fatalf("run never reached RequiresAction with a token: %v", err)
	}
	if out, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "pod", run, "--ignore-not-found", "-o", "name"); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a pod exists while the run is still awaiting approval: %q", out)
	}
	t.Logf("M5 gate: run held in RequiresAction (token=%s), no pod", token)

	// 2. Approve with the matching token → the run leaves RequiresAction.
	patch := `{"spec":{"decision":{"token":"` + token + `","approve":true}}}`
	if _, err := env.Exec(ctx, ExecTarget{}, "patch", "-n", "tenant-a", "agentrun", run, "--type=merge", "-p", patch); err != nil {
		t.Fatalf("apply approval decision: %v", err)
	}
	if err := env.WaitFor(ctx, "run-proceeds", 60*time.Second, func(ctx context.Context) bool {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "agentrun", run, "-o", "jsonpath={.status.state}")
		s := strings.TrimSpace(string(st))
		return s != "" && s != "RequiresAction"
	}); err != nil {
		t.Fatalf("run did not proceed after approval: %v", err)
	}
	t.Log("M5 gate: approved run left RequiresAction and proceeded")
}

var approvalReject = Scenario{
	ID:       "R-E2E-SCN-APPROVAL-REJECT",
	Name:     "prerun-approval-reject",
	Requires: CapKubernetes,
	Run:      runApprovalReject,
}

// runApprovalReject exercises the M5 pre-run gate's DENY half through the real
// operator (the approve half is runApprovalGate): an AgentRun held in
// RequiresAction that receives a matching spec.decision with approve:false must
// be driven to a terminal Cancelled state (TerminationReason carrying
// "decision:denied") with NO pod ever created. Reuses the researcher Agent;
// distinct run name so it can't collide with the approve scenario.
func runApprovalReject(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const run = "approval-reject-test"
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentrun", run, "-n", "tenant-a", "--ignore-not-found")
	manifest := []byte(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentRun
metadata:
  name: approval-reject-test
  namespace: tenant-a
spec:
  agentRef: researcher
  requireApprovalBeforeRun: true
  input:
    q: "reject me"
`)
	if err := env.Apply(ctx, manifest); err != nil {
		t.Fatalf("apply approval-reject run: %v", err)
	}

	// 1. Gate holds the run in RequiresAction with a minted token.
	var token string
	if err := env.WaitFor(ctx, "reject-requires-action", 60*time.Second, func(ctx context.Context) bool {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "agentrun", run, "-o", "jsonpath={.status.state}")
		if strings.TrimSpace(string(st)) != "RequiresAction" {
			return false
		}
		tk, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "agentrun", run, "-o", "jsonpath={.status.pendingAction.token}")
		token = strings.TrimSpace(string(tk))
		return token != ""
	}); err != nil {
		t.Fatalf("run never reached RequiresAction with a token: %v", err)
	}

	// 2. Deny with the matching token → terminal Cancelled, no pod.
	patch := `{"spec":{"decision":{"token":"` + token + `","approve":false,"reason":"e2e-reject"}}}`
	if _, err := env.Exec(ctx, ExecTarget{}, "patch", "-n", "tenant-a", "agentrun", run, "--type=merge", "-p", patch); err != nil {
		t.Fatalf("apply reject decision: %v", err)
	}
	if err := env.WaitFor(ctx, "run-cancelled", 60*time.Second, func(ctx context.Context) bool {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "agentrun", run, "-o", "jsonpath={.status.state}")
		return strings.TrimSpace(string(st)) == "Cancelled"
	}); err != nil {
		t.Fatalf("denied run did not reach Cancelled: %v", err)
	}
	tr, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "agentrun", run, "-o", "jsonpath={.status.terminationReason}")
	if !strings.Contains(string(tr), "decision:denied") {
		t.Errorf("terminationReason=%q, want it to carry decision:denied", tr)
	}
	if out, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "pod", run, "--ignore-not-found", "-o", "name"); strings.TrimSpace(string(out)) != "" {
		t.Errorf("a pod exists for a denied run: %q", out)
	}
	t.Log("M5 gate: denied run reached terminal Cancelled (decision:denied) with no pod")
}

var approvalTimeout = Scenario{
	ID:       "R-E2E-SCN-APPROVAL-TIMEOUT",
	Name:     "prerun-approval-timeout",
	Requires: CapKubernetes,
	Run:      runApprovalTimeout,
}

// runApprovalTimeout exercises the M5 gate's TIMEOUT path (the third terminal
// outcome alongside approve→proceed and reject→Cancelled): an un-decided run
// whose Agent sets a short spec.approval.approvalTimeoutSeconds must expire to a
// terminal Expired state (TerminationReason "approval:timeout") with NO pod. This
// is gated by the AGENT-level approval policy (approvalGate/Reject use the
// run-level override), so it also covers that branch. Dedicated namespace.
func runApprovalTimeout(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const ns, run = "e2e-approval-to", "approval-timeout-test"
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentrun", run, "-n", ns, "--ignore-not-found")
	manifest := []byte(`apiVersion: v1
kind: Namespace
metadata: {name: e2e-approval-to}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: {name: prov, namespace: e2e-approval-to}
spec:
  kind: anthropic
  endpoint: https://api.anthropic.com
  secretRef: {secretName: anthropic-key}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: {name: to-agent, namespace: e2e-approval-to}
spec:
  model: {providerRef: prov, name: m}
  instructions: "hi"
  budget: {maxSteps: 1, maxTokens: 100, maxWallClockSeconds: 10, maxToolCalls: 0}
  approval: {requireApprovalBeforeRun: true, approvalTimeoutSeconds: 2}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentRun
metadata: {name: approval-timeout-test, namespace: e2e-approval-to}
spec:
  agentRef: to-agent
  input:
    q: "let me expire"
`)
	if err := env.Apply(ctx, manifest); err != nil {
		t.Fatalf("apply approval-timeout manifest: %v", err)
	}

	// The 2s timeout fires on the controller's requeue; allow generous margin.
	if err := env.WaitFor(ctx, "run-expired", 75*time.Second, func(ctx context.Context) bool {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.state}")
		return strings.TrimSpace(string(st)) == "Expired"
	}); err != nil {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.state}")
		t.Fatalf("un-decided run did not expire (observed state %q): %v", strings.TrimSpace(string(st)), err)
	}
	tr, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.terminationReason}")
	if !strings.Contains(string(tr), "approval:timeout") {
		t.Errorf("terminationReason=%q, want it to carry approval:timeout", tr)
	}
	if out, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "pod", run, "--ignore-not-found", "-o", "name"); strings.TrimSpace(string(out)) != "" {
		t.Errorf("a pod exists for an expired run: %q", out)
	}
	t.Log("M5 gate: un-decided run expired to terminal Expired (approval:timeout) with no pod")
}

var egressFloor = Scenario{
	ID:       "R-E2E-SCN-EGRESS-FLOOR",
	Name:     "serving-pod-egress-floor",
	Requires: CapKubernetes,
	Run:      runEgressFloor,
}

// runEgressFloor exercises the M1.17 default-ON serving egress floor: the
// operator's EgressFloorReconciler must have created an egress NetworkPolicy
// selecting the served pods of the SmolAgent kind-verify.sh already applied
// (tenant-a/hello → hello-serving-egress).
func runEgressFloor(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const np = "hello-serving-egress"
	if err := env.WaitFor(ctx, "serving-egress-floor", 60*time.Second, func(ctx context.Context) bool {
		out, err := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "networkpolicy", np, "-o", "jsonpath={.metadata.name}")
		return err == nil && strings.TrimSpace(string(out)) == np
	}); err != nil {
		t.Fatalf("default-ON serving egress NetworkPolicy %q not created: %v", np, err)
	}
	pt, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "networkpolicy", np, "-o", "jsonpath={.spec.policyTypes}")
	if !strings.Contains(string(pt), "Egress") {
		t.Errorf("policyTypes=%s, want it to include Egress", pt)
	}
	sel, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", "tenant-a", "networkpolicy", np, "-o", "jsonpath={.spec.podSelector.matchLabels}")
	if !strings.Contains(string(sel), "hello") {
		t.Errorf("podSelector=%s, want it to select the served (hello) pods", sel)
	}
	t.Log("M1.17: default-ON serving egress floor present + selects the served pods")
}

var policyGate = Scenario{
	ID:       "R-E2E-SCN-POLICY-WEBHOOK",
	Name:     "agentpolicy-webhook-denies-provider",
	Requires: CapKubernetes | CapWebhook,
	Run:      runPolicyGate,
}

// runPolicyGate exercises the M1.6 AgentPolicy ADMISSION webhook (failurePolicy=Fail)
// through the real operator: with a namespace AgentPolicy whose allow-list excludes a
// provider, applying an Agent referencing it must be REJECTED AT ADMISSION (kubectl
// apply fails) — the primary fail-closed enforcement, distinct from the M1.5 reconcile
// backstop (runPolicyReconcileGate). Regression guard for a real wiring gap fixed here:
// the webhook handlers were registered in main.go but the ValidatingWebhookConfiguration
// omitted agents/agentruns, so the webhook silently never fired. Dedicated namespace.
func runPolicyGate(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const ns, agent = "e2e-policy", "denied-agent"
	// Reset for idempotency: drop any prior denied-agent (a pre-fix run may have left
	// one admitted+Failed before the webhook was wired).
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agent", agent, "-n", ns, "--ignore-not-found")
	// 1. Prerequisites (namespace + provider + the excluding policy) must persist
	//    BEFORE the Agent is admitted, so the webhook's policy list sees them.
	prereq := []byte(`apiVersion: v1
kind: Namespace
metadata: {name: e2e-policy}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: {name: anthropic-prov, namespace: e2e-policy}
spec:
  kind: anthropic
  endpoint: https://api.anthropic.com
  secretRef: {secretName: anthropic-key}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentPolicy
metadata: {name: only-openai, namespace: e2e-policy}
spec:
  allowedProviders: ["openai-prov"]
`)
	if err := env.Apply(ctx, prereq); err != nil {
		t.Fatalf("apply policy prerequisites: %v", err)
	}
	// 2. The violating Agent must be REJECTED at admission by the M1.6 webhook.
	deniedAgent := []byte(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: {name: denied-agent, namespace: e2e-policy}
spec:
  model: {providerRef: anthropic-prov, name: m}
  instructions: "hi"
  budget: {maxSteps: 1, maxTokens: 100, maxWallClockSeconds: 10, maxToolCalls: 0}
`)
	err := env.Apply(ctx, deniedAgent)
	if err == nil {
		_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agent", agent, "-n", ns, "--ignore-not-found")
		t.Fatal("M1.6 webhook FAILED to reject a policy-violating Agent at admission (apply succeeded)")
	}
	if !strings.Contains(err.Error(), "AgentPolicy allow-list") {
		t.Errorf("admission rejection message missing %q: %v", "AgentPolicy allow-list", err)
	}
	t.Log("M1.6: AgentPolicy admission webhook rejected the disallowed-provider Agent at apply (fail-closed)")
}

var policyReconcileGate = Scenario{
	ID:       "R-E2E-SCN-POLICY-RECONCILE",
	Name:     "agentpolicy-reconcile-backstop",
	Requires: CapKubernetes,
	Run:      runPolicyReconcileGate,
}

// runPolicyReconcileGate exercises the M1.5 reconcile backstop (the layer behind the
// M1.6 admission webhook): an Agent admitted under a CONFORMING policy is later flipped
// to Failed/PolicyViolation when the policy is TIGHTENED to exclude its provider. The
// webhook doesn't re-admit an existing Agent, so the AgentPolicy→Agent watch + reconcile
// gate (agent_controller SetupWithManager) is what catches a post-hoc policy change.
func runPolicyReconcileGate(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const ns, agent, policy = "e2e-policy-reconcile", "tighten-agent", "prov-policy"
	// Reset: drop the Agent + (re-apply below) reset the policy to MATCHING so the
	// conforming Agent admits cleanly on a reused cluster (a prior run leaves it tightened).
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agent", agent, "-n", ns, "--ignore-not-found")
	prereq := []byte(`apiVersion: v1
kind: Namespace
metadata: {name: e2e-policy-reconcile}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: {name: anthropic-prov, namespace: e2e-policy-reconcile}
spec:
  kind: anthropic
  endpoint: https://api.anthropic.com
  secretRef: {secretName: anthropic-key}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentPolicy
metadata: {name: prov-policy, namespace: e2e-policy-reconcile}
spec:
  allowedProviders: ["anthropic-prov"]
`)
	if err := env.Apply(ctx, prereq); err != nil {
		t.Fatalf("apply matching-policy prerequisites: %v", err)
	}
	// Conforming Agent admits under the matching policy and reconciles Ready.
	conforming := []byte(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: {name: tighten-agent, namespace: e2e-policy-reconcile}
spec:
  model: {providerRef: anthropic-prov, name: m}
  instructions: "hi"
  budget: {maxSteps: 1, maxTokens: 100, maxWallClockSeconds: 10, maxToolCalls: 0}
`)
	if err := env.Apply(ctx, conforming); err != nil {
		t.Fatalf("conforming agent rejected unexpectedly (webhook over-broad?): %v", err)
	}
	if err := env.WaitFor(ctx, "agent-ready", 60*time.Second, func(ctx context.Context) bool {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", agent, "-o", "jsonpath={.status.phase}")
		return strings.TrimSpace(string(ph)) == "Ready"
	}); err != nil {
		t.Fatalf("conforming agent never reached Ready: %v", err)
	}
	// Tighten the policy to exclude the provider → the AgentPolicy watch re-enqueues
	// the Agent → the reconcile gate flips it to Failed/PolicyViolation (no re-admission).
	patch := `{"spec":{"allowedProviders":["openai-prov"]}}`
	if _, err := env.Exec(ctx, ExecTarget{}, "patch", "-n", ns, "agentpolicy", policy, "--type=merge", "-p", patch); err != nil {
		t.Fatalf("tighten policy: %v", err)
	}
	assertAgentFailedClosed(t, env, ctx, ns, agent, "PolicyViolation")
	t.Log("M1.5: tightening the AgentPolicy flipped the already-admitted Agent to Failed/PolicyViolation via the reconcile backstop")
}

var toolKindGuard = Scenario{
	ID:       "R-E2E-SCN-TOOL-KIND",
	Name:     "unwired-tool-kind-failclosed",
	Requires: CapKubernetes,
	Run:      runToolKindGuard,
}

// runToolKindGuard exercises the M2.16 fail-closed tool-kind guard through the
// real operator: a loop-mode Agent referencing a Tool whose kind has no
// production loop invoker (kind=agent) must be flipped to
// Failed/ToolKindUnsupported rather than silently scheduled. Dedicated namespace
// for isolation.
func runToolKindGuard(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const ns, agent = "e2e-toolkind", "loopy"
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agent", agent, "-n", ns, "--ignore-not-found")
	manifest := []byte(`apiVersion: v1
kind: Namespace
metadata: {name: e2e-toolkind}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: {name: prov, namespace: e2e-toolkind}
spec:
  kind: anthropic
  endpoint: https://api.anthropic.com
  secretRef: {secretName: anthropic-key}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Tool
metadata: {name: fn, namespace: e2e-toolkind}
spec:
  kind: function
  function: {name: noop}
  inputSchema: {type: object}
  outputSchema: {type: object}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: {name: loopy, namespace: e2e-toolkind}
spec:
  model: {providerRef: prov, name: m}
  instructions: "hi"
  tools:
    - name: fn
  budget: {maxSteps: 1, maxTokens: 100, maxWallClockSeconds: 10, maxToolCalls: 0}
`)
	if err := env.Apply(ctx, manifest); err != nil {
		t.Fatalf("apply tool-kind manifest: %v", err)
	}
	// kind=function has no production invoker (test-only) and stays reserved even
	// after A2A (kind=agent) was wired, so it remains the canonical fail-closed
	// case: a loop Agent referencing it must flip Failed/ToolKindUnsupported.
	assertAgentFailedClosed(t, env, ctx, ns, agent, "ToolKindUnsupported")
	t.Log("M2.16: loop Agent with an unwired kind=function Tool was failed closed (ToolKindUnsupported)")
}

var stdioMCPGate = Scenario{
	ID:       "R-E2E-SCN-STDIO-MCP",
	Name:     "stdio-mcp-not-allowlisted-failclosed",
	Requires: CapKubernetes,
	Run:      runStdioMCPGate,
}

// runStdioMCPGate exercises the M2.15 stdio-MCP allow-list gate through the real
// operator: a loop-mode Agent referencing a kind=mcp Tool whose URL is a stdio
// (mcp://) endpoint NOT on the operator allow-list must be flipped to
// Failed/StdioMCPNotAllowed (arbitrary tenant stdio is denied; http(s) MCP is
// unaffected). The L1 operator ships an empty allow-list, so the deny path needs
// no extra flag. Dedicated namespace for isolation.
func runStdioMCPGate(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const ns, agent = "e2e-mcp", "mcpy"
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agent", agent, "-n", ns, "--ignore-not-found")
	manifest := []byte(`apiVersion: v1
kind: Namespace
metadata: {name: e2e-mcp}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: {name: prov, namespace: e2e-mcp}
spec:
  kind: anthropic
  endpoint: https://api.anthropic.com
  secretRef: {secretName: anthropic-key}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Tool
metadata: {name: stdio-mcp, namespace: e2e-mcp}
spec:
  kind: mcp
  inputSchema: {type: object}
  outputSchema: {type: object}
  mcp: {url: "mcp://local-server"}
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: {name: mcpy, namespace: e2e-mcp}
spec:
  model: {providerRef: prov, name: m}
  instructions: "hi"
  tools:
    - name: stdio-mcp
  budget: {maxSteps: 1, maxTokens: 100, maxWallClockSeconds: 10, maxToolCalls: 0}
`)
	if err := env.Apply(ctx, manifest); err != nil {
		t.Fatalf("apply stdio-mcp manifest: %v", err)
	}
	assertAgentFailedClosed(t, env, ctx, ns, agent, "StdioMCPNotAllowed")
	t.Log("M2.15: loop Agent with an un-allow-listed stdio (mcp://) MCP Tool was failed closed (StdioMCPNotAllowed)")
}

// assertAgentFailedClosed waits for an Agent to reach Failed/<reason> via the
// real reconciler, failing the test (with the observed phase/reason for a
// precise pointer) if it does not converge in time.
func assertAgentFailedClosed(t *testing.T, env Env, ctx context.Context, ns, name, reason string) {
	t.Helper()
	if err := env.WaitFor(ctx, "agent-failclosed-"+reason, 60*time.Second, func(ctx context.Context) bool {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", name, "-o", "jsonpath={.status.phase}")
		rs, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", name, "-o", "jsonpath={.status.reason}")
		return strings.TrimSpace(string(ph)) == "Failed" && strings.TrimSpace(string(rs)) == reason
	}); err != nil {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", name, "-o", "jsonpath={.status.phase}")
		rs, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", name, "-o", "jsonpath={.status.reason}")
		t.Fatalf("agent %s/%s never reached Failed/%s (observed %q/%q): %v", ns, name, reason, strings.TrimSpace(string(ph)), strings.TrimSpace(string(rs)), err)
	}
}

var webhook = Scenario{
	ID:       "R-E2E-SCN-WEBHOOK",
	Name:     "webhook-rejects-bad-specs",
	Requires: CapKubernetes | CapWebhook,
	Run:      runWebhook,
}

// runWebhook exercises both validating webhooks the operator ships:
//
//  1. SmolAgent: mode=insecure without the allow-insecure
//     annotation must be rejected (R-OP-WH-1).
//  2. AgentNetwork: setting both `identityProxy` and `wireguardMesh`
//     simultaneously must be rejected by the transport-mutex
//     validation (R-AN-API-1).
//
// Each rejection's error message is sniffed for the corresponding
// validation reason so a regression in one webhook fails this
// scenario with a precise pointer.
func runWebhook(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cases := []struct {
		name    string
		yaml    string
		matches []string
		cleanup []string
	}{
		{
			name: "smolagent-insecure-no-annotation",
			yaml: `apiVersion: agents.smol-agents.ai/v1
kind: SmolAgent
metadata: {name: webhook-bad-mode, namespace: tenant-a}
spec:
  trustDomain: smol-agents.ai
  mode: insecure
`,
			matches: []string{"denied", "allow-insecure"},
			cleanup: []string{"delete", "smolagent", "webhook-bad-mode", "-n", "tenant-a"},
		},
		{
			name: "agentnetwork-both-transports",
			yaml: `apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentNetwork
metadata: {name: webhook-bad-anet, namespace: tenant-a}
spec:
  kind: identityProxy
  identityProxy:
    resources:
      - {name: x, kind: tcp, localAddr: "127.0.0.1:5432", gateway: g.svc:8443, authorize: ["spiffe://x"]}
  wireguardMesh:
    mode: client
    privateKeyRef: {secretName: x}
`,
			matches: []string{"denied", "wireguardMesh must be nil"},
			cleanup: []string{"delete", "agentnetwork", "webhook-bad-anet", "-n", "tenant-a"},
		},
		{
			// M2.22: an AgentSession referencing a non-existent Agent must be
			// rejected at admission (regression guard for the same webhook-config
			// gap fixed for the agentpolicy gate — agentsessions was omitted from
			// the ValidatingWebhookConfiguration, silencing this webhook).
			name: "agentsession-dangling-agentref",
			yaml: `apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentSession
metadata: {name: webhook-bad-session, namespace: tenant-a}
spec:
  agentRef: no-such-agent-xyz
`,
			matches: []string{"denied", "no such Agent"},
			cleanup: []string{"delete", "agentsession", "webhook-bad-session", "-n", "tenant-a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := env.Apply(ctx, []byte(tc.yaml))
			if err == nil {
				_, _ = env.Exec(ctx, ExecTarget{}, tc.cleanup...)
				t.Fatalf("expected apiserver to reject %s; admission accepted", tc.name)
			}
			msg := err.Error()
			for _, want := range tc.matches {
				if !strings.Contains(msg, want) {
					t.Errorf("rejection message missing %q: %v", want, err)
				}
			}
			t.Logf("rejected as expected: %s",
				strings.SplitN(msg, "\n", 2)[0])
		})
	}
}

var kataIsolation = Scenario{
	ID:       "R-E2E-SCN-KATA",
	Name:     "kata-microvm-runs",
	Requires: CapKubernetes | CapKata,
	Run:      runKataIsolation,
}

// runKataIsolation proves a Pod with runtimeClassName=kata-fc runs
// under a Firecracker microVM whose guest kernel differs from the
// host kernel. The host kernel comes from kubelet's
// nodeInfo.kernelVersion (no host shell needed); the pod kernel
// comes from `uname -r` inside a one-shot Pod. If both match, the
// Pod is sharing the host kernel — i.e. Kata silently fell back to
// runc.
func runKataIsolation(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// RuntimeClass kata-fc must exist; otherwise the Pod is rejected
	// at admission with a clearer error than "kernel matched".
	if out, err := env.Exec(ctx, ExecTarget{},
		"get", "runtimeclass", "kata-fc",
		"-o", "jsonpath={.metadata.name}"); err != nil ||
		strings.TrimSpace(string(out)) != "kata-fc" {
		t.Skipf("RuntimeClass kata-fc not registered (out=%q err=%v) — "+
			"L2 bootstrap should land it before scenarios run", out, err)
	}

	hostOut, err := env.Exec(ctx, ExecTarget{},
		"get", "nodes",
		"-o", "jsonpath={.items[0].status.nodeInfo.kernelVersion}")
	if err != nil {
		t.Fatalf("read host kernel: %v\nout: %s", err, hostOut)
	}
	hostKernel := strings.TrimSpace(string(hostOut))
	if hostKernel == "" {
		t.Fatalf("host kernel empty — kubelet may not have populated nodeInfo yet")
	}

	pod := fmt.Sprintf("kata-uname-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: { name: %s, namespace: tenant-a }
spec:
  restartPolicy: Never
  runtimeClassName: kata-fc
  containers:
    - name: uname
      image: docker.io/library/busybox:1.36
      command: ["sh", "-c", "uname -r"]
`, pod)
	if err := env.Apply(ctx, []byte(manifest)); err != nil {
		t.Fatalf("apply kata pod: %v", err)
	}
	defer func() {
		_, _ = env.Exec(ctx, ExecTarget{},
			"-n", "tenant-a", "delete", "pod", pod, "--ignore-not-found")
	}()

	// Pod must reach Succeeded; Failed or Pending past deadline both
	// mean Kata didn't run. But "Pending + FailedCreatePodSandBox"
	// usually points at a kata-fc bring-up gap (missing binaries,
	// shim path mismatch, virtiofsd/snapshotter issue) rather than
	// a regression — treat that as Skip so a half-wired runtime
	// doesn't block the rest of the suite.
	if err := env.WaitFor(ctx, "kata-pod-succeeded", 90*time.Second,
		func(ctx context.Context) bool {
			out, err := env.Exec(ctx, ExecTarget{},
				"-n", "tenant-a", "get", "pod", pod,
				"-o", "jsonpath={.status.phase}")
			return err == nil && strings.TrimSpace(string(out)) == "Succeeded"
		}); err != nil {
		desc, _ := env.Exec(ctx, ExecTarget{},
			"-n", "tenant-a", "describe", "pod", pod)
		if strings.Contains(string(desc), "FailedCreatePodSandBox") {
			t.Skipf("kata-fc sandbox creation failed — kata-fc 3.10 "+
				"requires a block-device-backed snapshotter "+
				"(devmapper or nydus) and k0s ships overlayfs only. "+
				"Phase D follow-up: install nydus-snapshotter + "+
				"per-runtime snapshotter selection, OR set up a "+
				"devmapper thinpool in the cloud-init.\n"+
				"describe:\n%s", desc)
		}
		t.Fatalf("kata pod did not Succeed: %v\ndescribe:\n%s", err, desc)
	}

	logsOut, err := env.Exec(ctx, ExecTarget{},
		"-n", "tenant-a", "logs", pod)
	if err != nil {
		t.Fatalf("kubectl logs %s: %v", pod, err)
	}
	podKernel := strings.TrimSpace(string(logsOut))
	if podKernel == "" {
		t.Fatalf("pod kernel empty — uname produced no output")
	}
	if podKernel == hostKernel {
		t.Fatalf("kata Pod kernel == host kernel (%q); runtimeClass silently fell back to runc",
			podKernel)
	}
	t.Logf("kata-fc isolation OK: host=%q pod=%q", hostKernel, podKernel)
}

var smolAgentPhase = Scenario{
	ID:       "R-E2E-SCN-KA-PHASE",
	Name:     "smolagent-status-phase-ready",
	Requires: CapKubernetes,
	Run:      runSmolAgentPhase,
}

// runSmolAgentPhase asserts that a SmolAgent CR reconciled by
// the operator reaches phase=Ready in-cluster. Uses the existing
// `tenant-a/hello` CR brought up by scripts/kind-verify.sh — the
// L1 driver hooks into that path. Doesn't apply a new CR; the point
// is to verify the operator's status reconciliation path works
// end-to-end against a live apiserver.
func runSmolAgentPhase(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := env.WaitFor(ctx, "smolagent-phase-ready", 60*time.Second, func(ctx context.Context) bool {
		out, err := env.Exec(ctx, ExecTarget{},
			"get", "-n", "tenant-a", "smolagent", "hello",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(out)) == "Ready"
	})
	if err != nil {
		// Fetch the current state for the failure log.
		out, _ := env.Exec(ctx, ExecTarget{},
			"get", "-n", "tenant-a", "smolagent", "hello",
			"-o", "jsonpath={.status}")
		t.Fatalf("SmolAgent hello never reached phase=Ready: %v\nstatus: %s", err, out)
	}
	t.Log("SmolAgent tenant-a/hello reconciled to phase=Ready")
}

var secretlessEgress = Scenario{
	ID:       "R-E2E-SCN-SECRETLESS",
	Name:     "secretless-egress-github",
	Requires: CapSPIRE | CapInClusterProbe,
	Run:      runSecretlessEgress,
}

// runSecretlessEgress drives the full secretless-egress chain from inside the
// cluster (the sidecar's resource listener binds loopback, so it can't be
// reached from the test process — same constraint as proxy-http). The
// in-cluster spiffe-probe runs an in-process broker (real SO_PEERCRED
// attestation, GitHubAppBackend→fake-github, JWKS verifier→fake-tts) and a
// real agentnet proxy that mints a TraT, mints a GitHub token via the broker,
// and injects it. It asserts the upstream rejects a non-minted token, the
// sidecar call succeeds, and fake-github observed a broker-minted token —
// proving agent-blind injection. R-E2E-SCN-SECRETLESS.
func runSecretlessEgress(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	lines, err := env.RunSpiffeProbe(ctx,
		[]string{"secretless"},
		"--github-url=http://fake-github.tenant-a.svc.cluster.local:8080",
		"--tts-url=http://fake-tts.tenant-a.svc.cluster.local:8080")
	if err != nil {
		t.Fatalf("RunSpiffeProbe: %v", err)
	}
	assertProbeOK(t, lines, "secretless")
}

var memoryAccess = Scenario{
	ID:       "R-E2E-SCN-MEMORY",
	Name:     "memory-mcp-tenant-isolation",
	Requires: CapSPIRE | CapInClusterProbe,
	Run:      runMemoryAccess,
}

// runMemoryAccess drives the memory-mcp gateway from inside the cluster via the
// spiffe-probe: the probe authenticates with its JWT-SVID, writes + retrieves a
// document through the MCP gateway (proving the gateway injects the caller's
// tenant and the worker returns it), and confirms a retriever scoped to another
// tenant is denied. Real SPIRE end-to-end. R-E2E-SCN-MEMORY.
func runMemoryAccess(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	lines, err := env.RunSpiffeProbe(ctx,
		[]string{"memory"},
		"--memory-mcp-url=http://memory-mcp.tenant-a.svc.cluster.local:8443",
		"--retriever-ref=tenant-a/kb",
		"--foreign-retriever-ref=tenant-b/kb")
	if err != nil {
		t.Fatalf("RunSpiffeProbe: %v", err)
	}
	assertProbeOK(t, lines, "memory")
}

// todo returns a Run that fails-loudly with a useful message until
// the scenario body lands. Tests using this helper get a clear "not
// yet implemented" signal instead of a silent pass.
func todo(message string) func(t *testing.T, env Env) {
	return func(t *testing.T, _ Env) {
		t.Helper()
		t.Skipf("scenario not yet implemented: %s", message)
	}
}

// agentSession (R-E2E-SCN-AGENTSESSION) exercises the M4 long-running session
// control plane. It applies a minimal loop Agent (provider + single-key secret,
// kata-fc default sandbox, no tools) and an AgentSession, then asserts the
// AgentSessionReconciler drives it through agent-lookup -> sandbox-resolve ->
// secret-gather -> run-spec ConfigMap + egress policy and FAIL-CLOSES at the
// node-placement gate: a single-node cluster with no AgentNodePool matching the
// kata class yields Pending/NoKVMCapacity (R-PROV-2). Reaching that reason
// proves the whole pre-placement pipeline ran (any earlier failure returns a
// different reason or an error), so it is real e2e of the M4 controller without
// requiring node-pool autoscaling, the agentfs-sidecar image, or a live
// serve-session worker (the happy-path turn datapath is a separate concern).
var agentSession = Scenario{
	ID:       "R-E2E-SCN-AGENTSESSION",
	Name:     "agentsession-reconcile-failclosed-placement",
	Requires: CapKubernetes,
	Run:      runAgentSession,
}

func runAgentSession(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const ns = "tenant-a"
	for _, d := range [][]string{
		{"agentsession", "e2e-sess"},
		{"agent", "e2e-sess-agent"},
		{"modelprovider", "e2e-sess-prov"},
		{"secret", "e2e-sess-key"},
	} {
		_, _ = env.Exec(ctx, ExecTarget{}, "delete", d[0], d[1], "-n", ns, "--ignore-not-found")
	}

	// The session defaults to the kata-fc runtimeClass (the operator's
	// fail-closed default; the L1 webhook overlay carries no runc override). On
	// a kataless kind cluster the operator never registers the kata-fc
	// RuntimeClass — the SmolAgent sandbox feature only provisions it alongside a
	// matching AgentNodePool, and without a pool it returns NoKVMCapacity / falls
	// back to gVisor. So without this fixture the session stops at the earlier
	// SandboxNotReady gate and never reaches the placement gate this scenario
	// asserts. Register a bare kata-fc RuntimeClass so sandbox resolution passes;
	// placement then fails closed with NoKVMCapacity (no AgentNodePool).
	//
	// CRITICAL: only register (and later remove) the fixture when kata-fc is
	// ABSENT. On a real kata cluster (L2) kata-fc already exists as a SHARED
	// cluster-scoped resource we did NOT create — deleting it on exit would break
	// every later scenario that needs kata isolation (A2A, AGENTSESSION-RUN).
	// Never delete a RuntimeClass we didn't create.
	rcOut, _ := env.Exec(ctx, ExecTarget{}, "get", "runtimeclass", "kata-fc", "--ignore-not-found", "-o", "name")
	if strings.TrimSpace(string(rcOut)) == "" {
		if err := env.Apply(ctx, []byte(`apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: kata-fc }
handler: kata-fc
`)); err != nil {
			t.Fatalf("register kata-fc RuntimeClass fixture: %v", err)
		}
		// Cluster-scoped + removed on exit (fresh context, runs even on t.Fatalf)
		// so a kataless cluster returns to its natural state. Guarded above so we
		// only ever remove the fixture WE registered.
		defer func() {
			dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer dcancel()
			_, _ = env.Exec(dctx, ExecTarget{}, "delete", "runtimeclass", "kata-fc", "--ignore-not-found")
		}()
	}

	// Single-key secret: resolveSecret with an empty key returns the value only
	// when the secret has exactly one data key (gatherRunSecrets/readSecretKey).
	setup := []byte(`apiVersion: v1
kind: Secret
metadata: { name: e2e-sess-key, namespace: tenant-a }
stringData: { apiKey: "dummy-unused-pre-placement" }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: { name: e2e-sess-prov, namespace: tenant-a }
spec:
  kind: openai
  endpoint: http://fake-llm.tenant-a.svc.cluster.local:8080
  secretRef: { secretName: e2e-sess-key }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: e2e-sess-agent, namespace: tenant-a }
spec:
  model: { providerRef: e2e-sess-prov, name: glm-4.6 }
  instructions: "M4 session reconcile e2e"
  budget: { maxSteps: 4, maxTokens: 2000, maxWallClockSeconds: 60, maxToolCalls: 2 }
`)
	if err := env.Apply(ctx, setup); err != nil {
		t.Fatalf("apply session agent + provider + secret: %v", err)
	}
	if err := env.Apply(ctx, []byte(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentSession
metadata: { name: e2e-sess, namespace: tenant-a }
spec:
  agentRef: e2e-sess-agent
`)); err != nil {
		t.Fatalf("apply agentsession: %v", err)
	}

	if err := env.WaitFor(ctx, "agentsession-nokvmcapacity", 90*time.Second, func(ctx context.Context) bool {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentsession", "e2e-sess", "-o", "jsonpath={.status.phase}")
		rs, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentsession", "e2e-sess", "-o", "jsonpath={.status.reason}")
		return strings.TrimSpace(string(ph)) == "Pending" && strings.TrimSpace(string(rs)) == "NoKVMCapacity"
	}); err != nil {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentsession", "e2e-sess", "-o", "jsonpath={.status.phase}")
		rs, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentsession", "e2e-sess", "-o", "jsonpath={.status.reason}")
		t.Fatalf("AgentSession never reached Pending/NoKVMCapacity (observed %q/%q): %v",
			strings.TrimSpace(string(ph)), strings.TrimSpace(string(rs)), err)
	}
	t.Log("M4: AgentSession reconciled agent->sandbox->secret->configmap/egress then fail-closed at placement (NoKVMCapacity)")
}

// agentSessionRunning (R-E2E-SCN-AGENTSESSION-RUN) is the M4 happy path: a live
// long-running session worker reaching Running. It provisions a kata-fc
// AgentNodePool and labels the (single) node so the placement gate resolves,
// applies a loop Agent with an ephemeral AgentFS workspace (serve-session
// requires a workspace) + provider/secret, and an AgentSession — then asserts
// the AgentSessionReconciler brings the worker Deployment to Available and
// reports status.phase=Running.
//
// This exercises the FULL M4 datapath on real kata metal: the operator renders
// the session run-pod (SMOL_AGENTS_IMAGE_REGISTRY/_TAG → ECR images), schedules
// it on the kata-fc node, the AgentFS restore init + serve sidecars come up
// (ephemeral: no S3 backup configured → fresh workspace, no-op backups), the
// secret broker serves the provider key, and `agent serve-session` loads the
// spec, opens the workspace, and blocks on its on-disk inbox — so the pod is
// Ready and the Deployment Available. The ephemeral path depends on the
// agentfs-sidecar gracefully skipping S3 when no bucket is configured.
var agentSessionRunning = Scenario{
	ID:       "R-E2E-SCN-AGENTSESSION-RUN",
	Name:     "agentsession-worker-reaches-running",
	Requires: CapKubernetes | CapKata,
	Run:      runAgentSessionRunning,
}

func runAgentSessionRunning(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const ns = "tenant-a"
	const pool = "e2e-sessr-pool"
	for _, d := range [][]string{
		{"agentsession", "e2e-sessr"}, {"agent", "e2e-sessr-agent"},
		{"modelprovider", "e2e-sessr-prov"}, {"secret", "e2e-sessr-key"},
	} {
		_, _ = env.Exec(ctx, ExecTarget{}, "delete", d[0], d[1], "-n", ns, "--ignore-not-found")
	}
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentnodepool", pool, "--ignore-not-found")

	// 1. kata-fc AgentNodePool (ResolvePlacementForClass matches by isolation) +
	//    label the single node with the pool label the placement affinity needs.
	//    The Karpenter NodePool the operator tries to render is harmless here —
	//    placement only Lists AgentNodePool CRs.
	if err := env.Apply(ctx, []byte(`apiVersion: agents.smol-agents.ai/v1
kind: AgentNodePool
metadata: { name: e2e-sessr-pool }
spec:
  isolation: kata-fc
  arch: arm64
  bootstrap: { mode: UserData, distro: al2023 }
`)); err != nil {
		t.Fatalf("apply AgentNodePool: %v", err)
	}
	nodeOut, err := env.Exec(ctx, ExecTarget{}, "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	node := strings.TrimSpace(string(nodeOut))
	if err != nil || node == "" {
		t.Fatalf("get node name: %v (out=%q)", err, nodeOut)
	}
	if _, err := env.Exec(ctx, ExecTarget{}, "label", "node", node, "agents.smol-agents.ai/pool="+pool, "--overwrite"); err != nil {
		t.Fatalf("label node %s: %v", node, err)
	}

	// 2. Minimal loop Agent: ephemeral AgentFS workspace + provider/secret.
	if err := env.Apply(ctx, []byte(`apiVersion: v1
kind: Secret
metadata: { name: e2e-sessr-key, namespace: tenant-a }
stringData: { apiKey: "dummy-session-running" }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: { name: e2e-sessr-prov, namespace: tenant-a }
spec:
  kind: openai
  endpoint: http://fake-llm.tenant-a.svc.cluster.local:8080
  secretRef: { secretName: e2e-sessr-key }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: e2e-sessr-agent, namespace: tenant-a }
spec:
  model: { providerRef: e2e-sessr-prov, name: glm-4.6 }
  instructions: "M4 session worker e2e"
  budget: { maxSteps: 4, maxTokens: 2000, maxWallClockSeconds: 120, maxToolCalls: 2 }
  storage:
    kind: agentfs
    agentfs: { sizeGiB: 1, mountPath: /workspace }
`)); err != nil {
		t.Fatalf("apply session agent: %v", err)
	}
	if err := env.Apply(ctx, []byte(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentSession
metadata: { name: e2e-sessr, namespace: tenant-a }
spec:
  agentRef: e2e-sessr-agent
`)); err != nil {
		t.Fatalf("apply agentsession: %v", err)
	}

	// 3. The reconciler schedules the worker on the kata node (operator-resolved
	//    ECR images), the AgentFS + broker sidecars and serve-session come up, the
	//    Deployment goes Available -> controller sets phase=Running.
	if err := env.WaitFor(ctx, "agentsession-running", 3*time.Minute, func(ctx context.Context) bool {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentsession", "e2e-sessr", "-o", "jsonpath={.status.phase}")
		return strings.TrimSpace(string(ph)) == "Running"
	}); err != nil {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentsession", "e2e-sessr", "-o", "jsonpath={.status.phase}")
		rs, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentsession", "e2e-sessr", "-o", "jsonpath={.status.reason}")
		pods, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "pods", "-o", "wide")
		t.Fatalf("AgentSession never reached Running (phase=%q reason=%q): %v\npods:\n%s",
			strings.TrimSpace(string(ph)), strings.TrimSpace(string(rs)), err, pods)
	}
	// Confirm the worker really landed on kata-fc with operator-resolved ECR images
	// (not a fallback) so "Running" reflects the full datapath, not a shortcut.
	rc, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "pods",
		"-l", "agents.smol-agents.ai/run=e2e-sessr-session", "-o", "jsonpath={.items[0].spec.runtimeClassName}")
	if got := strings.TrimSpace(string(rc)); got != "kata-fc" {
		t.Errorf("session worker runtimeClassName=%q, want kata-fc", got)
	}
	t.Log("M4: AgentSession worker Deployment Available -> phase=Running (serve-session live on kata-fc)")
}

// a2aComposition (R-E2E-SCN-A2A) is the M3 positive-path: one agent invokes
// another. A parent loop Agent has a kind=agent Tool targeting a child Agent.
// Both run as operator-scheduled kata pods against a deterministic OpenAI-wire
// mock scripted per-agent (keyed on sha256 of each Agent's instructions, which
// the executor sends verbatim as the system message): the PARENT is scripted to
// emit the kind=agent tool call then finalize; the CHILD to finalize directly.
// At runtime the parent's in-pod AgentRunInvoker creates a CHILD AgentRun, polls
// it to terminal, and folds its output. Asserts: (1) a child AgentRun is created
// carrying the parent-run label, and (2) the parent run reaches Completed — i.e.
// the full A2A datapath (loop pod -> agent-call -> child AgentRun -> fold) works.
var a2aComposition = Scenario{
	ID:       "R-E2E-SCN-A2A",
	Name:     "agent-to-agent-composition",
	Requires: CapKubernetes | CapKata,
	Run:      runA2AComposition,
}

func runA2AComposition(t *testing.T, env Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	const ns, pool = "tenant-a", "e2e-a2a-pool"
	const parentRun = "e2e-a2a-run"
	const parentInstr = "e2e-a2a PARENT: call the delegate tool, then finish."
	const childInstr = "e2e-a2a CHILD: return the answer."
	sha := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

	for _, d := range [][]string{
		{"agentrun", parentRun}, {"agent", "e2e-a2a-parent"}, {"agent", "e2e-a2a-child"},
		{"tool", "delegate"}, {"modelprovider", "a2a-prov"}, {"secret", "a2a-key"},
		{"configmap", "a2a-plans"}, {"deployment", "a2a-llm"}, {"service", "a2a-llm"},
	} {
		_, _ = env.Exec(ctx, ExecTarget{}, "delete", d[0], d[1], "-n", ns, "--ignore-not-found")
	}
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentrun", "-n", ns, "-l", "agents.smol-agents.ai/parent-run="+parentRun, "--ignore-not-found")
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentnodepool", pool, "--ignore-not-found")

	// kata placement (single-node L2): a kata-fc AgentNodePool + the pool label
	// on the node, so both parent and child run pods schedule.
	if err := env.Apply(ctx, []byte(`apiVersion: agents.smol-agents.ai/v1
kind: AgentNodePool
metadata: { name: e2e-a2a-pool }
spec: { isolation: kata-fc, arch: arm64, bootstrap: { mode: UserData, distro: al2023 } }
`)); err != nil {
		t.Fatalf("apply AgentNodePool: %v", err)
	}
	nodeOut, err := env.Exec(ctx, ExecTarget{}, "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	node := strings.TrimSpace(string(nodeOut))
	if err != nil || node == "" {
		t.Fatalf("get node: %v (%q)", err, nodeOut)
	}
	if _, err := env.Exec(ctx, ExecTarget{}, "label", "node", node, "agents.smol-agents.ai/pool="+pool, "--overwrite"); err != nil {
		t.Fatalf("label node: %v", err)
	}

	// Deterministic OpenAI-wire mock: parent → [toolCall(delegate), final]; child → [final].
	plans := fmt.Sprintf(`{"plans":{
  %q:{"sequence":[{"toolCall":{"tool":"delegate","arguments":{}}},{"finalAnswer":{"output":{"composed":"by-parent"}}}]},
  %q:{"plan":{"finalAnswer":{"output":{"child":"result"}}}}
}}`, sha(parentInstr), sha(childInstr))
	cm := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata: { name: a2a-plans, namespace: tenant-a }
data:
  plans.json: |
%s
`, indent(plans, "    "))
	if err := env.Apply(ctx, []byte(cm)); err != nil {
		t.Fatalf("apply plans ConfigMap: %v", err)
	}

	// Scripted fake-llm (OpenAI-wire endpoint) + Service, from the L2 ECR.
	llm := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: { name: a2a-llm, namespace: tenant-a }
spec:
  replicas: 1
  selector: { matchLabels: { app: a2a-llm } }
  template:
    metadata: { labels: { app: a2a-llm } }
    spec:
      containers:
        - name: fake-llm
          image: %s
          env: [{ name: PLANS_FILE, value: /plans/plans.json }]
          ports: [{ containerPort: 8080 }]
          volumeMounts: [{ name: plans, mountPath: /plans }]
      volumes: [{ name: plans, configMap: { name: a2a-plans } }]
---
apiVersion: v1
kind: Service
metadata: { name: a2a-llm, namespace: tenant-a }
spec:
  selector: { app: a2a-llm }
  ports: [{ port: 8080, targetPort: 8080 }]
`, ecrImage("fake-llm"))
	if err := env.Apply(ctx, []byte(llm)); err != nil {
		t.Fatalf("apply scripted fake-llm: %v", err)
	}
	if err := env.WaitFor(ctx, "a2a-llm-available", 90*time.Second, func(ctx context.Context) bool {
		out, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "deploy", "a2a-llm", "-o", "jsonpath={.status.availableReplicas}")
		return strings.TrimSpace(string(out)) == "1"
	}); err != nil {
		t.Fatalf("scripted fake-llm not Available: %v", err)
	}

	// Provider + secret + child Agent + delegate Tool + parent Agent.
	setup := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata: { name: a2a-key, namespace: tenant-a }
stringData: { apiKey: "dummy" }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: ModelProvider
metadata: { name: a2a-prov, namespace: tenant-a }
spec: { kind: openai, endpoint: "http://a2a-llm.tenant-a.svc.cluster.local:8080", secretRef: { secretName: a2a-key } }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: e2e-a2a-child, namespace: tenant-a }
spec:
  model: { providerRef: a2a-prov, name: m }
  instructions: %q
  budget: { maxSteps: 2, maxTokens: 2000, maxWallClockSeconds: 120, maxToolCalls: 1 }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Tool
metadata: { name: delegate, namespace: tenant-a }
spec:
  kind: agent
  agent: { ref: { name: e2e-a2a-child } }
  inputSchema: { type: object }
  outputSchema: { type: object }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: e2e-a2a-parent, namespace: tenant-a }
spec:
  model: { providerRef: a2a-prov, name: m }
  instructions: %q
  tools: [{ name: delegate }]
  budget: { maxSteps: 4, maxTokens: 4000, maxWallClockSeconds: 180, maxToolCalls: 2 }
`, childInstr, parentInstr)
	if err := env.Apply(ctx, []byte(setup)); err != nil {
		t.Fatalf("apply A2A agents/tool/provider: %v", err)
	}

	// Run the parent. Its in-pod invoker creates a child AgentRun and folds it.
	if err := env.Apply(ctx, []byte(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentRun
metadata: { name: e2e-a2a-run, namespace: tenant-a }
spec: { agentRef: e2e-a2a-parent, input: {} }
`)); err != nil {
		t.Fatalf("apply parent AgentRun: %v", err)
	}

	// 1. A child AgentRun is created carrying the parent-run label.
	if err := env.WaitFor(ctx, "a2a-child-created", 4*time.Minute, func(ctx context.Context) bool {
		out, _ := env.Exec(ctx, ExecTarget{}, "get", "agentruns", "-n", ns,
			"-l", "agents.smol-agents.ai/parent-run="+parentRun, "-o", "name")
		return strings.TrimSpace(string(out)) != ""
	}); err != nil {
		pods, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "pods,agentruns", "-o", "wide")
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", parentRun, "-o", "jsonpath={.status.state}:{.status.terminationReason}")
		t.Fatalf("no child AgentRun created by the parent's A2A invoker: %v\nparent=%s\n%s", err, st, pods)
	}

	// 2. The parent run reaches a terminal Completed (folded the child's output).
	if err := env.WaitFor(ctx, "a2a-parent-completed", 3*time.Minute, func(ctx context.Context) bool {
		out, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", parentRun, "-o", "jsonpath={.status.state}")
		return strings.TrimSpace(string(out)) == "Completed"
	}); err != nil {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", parentRun, "-o", "jsonpath={.status.state}:{.status.terminationReason}")
		t.Fatalf("parent A2A run did not Complete (%s): %v", st, err)
	}
	t.Log("M3: parent loop pod emitted agent-call -> child AgentRun created (parent-run label) -> parent Completed folding child output")
}

// claudeHarnessLive (R-E2E-SCN-CLAUDE-LIVE) drives the REAL claude-code harness
// against a live LLM (z.ai's Anthropic-compatible endpoint). It applies a
// mode=harness Agent (kind=claude-code) on a kata-fc node — kata is mandatory for
// approvalMode=never, which becomes --dangerously-skip-permissions — whose
// ANTHROPIC_API_KEY is broker-leased from a pre-created single-key secret and
// ANTHROPIC_BASE_URL points at z.ai. It then runs an AgentRun and asserts the run
// Completes with a non-empty Output and honest (non-zero) token accounting,
// proving the operator resolves the per-kind harness image, the executor leases +
// injects the secret env, and the subprocess harness reaches a live model and
// reports usage. The driver pre-creates namespace + secret (setupLiveLLMSecrets);
// requires CapLiveLLM so it self-skips without injected keys.
var claudeHarnessLive = Scenario{
	ID:       "R-E2E-SCN-CLAUDE-LIVE",
	Name:     "claude-code-harness-live",
	Requires: CapKubernetes | CapKata | CapLiveLLM,
	Run:      runClaudeHarnessLive,
}

func runClaudeHarnessLive(t *testing.T, env Env) {
	t.Helper()
	const ns, pool, agent, run = "e2e-live-claude", "e2e-live-claude-pool", "e2e-live-claude-agent", "e2e-live-claude-run"
	// claude-code harness Agent: ANTHROPIC_API_KEY is broker-leased from the
	// driver-created single-key secret zai-anthropic-key; ANTHROPIC_BASE_URL is a
	// literal pointing at z.ai. approvalMode=never (-> --dangerously-skip-permissions,
	// admission-allowed only on the kata-fc microVM we place it on). outputFormat=json
	// so the harness can parse token accounting.
	agentYAML := fmt.Sprintf(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: %s, namespace: %s }
spec:
  mode: harness
  harness:
    kind: claude-code
    env:
      - { name: ANTHROPIC_API_KEY, secretRef: { secretName: zai-anthropic-key } }
      - { name: ANTHROPIC_BASE_URL, value: "https://api.z.ai/api/anthropic" }
      - { name: ANTHROPIC_MODEL, value: "glm-4.6" }
      - { name: ANTHROPIC_SMALL_FAST_MODEL, value: "glm-4.6" }
    cli:
      approvalMode: never
      outputFormat: json
      allowedTools: [Bash, Read, Write]
  instructions: "You are a terse assistant. Follow the user's instruction exactly."
  sandbox: { runtimeClass: kata-fc }
  budget: { maxSteps: 1, maxTokens: 4096, maxWallClockSeconds: 300, maxToolCalls: 10 }
`, agent, ns)
	runHarnessLive(t, env, ns, pool, agent, run, agentYAML, "Reply with exactly PONG.")
}

// codexHarnessLive (R-E2E-SCN-CODEX-LIVE) drives the REAL codex harness against
// the live OpenAI API. It applies a mode=harness Agent (kind=codex) on a kata-fc
// node — kata is mandatory for approvalMode=never — whose CODEX_API_KEY is
// broker-leased from a pre-created single-key secret; cli.codexBaseURL makes the
// operator render ~/.codex/config.toml (base_url + wire_api=responses +
// env_key=CODEX_API_KEY). It runs an AgentRun and asserts Completion with a
// non-empty Output and non-zero tokens, proving the codex harness image resolves,
// the secret env is leased + injected, the config.toml routes codex at the
// configured provider, and a live model is reached. The driver pre-creates the
// namespace + secret; requires CapLiveLLM so it self-skips without injected keys.
var codexHarnessLive = Scenario{
	ID:       "R-E2E-SCN-CODEX-LIVE",
	Name:     "codex-harness-live",
	Requires: CapKubernetes | CapKata | CapLiveLLM,
	Run:      runCodexHarnessLive,
}

func runCodexHarnessLive(t *testing.T, env Env) {
	t.Helper()
	const ns, pool, agent, run = "e2e-live-codex", "e2e-live-codex-pool", "e2e-live-codex-agent", "e2e-live-codex-run"
	// codex harness Agent: CODEX_API_KEY broker-leased from the driver-created
	// single-key secret openai-codex-key; codexBaseURL+codexModel drive the
	// operator-rendered config.toml. approvalMode=never (kata-only). outputFormat=json
	// for token accounting.
	agentYAML := fmt.Sprintf(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: %s, namespace: %s }
spec:
  mode: harness
  harness:
    kind: codex
    env:
      - { name: CODEX_API_KEY, secretRef: { secretName: openai-codex-key } }
    cli:
      approvalMode: never
      outputFormat: json
      codexBaseURL: "https://api.openai.com/v1"
      codexModel: "gpt-4o-mini"
  instructions: "You are a terse assistant. Follow the user's instruction exactly."
  sandbox: { runtimeClass: kata-fc }
  budget: { maxSteps: 1, maxTokens: 2048, maxWallClockSeconds: 300, maxToolCalls: 5 }
`, agent, ns)
	runHarnessLive(t, env, ns, pool, agent, run, agentYAML, "What is 2+2? Reply with only the number.")
}

// runHarnessLive is the shared body for the live-harness scenarios: it places a
// kata-fc node (AgentNodePool + node label), applies the per-kind harness Agent,
// waits for it to reach phase=Ready, runs an AgentRun with the given prompt, and
// asserts the run Completes with non-empty Output and non-zero token accounting.
// The namespace + provider secret are pre-created by the L2 driver
// (setupLiveLLMSecrets) — this never (re-)creates the secret. No secret value is
// ever logged.
func runHarnessLive(t *testing.T, env Env, ns, pool, agent, run, agentYAML, prompt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// Idempotent re-run on a reused cluster: drop any prior run/agent/pool.
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentrun", run, "-n", ns, "--ignore-not-found")
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agent", agent, "-n", ns, "--ignore-not-found")
	// ResolvePlacement picks the lowest-named matching AgentNodePool, so multiple
	// kata pools make a kata agent resolve to the WRONG pool (≠ the node label this
	// scenario sets) and the run pod goes unschedulable. Ensure ONLY this scenario's
	// pool exists. Safe: the live-harness scenarios run last in the suite.
	_, _ = env.Exec(ctx, ExecTarget{}, "delete", "agentnodepool", "--all", "--ignore-not-found")

	// kata placement (single-node L2): a kata-fc AgentNodePool + the pool label on
	// the node so the harness run pod schedules (mirrors runA2AComposition).
	if err := env.Apply(ctx, []byte(fmt.Sprintf(`apiVersion: agents.smol-agents.ai/v1
kind: AgentNodePool
metadata: { name: %s }
spec: { isolation: kata-fc, arch: arm64, bootstrap: { mode: UserData, distro: al2023 } }
`, pool))); err != nil {
		t.Fatalf("apply AgentNodePool: %v", err)
	}
	nodeOut, err := env.Exec(ctx, ExecTarget{}, "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	node := strings.TrimSpace(string(nodeOut))
	if err != nil || node == "" {
		t.Fatalf("get node: %v (%q)", err, nodeOut)
	}
	if _, err := env.Exec(ctx, ExecTarget{}, "label", "node", node, "agents.smol-agents.ai/pool="+pool, "--overwrite"); err != nil {
		t.Fatalf("label node: %v", err)
	}

	// Apply the harness Agent (NS + secret are pre-created by the driver).
	if err := env.Apply(ctx, []byte(agentYAML)); err != nil {
		t.Fatalf("apply harness Agent: %v", err)
	}

	// The Agent reconciler validates the harness spec + ensures the per-agent SA;
	// a run pod won't schedule until the Agent is Ready.
	if err := env.WaitFor(ctx, "harness-agent-ready", 60*time.Second, func(ctx context.Context) bool {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", agent, "-o", "jsonpath={.status.phase}")
		return strings.TrimSpace(string(ph)) == "Ready"
	}); err != nil {
		ph, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", agent, "-o", "jsonpath={.status.phase}")
		rs, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agent", agent, "-o", "jsonpath={.status.reason}")
		t.Fatalf("harness Agent never reached Ready (observed %q/%q): %v",
			strings.TrimSpace(string(ph)), strings.TrimSpace(string(rs)), err)
	}

	// Run the harness once.
	if err := env.Apply(ctx, []byte(fmt.Sprintf(`apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentRun
metadata: { name: %s, namespace: %s }
spec:
  agentRef: %s
  input: { prompt: %q }
`, run, ns, agent, prompt))); err != nil {
		t.Fatalf("apply AgentRun: %v", err)
	}

	// Wait for a terminal state. A live model + image pull on kata can take a few
	// minutes; give it 6m.
	terminal := func(s string) bool {
		switch s {
		case "Completed", "Failed", "Expired":
			return true
		}
		return false
	}
	if err := env.WaitFor(ctx, "harness-run-terminal", 6*time.Minute, func(ctx context.Context) bool {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.state}")
		return terminal(strings.TrimSpace(string(st)))
	}); err != nil {
		st, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.state}")
		pods, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "pods", "-o", "wide")
		t.Fatalf("AgentRun never reached a terminal state (observed %q): %v\npods:\n%s",
			strings.TrimSpace(string(st)), err, pods)
	}

	state, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.state}")
	if got := strings.TrimSpace(string(state)); got != "Completed" {
		tr, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.terminationReason}")
		t.Fatalf("AgentRun state=%q, want Completed (terminationReason=%q)", got, strings.TrimSpace(string(tr)))
	}

	out, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.output}")
	if o := strings.TrimSpace(string(out)); o == "" || o == "null" {
		t.Errorf("AgentRun .status.output is empty/null, want non-empty")
	}

	// Honest token accounting (json outputFormat): tokens must be present + non-zero.
	tokens, _ := env.Exec(ctx, ExecTarget{}, "get", "-n", ns, "agentrun", run, "-o", "jsonpath={.status.usage.tokens}")
	if tk := strings.TrimSpace(string(tokens)); tk == "" || tk == "0" {
		t.Errorf("AgentRun .status.usage.tokens=%q, want non-empty + non-zero (json outputFormat)", tk)
	}
	t.Logf("live harness %s Completed: output non-empty, usage.tokens=%s", agent, strings.TrimSpace(string(tokens)))
}

// indent prefixes every line of s with pad (for embedding a JSON blob under a
// YAML block scalar).
func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// ecrImage returns the full image ref for a platform component: the L2 ECR copy
// when L2_ECR_REGISTRY/L2_IMAGE_TAG are set (the L2 ring, where scenarios deploy
// their own workloads), else the kind-loaded dev tag (L0/L1).
func ecrImage(component string) string {
	reg, tag := os.Getenv("L2_ECR_REGISTRY"), os.Getenv("L2_IMAGE_TAG")
	if reg == "" || tag == "" {
		return "smol-agents/" + component + ":dev"
	}
	return reg + "/smol-agents/" + component + ":" + tag
}

// silence "imported and not used" if a future refactor drops one of
// these. Keep the import block stable.
var (
	_ = spiffeid.ID{}
	_ = rt.LLMDecision{}
)
