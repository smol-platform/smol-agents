// Package memory — durable, embedded, pure-Go vector Backend.
//
// PersistentVectorBackend wraps the in-memory VectorBackend with gob-snapshot
// persistence to a single file (on a PVC), so a self-host install gets a
// DURABLE vector store with NO external database (pgvector/Qdrant) and NO cgo —
// the binary stays CGO_ENABLED=0. This is the lighter self-host default the
// 7fr.5 analysis called for; it deliberately does NOT use cgo sqlite-vec
// (which would force CGO on the build). Cosine ranking + tenant/namespace
// isolation are inherited unchanged from VectorBackend.
//
// Model: the full index is held in memory and snapshotted to disk after each
// mutation (atomic temp-file + rename), and loaded on startup. This suits the
// platform's mid-scale (~100s concurrent, modest corpora) self-host target; a
// debounced/segmented snapshot is a future optimization. For large corpora,
// pgvector/Qdrant remain the opt-in path.
package memory

import (
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// vectorState is the on-disk snapshot of a VectorBackend. The namespace
// presence set is rebuilt from Docs on load, so it isn't serialized.
type vectorState struct {
	Entries []VectorEntry
	Docs    map[string]Document
}

// exportState returns a deep-enough copy of the index for serialization.
// Documents/Chunks are value types (slices/maps are referenced, but the
// snapshot is written immediately and not mutated), so a shallow copy of the
// slices/maps is safe for the encode that follows.
func (b *VectorBackend) exportState() vectorState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entries := make([]VectorEntry, len(b.entries))
	copy(entries, b.entries)
	docs := make(map[string]Document, len(b.docs))
	for k, v := range b.docs {
		docs[k] = v
	}
	return vectorState{Entries: entries, Docs: docs}
}

// importState replaces the index contents and rebuilds the namespace set.
func (b *VectorBackend) importState(s vectorState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = s.Entries
	if s.Docs != nil {
		b.docs = s.Docs
	} else {
		b.docs = make(map[string]Document)
	}
	b.rebuildNSLocked()
}

// PersistentVectorBackend is a VectorBackend that snapshots to a gob file.
type PersistentVectorBackend struct {
	*VectorBackend
	path   string
	saveMu sync.Mutex // serializes snapshot writes
}

// NewPersistentVectorBackend constructs a durable vector backend backed by the
// gob snapshot at path. If the file exists it is loaded; otherwise an empty
// index is created and the parent directory is ensured.
func NewPersistentVectorBackend(path string) (*PersistentVectorBackend, error) {
	if path == "" {
		return nil, Invalid("persistent vector backend: path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("persistent vector backend: mkdir: %w", err)
	}
	b := &PersistentVectorBackend{VectorBackend: NewVectorBackend(), path: path}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
}

// load reads the snapshot file into the in-memory index. A missing file is not
// an error (first run).
func (b *PersistentVectorBackend) load() error {
	f, err := os.Open(b.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("persistent vector backend: open: %w", err)
	}
	defer f.Close()
	var s vectorState
	if err := gob.NewDecoder(f).Decode(&s); err != nil {
		return fmt.Errorf("persistent vector backend: decode %s: %w", b.path, err)
	}
	b.importState(s)
	return nil
}

// persist snapshots the current index to disk atomically (temp + rename).
func (b *PersistentVectorBackend) persist() error {
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	s := b.exportState()
	tmp := b.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("persistent vector backend: create tmp: %w", err)
	}
	if err := gob.NewEncoder(f).Encode(s); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("persistent vector backend: encode: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("persistent vector backend: close tmp: %w", err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return fmt.Errorf("persistent vector backend: rename: %w", err)
	}
	return nil
}

// Write stores+indexes a document, then snapshots.
func (b *PersistentVectorBackend) Write(ctx context.Context, doc Document) (WriteResult, error) {
	res, err := b.VectorBackend.Write(ctx, doc)
	if err != nil {
		return res, err
	}
	return res, b.persist()
}

// WriteChunk indexes a chunk, then snapshots.
func (b *PersistentVectorBackend) WriteChunk(ctx context.Context, doc Document, chunk Chunk) error {
	if err := b.VectorBackend.WriteChunk(ctx, doc, chunk); err != nil {
		return err
	}
	return b.persist()
}

// Delete removes a document, then snapshots.
func (b *PersistentVectorBackend) Delete(ctx context.Context, id string, filter Filter) error {
	if err := b.VectorBackend.Delete(ctx, id, filter); err != nil {
		return err
	}
	return b.persist()
}

// compile-time assertion: PersistentVectorBackend satisfies the Backend interface.
var _ Backend = (*PersistentVectorBackend)(nil)
