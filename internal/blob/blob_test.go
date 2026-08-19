package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/esaiaswestberg/imapped/internal/blob"
)

// stores runs a test body against every implementation, so the filesystem and
// in-memory backends cannot drift apart in behaviour.
func stores(t *testing.T) map[string]blob.Store {
	t.Helper()
	fs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating filesystem store: %v", err)
	}
	return map[string]blob.Store{
		"filesystem": fs,
		"memory":     blob.NewMemStore(),
	}
}

func TestPutAndGetRoundTrip(t *testing.T) {
	content := []byte("From: a@example.com\r\nSubject: hello\r\n\r\nbody\r\n")

	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			info, err := store.Put(ctx, blob.TypeRFC822, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if info.Size != int64(len(content)) {
				t.Errorf("Size = %d, want %d", info.Size, len(content))
			}
			if !info.Key.Valid() {
				t.Errorf("Put produced an invalid key %q", info.Key)
			}
			if info.Key.Type() != blob.TypeRFC822 {
				t.Errorf("key type = %q, want rfc822", info.Key.Type())
			}

			r, err := store.Get(ctx, info.Key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer r.Close()

			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("round trip changed the content")
			}
		})
	}
}

// Content addressing is what makes message deduplication safe: the same bytes
// must always produce the same key, and storing them twice must not duplicate.
func TestPutIsContentAddressedAndIdempotent(t *testing.T) {
	content := []byte("identical content")

	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			first, err := store.Put(ctx, blob.TypeRFC822, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("first Put: %v", err)
			}
			second, err := store.Put(ctx, blob.TypeRFC822, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("second Put: %v", err)
			}

			if first.Key != second.Key {
				t.Errorf("identical content produced different keys: %q and %q",
					first.Key, second.Key)
			}

			if mem, ok := store.(*blob.MemStore); ok && mem.Len() != 1 {
				t.Errorf("storing identical content twice created %d objects, want 1", mem.Len())
			}
		})
	}
}

// The same bytes stored under different types are different objects, which is
// what keeps their reference counts from being conflated.
func TestKeysAreNamespacedByType(t *testing.T) {
	content := []byte("shared bytes")

	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			asMessage, err := store.Put(ctx, blob.TypeRFC822, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			asPart, err := store.Put(ctx, blob.TypeMIMEPart, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}

			if asMessage.Key == asPart.Key {
				t.Error("different object types must not share a key")
			}
			if !bytes.Equal(asMessage.SHA256, asPart.SHA256) {
				t.Error("identical content must hash identically regardless of type")
			}
		})
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	missing := blob.NewKey(blob.TypeRFC822, make([]byte, 32))

	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := store.Get(context.Background(), missing)
			if !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("got %v, want ErrNotFound", err)
			}
		})
	}
}

// Partial FETCH must not require transferring an entire message.
func TestGetRange(t *testing.T) {
	content := []byte("0123456789abcdefghij")

	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			info, err := store.Put(ctx, blob.TypeRFC822, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}

			for _, tc := range []struct {
				offset, length int64
				want           string
			}{
				{0, 5, "01234"},
				{10, 5, "abcde"},
				{15, 0, "fghij"}, // zero length means "to the end"
				{0, 0, string(content)},
			} {
				r, err := store.GetRange(ctx, info.Key, tc.offset, tc.length)
				if err != nil {
					t.Fatalf("GetRange(%d,%d): %v", tc.offset, tc.length, err)
				}
				got, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatalf("reading range: %v", err)
				}
				if string(got) != tc.want {
					t.Errorf("GetRange(%d,%d) = %q, want %q",
						tc.offset, tc.length, got, tc.want)
				}
			}
		})
	}
}

// Deleting an absent object must be a no-op, so a repeated sweep is safe.
func TestDeleteIsIdempotent(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			info, err := store.Put(ctx, blob.TypeRFC822, strings.NewReader("delete me"))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}

			for i := range 3 {
				if err := store.Delete(ctx, info.Key); err != nil {
					t.Errorf("Delete attempt %d: %v", i+1, err)
				}
			}
			if _, err := store.Get(ctx, info.Key); !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("object still present after delete: %v", err)
			}
		})
	}
}

func TestStatDoesNotTransferContent(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 4096)

	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			info, err := store.Put(ctx, blob.TypeRFC822, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			stat, err := store.Stat(ctx, info.Key)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if stat.Size != int64(len(content)) {
				t.Errorf("Stat size = %d, want %d", stat.Size, len(content))
			}
			if !bytes.Equal(stat.SHA256, info.SHA256) {
				t.Error("Stat reported a different digest than Put")
			}
		})
	}
}

func TestInvalidKeysAreRejected(t *testing.T) {
	for _, key := range []blob.Key{
		"",
		"nonsense",
		"rfc822",
		"rfc822/",
		"rfc822/short",
		blob.Key("unknown_type/" + strings.Repeat("a", 64)),
	} {
		if key.Valid() {
			t.Errorf("key %q should be invalid", key)
		}
	}

	valid := blob.NewKey(blob.TypeRFC822, make([]byte, 32))
	if !valid.Valid() {
		t.Errorf("key %q should be valid", valid)
	}
}
