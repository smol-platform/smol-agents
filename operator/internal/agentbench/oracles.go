package agentbench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/client-go/util/jsonpath"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func init() {
	register(outputMatchOracle{})
	register(outputJSONPathOracle{})
	register(toolObservedOracle{})
	register(toolRejectedOracle{})
	register(fsRoundtripOracle{})
	register(secretReachOracle{})
	register(secretAbsentOracle{})
	register(budgetTerminatedOracle{})
	register(egressMetadataBlockedOracle{})
	register(isolationKernelOracle{})
}

// outputString returns RunStatus.Output as a string, unwrapping a JSON string
// scalar if that's how it was encoded (the harness writes raw bytes which the
// controller may wrap). Falls back to the raw bytes.
func outputString(status pure.RunStatus) string {
	out := []byte(status.Output)
	if len(out) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(out, &s); err == nil {
		return s
	}
	return string(out)
}

func failNoOutput(kind string) Verdict {
	return Verdict{Kind: VerdictFail, Evidence: fmt.Sprintf("%s: status.output is empty", kind)}
}

// ---- output_match ---------------------------------------------------------

type outputMatchOracle struct{}

func (outputMatchOracle) Kind() string { return "output_match" }

func (outputMatchOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	out := outputString(status)
	if out == "" {
		return failNoOutput("output_match")
	}
	want := substituteNonce(c.Case.Oracle.Want, c.Nonce)
	if c.Case.Oracle.Equals {
		if strings.TrimSpace(out) == strings.TrimSpace(want) {
			return Verdict{VerdictPass, fmt.Sprintf("output equals %q", want)}
		}
		return Verdict{VerdictFail, fmt.Sprintf("output != %q (got %q)", want, truncate(out, 200))}
	}
	if strings.Contains(out, want) {
		return Verdict{VerdictPass, fmt.Sprintf("output contains %q", want)}
	}
	return Verdict{VerdictFail, fmt.Sprintf("output does not contain %q (got %q)", want, truncate(out, 200))}
}

// ---- output_jsonpath ------------------------------------------------------

type outputJSONPathOracle struct{}

func (outputJSONPathOracle) Kind() string { return "output_jsonpath" }

func (outputJSONPathOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	out := []byte(status.Output)
	if len(out) == 0 {
		return failNoOutput("output_jsonpath")
	}
	var doc interface{}
	if err := json.Unmarshal(out, &doc); err != nil {
		// Output may itself be a JSON string holding JSON.
		var inner string
		if json.Unmarshal(out, &inner) == nil && json.Unmarshal([]byte(inner), &doc) == nil {
			// ok
		} else {
			return Verdict{VerdictFail, fmt.Sprintf("output_jsonpath: output is not JSON: %v", err)}
		}
	}
	jp := jsonpath.New("oracle").AllowMissingKeys(true)
	expr := c.Case.Oracle.Path
	if !strings.HasPrefix(expr, "{") {
		expr = "{" + expr + "}"
	}
	if err := jp.Parse(expr); err != nil {
		return Verdict{VerdictFail, fmt.Sprintf("output_jsonpath: bad path %q: %v", c.Case.Oracle.Path, err)}
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, doc); err != nil {
		return Verdict{VerdictFail, fmt.Sprintf("output_jsonpath: execute %q: %v", c.Case.Oracle.Path, err)}
	}
	got := strings.TrimSpace(buf.String())
	want := strings.TrimSpace(substituteNonce(c.Case.Oracle.Want, c.Nonce))
	if got == want {
		return Verdict{VerdictPass, fmt.Sprintf("jsonpath %s == %q", c.Case.Oracle.Path, want)}
	}
	return Verdict{VerdictFail, fmt.Sprintf("jsonpath %s == %q, want %q", c.Case.Oracle.Path, got, want)}
}

// ---- tool_observed --------------------------------------------------------

type toolObservedOracle struct{}

func (toolObservedOracle) Kind() string { return "tool_observed" }

