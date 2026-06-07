package teammailbox

import (
	"context"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

func TestMemMailbox_SendReceiveIsolation(t *testing.T) {
	ctx := context.Background()
	mb := NewMemMailbox()

	if err := mb.Send(ctx, Message{From: "alice", To: "bob", Body: "hi bob"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := mb.Send(ctx, Message{From: "alice", To: "bob", Body: "second"}); err != nil {
		t.Fatalf("send2: %v", err)
	}
	// Alice's own inbox is empty — she sent, did not receive.
	if got, _ := mb.Receive(ctx, "alice", 0); len(got) != 0 {
		t.Fatalf("sender inbox must be empty, got %d", len(got))
	}
	// Bob receives both, FIFO.
	got, _ := mb.Receive(ctx, "bob", 0)
	if len(got) != 2 || got[0].Body != "hi bob" || got[1].Body != "second" {
		t.Fatalf("bob inbox FIFO wrong: %+v", got)
	}
	// Drained.
	if again, _ := mb.Receive(ctx, "bob", 0); len(again) != 0 {
		t.Fatalf("inbox must drain, got %d", len(again))
	}
}

func TestMemMailbox_ReceiveMax(t *testing.T) {
	ctx := context.Background()
	mb := NewMemMailbox()
	for i := 0; i < 5; i++ {
		_ = mb.Send(ctx, Message{To: "m", Body: "x"})
	}
	got, _ := mb.Receive(ctx, "m", 2)
	if len(got) != 2 {
		t.Fatalf("max=2: want 2, got %d", len(got))
	}
	rest, _ := mb.Receive(ctx, "m", 0)
	if len(rest) != 3 {
		t.Fatalf("rest: want 3, got %d", len(rest))
	}
}

func TestMemberMailboxPermissions_Isolation(t *testing.T) {
	pub, sub := MemberMailboxPermissions("tenant-a", "researchers", "alice")
	// Publish: only this team's mbox wildcard (send to any teammate in-team).
	if len(pub) != 1 || pub[0] != "agentteam.tenant-a.researchers.mbox.*" {
		t.Fatalf("pub allow wrong: %+v", pub)
	}
	// Subscribe: only alice's own inbox (+ _INBOX.> for replies) — NOT a wildcard,
	// NOT another member, NOT another team/tenant.
	wantInbox := "agentteam.tenant-a.researchers.mbox.alice"
	foundInbox, foundInboxReply := false, false
	for _, s := range sub {
		switch s {
		case wantInbox:
			foundInbox = true
		case "_INBOX.>":
			foundInboxReply = true
		default:
			t.Fatalf("unexpected sub allow %q (member must read only its own inbox)", s)
		}
		if strings.Contains(s, ".mbox.*") {
			t.Fatalf("member must NOT subscribe a wildcard inbox: %q", s)
		}
	}
	if !foundInbox || !foundInboxReply {
		t.Fatalf("sub must allow own inbox + _INBOX.>: %+v", sub)
	}
}

func TestMintMemberCreds(t *testing.T) {
	akp, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	seed, err := akp.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	creds, err := MintMemberCreds(seed, "tenant-a", "researchers", "alice")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	s := string(creds)
	if !strings.Contains(s, "BEGIN NATS USER JWT") || !strings.Contains(s, "BEGIN USER NKEY SEED") {
		t.Fatalf("creds not a decorated user config:\n%s", s)
	}
}
