// Package memory — in-memory event log Backend.
//
// EventLogBackend implements memory.Backend as an append-only event log. Each
// Write call appends a Document to a per-tenant+namespace log; Get returns the
// document by ID; Delete marks it as tombstoned. Retrieve returns entries in
// insertion order filtered by query/metadata. ListNamespaces returns the
// distinct namespaces seen.
//
// This is a pure in-memory implementation suitable for development and tests.
// A production event log would persist entries to a durable store (Kafka,
// Kinesis, PostgreSQL with APPEND-only semantics, etc.) — that adapter can
// replace this one behind the same Backend interface without touching the
// gateway or the worker.
//
// Isolation: tenant+namespace keys are enforced in every method (R-MEM-WORK-1,
// R-MEM-SEC-1). Tombstoned entries are hidden from all read paths.
//
// FS-only operations return *ErrNotSupported. Summarize returns *ErrNotSupported.
//
// Implements R-MEM-WORK-2.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// eventEntry is one record in the append-only log.
type eventEntry struct {
	doc       Document
	tombstone bool
}

// EventLogBackend is a thread-safe, in-memory append-only event log Backend.
type EventLogBackend struct {
	mu  sync.RWMutex
	log []eventEntry
	idx map[string]int // id → last log index (for O(1) ID lookup)
}

// NewEventLogBackend returns an empty EventLogBackend.
func NewEventLogBackend() *EventLogBackend {
	return &EventLogBackend{
		idx: make(map[string]int),
	}
}

// ── Backend.Write ─────────────────────────────────────────────────────────────

