// Package worker — Summarizer interface + implementations.
//
// Summarizer produces a free-text summary of a set of retrieved documents.
// The retrieval side calls Worker.summarize (which calls Backend.Retrieve to
// get top-K documents, then passes the combined text to a Summarizer).
//
// Two implementations are provided:
//   - ModelProviderSummarizer: calls an OpenAI-compatible /v1/chat/completions
//     endpoint. Credentials are broker-resolved (same pattern as the embedder).
//   - FakeSummarizer: deterministic stub for unit tests. Returns a predictable
//     string derived from the input so tests can assert without an LLM.
//
// The Worker's Summarize method now delegates to a Summarizer when one is
// wired; otherwise it returns the backend's Summarize response (which is
// typically ErrNotSupported for all current adapters).
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/stigen/smol-agents/pkg/secrets"
)

// Summarizer produces a natural-language summary of a body of text.
// Implementations must be safe for concurrent use.
type Summarizer interface {
	// Summarize returns a summary of text scoped to the given query/topic.
	// The query is used as a framing hint ("summarise the documents as they
	// relate to <query>"). An empty query requests a general summary.
	Summarize(ctx context.Context, query string, docs []string) (string, error)
}

// ── ModelProviderSummarizer ───────────────────────────────────────────────────

// SummarizerConfig holds configuration for a real LLM summarizer.
type SummarizerConfig struct {
	// Endpoint is the OpenAI-compatible chat completions URL,
	// e.g. "https://api.openai.com/v1/chat/completions".
	Endpoint string

	// Model is the chat model, e.g. "gpt-4o-mini" or "claude-3-haiku".
	Model string

	// SecretName is the broker secret name for the API key.
	SecretName string

	// MaxTokens is the maximum number of tokens for the summary (default 512).
	MaxTokens int

	// SystemPrompt overrides the default system instruction.
	SystemPrompt string
}

// ModelProviderSummarizer calls an OpenAI-compatible chat completions endpoint.
// API key is fetched from the secrets broker on each call.
type ModelProviderSummarizer struct {
	cfg    SummarizerConfig
	client *http.Client
	broker *secrets.Client
}

// NewModelProviderSummarizer constructs a ModelProviderSummarizer.
// brokerSocket is the Unix socket path of the secrets broker.
func NewModelProviderSummarizer(cfg SummarizerConfig, brokerSocket string) (*ModelProviderSummarizer, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("summarizer: Endpoint is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("summarizer: Model is required")
	}
	if cfg.SecretName == "" {
		return nil, fmt.Errorf("summarizer: SecretName is required")
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 512
	}
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = "You are a concise summariser. Given a set of document excerpts " +
			"and a topic query, produce a brief summary (2-5 sentences) covering the key " +
			"points relevant to the query. Do not include document IDs or metadata."
	}
	return &ModelProviderSummarizer{
		cfg:    cfg,
		client: &http.Client{},
		broker: secrets.NewClient(brokerSocket),
	}, nil
}

// chatRequest is the minimal OpenAI chat completions request body.
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Summarize builds a prompt from the documents and calls the LLM endpoint.
func (s *ModelProviderSummarizer) Summarize(ctx context.Context, query string, docs []string) (string, error) {
	lease, err := s.broker.Lease(ctx, s.cfg.SecretName, 0)
	if err != nil {
		return "", fmt.Errorf("summarizer: fetch api key: %w", err)
	}
	apiKey := string(lease.Value)

	userContent := buildSummarizePrompt(query, docs)

	body, err := json.Marshal(chatRequest{
		Model: s.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: s.cfg.SystemPrompt},
			{Role: "user", Content: userContent},
		},
		MaxTokens: s.cfg.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("summarizer: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("summarizer: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("summarizer: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("summarizer: endpoint returned %d", resp.StatusCode)
	}

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("summarizer: decode response: %w", err)
	}
	if len(cr.Choices) == 0 || cr.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("summarizer: empty response from LLM")
	}
	return cr.Choices[0].Message.Content, nil
}

// buildSummarizePrompt constructs the user turn prompt from query + docs.
func buildSummarizePrompt(query string, docs []string) string {
	var b strings.Builder
	if query != "" {
		b.WriteString("Topic: ")
		b.WriteString(query)
		b.WriteString("\n\n")
	}
	b.WriteString("Documents:\n")
	for i, d := range docs {
		b.WriteString(fmt.Sprintf("[%d] %s\n", i+1, d))
	}
	return b.String()
}

// ── FakeSummarizer ────────────────────────────────────────────────────────────

// FakeSummarizer is a deterministic summarizer for unit tests. It returns a
// predictable string embedding the query and the number of docs, so tests can
// assert on the structure without needing an LLM.
type FakeSummarizer struct{}

// Summarize returns a stub summary combining query + doc count.
func (f *FakeSummarizer) Summarize(_ context.Context, query string, docs []string) (string, error) {
	if len(docs) == 0 {
		return "No documents to summarise.", nil
	}
	topic := query
	if topic == "" {
		topic = "(general)"
	}
	return fmt.Sprintf("Summary[topic=%q docs=%d]: %s", topic, len(docs),
		strings.Join(docs[:min(3, len(docs))], "; ")), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
