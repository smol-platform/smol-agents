package harness

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// OpenClawHarness drives an OpenClaw agent-loop daemon over a WebSocket RPC
// (M4.20): open a session, send the turn, await the reply, fold it into Output.
// It is WS-first because the single-POST GenericHTTPHarness cannot speak the
// session-open→send→reply envelope. Honest accounting: tokens are 0 unless the
// daemon returns a usage frame; ToolCalls are left unset (OpenClaw's loop runs
// tools internally and does not surface a per-call trace here).
//
// NOTE: OpenClaw's WS envelope is under-documented — the frame shapes below are
// the assumed protocol and MUST be confirmed against the running binary
// (deployment-gated). The structure (dial → session.open → message → reply) is
// stable; only field names may need tuning.
type OpenClawHarness struct {
	// Dialer overrides the WebSocket dial (tests inject a fake server URL via the
	// spec; this hook lets a test supply a custom origin/config if needed).
	Dialer func(ctx context.Context, url string) (*websocket.Conn, error)
}

func (h *OpenClawHarness) Kind() v1.HarnessKind { return v1.HarnessOpenClaw }

type ocFrame struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	Error     string `json:"error,omitempty"`
	TokensIn  int64  `json:"tokensIn,omitempty"`
	TokensOut int64  `json:"tokensOut,omitempty"`
}

func (h *OpenClawHarness) Run(ctx context.Context, req Request) (Response, error) {
	spec := req.Spec.HTTP
	if spec == nil || strings.TrimSpace(spec.URL) == "" {
		return Response{}, errors.New("harness: openclaw requires spec.http.url (ws://…)")
	}
	ctx, cancel := budgetTimeout(ctx, req.Budget)
	defer cancel()

	attempts := 3
	base := 200 * time.Millisecond
	if r := spec.Retry; r != nil && r.MaxAttempts > 0 {
		attempts = int(r.MaxAttempts)
		if r.BackoffBaseMs > 0 {
			base = time.Duration(r.BackoffBaseMs) * time.Millisecond
		}
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		resp, err := h.rpc(ctx, spec.URL, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(base * time.Duration(i+1)):
		}
	}
	return Response{}, lastErr
}

func (h *OpenClawHarness) rpc(ctx context.Context, url string, req Request) (Response, error) {
	dial := h.Dialer
	if dial == nil {
		dial = func(_ context.Context, u string) (*websocket.Conn, error) {
			return websocket.Dial(u, "", "http://localhost")
		}
	}
	conn, err := dial(ctx, url)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()

	// ctx cancellation closes the conn so a blocked Receive returns promptly.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	if err := websocket.JSON.Send(conn, ocFrame{Type: "session.open"}); err != nil {
		return Response{}, err
	}
	if err := websocket.JSON.Send(conn, ocFrame{
		Type: "message", Role: "user", Content: promptFromInput(req.Input),
	}); err != nil {
		return Response{}, err
	}

	var text strings.Builder
	var resp Response
	for {
		var f ocFrame
		if err := websocket.JSON.Receive(conn, &f); err != nil {
			if ctx.Err() != nil {
				return Response{}, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return Response{}, err
		}
		switch f.Type {
		case "message", "reply", "content":
			if f.Role == "" || f.Role == "assistant" {
				text.WriteString(f.Content)
			}
		case "usage":
			resp.TokensIn, resp.TokensOut = f.TokensIn, f.TokensOut
		case "error":
			return Response{}, errors.New("openclaw: " + f.Error)
		case "done", "final", "session.close":
			resp.Output = []byte(text.String())
			return resp, nil
		}
	}
	resp.Output = []byte(text.String())
	return resp, nil
}
