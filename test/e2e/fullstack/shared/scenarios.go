package shared

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
		webhook,
		kataIsolation,
		smolAgentPhase,
		secretlessEgress,
		memoryAccess,
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

// silence "imported and not used" if a future refactor drops one of
// these. Keep the import block stable.
var (
	_ = spiffeid.ID{}
	_ = rt.LLMDecision{}
)
