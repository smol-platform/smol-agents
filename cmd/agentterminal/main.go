// Command agentterminal is the terminal attach gateway (M4.10): a hardened,
// out-of-pod reverse proxy that authenticates a human via OIDC, authorizes them
// against an AttachGrant, mints an audience-bound attach token, and proxies a
// WebSocket to the agent's ttyd (driver/viewer by signed role). It deploys
// separately (deploy/agentterminal/), NOT through the agent's Knative path.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/observability"
)

// ttyd ports mirror builders.TerminalDriverPort / TerminalViewerPort (operator
// internal pkg is not importable from here).
const (
	driverPort = 7681
	viewerPort = 7682
)

func main() {
	addr := flag.String("addr", ":8443", "listen address")
	signingKeyFile := flag.String("signing-key-file", "/etc/agentterminal/signing.key", "attach-token HMAC signing key file")
	trustDomain := flag.String("trust-domain", "smol-agents.ai", "SPIFFE trust domain (token audience)")
	oidcIssuer := flag.String("oidc-issuer", os.Getenv("OIDC_ISSUER"), "OIDC issuer URL (bundled Dex)")
	oidcJWKS := flag.String("oidc-jwks-url", os.Getenv("OIDC_JWKS_URL"), "OIDC JWKS endpoint")
	oidcClient := flag.String("oidc-client-id", os.Getenv("OIDC_CLIENT_ID"), "OIDC client id (token audience)")
	allowOrigins := flag.String("allow-origins", "", "comma-separated Origin hosts permitted for browser attach")
	tokenTTL := flag.Duration("token-ttl", 2*time.Minute, "minted attach-token lifetime")
	svcDomain := flag.String("svc-domain", "svc.cluster.local", "cluster service DNS domain")
	flag.Parse()

	logger := observability.MustLogger(slog.LevelInfo)

	key, err := os.ReadFile(*signingKeyFile)
	if err != nil {
		logger.Error("agentterminal: read signing key", "err", err)
		os.Exit(2)
	}

	g := &Gateway{
		SigningKey:  key,
		TrustDomain: *trustDomain,
		OIDC:        &JWKSVerifier{Issuer: *oidcIssuer, Audience: *oidcClient, JWKSURL: *oidcJWKS},
		Grants:      &k8sGrantResolver{c: buildK8sClient(logger), log: logger},
		Target:      &svcTargetResolver{domain: *svcDomain},
		TokenTTL:    *tokenTTL,
		AllowOrigin: parseOrigins(*allowOrigins),
		Audit: func(e AuditEvent) {
			logger.Info("attach-audit", "action", e.Action, "subject", e.Subject, "agent", e.Agent, "role", e.Role, "grant", e.Grant, "reason", e.Reason)
		},
	}

	logger.Info("agentterminal listening", "addr", *addr, "issuer", *oidcIssuer)
	srv := &http.Server{Addr: *addr, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("agentterminal: serve", "err", err)
		os.Exit(1)
	}
}

func parseOrigins(csv string) map[string]bool {
	out := map[string]bool{}
	for _, o := range strings.Split(csv, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out[o] = true
		}
	}
	return out
}

func buildK8sClient(logger *slog.Logger) client.Client {
	cfg, err := config.GetConfig()
	if err != nil {
		logger.Info("agentterminal: no kube config; AttachGrant resolution disabled")
		return nil
	}
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		return nil
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		logger.Error("agentterminal: build k8s client", "err", err)
		return nil
	}
	return c
}

// svcTargetResolver dials the agent's ClusterIP terminal Service by DNS,
// selecting the driver or viewer port from the (signed) role.
type svcTargetResolver struct{ domain string }

func (s *svcTargetResolver) TTYD(ns, agent, role string) (*url.URL, error) {
	port := viewerPort
	if role == pure.AttachRoleDriver {
		port = driverPort
	}
	host := fmt.Sprintf("%s-terminal.%s.%s:%d", agent, ns, s.domain, port)
	return &url.URL{Scheme: "http", Host: host}, nil
}

// k8sGrantResolver finds a live (unexpired) AttachGrant for (ns, agent, subject)
// and returns its role + name. Driver grants win over viewer when both exist.
type k8sGrantResolver struct {
	c   client.Client
	log *slog.Logger
}

func (r *k8sGrantResolver) Resolve(ctx context.Context, ns, agent, subject string, now time.Time) (string, string, bool) {
	if r.c == nil {
		return "", "", false
	}
	var list amv1.AttachGrantList
	if err := r.c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		r.log.Error("agentterminal: list attachgrants", "err", err)
		return "", "", false
	}
	role, name := "", ""
	for i := range list.Items {
		g := &list.Items[i]
		if g.Spec.AgentRef != agent || g.Spec.Subject != subject {
			continue
		}
		if g.Spec.ExpiresAt == nil || !now.Before(g.Spec.ExpiresAt.Time) {
			continue // missing/elapsed expiry → not live
		}
		// Driver takes precedence over viewer.
		if g.Spec.Role == pure.AttachRoleDriver {
			return pure.AttachRoleDriver, g.Name, true
		}
		if role == "" {
			role, name = g.Spec.Role, g.Name
		}
	}
	return role, name, role != ""
}
