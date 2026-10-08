package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local keeps files in a folder: each at Root/<key>, written as <key>.part while its upload is in flight and renamed
// when it is complete. On a cluster the folder must be the same shared one on every api and worker server (NFS, EFS,
// CephFS, a ReadWriteMany volume): Manager checks that it is.
type Local struct {
	Root string
}

func (l *Local) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.Contains(key, `\`) {
		return "", fmt.Errorf("storage: bad key %q", key)
	}
	return filepath.Join(l.Root, filepath.FromSlash(key)), nil
}

func (l *Local) Begin(_ context.Context, key, _ string) (string, error) {
	p, err := l.path(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	f, err := os.OpenFile(p+".part", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640) //nolint:gosec // p is under Root: path() refuses keys that leave it
	if err != nil {
		return "", err
	}
	return "", f.Close()
}

// PutPart writes the part at its offset in the .part file. Parts may come in any order and at once, from different
// servers: each writes its own range.
func (l *Local) PutPart(_ context.Context, key, _ string, _ int, offset, size int64, r io.Reader) (string, error) {
	p, err := l.path(key)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(p+".part", os.O_WRONLY, 0) //nolint:gosec // p is under Root: path() refuses keys that leave it
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }() // closed below; this is for the early returns
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(io.NewOffsetWriter(f, offset), h), io.LimitReader(r, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", ErrSize
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:32], f.Close()
}

func (l *Local) Complete(_ context.Context, key, _ string, _ []Part, size int64) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	st, err := os.Stat(p + ".part")
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if st.Size() != size {
		return ErrSize
	}
	if err := os.Rename(p+".part", p); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

func (l *Local) Abort(_ context.Context, key, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	return ignoreMissing(os.Remove(p + ".part"))
}

type localFile struct {
	*os.File
	st fs.FileInfo
}

func (f localFile) Size() int64        { return f.st.Size() }
func (f localFile) ModTime() time.Time { return f.st.ModTime() }

func (l *Local) Open(_ context.Context, key string) (File, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // p is under Root: path() refuses keys that leave it
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return localFile{f, st}, nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	return errors.Join(ignoreMissing(os.Remove(p)), ignoreMissing(os.Remove(p+".part")))
}

func (l *Local) Probe(_ context.Context) error {
	if err := os.MkdirAll(l.Root, 0o750); err != nil {
		return err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	p := filepath.Join(l.Root, ".probe-"+hex.EncodeToString(b))
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	got, err := os.ReadFile(p) //nolint:gosec // the probe file just written under Root
	if err == nil && string(got) != string(b) {
		err = errors.New("storage: a file read back differs from what was written")
	}
	return errors.Join(err, os.Remove(p))
}

func ignoreMissing(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // a folder under Root
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
