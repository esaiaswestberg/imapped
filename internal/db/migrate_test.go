//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/esaiaswestberg/imapped/internal/db"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/testutil/pgtest"
)

func TestMigrationsApply(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	statuses, err := db.Status(ctx, pool)
	if err != nil {
		t.Fatalf("reading migration status: %v", err)
	}
	if len(statuses) == 0 {
		t.Fatal("no migrations found")
	}
	for _, s := range statuses {
		if !s.Applied {
			t.Errorf("migration %d (%s) was not applied", s.Version, s.Name)
		}
	}
}

// Startup runs migrations unconditionally, so applying an already-current
// schema must be a no-op rather than an error.
func TestMigrateIsIdempotent(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := db.Migrate(ctx, pool, logging.Discard()); err != nil {
			t.Fatalf("re-running migrations (attempt %d): %v", i+1, err)
		}
	}
}

func TestSchemaHasExpectedTables(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	want := []string{
		"users", "mail_accounts", "mailboxes", "messages", "mailbox_messages",
		"mime_parts", "cache_objects", "sync_runs", "pending_mutations",
		"sessions", "audit_log", "quotas",
	}
	for _, table := range want {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("checking for table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s is missing", table)
		}
	}
}

// The generated tsvector must exist and be maintained by Postgres itself: a
// trigger-maintained column can be missed by an UPDATE path, a generated one
// cannot.
func TestSearchVectorIsGenerated(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	var generated string
	err := pool.QueryRow(ctx,
		`SELECT is_generated FROM information_schema.columns
		 WHERE table_name = 'messages' AND column_name = 'search_tsv'`).Scan(&generated)
	if err != nil {
		t.Fatalf("looking up search_tsv: %v", err)
	}
	if generated != "ALWAYS" {
		t.Errorf("search_tsv is_generated = %q, want ALWAYS", generated)
	}
}

// Deduplication is the point of the content-hash unique index; without it the
// same message in two mailboxes is stored twice, as it was in the Rust schema.
func TestMessageDedupConstraint(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	accountID := seedAccount(t, pool)
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = byte(i)
	}

	insert := func() error {
		_, err := pool.Exec(ctx,
			`INSERT INTO messages (account_id, rfc822_sha256, rfc822_size) VALUES ($1, $2, $3)`,
			accountID, digest, 1234)
		return err
	}

	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Error("expected the duplicate content hash to be rejected")
	}
}

// A negative refcount would later read as "safe to delete" and destroy a blob
// that is still referenced, so the database must reject it outright.
func TestCacheObjectRefCountCannotGoNegative(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	accountID := seedAccount(t, pool)
	digest := make([]byte, 32)

	_, err := pool.Exec(ctx,
		`INSERT INTO cache_objects (account_id, object_type, blob_key, sha256, ref_count)
		 VALUES ($1, 'rfc822', 'rfc822/abc', $2, 1)`, accountID, digest)
	if err != nil {
		t.Fatalf("inserting cache object: %v", err)
	}

	_, err = pool.Exec(ctx,
		`UPDATE cache_objects SET ref_count = ref_count - 2 WHERE account_id = $1`, accountID)
	if err == nil {
		t.Error("expected a negative ref_count to be rejected")
	}
}

// object_type is an enum precisely so the Rust bug — two spellings splitting
// one refcount across two rows — cannot be expressed at all.
func TestCacheObjectTypeIsConstrained(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	accountID := seedAccount(t, pool)
	_, err := pool.Exec(ctx,
		`INSERT INTO cache_objects (account_id, object_type, blob_key, sha256)
		 VALUES ($1, 'MimePart', 'x', $2)`, accountID, make([]byte, 32))
	if err == nil {
		t.Error("expected an invalid object_type to be rejected")
	}
}

func TestBodyStateIsConstrained(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	accountID := seedAccount(t, pool)
	var mailboxID, messageID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (account_id, name, canonical_name)
		 VALUES ($1, 'INBOX', 'inbox') RETURNING id`, accountID).Scan(&mailboxID); err != nil {
		t.Fatalf("inserting mailbox: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO messages (account_id, rfc822_sha256, rfc822_size)
		 VALUES ($1, $2, 10) RETURNING id`, accountID, make([]byte, 32)).Scan(&messageID); err != nil {
		t.Fatalf("inserting message: %v", err)
	}

	_, err := pool.Exec(ctx,
		`INSERT INTO mailbox_messages (mailbox_id, message_id, local_uid, body_state)
		 VALUES ($1, $2, 1, 'downloading')`, mailboxID, messageID)
	if err == nil {
		t.Error("expected an invalid body_state to be rejected")
	}
}

// seedAccount inserts a user and mail account, returning the account id.
func seedAccount(t *testing.T, pool *db.Pool) int64 {
	t.Helper()
	ctx := context.Background()

	var userID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('test@example.com', 'x') RETURNING id`).
		Scan(&userID)
	if err != nil {
		t.Fatalf("inserting user: %v", err)
	}

	var accountID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO mail_accounts
		   (user_id, email_address, upstream_host, upstream_port, encrypted_username, encrypted_secret)
		 VALUES ($1, 'test@example.com', 'imap.example.com', 993, '\x00', '\x00')
		 RETURNING id`, userID).Scan(&accountID)
	if err != nil {
		t.Fatalf("inserting mail account: %v", err)
	}
	return accountID
}
