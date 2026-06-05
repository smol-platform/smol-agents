package agentmodel

import (
	"context"
	"fmt"
	"strings"

	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	pkgsandbox "github.com/smol-platform/smol-agents/pkg/sandbox"
)

// resolveSandbox picks a run/session pod's RuntimeClass, fail-closed. At most
// one of (class, pending, failed) is non-empty: failed is a hard R-SBX-1
// violation (runc without operator opt-in); pending means the chosen hardened
// RuntimeClass is not registered yet, so the caller refuses to schedule an
// unisolated pod and waits. An empty requested class falls back to defaultClass,
// then kata-fc. Shared by the AgentRun and AgentSession reconcilers.
func resolveSandbox(ctx context.Context, r client.Reader, requested, defaultClass string, allowHostRuntime bool) (class, pending, failed string) {
	class = requested
	if class == "" {
		class = defaultClass
	}
	if class == "" {
		class = string(pkgsandbox.KindKataFC)
	}
	if pkgsandbox.ParseKind(class) == pkgsandbox.KindRunc {
		if allowHostRuntime {
			return class, "", "" // deliberately permitted (dev/CI cluster)
		}
		return "", "", "runc-requires-allow-host-runtime"
	}
	var rc nodev1.RuntimeClass
	if err := r.Get(ctx, types.NamespacedName{Name: class}, &rc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("RuntimeClass %q not registered; refusing to run unisolated", class), ""
		}
		return "", fmt.Sprintf("checking RuntimeClass %q: %v", class, err), ""
	}
	return class, "", ""
}

// dangerFlagViolation enforces D3 (M3.15): danger permission/sandbox flags are
// opt-in AND admission-refused unless the resolved RuntimeClass is a microVM.
// Returns a non-empty reason when the Agent's harness requests a danger flag but
// sbClass is a shared-kernel class (runc/gvisor) — fail-closed, mirroring
// resolveSandbox. Empty (permitted) when there are no danger flags or the class
// is a kata microVM. The default safe posture is never affected.
func dangerFlagViolation(agent *amv1.Agent, sbClass string) string {
	if agent.Spec.Harness == nil || agent.Spec.Harness.CLI == nil {
		return ""
	}
	if !harnessHasDangerFlags(agent.Spec.Harness.CLI) {
		return ""
	}
	if builders.RequiresKVM(sbClass) {
		return "" // microVM (kata) — the danger flag is permitted (D3 opt-in)
	}
	return fmt.Sprintf("danger permission/sandbox flags require a microVM runtimeClass (kata), got %q (D3)", sbClass)
}

// harnessHasDangerFlags reports whether a CLI harness requests a posture that
// disables the agent's permission/sandbox guardrails: the typed ApprovalMode
// "never", or a known danger token smuggled through ExtraFlags
// (--dangerously-*, codex danger-full-access, --ask-for-approval never).
func harnessHasDangerFlags(cli *pure.HarnessCLISpec) bool {
	if cli.ApprovalMode == "never" {
		return true
	}
	joined := strings.ToLower(strings.Join(cli.ExtraFlags, " "))
	switch {
	case strings.Contains(joined, "--dangerously"):
		return true
	case strings.Contains(joined, "danger-full-access"):
		return true
	case strings.Contains(joined, "--ask-for-approval never"), strings.Contains(joined, "--ask-for-approval=never"):
		return true
	}
	return false
}
