//go:build !linux

package ebpf

import "context"

// noopLoader returns ErrUnsupportedOS on Load.
type noopLoader struct{ bus EventBus }

func NewLoader(bus EventBus, _ int) Loader {
	if bus == nil {
		bus = NewMemoryBus()
	}
	return &noopLoader{bus: bus}
}

func (l *noopLoader) Load(_ context.Context, _ ...Program) (Manager, error) {
	return nil, ErrUnsupportedOS
}
