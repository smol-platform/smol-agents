package v1

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Predicate is a compiled AgentWorkflow routing predicate: a single, side-effect-
// free comparison "<path> <op> <literal>" over the prior node's status.output
// (e.g. `score >= 80`, `result.status == "ok"`, `done == true`). It is the
// allow-listed, OPERATOR-EVALUATED DSL that replaces a free-form / LLM router —
// fail-closed (D3): a missing field or type mismatch evaluates to false (the edge
// is not taken), and paths into usage (cost/toolCalls) are forbidden so routing
// can never gate on them.
type Predicate struct {
	path []string
	op   string
	kind string // num | str | bool
	num  float64
	str  string
	b    bool
}

// predicateOps are matched longest-first so ">=" wins over ">".
var predicateOps = []string{"==", "!=", "<=", ">=", "<", ">"}

// CompilePredicate parses expr or returns an error (used at admission to reject a
// malformed predicate fail-closed).
func CompilePredicate(expr string) (Predicate, error) {
	expr = strings.TrimSpace(expr)
	for _, op := range predicateOps {
		i := strings.Index(expr, op)
		if i <= 0 {
			continue
		}
		left := strings.TrimSpace(expr[:i])
		right := strings.TrimSpace(expr[i+len(op):])
		if left == "" || right == "" {
			break
		}
		if left == "usage" || strings.HasPrefix(left, "usage.") {
			return Predicate{}, fmt.Errorf("predicate path %q reads usage — routing must never gate on cost/toolCalls", left)
		}
		p := Predicate{path: strings.Split(left, "."), op: op}
		switch {
		case len(right) >= 2 && strings.HasPrefix(right, `"`) && strings.HasSuffix(right, `"`):
			p.kind, p.str = "str", right[1:len(right)-1]
		case right == "true" || right == "false":
			p.kind, p.b = "bool", right == "true"
		default:
			n, err := strconv.ParseFloat(right, 64)
			if err != nil {
				return Predicate{}, fmt.Errorf("predicate literal %q must be a number, a \"string\", or true/false", right)
			}
			p.kind, p.num = "num", n
		}
		if (p.kind == "str" || p.kind == "bool") && op != "==" && op != "!=" {
			return Predicate{}, fmt.Errorf("predicate op %q is only valid for numbers (string/bool support == and !=)", op)
		}
		return p, nil
	}
	return Predicate{}, fmt.Errorf("predicate %q must be '<path> <op> <literal>' (op: == != < <= > >=)", expr)
}

// Eval evaluates the predicate against a node output document. A missing path or
// type mismatch returns false (fail-closed); only a malformed (non-JSON) output
// is an error.
func (p Predicate) Eval(output json.RawMessage) (bool, error) {
	var doc any
	if len(output) > 0 {
		if err := json.Unmarshal(output, &doc); err != nil {
			return false, fmt.Errorf("predicate: node output is not JSON: %w", err)
		}
	}
	v := navigate(doc, p.path)
	switch p.kind {
	case "num":
		f, ok := v.(float64) // JSON numbers decode to float64
		if !ok {
			return false, nil
		}
		switch p.op {
		case "==":
			return f == p.num, nil
		case "!=":
			return f != p.num, nil
		case "<":
			return f < p.num, nil
		case "<=":
			return f <= p.num, nil
		case ">":
			return f > p.num, nil
		case ">=":
			return f >= p.num, nil
		}
	case "str":
		s, ok := v.(string)
		if !ok {
			return false, nil
		}
		if p.op == "==" {
			return s == p.str, nil
		}
		return s != p.str, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return false, nil
		}
		if p.op == "==" {
			return b == p.b, nil
		}
		return b != p.b, nil
	}
	return false, nil
}

// navigate walks a dot-path into a decoded JSON document; returns nil if any hop
// is missing or not an object.
func navigate(doc any, path []string) any {
	cur := doc
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}
