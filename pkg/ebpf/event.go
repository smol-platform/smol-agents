package ebpf

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Event is a single eBPF observation delivered to the bus.
type Event struct {
	Source    string // "syscalls" | "network" | ...
	PID       uint32
	CGroupID  uint64
	Timestamp time.Time
	Payload   []byte // typed by Source
}

// EventBus fans events out to multiple subscribers without blocking
// upstream readers. Implements R-EBP-2.
type EventBus interface {
	// Subscribe returns a channel of events for the named source.
	// The channel is closed when the bus is shut down.
	Subscribe(source string, buffer int) <-chan Event

	// Publish delivers an event to all subscribers of e.Source.
	// Slow subscribers see drops (DropOnSlow) by default; the
	// drop counter is exposed via Drops().
	Publish(e Event)

	// Drops returns the cumulative drop count.
	Drops() uint64

	// Close terminates the bus and closes all subscriber channels.
	Close()
}

// DropOnSlow is a small in-memory bus implementation. Tests can replace it.
type memoryBus struct {
	mu     sync.RWMutex
	subs   map[string][]chan Event
	drops  uint64
	closed bool
}

// NewMemoryBus returns an EventBus suitable for in-process use.
func NewMemoryBus() EventBus {
	return &memoryBus{subs: make(map[string][]chan Event)}
}

func (b *memoryBus) Subscribe(source string, buffer int) <-chan Event {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch
	}
	b.subs[source] = append(b.subs[source], ch)
	return ch
}

func (b *memoryBus) Publish(e Event) {
	b.mu.RLock()
	channels := b.subs[e.Source]
	b.mu.RUnlock()
	for _, ch := range channels {
		select {
		case ch <- e:
		default:
			b.mu.Lock()
			b.drops++
			b.mu.Unlock()
		}
	}
}

func (b *memoryBus) Drops() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.drops
}

func (b *memoryBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, chans := range b.subs {
		for _, ch := range chans {
			close(ch)
		}
	}
	b.subs = nil
}

// Drain helper: consumes ch until ctx is done or ch is closed,
// invoking fn for each event. Returns ctx.Err if ctx ends first.
func Drain(ctx context.Context, ch <-chan Event, fn func(Event)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-ch:
			if !ok {
				return errors.New("ebpf: channel closed")
			}
			fn(e)
		}
	}
}
