package agentmodel

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
)

// agentHasA2AGrant reports whether the Agent's A2A Role exists — the
// authoritative signal that the Agent declares a kind=agent tool (the Agent
// reconciler creates it only then, see agent_controller.go ensureA2ARBAC). Used
// to decide whether the run pod gets an apiserver token: the token is mounted
// exactly when there is RBAC to use it.
func agentHasA2AGrant(ctx context.Context, c client.Client, agent *amv1.Agent) (bool, error) {
	role := &rbacv1.Role{}
	key := types.NamespacedName{Namespace: agent.Namespace, Name: builders.AgentA2ARoleName(agent.Name)}
	err := c.Get(ctx, key, role)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get A2A Role %q: %w", key, err)
	}
	return true, nil
}
