package agentbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

// gatewayDriver submits turns to the agentgateway HTTP API
// (POST /v1/sessions/{ns}/{name}/turns?wait=…) and folds the result body into a
// RunStatus. The result body's `result` field is the worker's SessionTurn
// (cmd/agentgateway/main.go folds {turnId, result}); we normalize it to the
// same pure.RunStatus the run driver returns so oracles are driver-agnostic.
type gatewayDriver struct {
	baseURL string
	http    *http.Client
	wait    time.Duration
}

// NewGatewayDriver builds a gatewayDriver. baseURL is the gateway root (e.g.
// https://gateway.bench.example). wait caps the synchronous ?wait per turn.
func NewGatewayDriver(baseURL string, wait time.Duration) *gatewayDriver {
	if wait <= 0 {
		wait = 60 * time.Second
	}
	return &gatewayDriver{
		baseURL: baseURL,
		http:    &http.Client{Timeout: wait + 30*time.Second},
		wait:    wait,
	}
}

func (d *gatewayDriver) Kind() DriverKind { return DriverGateway }

// turnResponse is the gateway's POST/GET turn response envelope.
type turnResponse struct {
	TurnID string          `json:"turnId"`
	Status string          `json:"status,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// sessionTurn mirrors pkg/turnmodel.SessionTurn (the result body). We decode
// only the fields the oracles + metrics need.
type sessionTurn struct {
	Output            json.RawMessage `json:"output,omitempty"`
	Phase             pure.Phase      `json:"phase"`
	Usage             pure.Usage      `json:"usage"`
	TerminationReason string          `json:"terminationReason,omitempty"`
	Error             string          `json:"error,omitempty"`
	StartedAt         time.Time       `json:"startedAt"`
	EndedAt           time.Time       `json:"endedAt"`
}

func (d *gatewayDriver) Submit(ctx context.Context, c BenchCase, ns, prompt string, sampleIdx int) (Handle, error) {
	spec := pure.AgentRunSpec{
		AgentRef: c.AgentRef,
		Seed:     c.Seed,
	}
	input, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return Handle{}, fmt.Errorf("gateway: marshal input: %w", err)
	}
	spec.Input = input
	body, err := json.Marshal(spec)
	if err != nil {
		return Handle{}, fmt.Errorf("gateway: marshal turn: %w", err)
	}
	url := fmt.Sprintf("%s/v1/sessions/%s/%s/turns?wait=%s", d.baseURL, ns, c.AgentRef, d.wait.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Handle{}, fmt.Errorf("gateway: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return Handle{}, fmt.Errorf("gateway: POST turn: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return Handle{}, fmt.Errorf("gateway: POST turn HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var tr turnResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Handle{}, fmt.Errorf("gateway: decode turn response: %w", err)
	}
	h := Handle{
		Name:      tr.TurnID,
		Namespace: ns,
		Cold:      sampleIdx == 0,
		extra:     map[string]string{"agentRef": c.AgentRef},
	}
	// Synchronous wait already returned a result — stash it so Collect is a
	// no-op round-trip.
	if resp.StatusCode == http.StatusOK && len(tr.Result) > 0 {
		h.extra["result"] = string(tr.Result)
	}
	return h, nil
}

func (d *gatewayDriver) Collect(ctx context.Context, h Handle) (pure.RunStatus, error) {
	if cached, ok := h.extra["result"]; ok && cached != "" {
		return foldSessionTurn([]byte(cached))
	}
	key := sessionqueue.SessionKey(h.Namespace, h.extra["agentRef"])
	_ = key // session key is encoded in the URL path below; kept for parity.
	url := fmt.Sprintf("%s/v1/sessions/%s/%s/turns/%s?wait=%s",
		d.baseURL, h.Namespace, h.extra["agentRef"], h.Name, d.wait.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return pure.RunStatus{}, fmt.Errorf("gateway: build get: %w", err)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return pure.RunStatus{}, fmt.Errorf("gateway: GET result: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return pure.RunStatus{}, fmt.Errorf("gateway: result not ready (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var tr turnResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return pure.RunStatus{}, fmt.Errorf("gateway: decode result response: %w", err)
	}
	return foldSessionTurn(tr.Result)
}

// foldSessionTurn normalizes a gateway SessionTurn body into a pure.RunStatus.
func foldSessionTurn(body []byte) (pure.RunStatus, error) {
	if len(body) == 0 {
		return pure.RunStatus{}, fmt.Errorf("gateway: empty result body")
	}
	var st sessionTurn
	if err := json.Unmarshal(body, &st); err != nil {
		return pure.RunStatus{}, fmt.Errorf("gateway: decode SessionTurn: %w", err)
	}
	status := pure.RunStatus{
		State:             st.Phase,
		Usage:             st.Usage,
		TerminationReason: st.TerminationReason,
		Output:            st.Output,
	}
	if !st.StartedAt.IsZero() {
		t := metav1.NewTime(st.StartedAt)
		status.StartedAt = &t
	}
	if !st.EndedAt.IsZero() {
		t := metav1.NewTime(st.EndedAt)
		status.EndedAt = &t
	}
	return status, nil
}
