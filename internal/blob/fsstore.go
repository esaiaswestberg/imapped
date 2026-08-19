package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FSStore keeps objects on a local filesystem.
type FSStore struct {
	root string
}

// NewFSStore creates a store rooted at dir, creating it if necessary.
func NewFSStore(dir string) (*FSStore, error) {
	if dir == "" {
		return nil, errors.New("blob: filesystem store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating blob directory %s: %w", dir, err)
	}
	return &FSStore{root: dir}, nil
}

// path maps a key to a file location.
//
// The digest's first two bytes become intermediate directories: a flat layout
// puts hundreds of thousands of entries in one directory, which degrades badly
// on most filesystems and makes routine operations like listing unusable.
func (s *FSStore) path(key Key) (string, error) {
	if !key.Valid() {
		return "", fmt.Errorf("blob: invalid key %q", key)
	}
	_, hexDigest, _ := cut(string(key), "/")
	return filepath.Join(s.root, string(key.Type()), hexDigest[:2], hexDigest[2:4], hexDigest), nil
}

func (s *FSStore) Put(ctx context.Context, t Type, r io.Reader) (Info, error) {
	if !t.Valid() {
		return Info{}, fmt.Errorf("blob: invalid object type %q", t)
	}

	// The digest is not known until the content has been read, so write to a
	// temporary file first and rename it into place once the key is known.
	// The rename is atomic, so a crash mid-write can never leave a partial
	// object visible under a key that claims to describe complete content.
	tmp, err := os.CreateTemp(s.root, ".partial-*")
	if err != nil {
		return Info{}, fmt.Errorf("creating temporary blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename has succeeded
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return Info{}, fmt.Errorf("writing blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return Info{}, fmt.Errorf("flushing blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Info{}, fmt.Errorf("closing blob: %w", err)
	}

	digest := hasher.Sum(nil)
	key := NewKey(t, digest)
	dest, err := s.path(key)
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return Info{}, fmt.Errorf("creating blob directory: %w", err)
	}

	// Content addressing makes this idempotent: if the object already exists it
	// holds identical bytes, so overwriting is harmless.
	if err := os.Rename(tmpName, dest); err != nil {
		return Info{}, fmt.Errorf("storing blob: %w", err)
	}

	return Info{Key: key, Size: written, SHA256: digest}, ctx.Err()
}

func (s *FSStore) Get(ctx context.Context, key Key) (io.ReadCloser, error) {
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", key, err)
	}
	return f, ctx.Err()
}

func (s *FSStore) GetRange(ctx context.Context, key Key, offset, length int64) (io.ReadCloser, error) {
	f, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	file, ok := f.(*os.File)
	if !ok {
		f.Close()
		return nil, errors.New("blob: unexpected reader type")
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			return nil, fmt.Errorf("seeking in blob %s: %w", key, err)
		}
	}
	if length <= 0 {
		return file, nil
	}
	return &limitedFile{File: file, Reader: io.LimitReader(file, length)}, nil
}

func (s *FSStore) Stat(ctx context.Context, key Key) (Info, error) {
	path, err := s.path(key)
	if err != nil {
		return Info{}, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return Info{}, fmt.Errorf("stating blob %s: %w", key, err)
	}
	digest, err := key.Digest()
	if err != nil {
		return Info{}, err
	}
	return Info{Key: key, Size: info.Size(), SHA256: digest}, ctx.Err()
}

func (s *FSStore) Delete(ctx context.Context, key Key) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting blob %s: %w", key, err)
	}
	return ctx.Err()
}

// limitedFile bounds a read while keeping the underlying file closable.
type limitedFile struct {
	*os.File
	Reader io.Reader
}

func (l *limitedFile) Read(p []byte) (int, error) { return l.Reader.Read(p) }

func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

var _ Store = (*FSStore)(nil)
