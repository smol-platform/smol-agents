package policy_test

import (
	"testing"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory"
	"github.com/stigen/smol-agents/pkg/memory/policy"
)

var checker = policy.Checker{}

func grants(gs ...v1.MemoryGrant) []v1.MemoryGrant { return gs }

func grant(identity string, ops []v1.MemoryOperation, namespaces []string) v1.MemoryGrant {
	return v1.MemoryGrant{Identity: identity, Operations: ops, Namespaces: namespaces}
}

func TestChecker_DenyByDefault(t *testing.T) {
	// Empty policy → deny everything. R-MEM-AUTH-2.
	err := checker.Allow("spiffe://td/ns/foo/sa/bar", v1.MemoryOpRead, "docs", nil)
	if err == nil {
		t.Fatal("empty policy should deny")
	}
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want KindPermissionDenied, got %v", memory.KindOf(err))
	}
}

func TestChecker_ExactIdentityGrant(t *testing.T) {
	caller := "spiffe://td/ns/foo/sa/bar"
	policy := grants(grant(caller, []v1.MemoryOperation{v1.MemoryOpRead}, []string{"docs"}))

	if err := checker.Allow(caller, v1.MemoryOpRead, "docs", policy); err != nil {
		t.Fatalf("should be allowed: %v", err)
	}
}

func TestChecker_PrefixIdentityGrant(t *testing.T) {
	prefix := "spiffe://td/ns/team-alpha/"
	caller := "spiffe://td/ns/team-alpha/sa/coder"
	pg := grants(grant(prefix, []v1.MemoryOperation{v1.MemoryOpRead}, []string{"*"}))

	if err := checker.Allow(caller, v1.MemoryOpRead, "anydocs", pg); err != nil {
		t.Fatalf("prefix grant should allow: %v", err)
	}
}

func TestChecker_PrefixDoesNotMatchDifferentTenant(t *testing.T) {
	prefix := "spiffe://td/ns/team-alpha/"
	other := "spiffe://td/ns/team-beta/sa/coder"
	pg := grants(grant(prefix, []v1.MemoryOperation{v1.MemoryOpRead}, []string{"*"}))

	err := checker.Allow(other, v1.MemoryOpRead, "docs", pg)
	if err == nil {
		t.Fatal("prefix grant for team-alpha should not match team-beta")
	}
}

func TestChecker_OperationsAreIndependent(t *testing.T) {
	caller := "spiffe://td/ns/foo/sa/bar"
	// Only read is granted.
	pg := grants(grant(caller, []v1.MemoryOperation{v1.MemoryOpRead}, []string{"docs"}))

	if err := checker.Allow(caller, v1.MemoryOpRead, "docs", pg); err != nil {
		t.Fatalf("read should be allowed: %v", err)
	}
	if err := checker.Allow(caller, v1.MemoryOpWrite, "docs", pg); err == nil {
		t.Fatal("write should be denied (not in grant)")
	}
	if err := checker.Allow(caller, v1.MemoryOpDelete, "docs", pg); err == nil {
		t.Fatal("delete should be denied (not in grant)")
	}
}

func TestChecker_NamespaceWildcard(t *testing.T) {
	caller := "spiffe://td/ns/foo/sa/bar"
	pg := grants(grant(caller, []v1.MemoryOperation{v1.MemoryOpRead, v1.MemoryOpWrite}, []string{"*"}))

	for _, ns := range []string{"docs", "images", "anything"} {
		if err := checker.Allow(caller, v1.MemoryOpRead, ns, pg); err != nil {
			t.Errorf("wildcard ns should allow %q: %v", ns, err)
		}
	}
}

func TestChecker_NamespaceExact_NoWildcard(t *testing.T) {
	caller := "spiffe://td/ns/foo/sa/bar"
	pg := grants(grant(caller, []v1.MemoryOperation{v1.MemoryOpRead}, []string{"docs"}))

	if err := checker.Allow(caller, v1.MemoryOpRead, "other", pg); err == nil {
		t.Fatal("exact namespace grant should not match different namespace")
	}
}

func TestChecker_MultipleGrantsFirstMatch(t *testing.T) {
	caller := "spiffe://td/ns/foo/sa/bar"
	other := "spiffe://td/ns/other/sa/x"
	pg := grants(
		grant(other, []v1.MemoryOperation{v1.MemoryOpDelete}, []string{"*"}),
		grant(caller, []v1.MemoryOperation{v1.MemoryOpRead, v1.MemoryOpWrite}, []string{"docs"}),
	)

	if err := checker.Allow(caller, v1.MemoryOpRead, "docs", pg); err != nil {
		t.Fatalf("second grant should allow: %v", err)
	}
	// caller has no delete grant.
	if err := checker.Allow(caller, v1.MemoryOpDelete, "docs", pg); err == nil {
		t.Fatal("caller has no delete grant")
	}
}

func TestChecker_EmptyIdentityInGrant(t *testing.T) {
	// A grant with empty Identity should never match.
	caller := "spiffe://td/ns/foo/sa/bar"
	pg := grants(grant("", []v1.MemoryOperation{v1.MemoryOpRead}, []string{"*"}))

	err := checker.Allow(caller, v1.MemoryOpRead, "docs", pg)
	if err == nil {
		t.Fatal("empty Identity in grant should never match")
	}
}
