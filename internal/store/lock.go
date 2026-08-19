package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// syncLockNamespace separates this project's advisory locks from any other
// application sharing the database.
const syncLockNamespace = 0x494D4150 // "IMAP"

// AccountLock is a held advisory lock preventing concurrent syncs of one account.
//
// The lock lives on a dedicated Postgres session. If this process is killed,
// the network drops, or the container is OOM-killed, the backend terminates and
// Postgres releases the lock immediately — there is no lease to expire and
// nothing to renew.
//
// This replaces a Redis lock with a 600-second TTL whose release was fire and
// forget: a wedged task never released it, so a hung sync also blocked every
// subsequent attempt for ten minutes at a time.
type AccountLock struct {
	conn      *pgxpool.Conn
	accountID int64
	released  bool
}

// TryLockAccount acquires the sync lock for an account without blocking.
//
// Returns nil when another process already holds it, which is a normal outcome
// rather than an error: the other process is doing the work.
func (s *Store) TryLockAccount(ctx context.Context, accountID int64) (*AccountLock, error) {
	// The lock must be held by one specific session for its whole lifetime, so
	// the connection is taken out of the pool rather than borrowed per query.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring connection for account lock: %w", err)
	}

	var acquired bool
	err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`,
		int32(syncLockNamespace), int32(accountID)).Scan(&acquired)
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("acquiring account lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, nil
	}
	return &AccountLock{conn: conn, accountID: accountID}, nil
}

// Conn exposes the locked session, so a heartbeat can run on the same
// connection. If that connection dies the heartbeat fails, which is precisely
// the signal that the lock is gone and the run must stop.
func (l *AccountLock) Conn() *pgxpool.Conn { return l.conn }

// Release drops the lock and returns the connection to the pool.
func (l *AccountLock) Release(ctx context.Context) error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	defer l.conn.Release()

	_, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`,
		int32(syncLockNamespace), int32(l.accountID))
	if err != nil {
		// Not fatal: closing the connection releases the lock regardless, which
		// is the whole reason a session lock was chosen.
		return fmt.Errorf("releasing account lock: %w", err)
	}
	return nil
}

// Heartbeat refreshes a run's liveness on the connection holding the account
// lock.
//
// Running it here rather than on a pooled connection is deliberate and load
// bearing. The lock lives on this session, so if the session dies the lock is
// gone; issuing the heartbeat on the same session means a heartbeat failure is
// direct evidence that the lock is no longer held, rather than merely evidence
// that the pool was busy. It also keeps heartbeats working when every pooled
// connection is occupied by the sync itself.
func (l *AccountLock) Heartbeat(ctx context.Context, runID int64, p SyncProgress) error {
	_, err := l.conn.Exec(ctx,
		`UPDATE sync_runs
		 SET heartbeat_at = now(), phase = $2,
		     mailboxes_total = $3, mailboxes_done = $4, messages_new = $5,
		     bodies_fetched = $6, bytes_fetched = $7, commands_issued = $8
		 WHERE id = $1`,
		runID, p.Phase, p.MailboxesTotal, p.MailboxesDone, p.MessagesNew,
		p.BodiesFetched, p.BytesFetched, p.CommandsIssued)
	return err
}