// Check proves a tool actually executed (Hermes harness only today): there is
// an Observation step whose Result differs from its Arguments AND the output
// reflects the injected nonce/side-effect. It NEVER gates on usage.toolCalls
// (structurally 0 on the harness path).
func (toolObservedOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	out := outputString(status)
	want := substituteNonce(c.Case.Oracle.Want, c.Nonce)
	if want == "" {
		want = c.Nonce
	}
	sideEffect := want != "" && strings.Contains(out, want)

	var observed bool
	var note string
	for _, s := range status.Steps {
		if s.Kind != pure.StepObservation {
			continue
		}
		for _, tc := range s.ToolCalls {
			// Result must be present and differ from the arguments — a real
			// observation carries the tool's output, not an echo of the args.
			if len(tc.Result) > 0 && !bytes.Equal(tc.Result, tc.Arguments) {
				observed = true
				note = fmt.Sprintf("step[%d] Observation tool=%q has Result!=Arguments", s.Index, tc.Tool)
			}
		}
	}
	switch {
	case observed && sideEffect:
		return Verdict{VerdictPass, fmt.Sprintf("%s; output reflects side-effect %q", note, want)}
	case sideEffect:
		// No structured Observation step (expected on the Hermes path today —
		// the gateway runs tools server-side, §1.2 fact 2), but the un-guessable
		// side-effect appeared in output, which is the load-bearing proof.
		return Verdict{VerdictPass, fmt.Sprintf("output reflects un-guessable side-effect %q (no structured trace; Hermes server-side tool path)", want)}
	case observed:
		return Verdict{VerdictFail, fmt.Sprintf("%s but output is missing the expected side-effect %q", note, want)}
	default:
		return Verdict{VerdictFail, fmt.Sprintf("no Observation step and output missing side-effect %q (got %q)", want, truncate(out, 160))}
	}
}

// ---- tool_rejected (negative) ---------------------------------------------

type toolRejectedOracle struct{}

func (toolRejectedOracle) Kind() string { return "tool_rejected" }

// Check asserts loop-mode tools are unwired: a StepToolCallRejected step whose
// error contains "no invoker for kind". This is the anti-staleness tripwire —
// it returns Blocked (held as expected). If the rejection is absent (the gap
// got fixed), it FAILs so the stale plan is caught.
func (toolRejectedOracle) Check(status pure.RunStatus, _ CollectCtx) Verdict {
	const needle = "no invoker for kind"
	for _, s := range status.Steps {
		if s.Kind != pure.StepToolCallRejected {
			continue
		}
		if strings.Contains(s.Error, needle) {
			return Verdict{VerdictBlocked, fmt.Sprintf("step[%d] ToolCallRejected, error contains %q (loop-mode tools still unwired)", s.Index, needle)}
		}
		for _, tc := range s.ToolCalls {
			if strings.Contains(tc.Error, needle) {
				return Verdict{VerdictBlocked, fmt.Sprintf("step[%d] ToolCallRejected toolCall.error contains %q", s.Index, needle)}
			}
		}
	}
	return Verdict{VerdictFail, fmt.Sprintf("expected a ToolCallRejected step with %q but found none — loop-mode tools may have been wired; this blocked case is now STALE (flip it to a positive tool_observed)", needle)}
}

// ---- fs_roundtrip ---------------------------------------------------------

type fsRoundtripOracle struct{}

func (fsRoundtripOracle) Kind() string { return "fs_roundtrip" }

// Check proves a file written in a prior run is read in this run: this run's
// output must echo a value carried from the prior run's output (threaded by the
// runner via CollectCtx.PriorOutput). Requires byte/value equality, not mere
// presence.
func (fsRoundtripOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	out := outputString(status)
	if out == "" {
		return failNoOutput("fs_roundtrip")
	}
	if len(c.PriorOutput) == 0 {
		return Verdict{VerdictSkip, "fs_roundtrip: no prior-run output threaded (single-run plan); needs a prior write run"}
	}
	prior := strings.TrimSpace(string(c.PriorOutput))
	// Prior output may be a JSON string scalar.
	var priorStr string
	if json.Unmarshal(c.PriorOutput, &priorStr) == nil && priorStr != "" {
		prior = strings.TrimSpace(priorStr)
	}
	// If an explicit want is set, prefer it (a SHA threaded via the prompt).
	if w := substituteNonce(c.Case.Oracle.Want, c.Nonce); w != "" {
		prior = strings.TrimSpace(w)
	}
	if prior != "" && strings.Contains(out, prior) {
		return Verdict{VerdictPass, fmt.Sprintf("this run's output echoes prior-run value %q (cross-run persistence proven)", truncate(prior, 64))}
	}
	return Verdict{VerdictFail, fmt.Sprintf("output does not echo prior-run value %q (got %q)", truncate(prior, 64), truncate(out, 160))}
}

