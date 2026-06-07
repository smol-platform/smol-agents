// Command pi-bridge wraps the pi coding-agent CLI behind a tiny loopback HTTP
// server so the pi-mono harness (M4.15) can drive it over HTTP. It performs the
// provider-key isolation (M4.16): it reads the key from its OWN env once, writes
// pi's models.json 0600, and spawns pi with the env scrubbed of *_API_KEY, so
// the key is never in pi's process environment. Binds 127.0.0.1 only.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/smol-platform/smol-agents/pkg/observability"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8848", "loopback listen address")
	piBin := flag.String("pi-bin", "pi", "pi CLI binary")
	provider := flag.String("provider", os.Getenv("PI_PROVIDER"), "model provider name for models.json")
	baseURL := flag.String("base-url", os.Getenv("PI_BASE_URL"), "provider base URL for models.json")
	model := flag.String("model", os.Getenv("PI_MODEL"), "default model for models.json")
	keyEnv := flag.String("key-env", "PI_API_KEY", "env var holding the provider key (read once, never forwarded to pi's env)")
	flag.Parse()

	logger := observability.MustLogger(slog.LevelInfo)

	if err := writeModelsJSON(*provider, *baseURL, *model, os.Getenv(*keyEnv)); err != nil {
		logger.Error("pi-bridge: write models.json", "err", err)
		os.Exit(2)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var req runRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp, err := runPi(r.Context(), *piBin, req, flag.Args())
		if err != nil {
			logger.Warn("pi-bridge: pi run", "err", err)
			// Still return whatever output we parsed (degraded), with 200, so the
			// harness surfaces partial output rather than an opaque 500.
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	logger.Info("pi-bridge listening", "addr", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("pi-bridge: serve", "err", err)
		os.Exit(1)
	}
}

// writeModelsJSON writes pi's provider config (0600) from the key the bridge
// holds, so pi reads the key from disk (file perms) rather than its env. A no-op
// when no key is set (pi may be configured another way). The key is never logged.
func writeModelsJSON(provider, baseURL, model, key string) error {
	if key == "" {
		return nil
	}
	dir := filepath.Join(homeDir(), ".pi", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cfg := map[string]any{
		"provider": provider,
		"baseURL":  baseURL,
		"model":    model,
		"apiKey":   key,
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "models.json"), b, 0o600)
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "/tmp"
}
