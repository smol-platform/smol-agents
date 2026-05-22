package quota_test

import (
	"testing"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory"
	"github.com/stigen/smol-agents/pkg/memory/quota"
)

func TestClampTopK_BelowCeiling(t *testing.T) {
	q := v1.QuotaSpec{MaxTopK: 50}
	got, err := quota.ClampTopK(20, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 20 {
		t.Fatalf("want 20, got %d", got)
	}
}

func TestClampTopK_AtCeiling(t *testing.T) {
	q := v1.QuotaSpec{MaxTopK: 50}
	got, err := quota.ClampTopK(50, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 50 {
		t.Fatalf("want 50, got %d", got)
	}
}

func TestClampTopK_ExceedsCeiling(t *testing.T) {
	q := v1.QuotaSpec{MaxTopK: 50}
	_, err := quota.ClampTopK(51, q)
	if err == nil {
		t.Fatal("expected quota error for topK > ceiling")
	}
	if memory.KindOf(err) != memory.KindQuotaExceeded {
		t.Fatalf("want KindQuotaExceeded, got %v", memory.KindOf(err))
	}
}

func TestClampTopK_NoCeiling(t *testing.T) {
	// MaxTopK=0 means unlimited — never reject. R-MEM-QUOTA-1.
	q := v1.QuotaSpec{MaxTopK: 0}
	got, err := quota.ClampTopK(9999, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 9999 {
		t.Fatalf("want 9999, got %d", got)
	}
}

func TestClampTopK_NoSilentTruncation(t *testing.T) {
	// The spec says "never silent truncation": exceeding → error, not clamp.
	q := v1.QuotaSpec{MaxTopK: 10}
	result, err := quota.ClampTopK(100, q)
	if err == nil {
		t.Fatalf("must not silently truncate; got result=%d, want error", result)
	}
}

func TestCheckWriteSize_UnderLimit(t *testing.T) {
	q := v1.QuotaSpec{MaxWriteBytes: 1024}
	if err := quota.CheckWriteSize(512, q); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckWriteSize_OverLimit(t *testing.T) {
	q := v1.QuotaSpec{MaxWriteBytes: 1024}
	err := quota.CheckWriteSize(2048, q)
	if err == nil {
		t.Fatal("expected quota error")
	}
	if memory.KindOf(err) != memory.KindQuotaExceeded {
		t.Fatalf("want KindQuotaExceeded, got %v", memory.KindOf(err))
	}
}

func TestCheckWriteSize_Unlimited(t *testing.T) {
	q := v1.QuotaSpec{MaxWriteBytes: 0}
	if err := quota.CheckWriteSize(1<<30, q); err != nil {
		t.Fatalf("unlimited should never error: %v", err)
	}
}

func TestCheckRate_UnderLimit(t *testing.T) {
	e := quota.NewEnforcer()
	q := v1.QuotaSpec{RequestsPerMinute: 5}
	caller := "spiffe://td/ns/x/sa/y"
	for i := 0; i < 5; i++ {
		if err := e.CheckRate(caller, q); err != nil {
			t.Fatalf("call %d should be allowed: %v", i+1, err)
		}
	}
}

func TestCheckRate_ExceedsLimit(t *testing.T) {
	e := quota.NewEnforcer()
	q := v1.QuotaSpec{RequestsPerMinute: 3}
	caller := "spiffe://td/ns/x/sa/y"
	for i := 0; i < 3; i++ {
		_ = e.CheckRate(caller, q)
	}
	err := e.CheckRate(caller, q) // 4th call should fail
	if err == nil {
		t.Fatal("4th call should exceed rate limit")
	}
	if memory.KindOf(err) != memory.KindQuotaExceeded {
		t.Fatalf("want KindQuotaExceeded, got %v", memory.KindOf(err))
	}
}

func TestCheckRate_Unlimited(t *testing.T) {
	e := quota.NewEnforcer()
	q := v1.QuotaSpec{RequestsPerMinute: 0}
	caller := "spiffe://td/ns/x/sa/y"
	for i := 0; i < 1000; i++ {
		if err := e.CheckRate(caller, q); err != nil {
			t.Fatalf("unlimited should never error: %v", err)
		}
	}
}

func TestCheckRate_PerCallerIsolation(t *testing.T) {
	e := quota.NewEnforcer()
	q := v1.QuotaSpec{RequestsPerMinute: 2}
	callerA := "spiffe://td/ns/a/sa/x"
	callerB := "spiffe://td/ns/b/sa/y"

	// Both callers exhaust their limit independently.
	_ = e.CheckRate(callerA, q)
	_ = e.CheckRate(callerA, q)
	_ = e.CheckRate(callerB, q)
	_ = e.CheckRate(callerB, q)

	if err := e.CheckRate(callerA, q); err == nil {
		t.Fatal("callerA should be rate-limited")
	}
	if err := e.CheckRate(callerB, q); err == nil {
		t.Fatal("callerB should be rate-limited")
	}
}
