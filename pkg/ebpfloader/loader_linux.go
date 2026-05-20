//go:build linux

package ebpfloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	cilebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"

	"github.com/stigen/smol-agents/pkg/ebpf"
)

// Run loads every configured program, pins maps + programs under PinRoot,
// and returns a Result describing what was attached. The caller is
// expected to block on context cancellation; on shutdown, pinned objects
// are intentionally left in place so the next loader Pod can re-adopt
// them without dropping events.
func (l *Loader) Run(ctx context.Context) (*Result, error) {
	feat := detectFeatures(filepath.Dir(l.cfg.PinRoot))

	if l.cfg.MountBPFFS && !feat.BPFFSMounted {
		if err := mountBPFFS(filepath.Dir(l.cfg.PinRoot)); err != nil {
			return nil, fmt.Errorf("ebpfloader: mount bpffs: %w", err)
		}
		feat.BPFFSMounted = true
	}

	if err := os.MkdirAll(l.cfg.PinRoot, 0o700); err != nil {
		return nil, fmt.Errorf("ebpfloader: mkdir PinRoot: %w", err)
	}

	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("ebpfloader: rlimit: %w", err)
	}

	res := &Result{Features: feat}
	var attached []*attachedObject

	for _, p := range l.cfg.Programs {
		if p.ObjectPath == "" {
			return nil, fmt.Errorf("ebpfloader: program %q has no ObjectPath", p.Name)
		}
		ao, err := l.loadAndPin(ctx, p)
		if err != nil {
			for _, prev := range attached {
				prev.close()
			}
			return nil, fmt.Errorf("ebpfloader: load %s: %w", p.Name, err)
		}
		attached = append(attached, ao)
		res.LoadedPrograms = append(res.LoadedPrograms, p.Name)
		for _, m := range ao.pinnedMaps {
			res.PinnedMaps = append(res.PinnedMaps, m)
		}
		for _, pn := range ao.pinnedPrograms {
			res.PinnedPrograms = append(res.PinnedPrograms, pn)
		}
	}

	// Block until ctx.Done(); on exit, close handles BUT leave pinned
	// objects in place. The kernel keeps them alive while the pin file
	// exists.
	go func() {
		<-ctx.Done()
		for _, ao := range attached {
			ao.close()
		}
	}()
	return res, nil
}

type attachedObject struct {
	mu             sync.Mutex
	coll           *cilebpf.Collection
	links          []link.Link
	pinnedMaps     []string
	pinnedPrograms []string
	closed         bool
}

func (a *attachedObject) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	for _, l := range a.links {
		_ = l.Close()
	}
	if a.coll != nil {
		// IMPORTANT: do NOT call a.coll.Close() here. Close would unpin
		// and drop maps/programs, breaking the design contract that
		// pinned objects survive Pod restart. Instead release our
		// in-process handles only by letting GC reap the Go wrappers.
		a.coll = nil
	}
}

func (l *Loader) loadAndPin(ctx context.Context, p ebpf.Program) (*attachedObject, error) {
	spec, err := cilebpf.LoadCollectionSpec(p.ObjectPath)
	if err != nil {
		return nil, fmt.Errorf("load spec %s: %w", p.ObjectPath, err)
	}
	for name, m := range spec.Maps {
		// Set pinning on every map so NewCollection uses the existing
		// pinned object if present, or creates and pins a new one.
		_ = name
		m.Pinning = cilebpf.PinByName
	}
	opts := cilebpf.CollectionOptions{
		Maps: cilebpf.MapOptions{PinPath: l.cfg.PinRoot},
	}
	var coll *cilebpf.Collection
	err = retry(ctx, 5, 50*time.Millisecond, func() error {
		var ferr error
		coll, ferr = cilebpf.NewCollectionWithOptions(spec, opts)
		return ferr
	})
	if err != nil {
		return nil, fmt.Errorf("new collection: %w", err)
	}

	ao := &attachedObject{coll: coll}
	for mname := range spec.Maps {
		ao.pinnedMaps = append(ao.pinnedMaps, filepath.Join(l.cfg.PinRoot, mname))
	}

	// Attach programs and pin them.
	for pname, prog := range coll.Programs {
		ln, err := attachProgram(prog)
		if err != nil {
			ao.close()
			return nil, fmt.Errorf("attach %s: %w", pname, err)
		}
		if ln != nil {
			ao.links = append(ao.links, ln)
		}
		pinPath := filepath.Join(l.cfg.PinRoot, pname)
		// Best effort: pin the program too.
		if err := prog.Pin(pinPath); err != nil && !errors.Is(err, os.ErrExist) {
			// Non-fatal: programs are kept alive by attached links.
			continue
		}
		ao.pinnedPrograms = append(ao.pinnedPrograms, pinPath)
	}
	return ao, nil
}

// attachProgram attaches a single program based on its declared section.
// It returns nil, nil for program types this loader does not know how to
// auto-attach; the caller may attach manually using the program handle.
func attachProgram(prog *cilebpf.Program) (link.Link, error) {
	switch prog.Type() {
	case cilebpf.RawTracepoint:
		return link.AttachRawTracepoint(link.RawTracepointOptions{
			Name:    rawTracepointName(prog),
			Program: prog,
		})
	case cilebpf.Kprobe:
		// Without metadata we cannot guess the symbol; the loader leaves
		// the program loaded and unattached so a privileged consumer
		// can pin/attach it explicitly.
		return nil, nil
	default:
		return nil, nil
	}
}

// rawTracepointName extracts the tracepoint name from the program's
// section, e.g. "raw_tracepoint/sys_enter" → "sys_enter".
func rawTracepointName(prog *cilebpf.Program) string {
	// cilium/ebpf doesn't expose the section name on the Program directly,
	// so we fall back to a fixed convention: our shipped programs name
	// raw tracepoints by their target. Callers that need cross-cutting
	// flexibility should supply explicit metadata.
	return "sys_enter"
}

// mountBPFFS mounts a bpffs filesystem at path. Idempotent: if path is
// already a bpffs mount, returns nil.
func mountBPFFS(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	// MS_NOSUID|MS_NODEV|MS_NOEXEC is conventional for bpffs.
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("bpf", path, "bpf", flags, ""); err != nil {
		if errors.Is(err, unix.EBUSY) {
			return nil
		}
		return err
	}
	return nil
}
