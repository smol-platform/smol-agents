package main

import (
	"context"
	"net/http"
	"os"
	"time"
)

// serveSessionStatus runs the worker's status endpoint (agentruntime.
// SessionStatusPort) until ctx is cancelled. It needs NO k8s RBAC for the worker
// — an in-cluster HTTP read of the session's own non-secret status counts,
// decoupled from the worker's state mutex by serving the atomically-written
// summary file.
// GET /status returns the checkpointed status-summary.json (503 before the first
// turn writes it). Best-effort: a bind/serve error is logged, never fatal — the
// durable turn loop is the source of truth and runs regardless.
func serveSessionStatus(ctx context.Context, addr, summaryPath string, logErr func(string, ...any)) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", sessionStatusHandler(summaryPath))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logErr("session status server", "err", err)
	}
}

// sessionStatusHandler serves the checkpointed status-summary.json (503 before
// the first turn writes it). Extracted for testing.
func sessionStatusHandler(summaryPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(summaryPath)
		if err != nil {
			http.Error(w, "no session summary yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
}
