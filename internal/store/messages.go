package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// BodyState tracks whether a message's body has been downloaded.
//
// This exists because a high-water UID mark cannot express "metadata is known
// but the body is not". The previous implementation had only such a mark, so an
// interrupted sync had no way to tell which messages still needed bodies and
// restarted from the beginning.
type BodyState string

const (
	BodyPending  BodyState = "pending"
	BodyFetching BodyState = "fetching"
	BodyStored   BodyState = "stored"
	BodyTooLarge BodyState = "skipped_too_large"
	BodyFailed   BodyState = "failed"
)

// MessageMeta is everything learned about a message before its body arrives.
type MessageMeta struct {
	AccountID int64
	MailboxID int64

	UpstreamUID    int64
	UpstreamModSeq int64
	Flags          []string

	Size          int64
	SHA256        []byte // nil until the body is fetched
	MessageID     *string
	InReplyTo     *string
	References    []string
	Subject       *string
	Addrs         json.RawMessage
	Envelope      json.RawMessage
	BodyStructure json.RawMessage
	InternalDate  *time.Time
	SentDate      *time.Time
}

// UpsertMetadata records a message and its presence in a mailbox.
//
// Returns the mailbox_messages id and whether the row was newly created.
//
// Before the body arrives there is no content hash, so a placeholder derived
// from the mailbox and UID identifies the row. Once the body is stored,
// AttachBody replaces it with the real hash and merges the row into any
// existing message with the same content — which is how the same message in
// several mailboxes ends up sharing one body.
func (s *Store) UpsertMetadata(ctx context.Context, m MessageMeta) (int64, bool, error) {
	var (
		mailboxMessageID int64
		created          bool
	)

	// These columns are NOT NULL with an empty-array default, and a nil Go
	// slice marshals to NULL rather than to that default.
	if m.References == nil {
		m.References = []string{}
	}
	if m.Flags == nil {
		m.Flags = []string{}
	}

	err := s.InTx(ctx, func(tx pgxTx) error {
		placeholder := placeholderDigest(m.MailboxID, m.UpstreamUID)

		var messageID int64
		err := tx.QueryRow(ctx,
			`INSERT INTO messages (
			     account_id, rfc822_sha256, rfc822_size, message_id_hdr, in_reply_to,
			     refs, subject, addrs, envelope, bodystructure, internal_date, sent_date)
			 VALUES ($1, $2, $3, $4, $5, $6, $7,
			         COALESCE($8, '{}'::jsonb), COALESCE($9, '{}'::jsonb),
			         COALESCE($10, '{}'::jsonb), $11, $12)
			 ON CONFLICT (account_id, rfc822_sha256) DO UPDATE
			   SET subject = EXCLUDED.subject,
			       envelope = EXCLUDED.envelope,
			       bodystructure = EXCLUDED.bodystructure,
			       updated_at = now()
			 RETURNING id`,
			m.AccountID, placeholder, m.Size, m.MessageID, m.InReplyTo,
			m.References, m.Subject, nullableJSON(m.Addrs), nullableJSON(m.Envelope),
			nullableJSON(m.BodyStructure), m.InternalDate, m.SentDate).Scan(&messageID)
		if err != nil {
			return fmt.Errorf("upserting message: %w", err)
		}

		// local_uid is allocated from a per-mailbox counter and never reused, so
		// a mail client's cached UIDs stay meaningful across expunges.
		err = tx.QueryRow(ctx,
			`WITH allocated AS (
			     UPDATE mailboxes SET local_uidnext = local_uidnext + 1
			     WHERE id = $1
			     RETURNING local_uidnext - 1 AS uid
			 )
			 INSERT INTO mailbox_messages
			     (mailbox_id, message_id, local_uid, upstream_uid, upstream_modseq, flags, body_state)
			 SELECT $1, $2, allocated.uid, $3, $4, $5, $6 FROM allocated
			 ON CONFLICT (mailbox_id, upstream_uid) WHERE upstream_uid IS NOT NULL
			 DO UPDATE SET flags = EXCLUDED.flags,
			               upstream_modseq = EXCLUDED.upstream_modseq,
			               updated_at = now()
			 RETURNING id, (xmax = 0) AS inserted`,
			m.MailboxID, messageID, m.UpstreamUID, nullableInt(m.UpstreamModSeq),
			m.Flags, string(BodyPending)).Scan(&mailboxMessageID, &created)
		if err != nil {
			return fmt.Errorf("upserting mailbox message: %w", err)
		}
		return nil
	})

	return mailboxMessageID, created, err
}

