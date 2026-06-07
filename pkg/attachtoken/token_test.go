package attachtoken

import "testing"

var testKey = []byte("test-operator-attach-signing-key")

func baseClaims() Claims {
	return Claims{
		Subject:  "alice@example.com",
		Role:     RoleDriver,
		Audience: Audience("smol-agents.ai", "tenant-a", "claude"),
		AgentRef: "tenant-a/claude",
		Expiry:   1000,
	}
}

// M4.6: a minted token round-trips and yields its claims when verified against
// its own audience before expiry.
func TestMintVerify_RoundTrip(t *testing.T) {
	tok, err := Mint(testKey, baseClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := Verify(testKey, tok, baseClaims().Audience, 999)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "alice@example.com" || got.Role != RoleDriver {
		t.Errorf("claims = %+v", got)
	}
}

// M4.6 (the core property): a token minted for one agent cannot be replayed
// against another — Verify against a different agent's audience fails.
func TestVerify_AudienceBoundNoCrossAgentReplay(t *testing.T) {
	tok, _ := Mint(testKey, baseClaims())
	otherAud := Audience("smol-agents.ai", "tenant-a", "codex") // different agent
	if _, err := Verify(testKey, tok, otherAud, 999); err != ErrAudience {
		t.Fatalf("replay at another agent must fail ErrAudience, got %v", err)
	}
	// Cross-tenant replay is also blocked (audience embeds the namespace).
	crossNS := Audience("smol-agents.ai", "tenant-b", "claude")
	if _, err := Verify(testKey, tok, crossNS, 999); err != ErrAudience {
		t.Fatalf("cross-tenant replay must fail ErrAudience, got %v", err)
	}
}

func TestVerify_Expiry(t *testing.T) {
	tok, _ := Mint(testKey, baseClaims())
	if _, err := Verify(testKey, tok, baseClaims().Audience, 1000); err != ErrExpired {
		t.Fatalf("at exp must be expired, got %v", err)
	}
	if _, err := Verify(testKey, tok, baseClaims().Audience, 1001); err != ErrExpired {
		t.Fatalf("after exp must be expired, got %v", err)
	}
}

func TestVerify_TamperAndKeyMismatch(t *testing.T) {
	tok, _ := Mint(testKey, baseClaims())
	// Wrong key → signature fails.
	if _, err := Verify([]byte("other-key"), tok, baseClaims().Audience, 999); err != ErrSignature {
		t.Fatalf("wrong key must fail ErrSignature, got %v", err)
	}
	// Tampered payload → signature fails.
	if _, err := Verify(testKey, "x"+tok, baseClaims().Audience, 999); err == nil {
		t.Fatal("tampered token must not verify")
	}
	// Malformed (no separator).
	if _, err := Verify(testKey, "nodot", baseClaims().Audience, 999); err != ErrMalformed {
		t.Fatalf("malformed must fail ErrMalformed, got %v", err)
	}
}

func TestMint_RejectsBadRole(t *testing.T) {
	c := baseClaims()
	c.Role = "admin"
	if _, err := Mint(testKey, c); err != ErrRole {
		t.Fatalf("bad role must fail ErrRole, got %v", err)
	}
}
