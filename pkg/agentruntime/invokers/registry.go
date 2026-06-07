package invokers

import (
	"net/http"
	"os"
	"strconv"

	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/teamtask"
)

// Default builds the production ToolInvoker set keyed by Tool.Spec.Kind that
// cmd/agent installs into Executor.Invokers: the HTTP invoker (M2.12) and the
// Streamable-HTTP MCP invoker (M2.14). A nil client falls back to
// http.DefaultClient inside each invoker.
func Default(leaser SecretLeaser, client *http.Client) map[v1.ToolKind]agentruntime.ToolInvoker {
	return map[v1.ToolKind]agentruntime.ToolInvoker{
		v1.ToolHTTP: &HTTPInvoker{Client: client, Leaser: leaser},
		v1.ToolMCP:  &MCPInvoker{Client: client, Leaser: leaser},
	}
}

// WireAgentInvoker best-effort-adds the kind=agent (A2A) invoker to base when the
// pod has in-cluster API access (SA token + the <agent>-a2a Role). It reads the
// operator's downward-API env: POD_NAMESPACE (where children are created),
// RUN_NAME (parent run, for the delegation-tree label) and A2A_DEPTH (recursion
// depth). On any failure — POD_NAMESPACE unset, no in-cluster config, or client
// build error — kind=agent stays ABSENT so the executor fail-closes the call,
// exactly as before A2A wiring. Mutates and returns base for chaining.
func WireAgentInvoker(base map[v1.ToolKind]agentruntime.ToolInvoker) map[v1.ToolKind]agentruntime.ToolInvoker {
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		return base
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return base
	}
	kc, err := crclient.New(cfg, crclient.Options{})
	if err != nil {
		return base
	}
	depth, _ := strconv.Atoi(os.Getenv("A2A_DEPTH"))
	maxDepth, _ := strconv.Atoi(os.Getenv("A2A_MAX_DEPTH")) // 0 → invoker defaults to 1 (conservative)
	base[v1.ToolAgent] = &AgentRunInvoker{
		Client:       kc,
		Namespace:    ns,
		ParentRun:    os.Getenv("RUN_NAME"),
		ParentRunUID: os.Getenv("AGENT_RUN_UID"),
		Depth:        depth,
		MaxDepth:     maxDepth,
	}
	return base
}

// WireTaskInvoker best-effort-adds the kind=task (team shared task list) invoker
// when the pod carries a team context: TEAM_NATS_URL + TEAM_NAMESPACE +
// TEAM_NAME (injected by the operator when it spawns a team member; P3). It binds
// the team's NATS KV bucket and claims as TEAM_MEMBER (falling back to RUN_NAME).
// On any failure — env unset or the KV bind erroring — kind=task stays ABSENT so
// the executor fail-closes the call (the team must grant access, D3). Mutates and
// returns base for chaining.
func WireTaskInvoker(base map[v1.ToolKind]agentruntime.ToolInvoker) map[v1.ToolKind]agentruntime.ToolInvoker {
	url := os.Getenv("TEAM_NATS_URL")
	ns := os.Getenv("TEAM_NAMESPACE")
	team := os.Getenv("TEAM_NAME")
	if url == "" || ns == "" || team == "" {
		return base
	}
	var opts []teamtask.NATSStoreOption
	if creds := os.Getenv("TEAM_NATS_CREDS"); creds != "" {
		opts = append(opts, teamtask.WithCredentials(creds))
	}
	store, err := teamtask.NewNATSStore(url, ns, team, opts...)
	if err != nil {
		return base // fail-closed: no store → no task invoker
	}
	owner := os.Getenv("TEAM_MEMBER")
	if owner == "" {
		owner = os.Getenv("RUN_NAME")
	}
	base[v1.ToolTask] = &TaskInvoker{Store: store, Owner: owner}
	return base
}
