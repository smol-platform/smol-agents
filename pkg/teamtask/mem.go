package teamtask

import (
	"context"
	"strconv"
	"sync"
)

// MemStore is an in-memory Store for tests and single-process dev. The mutex
// makes Claim/Complete trivially atomic.
type MemStore struct {
	mu    sync.Mutex
	tasks map[string]Task
	seq   int
}

// NewMemStore returns an empty in-memory task list.
func NewMemStore() *MemStore {
	return &MemStore{tasks: map[string]Task{}}
}

func (m *MemStore) Create(_ context.Context, t Task) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == "" {
		m.seq++
		t.ID = "task-" + strconv.Itoa(m.seq)
	}
	if t.State == "" {
		t.State = TaskPending
	}
	m.tasks[t.ID] = t
	return t.ID, nil
}

func (m *MemStore) List(_ context.Context) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	return out, nil
}

func (m *MemStore) Get(_ context.Context, id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return t, nil
}

func (m *MemStore) Claim(_ context.Context, id, owner string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	if t.State != TaskPending || !depsSatisfied(t, m.tasks) {
		return Task{}, ErrNotClaimable
	}
	t.State = TaskInProgress
	t.Owner = owner
	m.tasks[id] = t
	return t, nil
}

func (m *MemStore) Complete(_ context.Context, id, owner, result string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if t.State != TaskInProgress {
		return ErrNotClaimable
	}
	if t.Owner != owner {
		return ErrNotOwner
	}
	t.State = TaskCompleted
	t.Result = result
	m.tasks[id] = t
	return nil
}

func (m *MemStore) Close() error { return nil }
