package agentfs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Restore reads a full snapshot from S3 (per the spec's RestorePolicy)
// and writes it back to the storage driver. R-AFS-4.
func (m *Manager) Restore() (Version, error) {
	if m.Backend != nil {
		return m.backendRestore()
	}
	if m.Spec.Restore == nil {
		// No restore policy → nothing to do.
		return Version{}, nil
	}
	versions, err := m.S3.ListVersions(m.keyForFull())
	if err != nil {
		return Version{}, fmt.Errorf("agentfs: list: %w", err)
	}
	if len(versions) == 0 {
		return Version{}, m.handleMissing()
	}
	picked, err := pickRestoreTarget(versions, m.Spec.Restore)
	if err != nil {
		return Version{}, err
	}
	body, err := m.S3.Get(picked.Key, picked.ID)
	if err != nil {
		return Version{}, fmt.Errorf("agentfs: get version %s: %w", picked.ID, err)
	}
	defer body.Close()

	if err := m.Storage.RestoreFrom(body); err != nil {
		return Version{}, fmt.Errorf("agentfs: restore: %w", err)
	}
	return picked, nil
}

// backendRestore drives the VersionedStore backend (kopia) for the init verb:
// Connect, resolve the ref from RestorePolicy, then materialize into the mount.
// A missing checkpoint honors RestorePolicy.IfMissing (fresh|fail) like the tar
// path's handleMissing.
func (m *Manager) backendRestore() (Version, error) {
	ctx := context.Background()
	if err := m.Backend.Connect(ctx); err != nil {
		return Version{}, fmt.Errorf("agentfs: backend connect: %w", err)
	}
	ref, err := m.backendRef(ctx)
	if err != nil {
		return Version{}, err
	}
	cp, err := m.Backend.Restore(ctx, ref, m.Spec.MountPath)
	if errors.Is(err, ErrNoVersion) || errors.Is(err, ErrRestoreNotFound) {
		return Version{}, m.handleMissing()
	}
	if err != nil {
		return Version{}, fmt.Errorf("agentfs: backend restore: %w", err)
	}
	return Version{ID: cp.ID, Key: "kopia", CreatedAt: cp.CreatedAt, SizeBytes: cp.SizeBytes, Kind: string(SnapshotFull)}, nil
}

// backendRef maps RestorePolicy.Mode to a VersionedStore ref ("latest", a
// checkpoint ID, or the newest checkpoint at/under a pointInTime).
func (m *Manager) backendRef(ctx context.Context) (string, error) {
	if m.Spec.Restore == nil {
		return "latest", nil
	}
	switch m.Spec.Restore.Mode {
	case "", "latest":
		return "latest", nil
	case "versionID":
		return m.Spec.Restore.VersionID, nil
	case "pointInTime":
		t, err := time.Parse(time.RFC3339, m.Spec.Restore.PointInTime)
		if err != nil {
			return "", fmt.Errorf("agentfs: parse pointInTime: %w", err)
		}
		hist, err := m.Backend.History(ctx)
		if err != nil {
			return "", err
		}
		for _, c := range hist { // newest-first
			if !c.CreatedAt.After(t) {
				return c.ID, nil
			}
		}
		return "", fmt.Errorf("%w: no checkpoint <= %s", ErrRestoreNotFound, m.Spec.Restore.PointInTime)
	default:
		return "", fmt.Errorf("agentfs: unknown restore mode %q", m.Spec.Restore.Mode)
	}
}

// pickRestoreTarget selects a Version per RestorePolicy.Mode.
func pickRestoreTarget(versions []Version, policy interface{}) (Version, error) {
	// Newest first.
	sort.SliceStable(versions, func(i, j int) bool {
		return versions[i].CreatedAt.After(versions[j].CreatedAt)
	})
	type restorePolicy interface {
		// silence unused warnings from the operator/v1 wrapper layer
	}
	_ = restorePolicy(nil)
	// We don't tightly couple to the v1 type at runtime to avoid
	// import cycles; the policy is passed as a struct value via
	// `any`-like duck-typing on Mode/VersionID/PointInTime fields.
	mode, vid, pit := readPolicy(policy)
	switch mode {
	case "", "latest":
		return versions[0], nil
	case "versionID":
		for _, v := range versions {
			if v.ID == vid {
				return v, nil
			}
		}
		return Version{}, fmt.Errorf("%w: versionID=%q", ErrRestoreNotFound, vid)
	case "pointInTime":
		t, err := time.Parse(time.RFC3339, pit)
		if err != nil {
			return Version{}, fmt.Errorf("agentfs: parse pointInTime: %w", err)
		}
		// Most recent version older than t.
		for _, v := range versions {
			if !v.CreatedAt.After(t) {
				return v, nil
			}
		}
		return Version{}, fmt.Errorf("%w: no version <= %s", ErrRestoreNotFound, pit)
	default:
		return Version{}, fmt.Errorf("agentfs: unknown restore mode %q", mode)
	}
}

// readPolicy extracts the three fields we care about from the v1
// RestorePolicy. Reflection-light alternative to importing the type
// into restore.go (avoids tight coupling).
func readPolicy(p interface{}) (mode, versionID, pit string) {
	type rp struct {
		Mode        string `json:"mode,omitempty"`
		VersionID   string `json:"versionID,omitempty"`
		PointInTime string `json:"pointInTime,omitempty"`
		IfMissing   string `json:"ifMissing,omitempty"`
	}
	if v, ok := p.(rp); ok {
		return v.Mode, v.VersionID, v.PointInTime
	}
	// The real type is v1.RestorePolicy; use a struct assertion.
	switch v := p.(type) {
	case interface {
		GetMode() string
		GetVersionID() string
		GetPointInTime() string
	}:
		return v.GetMode(), v.GetVersionID(), v.GetPointInTime()
	default:
		// Fallback via JSON round-trip to decouple from import.
		return readPolicyJSON(p)
	}
}

// handleMissing implements RestorePolicy.IfMissing.
func (m *Manager) handleMissing() error {
	policy := m.Spec.Restore
	if policy == nil {
		return nil
	}
	switch policy.IfMissing {
	case "", "fresh":
		return nil
	case "fail":
		return errors.New("agentfs: restore.ifMissing=fail and no versions in S3")
	default:
		return fmt.Errorf("agentfs: unknown ifMissing=%q", policy.IfMissing)
	}
}