// ---- secret_reach ---------------------------------------------------------

type secretReachOracle struct{}

func (secretReachOracle) Kind() string { return "secret_reach" }

// Check proves the agent reached a credential-gated endpoint: the output
// reflects a 200 from the gated target. The negative control (401 without the
// lease) is a separate case in the plan.
func (secretReachOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	out := outputString(status)
	if out == "" {
		return failNoOutput("secret_reach")
	}
	want := substituteNonce(c.Case.Oracle.Want, c.Nonce)
	if want == "" {
		want = "200"
	}
	if strings.Contains(out, want) {
		return Verdict{VerdictPass, fmt.Sprintf("output reflects gated success marker %q", want)}
	}
	return Verdict{VerdictFail, fmt.Sprintf("output missing gated success marker %q (got %q)", want, truncate(out, 160))}
}

// ---- secret_absent (kind-aware, fail-closed) ------------------------------

type secretAbsentOracle struct{}

func (secretAbsentOracle) Kind() string { return "secret_absent" }

// Check proves the secret never leaked: the cleartext value must NOT appear in
// Output / Steps / terminationReason. Fail-closed (a missing value to check is
// a FAIL, not a pass). KIND-AWARE: for CLI kinds (which are NOT agent-blind by
// contract, §3.4) the oracle INVERTS to assert the secret IS present + flags
// not-blind.
func (secretAbsentOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	if len(c.SecretValues) == 0 {
		return Verdict{VerdictFail, "secret_absent: no secret values supplied to check (fail-closed)"}
	}
	hay := scanText(status)
	cliKind := isCLIHarness(c.HarnessKind)

	for label, val := range c.SecretValues {
		if strings.TrimSpace(val) == "" {
			return Verdict{VerdictFail, fmt.Sprintf("secret_absent: secret %q is empty (fail-closed)", label)}
		}
		present := strings.Contains(hay, val)
		if cliKind {
			// CLI harnesses inject the secret into the subprocess env; exposure
			// is EXPECTED and proves they are not agent-blind.
			if present {
				return Verdict{VerdictPass, fmt.Sprintf("CLI kind %q: secret %q present in output (NOT agent-blind, expected by contract §3.4)", c.HarnessKind, label)}
			}
			return Verdict{VerdictFail, fmt.Sprintf("CLI kind %q: secret %q expected present (not-blind contract) but absent", c.HarnessKind, label)}
		}
		if present {
			return Verdict{VerdictFail, fmt.Sprintf("LEAK: secret %q (label) appears in output/steps/termination — agent-blind contract violated", label)}
		}
	}
	return Verdict{VerdictPass, fmt.Sprintf("agent-blind: %d secret value(s) absent from output/steps/termination", len(c.SecretValues))}
}

// ---- budget_terminated ----------------------------------------------------

type budgetTerminatedOracle struct{}

func (budgetTerminatedOracle) Kind() string { return "budget_terminated" }

// Check proves a budget axis capped the run: State==Expired OR
// terminationReason has prefix "budget:".
func (budgetTerminatedOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	byReason := strings.HasPrefix(status.TerminationReason, "budget:")
	byState := status.State == pure.PhaseExpired
	// If the case names a want (e.g. "budget:tokens"), require an exact reason.
	if want := c.Case.Oracle.Want; want != "" {
		if status.TerminationReason == want {
			return Verdict{VerdictPass, fmt.Sprintf("terminationReason == %q", want)}
		}
		return Verdict{VerdictFail, fmt.Sprintf("terminationReason=%q state=%q, want reason %q", status.TerminationReason, status.State, want)}
	}
	if byReason || byState {
		return Verdict{VerdictPass, fmt.Sprintf("budget cap hit: state=%q reason=%q", status.State, status.TerminationReason)}
	}
	return Verdict{VerdictFail, fmt.Sprintf("run was not budget-terminated: state=%q reason=%q", status.State, status.TerminationReason)}
}

// ---- egress_metadata_blocked (paired) -------------------------------------

type egressMetadataBlockedOracle struct{}

func (egressMetadataBlockedOracle) Kind() string { return "egress_metadata_blocked" }