func (b *EventLogBackend) Write(_ context.Context, doc Document) (WriteResult, error) {
	if doc.Tenant == "" || doc.Namespace == "" {
		return WriteResult{}, Invalid("eventlog write: tenant and namespace are required")
	}
	if doc.ID == "" {
		doc.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}
	doc.UpdatedAt = now
	if doc.Version == "" {
		doc.Version = now.Format(time.RFC3339Nano)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// Append event; the latest entry for an ID wins on Get.
	b.log = append(b.log, eventEntry{doc: doc})
	b.idx[doc.ID] = len(b.log) - 1
	return WriteResult{ID: doc.ID, Version: doc.Version}, nil
}

// ── Backend.Get ───────────────────────────────────────────────────────────────

func (b *EventLogBackend) Get(_ context.Context, id string, filter Filter) (Document, error) {
	if filter.Tenant == "" {
		return Document{}, Invalid("eventlog get: tenant is required")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	idx, ok := b.idx[id]
	if !ok {
		return Document{}, NotFound("eventlog: document not found: " + id)
	}
	entry := b.log[idx]
	if entry.tombstone {
		return Document{}, NotFound("eventlog: document not found: " + id)
	}
	if entry.doc.Tenant != filter.Tenant {
		return Document{}, NotFound("eventlog: document not found: " + id)
	}
	if filter.Namespace != "" && entry.doc.Namespace != filter.Namespace {
		return Document{}, NotFound("eventlog: document not found: " + id)
	}
	return entry.doc, nil
}

// ── Backend.Delete ────────────────────────────────────────────────────────────

func (b *EventLogBackend) Delete(_ context.Context, id string, filter Filter) error {
	if filter.Tenant == "" {
		return Invalid("eventlog delete: tenant is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	idx, ok := b.idx[id]
	if !ok {
		return NotFound("eventlog: document not found: " + id)
	}
	entry := b.log[idx]
	if entry.tombstone {
		return NotFound("eventlog: document not found: " + id)
	}
	if entry.doc.Tenant != filter.Tenant {
		return NotFound("eventlog: document not found: " + id)
	}
	if filter.Namespace != "" && entry.doc.Namespace != filter.Namespace {
		return NotFound("eventlog: document not found: " + id)
	}
	// Append a tombstone event (preserve log integrity).
	tombstoned := entry.doc
	tombstoned.UpdatedAt = time.Now().UTC()
	b.log = append(b.log, eventEntry{doc: tombstoned, tombstone: true})
	b.idx[id] = len(b.log) - 1
	return nil
}

// ── Backend.Retrieve ──────────────────────────────────────────────────────────

// Retrieve scans the log in insertion order. Results are filtered by
// tenant, namespace, metadata, and (if non-empty) query substring match.
// topK limits the returned set.
func (b *EventLogBackend) Retrieve(_ context.Context, query string, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("eventlog retrieve: tenant is required")
	}
	if topK <= 0 {
		topK = 10
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// Collect the latest non-tombstoned version of each document.
	seen := make(map[string]bool) // id → already included
	var candidates []ScoredChunk

	// Iterate in reverse so the latest write for each ID is seen first.
	for i := len(b.log) - 1; i >= 0; i-- {
		e := b.log[i]
		if seen[e.doc.ID] {
			continue
		}
		seen[e.doc.ID] = true
		if e.tombstone {
			continue
		}
		if e.doc.Tenant != filter.Tenant {
			continue
		}
		if filter.Namespace != "" && e.doc.Namespace != filter.Namespace {
			continue
		}
		if !matchMetadata(e.doc.Metadata, filter.Metadata) {
			continue
		}
		var score float32
		if query != "" {
			lower := strings.ToLower(string(e.doc.Content))
			q := strings.ToLower(query)
			terms := strings.Fields(q)
			var hits int
			for _, t := range terms {
				if strings.Contains(lower, t) {
					hits++
				}
			}
			if hits == 0 {
				continue
			}
			if len(terms) > 0 {
				score = float32(hits) / float32(len(terms))
			}
		} else {
			score = 0.5
		}
		candidates = append(candidates, ScoredChunk{
			Chunk: Chunk{
				Text:       string(e.doc.Content),
				DocumentID: e.doc.ID,
				EndByte:    len(e.doc.Content),
			},
			Score: score,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	total := int64(len(candidates))
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	return RetrieveResult{Chunks: candidates, Total: total}, nil
}

// ── Backend.ListNamespaces ────────────────────────────────────────────────────

func (b *EventLogBackend) ListNamespaces(_ context.Context, filter Filter) ([]string, error) {
	if filter.Tenant == "" {
		return nil, PermissionDenied("eventlog list-namespaces: tenant is required")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	seen := make(map[string]struct{})
	for i := len(b.log) - 1; i >= 0; i-- {
		e := b.log[i]
		if e.tombstone {
			continue
		}
		if e.doc.Tenant != filter.Tenant {
			continue
		}
		seen[e.doc.Namespace] = struct{}{}
	}
	nss := make([]string, 0, len(seen))
	for ns := range seen {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	return nss, nil
}

// ── Backend.Summarize ─────────────────────────────────────────────────────────

func (b *EventLogBackend) Summarize(_ context.Context, _ string, _ Filter) (string, error) {
	return "", &ErrNotSupported{Op: "Summarize", Backend: "eventlog"}
}

// ── Filesystem-only stubs ─────────────────────────────────────────────────────

func (b *EventLogBackend) Branch(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Branch", Backend: "eventlog"}
}

func (b *EventLogBackend) Snapshot(_ context.Context, _ string, _ Filter) (SnapshotInfo, error) {
	return SnapshotInfo{}, &ErrNotSupported{Op: "Snapshot", Backend: "eventlog"}
}

func (b *EventLogBackend) ListBranches(_ context.Context, _ Filter) ([]BranchInfo, error) {
	return nil, &ErrNotSupported{Op: "ListBranches", Backend: "eventlog"}
}

func (b *EventLogBackend) Merge(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Merge", Backend: "eventlog"}
}

// compile-time assertion: EventLogBackend satisfies the Backend interface.
var _ Backend = (*EventLogBackend)(nil)
