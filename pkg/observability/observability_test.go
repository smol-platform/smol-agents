package observability

import (
	"context"
	"log/slog"
	"testing"
)

func TestInit_NoEndpoint_NoOp(t *testing.T) {
	shut, err := Init(context.Background(), Config{ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shut(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestMustLogger(t *testing.T) {
	l := MustLogger(slog.LevelInfo)
	if l == nil {
		t.Fatal("nil logger")
	}
}

func TestJoinShutdown(t *testing.T) {
	called := 0
	a := func(context.Context) error { called++; return nil }
	b := func(context.Context) error { called++; return nil }
	if err := JoinShutdown(a, b, nil)(context.Background()); err != nil {
		t.Errorf("JoinShutdown: %v", err)
	}
	if called != 2 {
		t.Errorf("called = %d, want 2", called)
	}
}
