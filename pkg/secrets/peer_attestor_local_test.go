package secrets

import "testing"

func TestLocalIDForUID(t *testing.T) {
	id, err := LocalIDForUID("", 65532)
	if err != nil {
		t.Fatalf("LocalIDForUID: %v", err)
	}
	if got := id.String(); got != "spiffe://local.smol-agents/uid/65532" {
		t.Errorf("default-TD id = %q", got)
	}

	id2, err := LocalIDForUID("local.test", 1000)
	if err != nil {
		t.Fatalf("LocalIDForUID custom: %v", err)
	}
	if got := id2.String(); got != "spiffe://local.test/uid/1000" {
		t.Errorf("custom-TD id = %q", got)
	}
}

func TestNewLocalPeerAttestor(t *testing.T) {
	a, err := NewLocalPeerAttestor("")
	if err != nil {
		t.Fatalf("NewLocalPeerAttestor: %v", err)
	}
	if a.TrustDomain.Name() != DefaultLocalTrustDomain {
		t.Errorf("trust domain = %q, want %q", a.TrustDomain.Name(), DefaultLocalTrustDomain)
	}
}
