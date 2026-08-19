// Package blob stores message bodies and MIME parts outside the database.
//
// Objects are content-addressed: the key is derived from the SHA-256 of the
// bytes, so identical content always lands at the same key and storing it twice
// is idempotent. That is what makes deduplication safe — a message appearing in
// several mailboxes references one object.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotFound is returned when a key has no object.
var ErrNotFound = errors.New("blob not found")

// Type classifies an object, and forms the first segment of its key.
//
// The Rust implementation derived this prefix from a debug-formatted enum in
// one place and a lowercase literal in another, so the same object could be
// accounted for under two names and its reference count split between them.
// Here the only values that exist are these constants.
type Type string

const (
	TypeRFC822   Type = "rfc822"
	TypeMIMEPart Type = "mime_part"
)

// Valid reports whether t is a known object type.
func (t Type) Valid() bool { return t == TypeRFC822 || t == TypeMIMEPart }

// Key is the location of an object in the store.
type Key string

// NewKey builds a content-addressed key from a digest.
func NewKey(t Type, digest []byte) Key {
	return Key(fmt.Sprintf("%s/%s", t, hex.EncodeToString(digest)))
}

// Type returns the object type encoded in the key.
func (k Key) Type() Type {
	prefix, _, ok := strings.Cut(string(k), "/")
	if !ok {
		return ""
	}
	return Type(prefix)
}

// Digest returns the raw digest encoded in the key.
func (k Key) Digest() ([]byte, error) {
	_, hexDigest, ok := strings.Cut(string(k), "/")
	if !ok {
		return nil, fmt.Errorf("malformed blob key %q", k)
	}
	return hex.DecodeString(hexDigest)
}

// Valid reports whether the key is well formed.
func (k Key) Valid() bool {
	if !k.Type().Valid() {
		return false
	}
	digest, err := k.Digest()
	return err == nil && len(digest) == sha256.Size
}

func (k Key) String() string { return string(k) }

// Info describes a stored object.
type Info struct {
	Key    Key
	Size   int64
	SHA256 []byte
}

// Store is a content-addressed object store.
type Store interface {
	// Put streams r into the store, returning the content-addressed location.
	// Storing identical bytes twice is idempotent and returns the same key.
	Put(ctx context.Context, t Type, r io.Reader) (Info, error)

	// Get opens an object for reading. The caller must close the reader.
	Get(ctx context.Context, key Key) (io.ReadCloser, error)

	// GetRange opens a byte range, for serving partial FETCH requests without
	// transferring an entire message.
	GetRange(ctx context.Context, key Key, offset, length int64) (io.ReadCloser, error)

	// Stat reports an object's size without transferring it.
	Stat(ctx context.Context, key Key) (Info, error)

	// Delete removes an object. Deleting an absent object is not an error, so
	// that a repeated sweep is safe.
	Delete(ctx context.Context, key Key) error
}

// hashingWriter computes a digest over everything written through it.
type hashingWriter struct {
	inner io.Writer
	hash  interface {
		io.Writer
		Sum([]byte) []byte
	}
	size int64
}

func (w *hashingWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if n > 0 {
		// Hash exactly what was accepted downstream, so a short write cannot
		// produce a digest for bytes that were never stored.
		_, _ = w.hash.Write(p[:n])
		w.size += int64(n)
	}
	return n, err
}