// UpdateFlags applies a flag change reported by the server.
func (s *Store) UpdateFlags(ctx context.Context, mailboxID, upstreamUID int64, flags []string, modseq int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mailbox_messages
		 SET flags = $3, upstream_modseq = COALESCE(NULLIF($4, 0), upstream_modseq), updated_at = now()
		 WHERE mailbox_id = $1 AND upstream_uid = $2`,
		mailboxID, upstreamUID, flags, modseq)
	if err != nil {
		return fmt.Errorf("updating flags for UID %d: %w", upstreamUID, err)
	}
	return nil
}

// PendingBody identifies a message whose body still needs downloading.
type PendingBody struct {
	MailboxMessageID int64
	MessageID        int64
	UpstreamUID      int64
	Size             int64
	Attempts         int
}

// ClaimPendingBodies reserves up to limit messages for a worker to download.
//
// Claiming marks rows so that parallel workers, and parallel replicas, never
// fetch the same message twice. SKIP LOCKED means a worker takes whatever is
// free rather than blocking behind another worker's rows.
//
// Newest first: recent mail is what a user opens their client to look at, so it
// should appear before a years-old archive finishes downloading.
func (s *Store) ClaimPendingBodies(ctx context.Context, mailboxID int64, limit int, maxAttempts int) ([]PendingBody, error) {
	rows, err := s.pool.Query(ctx,
		`WITH claimed AS (
		     SELECT mm.id
		     FROM mailbox_messages mm
		     WHERE mm.mailbox_id = $1
		       AND mm.body_state = 'pending'
		       AND mm.upstream_uid IS NOT NULL
		       AND mm.body_attempts < $3
		     ORDER BY mm.upstream_uid DESC
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		 )
		 UPDATE mailbox_messages mm
		 SET body_state = 'fetching', claimed_at = now(), updated_at = now()
		 FROM claimed, messages m
		 WHERE mm.id = claimed.id AND m.id = mm.message_id
		 RETURNING mm.id, mm.message_id, mm.upstream_uid, m.rfc822_size, mm.body_attempts`,
		mailboxID, limit, maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("claiming pending bodies: %w", err)
	}
	defer rows.Close()

	var out []PendingBody
	for rows.Next() {
		var p PendingBody
		if err := rows.Scan(&p.MailboxMessageID, &p.MessageID, &p.UpstreamUID,
			&p.Size, &p.Attempts); err != nil {
			return nil, fmt.Errorf("scanning pending body: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AttachBody records a downloaded body and deduplicates by content.
//
// If another message with the same content hash already exists for this
// account, the mailbox row is repointed at it and the redundant placeholder row
// is removed. That is what makes one stored object serve a message that appears
// in several mailboxes.
func (s *Store) AttachBody(ctx context.Context, mailboxMessageID int64, digest []byte, size int64, blobKey string) error {
	return s.InTx(ctx, func(tx pgxTx) error {
		var accountID, messageID int64
		err := tx.QueryRow(ctx,
			`SELECT m.account_id, m.id
			 FROM mailbox_messages mm JOIN messages m ON m.id = mm.message_id
			 WHERE mm.id = $1 FOR UPDATE OF mm`, mailboxMessageID).Scan(&accountID, &messageID)
		if err != nil {
			return fmt.Errorf("loading message for body attach: %w", notFound(err))
		}

		var existingID int64
		err = tx.QueryRow(ctx,
			`SELECT id FROM messages WHERE account_id = $1 AND rfc822_sha256 = $2 AND id <> $3`,
			accountID, digest, messageID).Scan(&existingID)
		switch {
		case err == nil:
			// The content is already stored under another row; point at it and
			// drop the duplicate.
			if _, err := tx.Exec(ctx,
				`UPDATE mailbox_messages
				 SET message_id = $2, body_state = 'stored', claimed_at = NULL,
				     body_error = NULL, updated_at = now()
				 WHERE id = $1`, mailboxMessageID, existingID); err != nil {
				return fmt.Errorf("relinking deduplicated message: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`DELETE FROM messages m
				 WHERE m.id = $1
				   AND NOT EXISTS (SELECT 1 FROM mailbox_messages WHERE message_id = m.id)`,
				messageID); err != nil {
				return fmt.Errorf("removing duplicate message row: %w", err)
			}
		case isNoRows(err):
			if _, err := tx.Exec(ctx,
				`UPDATE messages SET rfc822_sha256 = $2, rfc822_size = $3, blob_key = $4, updated_at = now()
				 WHERE id = $1`, messageID, digest, size, blobKey); err != nil {
				return fmt.Errorf("recording message body: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE mailbox_messages
				 SET body_state = 'stored', claimed_at = NULL, body_error = NULL, updated_at = now()
				 WHERE id = $1`, mailboxMessageID); err != nil {
				return fmt.Errorf("marking body stored: %w", err)
			}
		default:
			return fmt.Errorf("checking for duplicate content: %w", err)
		}
		return nil
	})
}

