// Package mcp implements a streamable-HTTP MCP server for the memory-mcp
// gateway.
//
// MCP library choice: hand-rolled JSON-RPC 2.0 subset.
//
// The official Go MCP SDK (github.com/modelcontextprotocol/go-sdk) did not
// exist in the module graph at the time M3 was written and would require a
// new external dependency with an unstable API surface. The subset we need
// (initialize, tools/list, tools/call, resources/list, resources/read) is
// small enough — and the streaming-HTTP transport well-specified enough — that
// a focused, auditable in-tree implementation is the right call for a
// security-critical gateway. The interface boundary (Gateway) means swapping
// to the SDK later costs a single file change.
//
// Implements R-MEM-MCP-1, R-MEM-MCP-2, R-MEM-MCP-3.
package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ── JSON-RPC 2.0 wire types ────────────────────────────────────────────────

// Request is a JSON-RPC 2.0 request object.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // number | string | null
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response object. Exactly one of Result or Error
// is non-nil for non-notification responses.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Standard JSON-RPC 2.0 error codes and MCP-specific extensions.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603

	// MCP application-level errors (implementation-defined range: -32000..-32099)
	CodeUnauthenticated  = -32000
	CodePermissionDenied = -32001
	CodeQuotaExceeded    = -32002
	CodeNotFound         = -32003
	CodeNotSupported     = -32004
	CodeBackendError     = -32005
)

// ── MCP protocol types ─────────────────────────────────────────────────────

// Tool describes one MCP tool in the tools/list response.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// ToolListResult is the result shape for tools/list.
type ToolListResult struct {
	Tools []Tool `json:"tools"`
}

// ToolCallParams is the params shape for tools/call.
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolCallResult wraps the tool result content.
type ToolCallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock is a single content item in a tool result.
type ContentBlock struct {
	Type string `json:"type"` // "text" | "resource"
	Text string `json:"text,omitempty"`
}

// Resource describes one MCP resource in the resources/list response.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// ResourceListResult is the result shape for resources/list.
type ResourceListResult struct {
	Resources []Resource `json:"resources"`
}

// ResourceReadParams is the params shape for resources/read.
type ResourceReadParams struct {
	URI string `json:"uri"`
}

// ResourceReadResult is the result shape for resources/read.
type ResourceReadResult struct {
	Contents []ResourceContent `json:"contents"`
}

// ResourceContent is one item in a resource/read result.
type ResourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// InitializeParams is the params shape for initialize.
type InitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

// InitializeResult is the result shape for initialize.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ServerInfo      map[string]any `json:"serverInfo"`
	Capabilities    map[string]any `json:"capabilities"`
}

// ── Dispatcher ─────────────────────────────────────────────────────────────

// Dispatcher is the MCP JSON-RPC dispatcher. It parses JSON-RPC requests and
// routes them to the appropriate handler method on a Handler implementation.
//
// The Handler interface is the seam between the protocol layer (this file) and
// the memory-domain logic (gateway.go). Tests can inject a fake Handler.
type Dispatcher struct {
	handler Handler
}

// NewDispatcher wraps h in a Dispatcher.
func NewDispatcher(h Handler) *Dispatcher {
	return &Dispatcher{handler: h}
}

// Handler is implemented by the Gateway and by test fakes.
type Handler interface {
	// HandleToolCall is invoked for tools/call requests. method is the tool name.
	HandleToolCall(r *http.Request, toolName string, rawArgs json.RawMessage) (ToolCallResult, *RPCError)

	// HandleResourceRead is invoked for resources/read requests.
	HandleResourceRead(r *http.Request, uri string) (ResourceReadResult, *RPCError)
}

// ServeHTTP implements http.Handler. It dispatches a single JSON-RPC 2.0
// request and writes the response. The MCP streamable-HTTP transport
// delivers one request per POST body; GET is used for SSE (not yet required).
func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// SSE stream — return empty (not implemented for P1/M3).
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeRPCErr(w, nil, CodeParseError, "read body: "+err.Error())
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCErr(w, nil, CodeParseError, "parse request: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPCErr(w, req.ID, CodeInvalidRequest, "jsonrpc must be 2.0")
		return
	}

	switch req.Method {
	case "initialize":
		d.handleInitialize(w, req)

	case "tools/list":
		d.handleToolsList(w, req)

	case "tools/call":
		d.handleToolsCall(w, r, req)

	case "resources/list":
		d.handleResourcesList(w, req)

	case "resources/read":
		d.handleResourcesRead(w, r, req)

	default:
		writeRPCErr(w, req.ID, CodeMethodNotFound, "method not found: "+req.Method)
	}
}

func (d *Dispatcher) handleInitialize(w http.ResponseWriter, req Request) {
	result := InitializeResult{
		ProtocolVersion: "2024-11-05",
		ServerInfo: map[string]any{
			"name":    "memory-mcp",
			"version": "m3",
		},
		Capabilities: map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		},
	}
	writeRPCResult(w, req.ID, result)
}

func (d *Dispatcher) handleToolsList(w http.ResponseWriter, req Request) {
	writeRPCResult(w, req.ID, ToolListResult{Tools: memoryTools})
}

func (d *Dispatcher) handleToolsCall(w http.ResponseWriter, r *http.Request, req Request) {
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCErr(w, req.ID, CodeInvalidParams, "decode tools/call params: "+err.Error())
		return
	}
	result, rpcErr := d.handler.HandleToolCall(r, params.Name, params.Arguments)
	if rpcErr != nil {
		writeRPCErr(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	writeRPCResult(w, req.ID, result)
}

func (d *Dispatcher) handleResourcesList(w http.ResponseWriter, req Request) {
	writeRPCResult(w, req.ID, ResourceListResult{Resources: memoryResources})
}

func (d *Dispatcher) handleResourcesRead(w http.ResponseWriter, r *http.Request, req Request) {
	var params ResourceReadParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCErr(w, req.ID, CodeInvalidParams, "decode resources/read params: "+err.Error())
		return
	}
	result, rpcErr := d.handler.HandleResourceRead(r, params.URI)
	if rpcErr != nil {
		writeRPCErr(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	writeRPCResult(w, req.ID, result)
}

// ── Wire helpers ───────────────────────────────────────────────────────────

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := Response{JSONRPC: "2.0", ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func writeRPCErr(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}
	w.Header().Set("Content-Type", "application/json")
	// JSON-RPC over HTTP: errors still use 200 for application-level errors.
	// We use 400 only for parse/protocol failures (codes >= -32700).
	status := http.StatusOK
	if code <= CodeParseError || code == CodeInvalidRequest || code == CodeMethodNotFound || code == CodeInvalidParams {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
