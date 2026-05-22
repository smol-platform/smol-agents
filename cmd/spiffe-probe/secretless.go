package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/proxy"
	"github.com/smol-platform/smol-agents/pkg/identity"
	"github.com/smol-platform/smol-agents/pkg/secrets"
	"github.com/smol-platform/smol-agents/pkg/trat"
)

// runSecretless exercises the full secretless-egress chain from inside the
// pod, with EVERY component real except the external services (GitHub, TTS):
// an in-process broker (real SO_PEERCRED attestation, GitHubAppBackend →
// fake-github, JWKS verifier → fake-tts, deny-by-default policy) and a real
// agentnet HTTPProxy that mints a TraT (→ fake-tts), asks the broker to mint a
// GitHub token, and injects it. Asserts: (1) the upstream rejects a non-minted
// token; (2) a request through the sidecar succeeds; (3) the upstream observed
// a broker-minted token (agent-blind injection). R-E2E-SCN-SECRETLESS.
func runSecretless(ctx context.Context, socket, githubURL, ttsURL, ttsAud string) bool {
	const id = "secretless"
	if githubURL == "" || ttsURL == "" {
		fail(id, "--github-url and --tts-url are required")
		return false
	}

	src, err := identity.Open(ctx, identity.SourceConfig{WorkloadAPIAddr: socket, Mode: identity.ModeStrict})
	if err != nil {
		fail(id, "identity.Open: %v", err)
		return false
	}
	defer src.Close()
	svid, err := src.X509Source().GetX509SVID()
	if err != nil {
		fail(id, "GetX509SVID: %v", err)
		return false
	}
	meID := svid.ID
	td := meID.TrustDomain().IDString() // e.g. spiffe://smol-agents.ai

	// (1) Upstream strictness: a non-minted token must be rejected.
	if code, _ := httpGet(ctx, githubURL+"/repos/smol-platform/app", "Bearer not-a-minted-token"); code != http.StatusUnauthorized {
		fail(id, "fake-github accepted a non-minted token: status %d, want 401", code)
		return false
	}

	// In-process broker with the dynamic mint path wired to the fakes.
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fail(id, "app key: %v", err)
		return false
	}
	attestor, err := secrets.NewSPIREPeerAttestor(socket)
	if err != nil {
		fail(id, "attestor: %v", err)
		return false
	}
	brokerDir, err := os.MkdirTemp("", "broker")
	if err != nil {
		fail(id, "tempdir: %v", err)
		return false
	}
	defer os.RemoveAll(brokerDir)
	brokerSock := filepath.Join(brokerDir, "b.sock")

	cp := secrets.NewStaticCredentialPolicy()
	cp.Grant(meID, "github:repo:read", "github", "smol-platform/app")
	broker := &secrets.Server{
		SocketPath:  brokerSock,
		MaxLeaseTTL: 5 * time.Minute,
		DefaultTTL:  time.Minute,
		Backend:     secrets.NewStaticBackend(),
		Policy:      secrets.NewStaticPolicy(),
		Attestor:    attestor,
		Dynamic: &secrets.GitHubAppBackend{
			AppID:            "123456",
			PrivateKey:       appKey,
			BaseURL:          githubURL,
			ScopePermissions: map[string]map[string]string{"github:repo:read": {"contents": "read"}},
		},
		TraTVerifier: &trat.JWKSVerifier{Keys: &trat.HTTPKeySource{URL: ttsURL + "/jwks"}, Audience: td},
		CredPolicy:   cp,
	}
	bctx, bcancel := context.WithCancel(ctx)
	defer bcancel()
	go func() { _ = broker.Listen(bctx) }()
	defer broker.Close()
	if !waitUnix(brokerSock, 3*time.Second) {
		fail(id, "broker socket never came up")
		return false
	}

	// Real agentnet proxy: TraT minter → fake-tts, broker client → in-proc broker.
	minter := &trat.ExchangeMinter{
		TokenURL:        ttsURL + "/token",
		SubjectAudience: ttsAud,
		SubjectToken: func(c context.Context, aud string) (string, error) {
			tok, err := src.JWTSource().FetchJWTSVID(c, jwtsvid.Params{Audience: aud})
			if err != nil {
				return "", err
			}
			return tok.Marshal(), nil
		},
	}
	bclient := secrets.NewClient(brokerSock)
	defer bclient.Close()

	port, err := freePort()
	if err != nil {
		fail(id, "freePort: %v", err)
		return false
	}
	p := &proxy.HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "github", Kind: "http", LocalPort: int32(port),
			Gateway: githubURL, JWTAudience: td + "/gh",
			Credential: &v1.CredentialInjection{Name: "github", Scope: "github:repo:read"},
		},
		Identity:     src,
		TraTMinter:   minter,
		Broker:       secrets.CredentialMinterAdapter{Client: bclient},
		TraTAudience: td,
	}
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go func() { _ = p.Run(pctx) }()
	sidecar := fmt.Sprintf("127.0.0.1:%d", port)
	if !waitTCP(sidecar, 3*time.Second) {
		fail(id, "sidecar listener never came up")
		return false
	}

	// (2) Request through the sidecar: mint → inject → upstream.
	code, body := httpGet(ctx, "http://"+sidecar+"/repos/smol-platform/app", "")
	if code != http.StatusOK {
		fail(id, "sidecar github resource: status %d: %s", code, body)
		return false
	}

	// (3) Upstream confirms it saw a broker-minted token (agent-blind).
	_, obsBody := httpGet(ctx, githubURL+"/_observed", "")
	var obs struct {
		SawMintedToken bool `json:"sawMintedToken"`
	}
	if err := json.Unmarshal([]byte(obsBody), &obs); err != nil {
		fail(id, "decode /_observed: %v: %s", err, obsBody)
		return false
	}
	if !obs.SawMintedToken {
		fail(id, "upstream never observed a broker-minted token (injection failed)")
		return false
	}
	pass(id, "minted+injected via broker; upstream saw a minted token; agent never held it")
	return true
}

// --- helpers ---

func httpGet(ctx context.Context, url, auth string) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err.Error()
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitUnix(path string, d time.Duration) bool { return waitDial("unix", path, d) }
func waitTCP(addr string, d time.Duration) bool  { return waitDial("tcp", addr, d) }

func waitDial(network, addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c, err := net.Dial(network, addr); err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
