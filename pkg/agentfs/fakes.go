package agentfs

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

// FakeStorage is an in-memory SQLite stand-in. It treats `data` as the
// canonical DB blob and `wal` as the in-flight WAL frames.
type FakeStorage struct {
	mu   sync.Mutex
	Data []byte
	Wal  []byte
}

func (f *FakeStorage) SnapshotTo(w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := w.Write(f.Data)
	return err
}

func (f *FakeStorage) WALFrames() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.Wal
	f.Wal = nil
	return out, nil
}

func (f *FakeStorage) RestoreFrom(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Data = b
	return nil
}

// FakeS3 is an in-memory S3 with versioning.
type FakeS3 struct {
	mu         sync.Mutex
	objects    map[string][]storedVersion
	versioning bool
	failHas    bool
	now        func() time.Time
	nextID     int64
}

type storedVersion struct {
	id   string
	body []byte
	t    time.Time
}

// NewFakeS3 returns a versioned-by-default in-memory S3.
func NewFakeS3() *FakeS3 {
	return &FakeS3{
		objects:    map[string][]storedVersion{},
		versioning: true,
		now:        time.Now,
	}
}

func (s *FakeS3) HasVersioning() (bool, error) {
	if s.failHas {
		return false, errors.New("fakes3: HasVersioning forced error")
	}
	return s.versioning, nil
}

// SetVersioning is a test helper.
func (s *FakeS3) SetVersioning(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versioning = on
}

func (s *FakeS3) Put(key string, body io.Reader, _ PutMeta) (Version, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return Version{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := nextIDString(s.nextID)
	t := s.now()
	s.objects[key] = append(s.objects[key], storedVersion{id: id, body: b, t: t})
	return Version{ID: id, Key: key, CreatedAt: t, SizeBytes: int64(len(b))}, nil
}

func (s *FakeS3) ListVersions(key string) ([]Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.objects[key]
	out := make([]Version, 0, len(versions))
	for _, v := range versions {
		out = append(out, Version{ID: v.id, Key: key, CreatedAt: v.t, SizeBytes: int64(len(v.body))})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *FakeS3) Get(key, versionID string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.objects[key]
	if len(versions) == 0 {
		return nil, errors.New("fakes3: not found")
	}
	if versionID == "" {
		// latest
		v := versions[len(versions)-1]
		return io.NopCloser(bytes.NewReader(v.body)), nil
	}
	for _, v := range versions {
		if v.id == versionID {
			return io.NopCloser(bytes.NewReader(v.body)), nil
		}
	}
	return nil, errors.New("fakes3: version not found")
}

func (s *FakeS3) Delete(key, versionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.objects[key]
	for i, v := range versions {
		if v.id == versionID {
			s.objects[key] = append(versions[:i], versions[i+1:]...)
			return nil
		}
	}
	return errors.New("fakes3: version not found for delete")
}

// SetClock lets tests advance time deterministically.
func (s *FakeS3) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func nextIDString(n int64) string {
	return "v" + itoa(n)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	out := make([]byte, 0, 16)
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
