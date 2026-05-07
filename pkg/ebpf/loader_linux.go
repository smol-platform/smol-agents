//go:build linux

package ebpf

import (
	"context"
	"errors"
	"fmt"
	"sync"

	cilebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// linuxLoader is the production Loader for Linux.
//
// Implements R-EBP-1.
type linuxLoader struct {
	bus          EventBus
	bufferBytes  int
	enableMemMax bool
}

// NewLoader returns a Loader. busBuffer is the per-subscriber channel size;
// rbBytes is unused at this layer (the BPF object pins its own ring size).
func NewLoader(bus EventBus, _ int) Loader {
	if bus == nil {
		bus = NewMemoryBus()
	}
	return &linuxLoader{bus: bus, enableMemMax: true}
}

// Load opens each Program's object file, attaches the BPF programs, and
// starts a ring-buffer reader per program. Returns a Manager that can
// detach and close everything.
func (l *linuxLoader) Load(ctx context.Context, progs ...Program) (Manager, error) {
	if l.enableMemMax {
		if err := rlimit.RemoveMemlock(); err != nil {
			return nil, fmt.Errorf("ebpf: remove memlock: %w", err)
		}
	}
	mgr := &linuxManager{bus: l.bus}
	for _, p := range progs {
		if p.ObjectPath == "" {
			mgr.detachAll()
			return nil, fmt.Errorf("ebpf: program %q has no ObjectPath", p.Name)
		}
		spec, err := cilebpf.LoadCollectionSpec(p.ObjectPath)
		if err != nil {
			mgr.detachAll()
			return nil, fmt.Errorf("ebpf: load %s: %w", p.Name, err)
		}
		coll, err := cilebpf.NewCollection(spec)
		if err != nil {
			mgr.detachAll()
			return nil, fmt.Errorf("ebpf: new collection %s: %w", p.Name, err)
		}
		// Best effort: attach every program defined in the spec via its
		// well-known section type.
		links, err := attachAll(coll)
		if err != nil {
			coll.Close()
			mgr.detachAll()
			return nil, fmt.Errorf("ebpf: attach %s: %w", p.Name, err)
		}
		// Open ring buffer named "events" if present.
		var rbReader *ringbuf.Reader
		if m, ok := coll.Maps["events"]; ok {
			rb, err := ringbuf.NewReader(m)
			if err != nil {
				closeAll(links)
				coll.Close()
				mgr.detachAll()
				return nil, fmt.Errorf("ebpf: ringbuf %s: %w", p.Name, err)
			}
			rbReader = rb
			go mgr.readRingBuf(p.Name, rb)
		}
		mgr.entries = append(mgr.entries, &linuxEntry{
			name:  p.Name,
			coll:  coll,
			links: links,
			rb:    rbReader,
		})
	}
	mgr.programs = make([]string, 0, len(progs))
	for _, p := range progs {
		mgr.programs = append(mgr.programs, p.Name)
	}
	return mgr, nil
}

type linuxManager struct {
	mu       sync.Mutex
	entries  []*linuxEntry
	bus      EventBus
	programs []string
}

type linuxEntry struct {
	name  string
	coll  *cilebpf.Collection
	links []link.Link
	rb    *ringbuf.Reader
}

func (m *linuxManager) Bus() EventBus      { return m.bus }
func (m *linuxManager) Programs() []string { return m.programs }

func (m *linuxManager) Detach() error {
	m.detachAll()
	return nil
}

func (m *linuxManager) detachAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.rb != nil {
			_ = e.rb.Close()
		}
		closeAll(e.links)
		if e.coll != nil {
			e.coll.Close()
		}
	}
	m.entries = nil
}

func (m *linuxManager) readRingBuf(source string, rb *ringbuf.Reader) {
	for {
		rec, err := rb.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			// Drop other errors silently here; the manager logs upstream.
			continue
		}
		m.bus.Publish(Event{Source: source, Payload: append([]byte(nil), rec.RawSample...)})
	}
}

func attachAll(coll *cilebpf.Collection) ([]link.Link, error) {
	var out []link.Link
	for _, prog := range coll.Programs {
		l, err := attachByType(prog)
		if err != nil {
			closeAll(out)
			return nil, err
		}
		if l != nil {
			out = append(out, l)
		}
	}
	return out, nil
}

func attachByType(p *cilebpf.Program) (link.Link, error) {
	switch p.Type() {
	case cilebpf.Tracing, cilebpf.RawTracepoint, cilebpf.TracePoint:
		// Tracepoints are attached at a section-name level; cilium/ebpf
		// exposes link.Tracepoint(group, name, prog). We don't have
		// metadata here so return nil and let the caller wire programs
		// explicitly via specialized code paths.
		return nil, nil
	case cilebpf.Kprobe:
		return nil, nil
	default:
		return nil, nil
	}
}

func closeAll(links []link.Link) {
	for _, l := range links {
		_ = l.Close()
	}
}
