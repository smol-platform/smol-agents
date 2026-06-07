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

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
)

// PlanFile is the on-disk shape of a fake-llm script.
//
//	{
//	  "globalSequence": [ <decision1>, <decision2>, ... ],
//	  "plans": {
//	    "<sha256-of-request-body>": { "finalAnswer": {"output": "..."} },
//	    ...
//	  },
//	  "fallback": { "finalAnswer": {"output": "..."} }
//	}
//
// `globalSequence` (when set) wins over per-key matching: every
// /v1/chat call advances the global cursor, regardless of body. Used
// to script a deterministic plan→tool→observation cycle without
// computing the request hash. When the sequence exhausts, the
// per-key Plans map is consulted; if THAT misses, fallback.
//
// Multiple plans for the same request body are supported by listing
// them in `sequence` inside a per-key entry.
type PlanFile struct {
	GlobalSequence []rt.LLMDecision        `json:"globalSequence"`
	Plans          map[string]ScriptedPlan `json:"plans"`
	Fallback       rt.LLMDecision          `json:"fallback"`
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
	// OpenAI chat-completions wire (what pkg/agentruntime/openaillm — the
	// production Mode=loop client in `agent run`/`serve-session` — speaks). Lets
	// an operator-scheduled loop pod drive against this same scripted mock, so
	// loop-mode tool invocation (and A2A) can be exercised as a real pod, not
	// only in-process. The scripted rt.LLMDecision is translated to an OpenAI
	// response (tool_calls or content).
	mux.HandleFunc("POST /v1/chat/completions", server.chatCompletions)
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
	plans          map[string]ScriptedPlan
	globalSequence []rt.LLMDecision
	globalCursor   int
	fallback       rt.LLMDecision
	mu             sync.Mutex
	cursors        map[string]int // per-key seq index
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
	s.globalSequence = pf.GlobalSequence
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

// chatCompletions serves the OpenAI chat-completions wire. It picks the next
// scripted decision (per-agent, keyed on the system message so parent/child
// agents script independently; global-sequence/body-hash fallback) and renders
// it as an OpenAI response openaillm can parse.
func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	key := keyFor(body)
	if sys := systemMessage(body); sys != "" {
		key = keyForString(sys)
	}
	d := s.next(key)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(decisionToOpenAI(d))
}

// systemMessage extracts the first system message's content (the Agent's
// instructions — stable across the loop, unlike the growing history) from an
// OpenAI request body, or "" if absent.
func systemMessage(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// decisionToOpenAI renders a scripted rt.LLMDecision as an OpenAI
// chat-completions response: a tool call → choices[0].message.tool_calls;
// otherwise the FinalAnswer output (or a canned done answer) → assistant
// content. Shapes match pkg/agentruntime/openaillm's parser.
func decisionToOpenAI(d rt.LLMDecision) map[string]any {
	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	switch {
	case d.ToolCall != nil:
		args := string(d.ToolCall.Arguments)
		if args == "" {
			args = "{}"
		}
		msg["tool_calls"] = []map[string]any{{
			"id":       "call_1",
			"type":     "function",
			"function": map[string]any{"name": d.ToolCall.Tool, "arguments": args},
		}}
		finish = "tool_calls"
	case d.FinalAnswer != nil:
		msg["content"] = string(d.FinalAnswer.Output)
	default:
		msg["content"] = `{"answer":"done"}`
	}
	return map[string]any{
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens":     d.TokensIn,
			"completion_tokens": d.TokensOut,
		},
	}
}

func keyFor(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func keyForString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (s *server) next(key string) rt.LLMDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Global sequence wins over per-key plans when configured.
	if len(s.globalSequence) > 0 {
		if s.globalCursor < len(s.globalSequence) {
			d := s.globalSequence[s.globalCursor]
			s.globalCursor++
			return d
		}
		return s.fallback
	}

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
