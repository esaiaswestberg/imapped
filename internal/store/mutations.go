package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// MutationType names a change to replay upstream.
type MutationType string

const (
	MutationStoreFlags MutationType = "store_flags"
)

// Mutation is a queued change awaiting replay.
type Mutation struct {
	ID        int64
	AccountID int64
	MailboxID *int64
	Type      MutationType
	Payload   json.RawMessage
	Attempts  int
	LastError *string
}

// FlagPayload is the body of a store_flags mutation.
type FlagPayload struct {
	UpstreamUID int64    `json:"upstream_uid"`
	Mailbox     string   `json:"mailbox"`
	Flags       []string `json:"flags"`
}

// IdempotencyKey derives a stable identity for a mutation.
//
// Replay must survive a crash between applying a change locally and recording
// that it was pushed. Keying on the logical change means enqueueing the same
// thing twice collapses to one row rather than applying it twice upstream.
func IdempotencyKey(accountID int64, mailboxID int64, kind MutationType, payload []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%d:%s:", accountID, mailboxID, kind)
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// EnqueueMutation records a change to replay upstream.
//
// A newer change to the same message supersedes an older pending one: flags are
// last-writer-wins, so replaying an intermediate state would be pointless work
// and could briefly resurrect a flag the user just cleared.
func (s *Store) EnqueueMutation(ctx context.Context, accountID, mailboxID int64,
	kind MutationType, payload any) error {

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding mutation payload: %w", err)
	}
	key := IdempotencyKey(accountID, mailboxID, kind, encoded)

	_, err = s.pool.Exec(ctx,
		`INSERT INTO pending_mutations
		   (account_id, mailbox_id, mutation_type, payload, idempotency_key, status)
		 VALUES ($1, $2, $3, $4, $5, 'pending')
		 ON CONFLICT (idempotency_key) DO UPDATE
		   SET status = 'pending', next_attempt_at = NULL, attempts = 0,
		       last_error = NULL, updated_at = now()`,
		accountID, mailboxID, kind, encoded, key)
	if err != nil {
		return fmt.Errorf("queueing mutation: %w", err)
	}
	return nil
}

// ClaimDueMutations reserves mutations ready to be replayed.
//
// SKIP LOCKED lets several workers, and several replicas, drain the queue
// without an external lock and without blocking behind one another.
func (s *Store) ClaimDueMutations(ctx context.Context, accountID int64, limit int, worker string) ([]Mutation, error) {
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		     SELECT id FROM pending_mutations
		     WHERE account_id = $1
		       AND status IN ('pending', 'failed')
		       AND (next_attempt_at IS NULL OR next_attempt_at <= now())
		     ORDER BY created_at
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		 )
		 UPDATE pending_mutations m
		 SET status = 'in_flight', attempts = m.attempts + 1,
		     locked_by = $3, locked_at = now(), updated_at = now()
		 FROM due
		 WHERE m.id = due.id
		 RETURNING m.id, m.account_id, m.mailbox_id, m.mutation_type, m.payload,
		           m.attempts, m.last_error`,
		accountID, limit, worker)
	if err != nil {
		return nil, fmt.Errorf("claiming mutations: %w", err)
	}
	defer rows.Close()

	var out []Mutation
	for rows.Next() {
		var m Mutation
		if err := rows.Scan(&m.ID, &m.AccountID, &m.MailboxID, &m.Type,
			&m.Payload, &m.Attempts, &m.LastError); err != nil {
			return nil, fmt.Errorf("scanning mutation: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MutationSucceeded marks a mutation as applied.
func (s *Store) MutationSucceeded(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE pending_mutations
		 SET status = 'succeeded', locked_by = NULL, locked_at = NULL,
		     last_error = NULL, updated_at = now()
		 WHERE id = $1`, id)
	return err
}

// MutationFailed schedules a retry, or gives up after maxAttempts.
//
// A mutation that can never succeed must not be retried forever: it would
// occupy the head of the queue and delay every change behind it.
func (s *Store) MutationFailed(ctx context.Context, id int64, cause error,
	retryAfter time.Duration, maxAttempts int) error {

	message := cause.Error()
	_, err := s.pool.Exec(ctx,
		`UPDATE pending_mutations
		 SET status = CASE WHEN attempts >= $4 THEN 'failed' ELSE 'pending' END,
		     next_attempt_at = now() + $3::interval,
		     last_error = $2, locked_by = NULL, locked_at = NULL, updated_at = now()
		 WHERE id = $1`, id, message, retryAfter.String(), maxAttempts)
	return err
}

// ReleaseStaleMutations returns mutations abandoned by a dead worker.
func (s *Store) ReleaseStaleMutations(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE pending_mutations
		 SET status = 'pending', locked_by = NULL, locked_at = NULL, updated_at = now()
		 WHERE status = 'in_flight' AND locked_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CountPendingMutations reports how many changes are waiting to be replayed.
func (s *Store) CountPendingMutations(ctx context.Context, accountID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pending_mutations
		 WHERE account_id = $1 AND status IN ('pending', 'in_flight')`, accountID).Scan(&n)
	return n, err
}
