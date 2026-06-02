// Command agentgateway is the HTTP front door for AgentSession turns. It accepts
// incoming turn requests and publishes them to the durable session queue (NATS
// JetStream), optionally waiting for the result (synchronous calls). It is
// stateless and horizontally scalable — deployed as a Knative Service that
// autoscales on HTTP concurrency (and scales to zero when idle), with NATS as
// the durable buffer between it and the long-running session workers.
//
//	POST /v1/sessions/{ns}/{name}/turns        body: AgentRunSpec JSON
//	     ?wait=<dur>  -> wait up to dur for the result (else 202 queued)
//	GET  /v1/sessions/{ns}/{name}/turns/{id}    ?wait=<dur> -> result or 404
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/observability"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

const maxTurnBytes = 1 << 20

// Gateway publishes turns to the queue and fetches results. Stateless.
type Gateway struct {
	Queue   sessionqueue.Queue
	MaxWait time.Duration // cap on synchronous ?wait (default 60s)
	Logger  *slog.Logger
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions/{ns}/{name}/turns", g.postTurn)
	mux.HandleFunc("GET /v1/sessions/{ns}/{name}/turns/{id}", g.getResult)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

func (g *Gateway) postTurn(w http.ResponseWriter, r *http.Request) {
	key := sessionqueue.SessionKey(r.PathValue("ns"), r.PathValue("name"))
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTurnBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body"})
		return
	}
	// Validate it's a well-formed turn (the worker decodes it as AgentRunSpec).
	var spec v1.AgentRunSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "turn body must be a JSON AgentRunSpec: " + err.Error()})
		return
	}
	id, err := g.Queue.Publish(r.Context(), key, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "enqueue: " + err.Error()})
		return
	}
	wait := g.waitFor(r)
	if wait <= 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{"turnId": id, "status": "queued"})
		return
	}
	res, ferr := g.Queue.FetchResult(r.Context(), key, id, wait)
	if ferr != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"turnId": id, "status": "pending"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turnId": id, "result": json.RawMessage(res)})
}

func (g *Gateway) getResult(w http.ResponseWriter, r *http.Request) {
	key := sessionqueue.SessionKey(r.PathValue("ns"), r.PathValue("name"))
	id := r.PathValue("id")
	wait := g.waitFor(r)
	if wait <= 0 {
		wait = time.Second
	}
	res, err := g.Queue.FetchResult(r.Context(), key, id, wait)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"turnId": id, "status": "pending"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turnId": id, "result": json.RawMessage(res)})
}

// waitFor parses ?wait=<duration>, capped at MaxWait. Absent/invalid → 0 (async).
func (g *Gateway) waitFor(r *http.Request) time.Duration {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0
	}
	maxWait := g.MaxWait
	if maxWait <= 0 {
		maxWait = 60 * time.Second
	}
	if d > maxWait {
		d = maxWait
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	natsURL := flag.String("nats-url", os.Getenv("AGENTSESSION_NATS_URL"), "NATS JetStream URL")
	maxWait := flag.Duration("max-wait", 60*time.Second, "cap on synchronous ?wait")
	flag.Parse()

	logger := observability.MustLogger(slog.LevelInfo)
	if *natsURL == "" {
		logger.Error("agentgateway: --nats-url (or AGENTSESSION_NATS_URL) is required")
		os.Exit(2)
	}
	q, err := sessionqueue.NewNATSQueue(*natsURL)
	if err != nil {
		logger.Error("agentgateway: connect NATS", "err", err)
		os.Exit(1)
	}
	defer q.Close()

	g := &Gateway{Queue: q, MaxWait: *maxWait, Logger: logger}
	srv := &http.Server{Addr: *addr, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()

	logger.Info("agentgateway listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("agentgateway", "err", err)
		os.Exit(1)
	}
}
