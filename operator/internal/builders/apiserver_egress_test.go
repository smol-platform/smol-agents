package builders

import "testing"

func TestAPIServerEgressRule(t *testing.T) {
	if r := APIServerEgressRule(nil, 6443); r != nil {
		t.Fatalf("no IPs must yield a nil rule (floor unchanged), got %v", r)
	}
	// v4 → /32, v6 → /128, invalid skipped.
	r := APIServerEgressRule([]string{"203.0.113.5", "2001:db8::1", "not-an-ip"}, 6443)
	if r == nil {
		t.Fatal("expected a rule for valid IPs")
	}
	if len(r.To) != 2 {
		t.Fatalf("want 2 peers (invalid skipped), got %d", len(r.To))
	}
	if r.To[0].IPBlock.CIDR != "203.0.113.5/32" {
		t.Errorf("v4 peer CIDR = %q, want 203.0.113.5/32", r.To[0].IPBlock.CIDR)
	}
	if r.To[1].IPBlock.CIDR != "2001:db8::1/128" {
		t.Errorf("v6 peer CIDR = %q, want 2001:db8::1/128", r.To[1].IPBlock.CIDR)
	}
	if len(r.Ports) != 1 || r.Ports[0].Port.IntValue() != 6443 {
		t.Errorf("want a single port 6443, got %v", r.Ports)
	}
}
