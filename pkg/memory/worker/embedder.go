// Package worker implements the retrieval worker data plane.
//
// This file defines the Embedder interface and two implementations:
//   - ModelProviderEmbedder: calls a real embeddings endpoint, with credentials
//     resolved via pkg/secrets broker.
//   - FakeEmbedder: deterministic n-gram hashing — zero external dependencies,
//     suitable for tests and the e2e probe.
//
// Implements R-MEM-WORK-1 (embedding step in the write/retrieve pipeline).
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	"github.com/stigen/smol-agents/pkg/secrets"
)

// Embedder turns a text string into a dense float32 vector. Implementations
// must be safe for concurrent use.
type Embedder interface {
	// Embed returns a normalised embedding vector for text. The returned slice
	// is owned by the caller and must not be modified by the Embedder.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Dims returns the dimensionality of the vectors this Embedder produces.
	// All vectors from the same Embedder have the same length.
	Dims() int
}

// ── ModelProviderEmbedder ───────────────────────────────────────────────────

// EmbeddingRequest is the JSON body sent to an OpenAI-compatible embeddings
// endpoint (supported by OpenAI, Azure OpenAI, Bedrock with the compatibility
// layer, and most local providers).
type EmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbeddingResponse is the minimal subset of the OpenAI embeddings response
// that the worker needs.
type EmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// ModelProviderConfig holds the configuration for a real embeddings call.
type ModelProviderConfig struct {
	// Endpoint is the full URL of the embeddings endpoint,
	// e.g. "https://api.openai.com/v1/embeddings".
	Endpoint string

	// Model is the embedding model name, e.g. "text-embedding-3-small".
	Model string

	// SecretName is the name passed to the secrets broker to retrieve the API
	// key. The broker returns it as a Lease; we use Lease.Value as a Bearer
	// token in the Authorization header.
	SecretName string

	// Dims is the expected output dimensionality.
	Dims int
}

// ModelProviderEmbedder calls an OpenAI-compatible embeddings endpoint. API
// key is fetched from the secrets broker on each call (the broker caches the
// lease until near-expiry).
type ModelProviderEmbedder struct {
	cfg    ModelProviderConfig
	client *http.Client
	broker *secrets.Client
}

// NewModelProviderEmbedder constructs a ModelProviderEmbedder. brokerSocket is
// the Unix socket path of the secrets broker (e.g. "/run/secrets/broker.sock").
func NewModelProviderEmbedder(cfg ModelProviderConfig, brokerSocket string) (*ModelProviderEmbedder, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("embedder: Endpoint is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("embedder: Model is required")
	}
	if cfg.SecretName == "" {
		return nil, fmt.Errorf("embedder: SecretName is required")
	}
	if cfg.Dims <= 0 {
		return nil, fmt.Errorf("embedder: Dims must be positive")
	}
	return &ModelProviderEmbedder{
		cfg:    cfg,
		client: &http.Client{},
		broker: secrets.NewClient(brokerSocket),
	}, nil
}

func (e *ModelProviderEmbedder) Dims() int { return e.cfg.Dims }

// Embed fetches an API key from the broker and calls the embeddings endpoint.
func (e *ModelProviderEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// Fetch (or reuse cached) API key lease.
	lease, err := e.broker.Lease(ctx, e.cfg.SecretName, 0)
	if err != nil {
		return nil, fmt.Errorf("embedder: fetch api key: %w", err)
	}
	apiKey := string(lease.Value)

	body, err := json.Marshal(EmbeddingRequest{Model: e.cfg.Model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("embedder: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedder: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedder: endpoint returned %d", resp.StatusCode)
	}

	var eresp EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&eresp); err != nil {
		return nil, fmt.Errorf("embedder: decode response: %w", err)
	}
	if len(eresp.Data) == 0 || len(eresp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedder: empty embedding in response")
	}
	vec := eresp.Data[0].Embedding
	normalise(vec)
	return vec, nil
}

// ── FakeEmbedder ────────────────────────────────────────────────────────────

// FakeEmbedder produces deterministic embedding vectors from a text string
// using n-gram character hashing. Semantically related strings will not
// necessarily be close, but the same string always maps to the same unit
// vector, which is enough for deterministic unit tests and the e2e probe.
type FakeEmbedder struct {
	dims int
}

// NewFakeEmbedder returns a FakeEmbedder with the given dimensionality.
// dims must be positive; 64 is a reasonable test value.
func NewFakeEmbedder(dims int) (*FakeEmbedder, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("fake embedder: dims must be positive, got %d", dims)
	}
	return &FakeEmbedder{dims: dims}, nil
}

func (f *FakeEmbedder) Dims() int { return f.dims }

// Embed hashes the text into a deterministic float32 vector using overlapping
// 3-character n-grams. Each n-gram is hashed to a bucket index modulo dims;
// the bucket accumulates a weight. The resulting vector is L2-normalised.
func (f *FakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, f.dims)
	if len(text) == 0 {
		return vec, nil
	}

	// 3-gram hashing.
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		// Use up to 3 chars, gracefully handle short strings.
		end := i + 3
		if end > len(runes) {
			end = len(runes)
		}
		gram := string(runes[i:end])
		h := fnv32(gram)
		bucket := int(h) % f.dims
		if bucket < 0 {
			bucket = -bucket
		}
		vec[bucket] += 1.0
	}

	normalise(vec)
	return vec, nil
}

// fnv32 is a simple FNV-1a 32-bit hash, dependency-free.
func fnv32(s string) uint32 {
	const (
		prime  uint32 = 16777619
		offset uint32 = 2166136261
	)
	h := offset
	for _, b := range []byte(s) {
		h ^= uint32(b)
		h *= prime
	}
	return h
}

// normalise converts v to a unit vector in-place. A zero vector is left as-is.
func normalise(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	scale := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= scale
	}
}
