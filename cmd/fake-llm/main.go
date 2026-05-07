// Command fake-llm is the deterministic LLM used by the
// fullstack-e2e L0 ring. It serves an HTTP endpoint compatible with
// pkg/agentruntime/fakellm's protocol: a POST /v1/chat takes a
// ChatRequest and returns the next scripted LLMDecision.
//
// Determinism comes from a SHA-256 of the request body keying into a
// scripted plan map loaded from --plans (or PLANS_FILE env). When no
// plan matches, the server falls back to a canned "I'm done" answer
// so tests never hang.
//
// Implements R-E2E-L0-3.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"

	rt "github.com/stigen/knative-agents/pkg/agentmodel/runtime"
)

// PlanFile is the on-disk shape of a fake-llm script.
//
//	{
//	  "plans": {
//	    "<sha256-of-request-body>": { "finalAnswer": {"output": "..."} },
//	    ...
//	  },
//	  "fallback": { "finalAnswer": {"output": "..."} }
//	}
//
// Multiple plans for the same request body are supported by listing
// them in `sequence` instead of a single `plan`; each call advances
// the cursor. Tests use the cursor mode for plan→tool→observation
// flows.
type PlanFile struct {
	Plans    map[string]ScriptedPlan `json:"plans"`
	Fallback rt.LLMDecision          `json:"fallback"`
}

// ScriptedPlan is either a single decision or a sequence; on the wire
// it's a discriminated union by which field is set.
type ScriptedPlan struct {
	Plan     *rt.LLMDecision  `json:"plan,omitempty"`
	Sequence []rt.LLMDecision `json:"sequence,omitempty"`
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	plansPath := flag.String("plans", os.Getenv("PLANS_FILE"), "JSON plan file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	server, err := newServer(*plansPath)
	if err != nil {
		logger.Error("load plans", "err", err)
		os.Exit(2)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat", server.chat)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	logger.Info("fake-llm listening", "addr", *addr, "plans", *plansPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
}

type server struct {
	plans    map[string]ScriptedPlan
	fallback rt.LLMDecision
	mu       sync.Mutex
	cursors  map[string]int // key → seq index
}

func newServer(path string) (*server, error) {
	s := &server{
		plans: map[string]ScriptedPlan{},
		fallback: rt.LLMDecision{
			FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"done"}`)},
		},
		cursors: map[string]int{},
	}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plans: %w", err)
	}
	var pf PlanFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("parse plans: %w", err)
	}
	s.plans = pf.Plans
	if pf.Fallback.IsTerminal() || pf.Fallback.ToolCall != nil {
		s.fallback = pf.Fallback
	}
	return s, nil
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	key := keyFor(body)
	d := s.next(key)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

func keyFor(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *server) next(key string) rt.LLMDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	plan, ok := s.plans[key]
	if !ok {
		return s.fallback
	}
	if plan.Plan != nil {
		return *plan.Plan
	}
	if len(plan.Sequence) == 0 {
		return s.fallback
	}
	cur := s.cursors[key]
	if cur >= len(plan.Sequence) {
		// Past end of scripted sequence — fall back so tests don't
		// hang on an unexpected re-call.
		return s.fallback
	}
	d := plan.Sequence[cur]
	s.cursors[key] = cur + 1
	return d
}
