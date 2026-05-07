package ebpf

import (
	"context"
	"errors"
	"path/filepath"
)

// Program is a name + location for an eBPF program object on disk.
//
// The actual loading happens in loader_linux.go (linux build tag); on
// other platforms NewLoader returns a no-op that fails at Load time so
// non-Linux unit tests can still build.
type Program struct {
	Name       string // e.g. "syscalls"
	ObjectPath string // path to .bpf.o
}

// Resolve fills in default ObjectPath relative to objectsDir if blank.
func (p Program) Resolve(objectsDir string) Program {
	if p.ObjectPath == "" {
		p.ObjectPath = filepath.Join(objectsDir, p.Name+".bpf.o")
	}
	return p
}

// Manager owns the lifetime of a set of attached BPF programs.
type Manager interface {
	// Detach removes all attached programs and closes maps.
	Detach() error
	// Bus returns the EventBus serving ring-buffer events.
	Bus() EventBus
	// Programs returns the loaded programs.
	Programs() []string
}

// Loader is the interface used by cmd/agent.
type Loader interface {
	Load(ctx context.Context, progs ...Program) (Manager, error)
}

// ErrUnsupportedOS indicates the loader cannot run on this platform.
var ErrUnsupportedOS = errors.New("ebpf: not supported on this OS")
