package identity

import (
	"strings"
	"testing"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

var stigen = spiffeid.RequireTrustDomainFromString("stigen.ai")

func mustID(t *testing.T, s string) spiffeid.ID {
	t.Helper()
	id, err := spiffeid.FromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAuthorizeAny(t *testing.T) {
	a := AuthorizeAny{TrustDomain: stigen}
	cases := map[string]bool{
		"spiffe://stigen.ai/ns/agents/sa/a": true,
		"spiffe://other.test/ns/agents":     false,
	}
	for s, want := range cases {
		err := a.Authorize(&x509svid.SVID{ID: mustID(t, s)})
		got := err == nil
		if got != want {
			t.Errorf("AuthorizeAny(%q) = %v err=%v, want %v", s, got, err, want)
		}
	}
}

func TestAuthorizeIDs(t *testing.T) {
	id := mustID(t, "spiffe://stigen.ai/ns/agents/sa/agent-a")
	a := AuthorizeIDs{IDs: []spiffeid.ID{id}}
	if err := a.Authorize(&x509svid.SVID{ID: id}); err != nil {
		t.Fatalf("expected match: %v", err)
	}
	other := mustID(t, "spiffe://stigen.ai/ns/agents/sa/agent-b")
	if err := a.Authorize(&x509svid.SVID{ID: other}); err == nil {
		t.Fatal("expected reject for non-listed id")
	}
}

func TestAuthorizePathPrefix(t *testing.T) {
	a := AuthorizePathPrefix{TrustDomain: stigen, Prefix: "/ns/agents"}
	if err := a.Authorize(&x509svid.SVID{ID: mustID(t, "spiffe://stigen.ai/ns/agents/sa/a")}); err != nil {
		t.Fatalf("expected match: %v", err)
	}
	if err := a.Authorize(&x509svid.SVID{ID: mustID(t, "spiffe://stigen.ai/ns/other/sa/x")}); err == nil {
		t.Fatal("expected reject for wrong prefix")
	}
	if err := a.Authorize(&x509svid.SVID{ID: mustID(t, "spiffe://other.test/ns/agents/sa/a")}); err == nil {
		t.Fatal("expected reject for wrong trust domain")
	}
}

func TestParseAuthorizer(t *testing.T) {
	for _, d := range []string{
		"any:spiffe://stigen.ai",
		"prefix:spiffe://stigen.ai/ns/agents",
		"spiffe://stigen.ai/ns/agents/sa/a",
	} {
		if _, err := ParseAuthorizer(stigen, d); err != nil {
			t.Errorf("ParseAuthorizer(%q) error: %v", d, err)
		}
	}
	if _, err := ParseAuthorizer(stigen, "garbage"); err == nil {
		t.Error("expected error for garbage descriptor")
	}
}

func TestParseAuthorizers_Composite(t *testing.T) {
	auth, err := ParseAuthorizers(stigen, []string{
		"prefix:spiffe://stigen.ai/ns/agents",
		"spiffe://stigen.ai/ns/control/sa/admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	good := mustID(t, "spiffe://stigen.ai/ns/agents/sa/x")
	if err := auth.Authorize(&x509svid.SVID{ID: good}); err != nil {
		t.Errorf("expected accept for %q: %v", good, err)
	}
	admin := mustID(t, "spiffe://stigen.ai/ns/control/sa/admin")
	if err := auth.Authorize(&x509svid.SVID{ID: admin}); err != nil {
		t.Errorf("expected accept for %q: %v", admin, err)
	}
	bad := mustID(t, "spiffe://stigen.ai/ns/intruder/sa/x")
	err = auth.Authorize(&x509svid.SVID{ID: bad})
	if err == nil {
		t.Error("expected reject")
	}
	if !strings.Contains(err.Error(), "no authorizer matched") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseAuthorizers_EmptyDescriptors(t *testing.T) {
	auth, err := ParseAuthorizers(stigen, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Authorize(&x509svid.SVID{ID: mustID(t, "spiffe://stigen.ai/anywhere")}); err != nil {
		t.Errorf("expected default-any to accept: %v", err)
	}
}

func TestModeParse(t *testing.T) {
	for _, ok := range []string{"insecure", "permissive", "strict"} {
		if _, err := ParseMode(ok); err != nil {
			t.Errorf("ParseMode(%q): %v", ok, err)
		}
	}
	if _, err := ParseMode("degraded"); err == nil {
		t.Error("ParseMode(degraded) should reject")
	}
}
