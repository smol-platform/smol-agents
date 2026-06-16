package agentmodel

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// readSecretKey fetches one key from a k8s Secret (the sole key when key is
// empty). Shared by the AgentRun and AgentSession reconcilers.
func readSecretKey(ctx context.Context, c client.Client, ns, name, key string) ([]byte, error) {
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sec); err != nil {
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	if key != "" {
		v, ok := sec.Data[key]
		if !ok {
			return nil, fmt.Errorf("secret %q: key %q not present", name, key)
		}
		return v, nil
	}
	if len(sec.Data) == 1 {
		for _, v := range sec.Data {
			return v, nil
		}
	}
	return nil, fmt.Errorf("secret %q: key required (has %d keys)", name, len(sec.Data))
}

// gatherRunSecrets resolves the loop-mode ModelProvider (for the run/session
// spec) and gathers every secret the broker must serve: each input/harness-env
// secretRef value plus the provider API key, keyed by lease name. Harness mode
// returns a nil provider; agents with no secrets return an empty map. Shared by
// the AgentRun and AgentSession reconcilers.
func gatherRunSecrets(ctx context.Context, c client.Client, agent *amv1.Agent, namespace string, inputs []pure.RunInputFile) (*builders.RunProvider, map[string][]byte, error) {
	values := map[string][]byte{}

	for _, in := range inputs {
		if in.SecretRef == nil || in.SecretRef.SecretName == "" {
			continue
		}
		val, err := readSecretKey(ctx, c, namespace, in.SecretRef.SecretName, in.SecretRef.Key)
		if err != nil {
			return nil, nil, err
		}
		values[in.SecretRef.SecretName] = val
	}

	if agent.Spec.Harness != nil {
		for _, e := range agent.Spec.Harness.Env {
			if e.SecretRef == nil || e.SecretRef.SecretName == "" {
				continue
			}
			val, err := readSecretKey(ctx, c, agent.Namespace, e.SecretRef.SecretName, e.SecretRef.Key)
			if err != nil {
				return nil, nil, err
			}
			values[e.SecretRef.SecretName] = val
		}
	}

	// Loop-mode tool auth: each referenced Tool's http/mcp Auth.SecretName must
	// be served by the broker under that name — the in-pod invoker leases by it
	// (invokers/http.go, invokers/mcp.go). Without this an auth'd HTTP/MCP tool
	// fails its lease at runtime. Ref resolution mirrors resolveRunTools (a
	// dangling ref is skipped, matching the catalog).
	if agent.Spec.Mode != pure.ModeHarness {
		for _, ref := range agent.Spec.Tools {
			tns := ref.Namespace
			if tns == "" {
				tns = agent.Namespace
			}
			t := &amv1.Tool{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: tns, Name: ref.Name}, t); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, nil, err
			}
			var auth *pure.AuthRef
			switch {
			case t.Spec.HTTP != nil && t.Spec.HTTP.Auth != nil:
				auth = t.Spec.HTTP.Auth
			case t.Spec.MCP != nil && t.Spec.MCP.Auth != nil:
				auth = t.Spec.MCP.Auth
			}
			if auth == nil || auth.SecretName == "" {
				continue
			}
			val, err := readSecretKey(ctx, c, tns, auth.SecretName, auth.Key)
			if err != nil {
				return nil, nil, err
			}
			values[auth.SecretName] = val
		}
	}

	var provider *builders.RunProvider
	if agent.Spec.Mode != pure.ModeHarness && agent.Spec.Model.ProviderRef != "" {
		mp := &amv1.ModelProvider{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Spec.Model.ProviderRef}, mp); err != nil {
			return nil, nil, fmt.Errorf("get ModelProvider %q: %w", agent.Spec.Model.ProviderRef, err)
		}
		provider = &builders.RunProvider{
			Kind:       mp.Spec.Kind,
			Endpoint:   mp.Spec.Endpoint,
			ChatPath:   mp.Spec.ChatPath,
			SecretName: mp.Spec.SecretRef.SecretName,
		}
		if mp.Spec.SecretRef.SecretName != "" {
			val, err := readSecretKey(ctx, c, agent.Namespace, mp.Spec.SecretRef.SecretName, mp.Spec.SecretRef.Key)
			if err != nil {
				return nil, nil, err
			}
			values[mp.Spec.SecretRef.SecretName] = val
		}
	}
	return provider, values, nil
}
