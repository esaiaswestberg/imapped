package store

import (
	"context"
	"fmt"
	"time"
)

// Mailbox is a mirrored mailbox and its sync checkpoint.
type Mailbox struct {
	ID        int64
	AccountID int64

	Name          string
	CanonicalName string
	Delimiter     *string
	Attributes    []string
	SpecialUse    *string
	Subscribed    bool

	UIDValidity   *int64
	UIDNext       *int64
	HighestModSeq *int64

	MetadataSyncedThroughUID int64
	LastFullScanAt           *time.Time
	ResyncRequired           bool
	SyncGeneration           int64

	LocalUIDNext int64
	ExistsCount  int64
	UnseenCount  int64
}

const mailboxColumns = `
	id, account_id, name, canonical_name, delimiter, attributes, special_use, subscribed,
	uidvalidity, uidnext, highestmodseq,
	metadata_synced_through_uid, last_full_scan_at, resync_required, sync_generation,
	local_uidnext, exists_count, unseen_count`

func scanMailbox(row interface{ Scan(...any) error }) (Mailbox, error) {
	var m Mailbox
	err := row.Scan(
		&m.ID, &m.AccountID, &m.Name, &m.CanonicalName, &m.Delimiter, &m.Attributes,
		&m.SpecialUse, &m.Subscribed,
		&m.UIDValidity, &m.UIDNext, &m.HighestModSeq,
		&m.MetadataSyncedThroughUID, &m.LastFullScanAt, &m.ResyncRequired, &m.SyncGeneration,
		&m.LocalUIDNext, &m.ExistsCount, &m.UnseenCount,
	)
	return m, err
}

// UpsertMailboxParams describes a mailbox as LIST reported it.
type UpsertMailboxParams struct {
	AccountID     int64
	Name          string
	CanonicalName string
	Delimiter     *string
	Attributes    []string
	SpecialUse    *string
}

// UpsertMailbox creates or updates a mailbox, preserving its sync checkpoint.
//
// Only the fields LIST reports are updated. Overwriting the checkpoint here
// would silently discard sync progress every time the mailbox list is
// refreshed, forcing a full re-enumeration on every run.
func (s *Store) UpsertMailbox(ctx context.Context, p UpsertMailboxParams) (Mailbox, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO mailboxes (account_id, name, canonical_name, delimiter, attributes, special_use)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (account_id, canonical_name) DO UPDATE
		   SET name = EXCLUDED.name,
		       delimiter = EXCLUDED.delimiter,
		       attributes = EXCLUDED.attributes,
		       special_use = EXCLUDED.special_use,
		       updated_at = now()
		 RETURNING `+mailboxColumns,
		p.AccountID, p.Name, p.CanonicalName, p.Delimiter, p.Attributes, p.SpecialUse)

	mailbox, err := scanMailbox(row)
	if err != nil {
		return Mailbox{}, fmt.Errorf("upserting mailbox %q: %w", p.Name, err)
	}
	return mailbox, nil
}

// ListMailboxes returns every mailbox for an account.
func (s *Store) ListMailboxes(ctx context.Context, accountID int64) ([]Mailbox, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+mailboxColumns+` FROM mailboxes WHERE account_id = $1 ORDER BY canonical_name`,
		accountID)
	if err != nil {
		return nil, fmt.Errorf("listing mailboxes: %w", err)
	}
	defer rows.Close()

	var out []Mailbox
	for rows.Next() {
		mailbox, err := scanMailbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning mailbox: %w", err)
		}
		out = append(out, mailbox)
	}
	return out, rows.Err()
}

// GetMailbox returns one mailbox by id.
func (s *Store) GetMailbox(ctx context.Context, id int64) (Mailbox, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE id = $1`, id)
	mailbox, err := scanMailbox(row)
	if err != nil {
		return Mailbox{}, fmt.Errorf("loading mailbox %d: %w", id, notFound(err))
	}
	return mailbox, nil
}

// Checkpoint records how far the metadata pass has progressed.
type Checkpoint struct {
	UIDValidity              int64
	UIDNext                  int64
	HighestModSeq            int64
	MetadataSyncedThroughUID int64
	FullScanCompleted        bool
}

// SaveCheckpoint persists sync progress for a mailbox.
//
// The modification sequence must only advance once the work that consumed it
// has committed. Advancing it early would mean the server never reports those
// flag changes again, silently losing them forever.
func (s *Store) SaveCheckpoint(ctx context.Context, mailboxID int64, c Checkpoint) error {
	query := `UPDATE mailboxes
		 SET uidvalidity = $2,
		     uidnext = $3,
		     highestmodseq = GREATEST(COALESCE(highestmodseq, 0), $4),
		     metadata_synced_through_uid = GREATEST(metadata_synced_through_uid, $5),
		     resync_required = FALSE,
		     updated_at = now()`
	if c.FullScanCompleted {
		query += `, last_full_scan_at = now()`
	}
	query += ` WHERE id = $1`

	_, err := s.pool.Exec(ctx, query,
		mailboxID, c.UIDValidity, c.UIDNext, c.HighestModSeq, c.MetadataSyncedThroughUID)
	if err != nil {
		return fmt.Errorf("saving checkpoint for mailbox %d: %w", mailboxID, err)
	}
	return nil
}

// ResetForUIDValidityChange handles the server renumbering a mailbox.
//
// Messages are kept and their upstream UIDs cleared, rather than deleted. The
// bodies are already stored and content-addressed, so the next metadata pass
// can re-link them by content hash instead of downloading everything again —
// which is what the previous implementation did, at the cost of re-fetching an
// entire mailbox for what is often a server-side maintenance operation.
func (s *Store) ResetForUIDValidityChange(ctx context.Context, mailboxID int64, newUIDValidity int64) error {
	return s.InTx(ctx, func(tx pgxTx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE mailbox_messages
			 SET upstream_uid = NULL, upstream_modseq = NULL, updated_at = now()
			 WHERE mailbox_id = $1`, mailboxID); err != nil {
			return fmt.Errorf("clearing upstream UIDs: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE mailboxes
			 SET uidvalidity = $2, highestmodseq = 0, metadata_synced_through_uid = 0,
			     resync_required = TRUE, sync_generation = sync_generation + 1, updated_at = now()
			 WHERE id = $1`, mailboxID, newUIDValidity); err != nil {
			return fmt.Errorf("resetting mailbox checkpoint: %w", err)
		}
		return nil
	})
}

// RefreshMailboxCounts recomputes the cached message counts.
func (s *Store) RefreshMailboxCounts(ctx context.Context, mailboxID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mailboxes m
		 SET exists_count = c.total, unseen_count = c.unseen, updated_at = now()
		 FROM (
		     SELECT count(*) AS total,
		            count(*) FILTER (WHERE NOT ('\Seen' = ANY (flags))) AS unseen
		     FROM mailbox_messages
		     WHERE mailbox_id = $1 AND expunged_at IS NULL
		 ) c
		 WHERE m.id = $1`, mailboxID)
	if err != nil {
		return fmt.Errorf("refreshing counts for mailbox %d: %w", mailboxID, err)
	}
	return nil
}

// DeleteMailboxesExcept removes mailboxes no longer present upstream.
func (s *Store) DeleteMailboxesExcept(ctx context.Context, accountID int64, keep []string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM mailboxes WHERE account_id = $1 AND NOT (canonical_name = ANY ($2))`,
		accountID, keep)
	if err != nil {
		return 0, fmt.Errorf("removing deleted mailboxes: %w", err)
	}
	return tag.RowsAffected(), nil
}
