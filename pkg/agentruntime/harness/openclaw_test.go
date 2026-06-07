package harness

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

func openClawReq(url string) Request {
	return Request{
		Spec:  v1.HarnessSpec{Kind: v1.HarnessOpenClaw, HTTP: &v1.HarnessHTTPSpec{URL: url}},
		Input: json.RawMessage(`{"prompt":"2+2"}`),
	}
}

// M4.20: the OpenClaw harness opens a session, sends the turn, accumulates the
// assistant reply, folds an optional usage frame, and stops on done.
func TestOpenClawHarness_SessionSendReply(t *testing.T) {
	srv := httptest.NewServer(websocket.Handler(func(c *websocket.Conn) {
		var open, msg ocFrame
		_ = websocket.JSON.Receive(c, &open) // session.open
		_ = websocket.JSON.Receive(c, &msg)  // message (user)
		if open.Type != "session.open" || msg.Type != "message" || msg.Content != "2+2" {
			_ = websocket.JSON.Send(c, ocFrame{Type: "error", Error: "bad envelope"})
			return
		}
		_ = websocket.JSON.Send(c, ocFrame{Type: "message", Role: "assistant", Content: "4"})
		_ = websocket.JSON.Send(c, ocFrame{Type: "usage", TokensIn: 3, TokensOut: 1})
		_ = websocket.JSON.Send(c, ocFrame{Type: "done"})
	}))
	defer srv.Close()

	resp, err := (&OpenClawHarness{}).Run(context.Background(), openClawReq(wsURL(srv.URL)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.Output) != "4" {
		t.Errorf("Output = %q, want 4", resp.Output)
	}
	if resp.TokensIn != 3 || resp.TokensOut != 1 {
		t.Errorf("tokens = in:%d out:%d, want 3/1", resp.TokensIn, resp.TokensOut)
	}
}

// M4.20: an error frame surfaces as a run error.
func TestOpenClawHarness_ErrorFrame(t *testing.T) {
	srv := httptest.NewServer(websocket.Handler(func(c *websocket.Conn) {
		var f ocFrame
		_ = websocket.JSON.Receive(c, &f)
		_ = websocket.JSON.Receive(c, &f)
		_ = websocket.JSON.Send(c, ocFrame{Type: "error", Error: "model unavailable"})
	}))
	defer srv.Close()

	if _, err := (&OpenClawHarness{}).Run(context.Background(), openClawReq(wsURL(srv.URL))); err == nil {
		t.Fatal("an error frame must surface as a run error")
	}
}

// M4.20: a missing url is rejected.
func TestOpenClawHarness_RequiresURL(t *testing.T) {
	req := Request{Spec: v1.HarnessSpec{Kind: v1.HarnessOpenClaw}, Input: json.RawMessage(`{"prompt":"x"}`)}
	if _, err := (&OpenClawHarness{}).Run(context.Background(), req); err == nil {
		t.Fatal("openclaw without spec.http.url must error")
	}
}
