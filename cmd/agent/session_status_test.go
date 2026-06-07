package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// M2.19: the worker's status handler serves the checkpointed summary file, and
// 503s before any turn has written it (so the operator's scrape simply no-ops).
func TestSessionStatusHandler(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status-summary.json")

	srv := httptest.NewServer(sessionStatusHandler(path))
	defer srv.Close()

	// Before the first turn: 503.
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("pre-turn status = %d, want 503", resp.StatusCode)
	}

	// After a checkpoint: 200 + the body verbatim.
	want := `{"phase":"Running","turns":3}`
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	resp2, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("post-turn status = %d, want 200", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}
