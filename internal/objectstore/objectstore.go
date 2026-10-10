// Package objectstore abstracts durable artifact storage (SRS 5.3). Large
// artifacts live in object storage; metadata and access control live in
// PostgreSQL. Implementations: local filesystem (development) and S3 or
// S3-compatible storage (production).
package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

type Store interface {
	// Put stores the object and returns its SHA-256 and size. It must only
	// return nil once the object is durable.
	Put(ctx context.Context, key string, r io.Reader, contentType string) (sha string, size int64, err error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._\-]+`)

// Key builds a tenant-scoped object key. Every component is sanitized so a
// crafted artifact name cannot escape its tenant/job prefix.
func Key(tenantID, jobID, artifactID, name string) string {
	clean := func(s string) string {
		s = unsafe.ReplaceAllString(s, "_")
		s = strings.Trim(s, "._")
		if s == "" {
			s = "x"
		}
		if len(s) > 120 {
			s = s[:120]
		}
		return s
	}
	return path.Join("tenants", clean(tenantID), "jobs", clean(jobID), clean(artifactID), clean(name))
}

// FS stores objects on a local filesystem.
type FS struct{ Root string }

func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &FS{Root: root}, nil
}

func (f *FS) path(key string) (string, error) {
	p := filepath.Join(f.Root, filepath.FromSlash(key))
	rel, err := filepath.Rel(f.Root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	return p, nil
}

func (f *FS) Put(_ context.Context, key string, r io.Reader, _ string) (string, int64, error) {
	p, err := f.path(key)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (f *FS) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return fh, err
}

func (f *FS) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
