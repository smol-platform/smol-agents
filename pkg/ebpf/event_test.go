package ebpf

import (
	"sync"
	"testing"
	"time"
)

func TestMemoryBus_Subscribe_Publish(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()
	ch := bus.Subscribe("syscalls", 4)
	bus.Publish(Event{Source: "syscalls", PID: 1, Timestamp: time.Now()})
	select {
	case e := <-ch:
		if e.PID != 1 {
			t.Errorf("got pid %d", e.PID)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestMemoryBus_DropOnSlow(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()
	_ = bus.Subscribe("syscalls", 1) // never read
	for i := 0; i < 100; i++ {
		bus.Publish(Event{Source: "syscalls", PID: uint32(i)})
	}
	if bus.Drops() == 0 {
		t.Error("expected drops on slow subscriber")
	}
}

func TestMemoryBus_MultipleSubscribers(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()
	ch1 := bus.Subscribe("net", 4)
	ch2 := bus.Subscribe("net", 4)
	bus.Publish(Event{Source: "net", PID: 7})
	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case e := <-ch:
			if e.PID != 7 {
				t.Errorf("sub %d got pid %d", i, e.PID)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d no event", i)
		}
	}
}

func TestMemoryBus_Close(t *testing.T) {
	bus := NewMemoryBus()
	ch := bus.Subscribe("x", 1)
	bus.Close()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel closed")
		}
	case <-time.After(time.Second):
		t.Fatal("subscribe channel not closed after Close")
	}
	// idempotent
	bus.Close()
}

func TestMemoryBus_Concurrent(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()
	const N = 4
	subs := make([]<-chan Event, N)
	for i := range subs {
		subs[i] = bus.Subscribe("x", 100)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			bus.Publish(Event{Source: "x", PID: uint32(i)})
		}
	}()
	wg.Wait()
}

func TestProgramResolve(t *testing.T) {
	p := Program{Name: "syscalls"}.Resolve("/etc/bpf")
	if p.ObjectPath != "/etc/bpf/syscalls.bpf.o" {
		t.Errorf("ObjectPath = %q", p.ObjectPath)
	}
	p2 := Program{Name: "net", ObjectPath: "/already/set.o"}.Resolve("/x")
	if p2.ObjectPath != "/already/set.o" {
		t.Errorf("explicit path overwritten: %q", p2.ObjectPath)
	}
}
