package invokers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func mcpTool(url string, auth *v1.AuthRef) v1.Tool {
	return v1.Tool{Name: "search", Spec: v1.ToolSpec{Kind: v1.ToolMCP, MCP: &v1.MCPSpec{URL: url, Auth: auth}}}
}

// mcpServer is a minimal Streamable-HTTP MCP server: it answers initialize +
// tools/call. callResult is the tools/call result JSON; sse returns the
// tools/call response as a text/event-stream; errOnCall returns a JSON-RPC error.
func mcpServer(callResult string, sse, errOnCall bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-1")
		var payload string
		switch req.Method {
		case "initialize":
			payload = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-11-25","capabilities":{}}}`, req.ID)
		case "tools/call":
			if errOnCall {
				payload = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"boom"}}`, req.ID)
			} else {
				payload = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, callResult)
			}
		}
		if sse && req.Method == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
}

func TestMCPInvoker_StructuredContentPreferred(t *testing.T) {
	srv := mcpServer(`{"structuredContent":{"hits":3},"content":[{"type":"text","text":"3 hits"}]}`, false, false)
	defer srv.Close()
	obs, err := (&MCPInvoker{Client: srv.Client()}).Invoke(context.Background(), mcpTool(srv.URL, nil), json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(obs.Output) != `{"hits":3}` {
		t.Errorf("structuredContent should win: %s", obs.Output)
	}
}

func TestMCPInvoker_TextFallback(t *testing.T) {
	srv := mcpServer(`{"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}`, false, false)
	defer srv.Close()
	obs, err := (&MCPInvoker{Client: srv.Client()}).Invoke(context.Background(), mcpTool(srv.URL, nil), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(obs.Output) != `"hello world"` { // joined text, wrapped as a JSON string
		t.Errorf("text fallback wrong: %s", obs.Output)
	}
}

func TestMCPInvoker_SSEResponse(t *testing.T) {
	srv := mcpServer(`{"structuredContent":{"ok":true}}`, true, false)
	defer srv.Close()
	obs, err := (&MCPInvoker{Client: srv.Client()}).Invoke(context.Background(), mcpTool(srv.URL, nil), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke (SSE): %v", err)
	}
	if string(obs.Output) != `{"ok":true}` {
		t.Errorf("SSE-decoded output wrong: %s", obs.Output)
	}
}

func TestMCPInvoker_JSONRPCError(t *testing.T) {
	srv := mcpServer("", false, true)
	defer srv.Close()
	_, err := (&MCPInvoker{Client: srv.Client()}).Invoke(context.Background(), mcpTool(srv.URL, nil), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("JSON-RPC error must surface, got %v", err)
	}
}

func TestMCPInvoker_RejectsNonHTTP(t *testing.T) {
	_, err := (&MCPInvoker{}).Invoke(context.Background(), mcpTool("stdio:///usr/bin/some-server", nil), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Errorf("non-http MCP URL must be rejected loudly, got %v", err)
	}
}
