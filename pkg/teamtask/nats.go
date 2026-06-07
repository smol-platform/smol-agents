package teamtask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

// casAttempts bounds how many times Claim/Complete re-read + retry a lost CAS
// race before giving up with ErrConflict.
const casAttempts = 5

// NATSStore is a JetStream KV–backed Store: one bucket per team
// (team_<ns>_<team>), one key per task, atomic claim via key-revision CAS.
type NATSStore struct {
	nc *nats.Conn
	kv nats.KeyValue
}

// NATSStoreOption tunes the connection (e.g. per-namespace credentials).
type NATSStoreOption func(*[]nats.Option)

// WithCredentials authenticates with a NATS .creds file (the operator-minted,
// per-namespace team credential).
func WithCredentials(path string) NATSStoreOption {
	return func(o *[]nats.Option) {
		if path != "" {
			*o = append(*o, nats.UserCredentials(path))
		}
	}
}

// NewNATSStore connects to url and binds (creating if absent) the team's KV
// bucket. The bucket is the GC/ACL unit: the operator owns its lifecycle and
// scopes each member's NATS credential to it.
func NewNATSStore(url, namespace, team string, opts ...NATSStoreOption) (*NATSStore, error) {
	connOpts := []nats.Option{nats.Name("smol-agents-teamtask"), nats.MaxReconnects(-1)}
	for _, f := range opts {
		f(&connOpts)
	}
	nc, err := nats.Connect(url, connOpts...)
	if err != nil {
		return nil, fmt.Errorf("teamtask: connect %q: %w", url, err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("teamtask: jetstream: %w", err)
	}
	bucket := BucketName(namespace, team)
	kv, err := js.KeyValue(bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, History: 1})
	}
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("teamtask: bind bucket %q: %w", bucket, err)
	}
	return &NATSStore{nc: nc, kv: kv}, nil
}

func (s *NATSStore) Create(_ context.Context, t Task) (string, error) {
	if t.ID == "" {
		t.ID = "task-" + nuid.Next()
	}
	if t.State == "" {
		t.State = TaskPending
	}
	data, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	if _, err := s.kv.Create(t.ID, data); err != nil {
		return "", fmt.Errorf("teamtask: create %s: %w", t.ID, err)
	}
	return t.ID, nil
}

func (s *NATSStore) Get(_ context.Context, id string) (Task, error) {
	t, _, err := s.get(id)
	return t, err
}

// get returns the task plus its KV revision (for CAS).
func (s *NATSStore) get(id string) (Task, uint64, error) {
	e, err := s.kv.Get(id)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return Task{}, 0, ErrNotFound
	}
	if err != nil {
		return Task{}, 0, err
	}
	var t Task
	if err := json.Unmarshal(e.Value(), &t); err != nil {
		return Task{}, 0, err
	}
	return t, e.Revision(), nil
}

func (s *NATSStore) List(_ context.Context) ([]Task, error) {
	keys, err := s.kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(keys))
	for _, k := range keys {
		t, _, err := s.get(k)
		if errors.Is(err, ErrNotFound) {
			continue // deleted between Keys() and Get()
		}
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *NATSStore) snapshotByID() (map[string]Task, error) {
	all, err := s.List(context.Background())
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Task, len(all))
	for _, t := range all {
		byID[t.ID] = t
	}
	return byID, nil
}

func (s *NATSStore) Claim(_ context.Context, id, owner string) (Task, error) {
	for attempt := 0; attempt < casAttempts; attempt++ {
		t, rev, err := s.get(id)
		if err != nil {
			return Task{}, err
		}
		byID, err := s.snapshotByID()
		if err != nil {
			return Task{}, err
		}
		if t.State != TaskPending || !depsSatisfied(t, byID) {
			return Task{}, ErrNotClaimable
		}
		t.State = TaskInProgress
		t.Owner = owner
		data, err := json.Marshal(t)
		if err != nil {
			return Task{}, err
		}
		if _, err := s.kv.Update(id, data, rev); err == nil {
			return t, nil
		}
		// Lost the CAS race (revision moved) — re-read and retry.
	}
	return Task{}, ErrConflict
}

func (s *NATSStore) Complete(_ context.Context, id, owner, result string) error {
	for attempt := 0; attempt < casAttempts; attempt++ {
		t, rev, err := s.get(id)
		if err != nil {
			return err
		}
		if t.State != TaskInProgress {
			return ErrNotClaimable
		}
		if t.Owner != owner {
			return ErrNotOwner
		}
		t.State = TaskCompleted
		t.Result = result
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err := s.kv.Update(id, data, rev); err == nil {
			return nil
		}
	}
	return ErrConflict
}

func (s *NATSStore) Close() error {
	if s.nc != nil {
		s.nc.Close()
	}
	return nil
}
