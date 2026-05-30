package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// HTTPClient lets tests inject a fake transport.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// PiHarness POSTs to Inflection AI's Pi API. Default endpoint is set
// when HarnessHTTPSpec.URL is empty.
type PiHarness struct {
	Client HTTPClient
}

func (h *PiHarness) Kind() v1.HarnessKind { return v1.HarnessPi }

func (h *PiHarness) Run(ctx context.Context, req Request) (Response, error) {
	spec := req.Spec.HTTP
	if spec == nil {
		spec = &v1.HarnessHTTPSpec{}
	}
	url := spec.URL
	if url == "" {
		url = "https://api.inflection.ai/external/api/inference"
	}
	field := spec.PromptField
	if field == "" {
		field = "context"
	}
	respField := spec.ResponseField
	if respField == "" {
		respField = "text"
	}
	return doHTTP(ctx, h.Client, req, url, field, respField, "POST", spec.Headers)
}

// GenericHTTPHarness POSTs to any URL with a configurable prompt field
// + response field path.
type GenericHTTPHarness struct {
	Client HTTPClient
}

func (h *GenericHTTPHarness) Kind() v1.HarnessKind { return v1.HarnessGenericHTTP }

func (h *GenericHTTPHarness) Run(ctx context.Context, req Request) (Response, error) {
	spec := req.Spec.HTTP
	if spec == nil || spec.URL == "" {
		return Response{}, errors.New("harness: generic-http requires spec.http.url")
	}
	method := spec.Method
	if method == "" {
		method = "POST"
	}
	field := spec.PromptField
	if field == "" {
		field = "prompt"
	}
	respField := spec.ResponseField
	if respField == "" {
		respField = "text"
	}
	return doHTTP(ctx, h.Client, req, spec.URL, field, respField, method, spec.Headers)
}

// doHTTP is the shared HTTP driver.
func doHTTP(ctx context.Context, client HTTPClient, req Request, url, promptField, responseField, method string, headers map[string]string) (Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	prompt := promptFromInput(req.Input)
	body := map[string]any{promptField: prompt}
	if req.Instructions != "" {
		body["system"] = req.Instructions
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("harness: marshal: %w", err)
	}

	ctx, cancel := budgetTimeout(ctx, req.Budget)
	defer cancel()

	reqHeaders := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		reqHeaders[k] = v
	}
	for _, e := range req.Spec.Env {
		// Pass-through Authorization-style headers if env name starts with HEADER_.
		if strings.HasPrefix(e.Name, "HEADER_") && e.Value != "" {
			reqHeaders[strings.TrimPrefix(e.Name, "HEADER_")] = e.Value
		}
	}

	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		for k, v := range reqHeaders {
			r.Header.Set(k, v)
		}
		return r, nil
	}

	var retry *v1.RetrySpec
	if req.Spec.HTTP != nil {
		retry = req.Spec.HTTP.Retry
	}
	res, err := doWithRetry(ctx, client, newReq, retry)
	if err != nil {
		return Response{DurationMs: res.DurationMs}, err
	}
	return Response{Output: []byte(extractField(res.Body, responseField)), DurationMs: res.DurationMs}, nil
}

// extractField walks a dotted path through a JSON document and returns
// the string at that path. Numeric segments index into arrays.
func extractField(body []byte, path string) string {
	if path == "" {
		return string(body)
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body)
	}
	parts := strings.Split(path, ".")
	for _, p := range parts {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			idx := 0
			fmt.Sscanf(p, "%d", &idx)
			if idx >= len(x) {
				return string(body)
			}
			v = x[idx]
		default:
			return string(body)
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	out, _ := json.Marshal(v)
	return string(out)
}
