package agentfs

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FilesystemStorage implements Storage over a directory tree (the AgentFS
// mount): a snapshot is a gzipped tar of the tree, restore replaces it. It does
// full snapshots only — WALFrames is a no-op — so it suits an agent's working
// files (durable across Runs via S3) without SQLite-specific WAL streaming,
// which remains a future enhancement. Use a SQLite-aware Storage when
// branchable-FS WAL semantics are required.
type FilesystemStorage struct {
	// Root is the directory backed up / restored.
	Root string
}

// SnapshotTo writes a gzipped tar of Root to w.
func (s FilesystemStorage) SnapshotTo(w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(s.Root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Only regular files + directories (skip symlinks/devices/sockets).
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.Root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// RestoreFrom replaces Root's contents with the gzipped tar in r.
func (s FilesystemStorage) RestoreFrom(r io.Reader) error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(s.Root, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // size-bounded by the snapshot we wrote
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// WALFrames is a no-op: FilesystemStorage does full snapshots only.
func (s FilesystemStorage) WALFrames() ([]byte, error) { return nil, nil }

// safeJoin joins root + a tar entry name, rejecting any entry that would escape
// root (e.g. "../"). A snapshot we produced never contains such entries; this is
// defense-in-depth against a tampered archive.
func safeJoin(root, name string) (string, error) {
	target := filepath.Join(root, filepath.FromSlash(name))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("agentfs: tar entry %q escapes root", name)
	}
	return target, nil
}
