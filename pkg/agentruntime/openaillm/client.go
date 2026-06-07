// Package openaillm is a real agentruntime.LLM backed by an OpenAI-compatible
// chat-completions endpoint (OpenAI, OpenRouter, Together, self-hosted vLLM,
// …). It drives Mode=loop agents: instructions + input + prior tool
// calls/results + the tool catalog become a chat request, and the model's reply
// (content or a tool call) becomes an rt.LLMDecision.
package openaillm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// Doer is the HTTP surface (swappable in tests).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client implements agentruntime.LLM against an OpenAI-compatible API.
type Client struct {
	Endpoint string // base URL, e.g. https://api.openai.com (no trailing /v1)
	APIKey   string
	HTTP     Doer
}

// New builds a Client. endpoint is the provider base; the /v1/chat/completions
// path is appended.
func New(endpoint, apiKey string) *Client {
	return &Client{
		Endpoint: strings.TrimRight(endpoint, "/"),
		APIKey:   apiKey,
		HTTP:     &http.Client{Timeout: 120 * time.Second},
	}
}

// Chat satisfies agentruntime.LLM.
func (c *Client) Chat(ctx context.Context, req agentruntime.ChatRequest) (rt.LLMDecision, error) {
	body := map[string]any{
		"model":    req.Model.Name,
		"messages": buildMessages(req),
	}
	if tools := buildTools(req.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	if req.Model.Temperature != nil {
		body["temperature"] = *req.Model.Temperature
	}
	if req.Model.TopP != nil {
		body["top_p"] = *req.Model.TopP
	}
	if req.Model.MaxOutputTokens != nil {
		body["max_tokens"] = *req.Model.MaxOutputTokens
	}
	// Forward the seed so a backend that honors it can be deterministic — a
	// hint, not a guarantee (many providers ignore it).
	if req.Seed != 0 {
		body["seed"] = req.Seed
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return rt.LLMDecision{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return rt.LLMDecision{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return rt.LLMDecision{}, fmt.Errorf("openaillm: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return rt.LLMDecision{}, fmt.Errorf("openaillm: read: %w", err)
	}
	if resp.StatusCode >= 400 {
		return rt.LLMDecision{}, fmt.Errorf("openaillm: http %d: %s", resp.StatusCode, string(rb))
	}

	var out struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return rt.LLMDecision{}, fmt.Errorf("openaillm: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return rt.LLMDecision{}, fmt.Errorf("openaillm: response had no choices")
	}

	msg := out.Choices[0].Message
	// Reasoning models on OpenAI-compatible endpoints (z.ai glm-4.6, deepseek-r1,
	// …) split the reply: the chain-of-thought goes in reasoning_content and the
	// answer in content. After a tool turn glm-4.6 sometimes returns an EMPTY
	// content with the answer text only in reasoning_content — so fall back rather
	// than fold a silently-empty final answer. content wins whenever it's present.
	content := msg.Content
	if strings.TrimSpace(content) == "" {
		content = msg.ReasoningContent
	}
	dec := rt.LLMDecision{
		Reasoning: content,
		TokensIn:  out.Usage.PromptTokens,
		TokensOut: out.Usage.CompletionTokens,
	}
	if len(msg.ToolCalls) > 0 {
		args := msg.ToolCalls[0].Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		dec.ToolCall = &rt.ToolCall{Tool: msg.ToolCalls[0].Function.Name, Arguments: json.RawMessage(args)}
		return dec, nil
	}
	// Final answer: wrap the text content as a JSON string (Output is RawMessage).
	ans, _ := json.Marshal(content)
	dec.FinalAnswer = &rt.FinalAnswer{Output: ans}
	return dec, nil
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is the answer text. omitempty keeps reasoning_content out of the
	// REQUEST messages we build (we only ever read it from responses).
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Name             string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// buildMessages renders system (instructions) + user (input) + a replay of
// prior tool calls/results so the model sees the loop so far.
func buildMessages(req agentruntime.ChatRequest) []chatMessage {
	msgs := make([]chatMessage, 0, 2+len(req.History))
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.Instructions})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: promptFromInput(req.Input)})
	for _, s := range req.History {
		for i, tc := range s.ToolCalls {
			id := fmt.Sprintf("call_%d_%d", s.Index, i)
			args := string(tc.Arguments)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			am := chatMessage{Role: "assistant"}
			am.ToolCalls = []wireToolCall{{ID: id, Type: "function"}}
			am.ToolCalls[0].Function.Name = tc.Tool
			am.ToolCalls[0].Function.Arguments = args
			msgs = append(msgs, am)

			result := string(tc.Result)
			if tc.Error != "" {
				result = `{"error":` + strconv.Quote(tc.Error) + `}`
			}
			if strings.TrimSpace(result) == "" {
				result = "{}"
			}
			msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: id, Name: tc.Tool, Content: result})
		}
	}
	return msgs
}

// buildTools maps the agent's tool catalog to OpenAI function tools.
func buildTools(tools []v1.Tool) []chatTool {
	out := make([]chatTool, 0, len(tools))
	for _, t := range tools {
		ct := chatTool{Type: "function"}
		ct.Function.Name = t.Name
		ct.Function.Description = t.Spec.Description
		ct.Function.Parameters = t.Spec.InputSchema
		out = append(out, ct)
	}
	return out
}

// promptFromInput extracts a user prompt from the Run input: a bare JSON string,
// a common field (prompt/question/input/task), else the raw JSON.
func promptFromInput(in json.RawMessage) string {
	if len(in) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(in, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(in, &m) == nil {
		for _, k := range []string{"prompt", "question", "input", "task"} {
			if v, ok := m[k]; ok {
				var sv string
				if json.Unmarshal(v, &sv) == nil {
					return sv
				}
			}
		}
	}
	return string(in)
}
