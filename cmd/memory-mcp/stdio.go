package main

// stdio.go implements the stdio JSON-RPC transport for memory-mcp.
//
// Stdio transport design
// ─────────────────────
// The stdio transport reads newline-delimited JSON-RPC 2.0 requests from stdin
// and writes responses to stdout, one per line. This is the de-facto convention
// used by local IDE tooling (VS Code, Zed, Claude Desktop, etc.) for launching
// MCP servers as child processes.
//
// Authentication in stdio mode
// ─────────────────────────────
// The local IDE / agent runtime is the trusted caller. When running under
// stdio transport there is no network-level JWT-SVID: the process runs as the
// user's local identity and the OS process boundary is the security perimeter.
//
// In this mode the gateway is invoked with --insecure, which disables JWT-SVID
// signature verification. A synthetic Bearer token (a well-known placeholder
// value whose sub/aud are filled by the caller via --stdio-spiffe-id) is added
// to each synthesised *http.Request so the gateway's identity pipeline still
// works end-to-end. The resulting CallerIdentity.Tenant is derived from the
// SPIFFE path as normal.
//
// Callers SHOULD pass --stdio-spiffe-id matching their actual workload SPIFFE ID
// so that policy, quota, and audit records reflect the real identity. If omitted,
// a default local identity is used.
//
// Security note: stdio mode is explicitly dev/local-IDE use only.  Do NOT run
// in stdio mode on a shared host or expose the binary's stdin/stdout over a
// network socket.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// stdioRunner reads JSON-RPC requests from in, dispatches them through handler,
// and writes responses to out. It runs until ctx is cancelled or in reaches EOF.
//
// Each request and response is a single JSON object terminated by a newline
// ("\n"). Blank lines are silently skipped. Parse errors produce a JSON-RPC
// parse-error response.
//
// The syntheticSPIFFEID is embedded as a Bearer token on each synthetic
// *http.Request so the gateway's auth pipeline sees a consistent identity.
func stdioRunner(
	ctx context.Context,
	handler http.Handler,
	in io.Reader,
	out io.Writer,
	syntheticSPIFFEID string,
	log *slog.Logger,
) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4<<20), 4<<20) // 4 MiB max line

	// Build the synthetic Authorization header value once.
	authHeader := buildSyntheticBearerToken(syntheticSPIFFEID)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !scanner.Scan() {
			// EOF or scanner error — normal shutdown.
			if err := scanner.Err(); err != nil {
				log.Error("stdio: scanner error", slog.Any("err", err))
			}
			return
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		resp := dispatchStdio(handler, []byte(line), authHeader)

		enc, err := json.Marshal(resp)
		if err != nil {
			log.Error("stdio: marshal response", slog.Any("err", err))
			continue
		}
		enc = append(enc, '\n')
		if _, err := out.Write(enc); err != nil {
			log.Error("stdio: write response", slog.Any("err", err))
			return
		}
	}
}

// dispatchStdio synthesises an *http.Request wrapping the JSON-RPC body,
// dispatches it through the handler, and returns the decoded response object.
func dispatchStdio(handler http.Handler, body []byte, authHeader string) map[string]any {
	req, err := http.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	if err != nil {
		return rpcParseError(nil, "internal: build request: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)

	rw := &responseRecorder{header: make(http.Header)}
	handler.ServeHTTP(rw, req)

	// Decode the response the handler wrote.
	var out map[string]any
	if decErr := json.Unmarshal(rw.body.Bytes(), &out); decErr != nil {
		return rpcParseError(nil, "internal: decode handler response: "+decErr.Error())
	}
	return out
}

// responseRecorder is a minimal http.ResponseWriter that captures the response
// body so dispatchStdio can re-encode it.
type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *responseRecorder) Header() http.Header { return r.header }
func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}
func (r *responseRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }

// rpcParseError returns a minimal JSON-RPC parse-error response.
func rpcParseError(id any, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32700,
			"message": msg,
		},
	}
}

// buildSyntheticBearerToken constructs a minimal unsigned JWT for the given
// SPIFFE ID so the gateway's identity pipeline (ParseInsecure path) works.
// This token is only valid in --insecure mode; in secure mode the process exits
// before reaching stdio dispatch.
func buildSyntheticBearerToken(spiffeID string) string {
	// Reuse the base64url encoding already present in the test helpers,
	// reimplemented here to avoid importing the test package.
	exp := time.Now().Add(24 * time.Hour).Unix()
	claims := map[string]any{
		"sub": spiffeID,
		"aud": []string{"memory-mcp"},
		"exp": exp,
		"iat": time.Now().Unix(),
	}
	header := b64urlJSON(map[string]any{"alg": "RS256", "typ": "JWT"})
	payload := b64urlJSON(claims)
	return "Bearer " + header + "." + payload + ".stdioplaceholder"
}

// b64urlJSON encodes v as JSON then as base64url (no padding).
func b64urlJSON(v any) string {
	b, _ := json.Marshal(v)
	return encodeBase64URL(b)
}

// encodeBase64URL is a minimal base64url encoder (RFC 4648 §5, no padding).
func encodeBase64URL(b []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var sb strings.Builder
	sb.Grow((len(b)*4 + 2) / 3)
	for i := 0; i < len(b); i += 3 {
		rem := len(b) - i
		var b0, b1, b2 byte
		b0 = b[i]
		if rem > 1 {
			b1 = b[i+1]
		}
		if rem > 2 {
			b2 = b[i+2]
		}
		sb.WriteByte(chars[b0>>2])
		sb.WriteByte(chars[((b0&3)<<4)|(b1>>4)])
		if rem > 1 {
			sb.WriteByte(chars[((b1&15)<<2)|(b2>>6)])
		}
		if rem > 2 {
			sb.WriteByte(chars[b2&63])
		}
	}
	return sb.String()
}
