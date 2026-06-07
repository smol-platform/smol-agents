package invokers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

const mcpProtocolVersion = "2025-11-25"

// MCPInvoker drives a kind=mcp tool over Streamable HTTP with a hand-rolled
// JSON-RPC client: initialize → notifications/initialized → tools/call. A POST
// response may be a JSON body OR a text/event-stream (both decoded). Only
// http(s) MCP URLs are accepted — stdio servers are gated to an operator
// allow-list (M2.15), not driven here. The session is re-initialized per call
// (stateless; caching is a later optimization).
type MCPInvoker struct {
	Client *http.Client
	Leaser SecretLeaser
}

func (m *MCPInvoker) Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	spec := tool.Spec.MCP
	if spec == nil || spec.URL == "" {
		return rt.Observation{}, fmt.Errorf("mcp tool %q: missing spec.mcp.url", tool.Name)
	}
	if !strings.HasPrefix(spec.URL, "http://") && !strings.HasPrefix(spec.URL, "https://") {
		return rt.Observation{}, fmt.Errorf("mcp tool %q: only http(s) MCP URLs are supported (got %q); stdio servers must be on the operator allow-list", tool.Name, spec.URL)
	}

	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	sess := &mcpSession{client: client, url: spec.URL, extra: http.Header{}}

	if spec.Auth != nil && spec.Auth.SecretName != "" {
		if m.Leaser == nil {
			return rt.Observation{}, fmt.Errorf("mcp tool %q: auth set but no secret leaser configured", tool.Name)
		}
		tok, err := m.Leaser.LeaseSecret(ctx, spec.Auth.SecretName, 0)
		if err != nil {
			return rt.Observation{}, fmt.Errorf("mcp tool %q: lease auth: %w", tool.Name, err)
		}
		sess.extra.Set("Authorization", "Bearer "+string(bytes.TrimSpace(tok)))
	}

	if _, err := sess.call(ctx, 1, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "smol-agents", "version": "0.1"},
	}); err != nil {
		return rt.Observation{}, fmt.Errorf("mcp tool %q: initialize: %w", tool.Name, err)
	}
	if err := sess.notify(ctx, "notifications/initialized"); err != nil {
		return rt.Observation{}, fmt.Errorf("mcp tool %q: initialized: %w", tool.Name, err)
	}
	result, err := sess.call(ctx, 2, "tools/call", map[string]any{
		"name":      tool.Name,
		"arguments": json.RawMessage(args),
	})
	if err != nil {
		return rt.Observation{}, fmt.Errorf("mcp tool %q: call: %w", tool.Name, err)
	}
	out, err := mcpResultOutput(result)
	if err != nil {
		return rt.Observation{}, fmt.Errorf("mcp tool %q: %w", tool.Name, err)
	}
	return rt.Observation{Output: out}, nil
}

type jsonrpcResp struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mcpSession carries the per-invocation HTTP state (the echoed session id).
type mcpSession struct {
	client *http.Client
	url    string
	extra  http.Header
}

func (s *mcpSession) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	for k, v := range s.extra {
		req.Header[k] = v
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		s.extra.Set("Mcp-Session-Id", sid) // echo on subsequent requests
	}
	return resp, nil
}

func (s *mcpSession) call(ctx context.Context, id int, method string, params any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	resp, err := s.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	rpc, err := decodeJSONRPC(resp, id)
	if err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("jsonrpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	return rpc.Result, nil
}

func (s *mcpSession) notify(ctx context.Context, method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, err := s.post(ctx, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// decodeJSONRPC reads a JSON-RPC response from either a JSON body or the
// terminal SSE data: event matching id.
func decodeJSONRPC(resp *http.Response, id int) (jsonrpcResp, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxToolResponseBytes+1))
	if err != nil {
		return jsonrpcResp{}, err
	}
	if int64(len(body)) > maxToolResponseBytes {
		return jsonrpcResp{}, fmt.Errorf("response exceeds %d bytes", maxToolResponseBytes)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return parseSSEJSONRPC(body, id)
	}
	var r jsonrpcResp
	if err := json.Unmarshal(body, &r); err != nil {
		return jsonrpcResp{}, fmt.Errorf("decode json: %w", err)
	}
	return r, nil
}

// parseSSEJSONRPC scans SSE `data:` lines for the JSON-RPC response with id.
func parseSSEJSONRPC(body []byte, id int) (jsonrpcResp, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), maxToolResponseBytes)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var r jsonrpcResp
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &r); err != nil {
			continue
		}
		if r.ID == id {
			return r, nil
		}
	}
	return jsonrpcResp{}, fmt.Errorf("no JSON-RPC response for id %d in SSE stream", id)
}

// mcpResultOutput maps a CallToolResult to the observation output, preferring
// structuredContent and falling back to the joined text content (wrapped as a
// JSON string so the observation is always valid JSON).
func mcpResultOutput(result json.RawMessage) (json.RawMessage, error) {
	var r struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return nil, fmt.Errorf("decode tool result: %w", err)
	}
	if r.IsError {
		return nil, fmt.Errorf("tool reported isError")
	}
	if len(r.StructuredContent) > 0 {
		return r.StructuredContent, nil
	}
	var sb strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	out, _ := json.Marshal(sb.String())
	return out, nil
}