// MarkBodyFailed records a failed download attempt.
//
// After maxAttempts the message is parked in the failed state rather than
// retried forever, so one unfetchable message cannot block a mailbox.
func (s *Store) MarkBodyFailed(ctx context.Context, mailboxMessageID int64, cause error, maxAttempts int) error {
	message := cause.Error()
	_, err := s.pool.Exec(ctx,
		`UPDATE mailbox_messages
		 SET body_attempts = body_attempts + 1,
		     body_error = $2,
		     body_state = CASE WHEN body_attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END,
		     claimed_at = NULL,
		     updated_at = now()
		 WHERE id = $1`, mailboxMessageID, message, maxAttempts)
	return err
}

// MarkBodyTooLarge parks a message that exceeds the configured size ceiling.
func (s *Store) MarkBodyTooLarge(ctx context.Context, mailboxMessageID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mailbox_messages
		 SET body_state = 'skipped_too_large', claimed_at = NULL, updated_at = now()
		 WHERE id = $1`, mailboxMessageID)
	return err
}

// ReapStaleClaims returns messages abandoned by a dead worker to the queue.
//
// Without this, a process killed mid-fetch would leave its claimed messages in
// the fetching state permanently, and they would never be downloaded.
func (s *Store) ReapStaleClaims(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE mailbox_messages
		 SET body_state = 'pending', claimed_at = NULL, updated_at = now()
		 WHERE body_state = 'fetching' AND claimed_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("reaping stale claims: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountPendingBodies reports how much of a mailbox still needs downloading.
func (s *Store) CountPendingBodies(ctx context.Context, mailboxID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM mailbox_messages
		 WHERE mailbox_id = $1 AND body_state IN ('pending', 'fetching')`, mailboxID).Scan(&n)
	return n, err
}

// ExistingUpstreamUIDs returns the UIDs already known in a mailbox, so the
// metadata pass can skip messages it has already recorded.
func (s *Store) ExistingUpstreamUIDs(ctx context.Context, mailboxID int64) (map[int64]struct{}, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT upstream_uid FROM mailbox_messages
		 WHERE mailbox_id = $1 AND upstream_uid IS NOT NULL AND expunged_at IS NULL`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("listing known UIDs: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]struct{})
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = struct{}{}
	}
	return out, rows.Err()
}

// DeleteMissingUIDs removes messages that no longer exist upstream.
func (s *Store) DeleteMissingUIDs(ctx context.Context, mailboxID int64, presentUIDs []int64) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM mailbox_messages
		 WHERE mailbox_id = $1 AND upstream_uid IS NOT NULL AND NOT (upstream_uid = ANY ($2))`,
		mailboxID, presentUIDs)
	if err != nil {
		return 0, fmt.Errorf("removing vanished messages: %w", err)
	}
	return tag.RowsAffected(), nil
}

// placeholderDigest builds a unique stand-in hash for a message whose body has
// not been downloaded yet. It is 32 bytes so it satisfies the same column
// constraint as a real digest, and is namespaced so it can never collide with
// the SHA-256 of actual content.
func placeholderDigest(mailboxID, uid int64) []byte {
	digest := make([]byte, 32)
	copy(digest, "pending:")
	for i := range 8 {
		digest[8+i] = byte(mailboxID >> (8 * i))
		digest[16+i] = byte(uid >> (8 * i))
	}
	return digest
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// CountMailboxMessages reports how many messages a mailbox holds locally.
func (s *Store) CountMailboxMessages(ctx context.Context, mailboxID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM mailbox_messages WHERE mailbox_id = $1 AND expunged_at IS NULL`,
		mailboxID).Scan(&n)
	return n, err
}