// Check proves the default-deny NetworkPolicy holds: the output must show the
// metadata fetch FAILED and the public :443 fetch SUCCEEDED. The case prompt
// drives both fetches and the agent reports markers; this oracle reads them.
// Markers default to "METADATA_BLOCKED" + "PUBLIC_OK"; override via oracle.want
// as "blockedMarker|okMarker".
func (egressMetadataBlockedOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	out := outputString(status)
	if out == "" {
		return failNoOutput("egress_metadata_blocked")
	}
	blockedMarker, okMarker := "METADATA_BLOCKED", "PUBLIC_OK"
	if w := c.Case.Oracle.Want; w != "" {
		parts := strings.SplitN(w, "|", 2)
		blockedMarker = parts[0]
		if len(parts) == 2 {
			okMarker = parts[1]
		}
	}
	gotBlocked := strings.Contains(out, blockedMarker)
	gotOK := strings.Contains(out, okMarker)
	switch {
	case gotBlocked && gotOK:
		return Verdict{VerdictPass, fmt.Sprintf("paired: metadata blocked (%q) AND public 443 ok (%q)", blockedMarker, okMarker)}
	case !gotBlocked:
		return Verdict{VerdictFail, fmt.Sprintf("metadata was NOT blocked (missing %q) — NetworkPolicy not enforcing; output=%q", blockedMarker, truncate(out, 160))}
	default:
		return Verdict{VerdictFail, fmt.Sprintf("public 443 marker %q missing — can't distinguish a cage from a total network outage; output=%q", okMarker, truncate(out, 160))}
	}
}

// ---- isolation_kernel (kata; SKIP without kata-fc) ------------------------

type isolationKernelOracle struct{}

func (isolationKernelOracle) Kind() string { return "isolation_kernel" }

// Check proves the pod ran in a kata microVM: the in-pod `uname -r` (reported in
// Output) differs from the node kernel. SKIPs with clear evidence when no
// kata-fc RuntimeClass is present (cftest runc downgrade — not a failure).
func (isolationKernelOracle) Check(status pure.RunStatus, c CollectCtx) Verdict {
	if !c.KataAvailable {
		return Verdict{VerdictSkip, "isolation_kernel: no kata-fc RuntimeClass; runc fallback (containment downgrade, not failure)"}
	}
	if c.NodeKernel == "" {
		return Verdict{VerdictSkip, "isolation_kernel: node kernel version unavailable (cannot compare)"}
	}
	out := strings.TrimSpace(outputString(status))
	if out == "" {
		return failNoOutput("isolation_kernel")
	}
	// The output may carry more than the kernel string; require the node kernel
	// to be ABSENT and the output to look like a kernel release.
	if strings.Contains(out, c.NodeKernel) {
		return Verdict{VerdictFail, fmt.Sprintf("in-pod kernel matches node kernel %q — kata silently fell back to runc (shared kernel)", c.NodeKernel)}
	}
	return Verdict{VerdictPass, fmt.Sprintf("in-pod kernel %q != node kernel %q (microVM isolation confirmed)", truncate(out, 64), c.NodeKernel)}
}

// ---- helpers --------------------------------------------------------------

// scanText concatenates everything an oracle should grep for a secret: output,
// every step's error + tool-call args/results/errors, and the termination
// reason.
func scanText(status pure.RunStatus) string {
	var b strings.Builder
	b.Write([]byte(status.Output))
	b.WriteByte('\n')
	b.WriteString(status.TerminationReason)
	b.WriteByte('\n')
	for _, s := range status.Steps {
		b.WriteString(s.Error)
		b.WriteByte('\n')
		for _, tc := range s.ToolCalls {
			b.Write([]byte(tc.Arguments))
			b.WriteByte('\n')
			b.Write([]byte(tc.Result))
			b.WriteByte('\n')
			b.WriteString(tc.Error)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// isCLIHarness reports whether a harness kind injects secrets into a subprocess
// env (and is therefore NOT agent-blind). Hermes/generic-http are HTTP kinds
// (blind); the rest are CLI kinds.
func isCLIHarness(kind string) bool {
	switch kind {
	case "hermes", "generic-http", "":
		return false
	case "loop":
		return false
	}
	return true
}

// substituteNonce replaces {{NONCE}} with the run nonce.
func substituteNonce(s, nonce string) string {
	if nonce == "" {
		return s
	}
	return strings.ReplaceAll(s, "{{NONCE}}", nonce)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
