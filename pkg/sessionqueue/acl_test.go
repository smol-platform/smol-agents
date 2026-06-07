package sessionqueue

import (
	"strings"
	"testing"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// The security invariant (M2.20, D1): a worker's permissions reference ONLY its
// own namespace — no subject grants access to another tenant's turns/results or
// consumers. _INBOX.> is the sole unscoped allow (random per-connection inboxes).
func TestWorkerPermissions_NamespaceScoped(t *testing.T) {
	pub, sub := WorkerPermissions("tenant-a")
	if len(pub) == 0 || len(sub) == 0 {
		t.Fatal("expected non-empty pub/sub allow-lists")
	}
	for _, s := range append(append([]string{}, pub...), sub...) {
		nsScoped := strings.Contains(s, ".tenant-a.") || strings.Contains(s, "w_tenant-a_")
		if !nsScoped && s != "_INBOX.>" {
			t.Errorf("subject %q is neither tenant-a-scoped nor _INBOX.> — possible cross-tenant leak", s)
		}
		if strings.Contains(s, "tenant-b") {
			t.Errorf("subject %q references another tenant", s)
		}
	}

	// Two namespaces share no tenant data subject.
	pubB, _ := WorkerPermissions("tenant-b")
	dataA := map[string]bool{}
	for _, s := range pub {
		if strings.HasPrefix(s, subjectPrefix+".") {
			dataA[s] = true
		}
	}
	for _, s := range pubB {
		if dataA[s] {
			t.Errorf("tenant-a and tenant-b share data subject %q", s)
		}
	}
}

// MintWorkerCreds signs a parseable user creds file whose baked-in permissions are
// exactly the ns-scoped allow-lists.
func TestMintWorkerCreds(t *testing.T) {
	akp, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := akp.Seed()
	if err != nil {
		t.Fatal(err)
	}

	creds, err := MintWorkerCreds(seed, "tenant-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.Contains(string(creds), "USER NKEY SEED") {
		t.Error("creds must embed the user seed the worker presents")
	}

	ujwt, err := jwt.ParseDecoratedJWT(creds)
	if err != nil {
		t.Fatalf("parse creds jwt: %v", err)
	}
	uc, err := jwt.DecodeUserClaims(ujwt)
	if err != nil {
		t.Fatalf("decode user claims: %v", err)
	}
	apub, _ := akp.PublicKey()
	if uc.Issuer != apub {
		t.Errorf("creds issuer = %q, want the account %q", uc.Issuer, apub)
	}
	wantPub, wantSub := WorkerPermissions("tenant-a")
	if strings.Join(uc.Permissions.Pub.Allow, ",") != strings.Join(wantPub, ",") {
		t.Errorf("pub perms = %v, want %v", uc.Permissions.Pub.Allow, wantPub)
	}
	if strings.Join(uc.Permissions.Sub.Allow, ",") != strings.Join(wantSub, ",") {
		t.Errorf("sub perms = %v, want %v", uc.Permissions.Sub.Allow, wantSub)
	}
}

func TestMintWorkerCreds_BadSeed(t *testing.T) {
	if _, err := MintWorkerCreds([]byte("not-a-seed"), "t"); err == nil {
		t.Error("a bad account seed must error")
	}
}
