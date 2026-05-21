package secrets

import (
	"context"
	"time"
)

// CredentialMinterAdapter adapts a *Client to the agentnet proxy's
// CredentialMinter interface — Mint(ctx, name, trat) (value, expiry, err) —
// without the proxy package importing this one (the match is structural).
//
// The returned value is the freshly minted provider credential; the proxy
// injects it into the upstream request and never returns it to the agent.
// R-SEGR-INJECT-1.
type CredentialMinterAdapter struct{ Client *Client }

func (a CredentialMinterAdapter) Mint(ctx context.Context, name, compactTraT string) ([]byte, time.Time, error) {
	lease, err := a.Client.Mint(ctx, name, compactTraT)
	if err != nil {
		return nil, time.Time{}, err
	}
	return lease.Value, lease.ExpiresAt, nil
}
