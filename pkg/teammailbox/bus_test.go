package teammailbox

import (
	"context"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

func TestMemBus_PubSub(t *testing.T) {
	ctx := context.Background()
	bus := NewMemBus()

	// An event published to a topic nobody subscribed to is dropped.
	_ = bus.Publish(ctx, BusEvent{Topic: "findings", From: "alice", Body: "x"})
	if got, _ := bus.Receive(ctx, 0); len(got) != 0 {
		t.Fatalf("unsubscribed topic must drop: %d", len(got))
	}
	// After subscribing, events on that topic are received; other topics are not.
	_ = bus.Subscribe(ctx, "findings")
	_ = bus.Publish(ctx, BusEvent{Topic: "findings", From: "alice", Body: "found A"})
	_ = bus.Publish(ctx, BusEvent{Topic: "noise", From: "bob", Body: "ignored"})
	got, _ := bus.Receive(ctx, 0)
	if len(got) != 1 || got[0].Body != "found A" || got[0].Topic != "findings" {
		t.Fatalf("subscribed delivery wrong: %+v", got)
	}
}

func TestBusSubject(t *testing.T) {
	if got := BusSubject("tenant-a", "researchers", "findings"); got != "agentteam.tenant-a.researchers.bus.findings" {
		t.Fatalf("bus subject: %q", got)
	}
}

func TestMemberBusPermissions_TeamScoped(t *testing.T) {
	pub, sub := MemberBusPermissions("tenant-a", "researchers")
	wantStem := "agentteam.tenant-a.researchers.bus.*"
	if len(pub) != 1 || pub[0] != wantStem {
		t.Fatalf("pub allow must be the team bus subtree only: %+v", pub)
	}
	// Subscribe = team bus subtree + _INBOX.> — and nothing that escapes the team.
	for _, s := range sub {
		if s != wantStem && s != "_INBOX.>" {
			t.Fatalf("unexpected bus sub allow %q (must stay in-team)", s)
		}
		if strings.Contains(s, "tenant-a.") && !strings.Contains(s, ".researchers.") && s != "_INBOX.>" {
			t.Fatalf("bus permission escaped the team: %q", s)
		}
	}
}

func TestMintMemberBusCreds(t *testing.T) {
	akp, _ := nkeys.CreateAccount()
	seed, _ := akp.Seed()
	creds, err := MintMemberBusCreds(seed, "tenant-a", "researchers", "alice")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.Contains(string(creds), "BEGIN NATS USER JWT") {
		t.Fatalf("not a decorated user config")
	}
}
