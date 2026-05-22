package secrets

import (
	"github.com/smol-platform/smol-agents/pkg/agentnet/proxy"
)

// The adapter must satisfy the proxy's CredentialMinter contract so the
// sidecar can wire the broker as its credential source.
var _ proxy.CredentialMinter = CredentialMinterAdapter{}
