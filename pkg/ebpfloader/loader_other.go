//go:build !linux

package ebpfloader

import "context"

// Run on non-Linux returns ErrPlatformUnsupported. Useful so the rest of
// the project's tests compile on macOS.
func (l *Loader) Run(_ context.Context) (*Result, error) {
	return nil, ErrPlatformUnsupported
}
