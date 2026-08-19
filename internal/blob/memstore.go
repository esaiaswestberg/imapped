package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
)

// MemStore keeps objects in memory, for tests and for running without any
// configured storage.
type MemStore struct {
	mu      sync.RWMutex
	objects map[Key][]byte
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{objects: make(map[Key][]byte)}
}

func (s *MemStore) Put(ctx context.Context, t Type, r io.Reader) (Info, error) {
	if !t.Valid() {
		return Info{}, fmt.Errorf("blob: invalid object type %q", t)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return Info{}, fmt.Errorf("reading blob: %w", err)
	}
	digest := sha256.Sum256(data)
	key := NewKey(t, digest[:])

	s.mu.Lock()
	s.objects[key] = data
	s.mu.Unlock()

	return Info{Key: key, Size: int64(len(data)), SHA256: digest[:]}, ctx.Err()
}

func (s *MemStore) Get(ctx context.Context, key Key) (io.ReadCloser, error) {
	s.mu.RLock()
	data, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(data)), ctx.Err()
}

func (s *MemStore) GetRange(ctx context.Context, key Key, offset, length int64) (io.ReadCloser, error) {
	s.mu.RLock()
	data, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	data = data[offset:]
	if length > 0 && length < int64(len(data)) {
		data = data[:length]
	}
	return io.NopCloser(bytes.NewReader(data)), ctx.Err()
}

func (s *MemStore) Stat(ctx context.Context, key Key) (Info, error) {
	s.mu.RLock()
	data, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return Info{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	digest, err := key.Digest()
	if err != nil {
		return Info{}, err
	}
	return Info{Key: key, Size: int64(len(data)), SHA256: digest}, ctx.Err()
}

func (s *MemStore) Delete(ctx context.Context, key Key) error {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return ctx.Err()
}

// Len reports how many objects are stored, for test assertions about
// deduplication.
func (s *MemStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.objects)
}

var _ Store = (*MemStore)(nil)
