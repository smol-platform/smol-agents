package wireguard

// TestDriverPrivKey / TestDriverPubKey are the WireGuard keypair the
// L0 e2e test driver uses to dial wg-hub. The matching public key is
// configured as a peer in scripts/e2e/wireguard/wg0.conf so wg-hub
// recognizes us. TestHubPrivKey / TestHubPubKey is the corresponding
// hub-side pair — the driver registers TestHubPubKey as its peer.
//
// These are TEST-ONLY keys. They're checked in deliberately because
// nothing sensitive routes through the L0 stack — wg-hub never
// reaches anything outside the compose network. Don't reuse them.
const (
	TestDriverPrivKey = "YNONZR3ZrSkGbllQN1SuQsAZwaWZgXMIYaNKsPly/0w="
	TestDriverPubKey  = "3KJpkmMoN1EB3NQux7w8X37BGNX6suNOWd1geYjKwU4="
	TestHubPrivKey    = "2LyfcbUQjZ8vpJZs5Est4eNTW22GDe1lf/cqpLo7F2A="
	TestHubPubKey     = "xF0tad7gR3HxjecZa4CG9U32tV/cBgXZ+JrfQPYLTho="
)
