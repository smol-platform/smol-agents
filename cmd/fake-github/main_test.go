package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newServer() *server {
	return &server{
		appID: "123", installID: "42", tokenTTL: time.Hour,
		now:    func() time.Time { return time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC) },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		minted: map[string]struct{}{},
	}
}

// fakeAppJWT builds an unsigned compact JWT carrying iss (issuerOf only decodes,
// never verifies — that's the broker/TTS's job, not this upstream's).
func fakeAppJWT(iss string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + iss + `"}`))
	return "h." + payload + ".sig"
}

func TestMintThenResourceAccepts(t *testing.T) {
	ts := httptest.NewServer(newServer().routes())
	defer ts.Close()

	// Broker mints an installation token.
	body, _ := json.Marshal(map[string]any{
		"repositories": []string{"app"},
		"permissions":  map[string]string{"contents": "read"},
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/app/installations/42/access_tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fakeAppJWT("123"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var minted struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	json.NewDecoder(resp.Body).Decode(&minted)
	resp.Body.Close()
	if !strings.HasPrefix(minted.Token, "ghs_") {
		t.Fatalf("minted token = %q, want ghs_ prefix", minted.Token)
	}

	// Agent (via sidecar) reaches the resource WITH the injected minted token.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/repos/stigen/app", nil)
	req2.Header.Set("Authorization", "Bearer "+minted.Token)
	resp2, _ := http.DefaultClient.Do(req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resource with minted token = %d, want 200", resp2.StatusCode)
	}

	// /_observed confirms the upstream saw a minted token.
	obs, _ := http.Get(ts.URL + "/_observed")
	var o struct {
		SawMintedToken bool `json:"sawMintedToken"`
		Count          int  `json:"count"`
	}
	json.NewDecoder(obs.Body).Decode(&o)
	obs.Body.Close()
	if !o.SawMintedToken || o.Count != 1 {
		t.Errorf("observed = %+v, want sawMintedToken=true count=1", o)
	}
}

func TestResourceRejectsUnmintedAndMissing(t *testing.T) {
	ts := httptest.NewServer(newServer().routes())
	defer ts.Close()

	cases := []struct {
		name, auth string
	}{
		{"missing", ""},
		{"agent-jwt-not-a-minted-token", "Bearer " + fakeAppJWT("agent")},
		{"random", "Bearer ghs_not_real"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/repos/stigen/app", nil)
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			resp, _ := http.DefaultClient.Do(req)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}

	// Nothing was accepted, so the upstream observed no minted token.
	obs, _ := http.Get(ts.URL + "/_observed")
	var o struct {
		SawMintedToken bool `json:"sawMintedToken"`
	}
	json.NewDecoder(obs.Body).Decode(&o)
	obs.Body.Close()
	if o.SawMintedToken {
		t.Error("upstream should not have observed any minted token")
	}
}

func TestInstallationIssuerCheck(t *testing.T) {
	ts := httptest.NewServer(newServer().routes())
	defer ts.Close()

	// Correct issuer → 200 with installation id.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/repos/stigen/app/installation", nil)
	req.Header.Set("Authorization", "Bearer "+fakeAppJWT("123"))
	resp, _ := http.DefaultClient.Do(req)
	var got struct {
		ID int `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || got.ID != 42 {
		t.Fatalf("installation = %d/%d, want 200/42", resp.StatusCode, got.ID)
	}

	// Wrong issuer → 401.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/repos/stigen/app/installation", nil)
	req2.Header.Set("Authorization", "Bearer "+fakeAppJWT("999"))
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-issuer installation = %d, want 401", resp2.StatusCode)
	}
}
