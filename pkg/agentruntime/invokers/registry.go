package invokers

import (
	"net/http"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
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
