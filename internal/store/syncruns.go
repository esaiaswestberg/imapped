package store

import (
	"context"
	"fmt"
	"time"
)

// SyncRun records one sync attempt.
type SyncRun struct {
	ID        int64
	AccountID int64

	Status  string
	Phase   string
	Trigger string

	MailboxesTotal int
	MailboxesDone  int
	MessagesNew    int64
	BodiesFetched  int64
	BytesFetched   int64
	CommandsIssued int64

	Error *string

	StartedAt   time.Time
	HeartbeatAt time.Time
	FinishedAt  *time.Time
}

// Stale reports whether a run claims to be running but has stopped reporting,
// which means the process executing it died.
func (r SyncRun) Stale(threshold time.Duration) bool {
	return r.Status == "running" && time.Since(r.HeartbeatAt) > threshold
}

// StartSyncRun opens a run record.
func (s *Store) StartSyncRun(ctx context.Context, accountID int64, trigger string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sync_runs (account_id, trigger) VALUES ($1, $2) RETURNING id`,
		accountID, trigger).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("starting sync run: %w", err)
	}
	return id, nil
}

// SyncProgress is a snapshot of counters to persist.
type SyncProgress struct {
	Phase          string
	MailboxesTotal int
	MailboxesDone  int
	MessagesNew    int64
	BodiesFetched  int64
	BytesFetched   int64
	CommandsIssued int64
}

// Heartbeat refreshes a run's liveness and counters.
//
// This is what makes a wedged sync visible. A run whose heartbeat has stopped
// while its status is still 'running' had its process die; the previous
// implementation recorded nothing at all, which is why a two-day hang looked
// identical to a sync that was merely slow.
func (s *Store) Heartbeat(ctx context.Context, runID int64, p SyncProgress) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sync_runs
		 SET heartbeat_at = now(), phase = $2,
		     mailboxes_total = $3, mailboxes_done = $4, messages_new = $5,
		     bodies_fetched = $6, bytes_fetched = $7, commands_issued = $8
		 WHERE id = $1`,
		runID, p.Phase, p.MailboxesTotal, p.MailboxesDone, p.MessagesNew,
		p.BodiesFetched, p.BytesFetched, p.CommandsIssued)
	return err
}

// FinishSyncRun closes a run with a terminal status.
func (s *Store) FinishSyncRun(ctx context.Context, runID int64, status string, runErr error, p SyncProgress) error {
	var message *string
	if runErr != nil {
		text := runErr.Error()
		message = &text
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE sync_runs
		 SET status = $2, error = $3, finished_at = now(), heartbeat_at = now(), phase = $4,
		     mailboxes_total = $5, mailboxes_done = $6, messages_new = $7,
		     bodies_fetched = $8, bytes_fetched = $9, commands_issued = $10
		 WHERE id = $1`,
		runID, status, message, p.Phase, p.MailboxesTotal, p.MailboxesDone,
		p.MessagesNew, p.BodiesFetched, p.BytesFetched, p.CommandsIssued)
	return err
}

// MarkOrphanedRuns fails runs whose process died without closing them.
//
// Called at startup: any run still marked running belongs to a previous
// process, since a live run in this process has not started yet.
func (s *Store) MarkOrphanedRuns(ctx context.Context, staleAfter time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sync_runs
		 SET status = 'failed', finished_at = now(),
		     error = COALESCE(error, 'the process running this sync exited without completing it')
		 WHERE status = 'running' AND heartbeat_at < now() - $1::interval`,
		staleAfter.String())
	if err != nil {
		return 0, fmt.Errorf("marking orphaned runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListSyncRuns returns recent runs for an account, newest first.
func (s *Store) ListSyncRuns(ctx context.Context, accountID int64, limit int) ([]SyncRun, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, account_id, status, phase, trigger,
		        mailboxes_total, mailboxes_done, messages_new, bodies_fetched,
		        bytes_fetched, commands_issued, error, started_at, heartbeat_at, finished_at
		 FROM sync_runs WHERE account_id = $1 ORDER BY started_at DESC LIMIT $2`,
		accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing sync runs: %w", err)
	}
	defer rows.Close()

	var out []SyncRun
	for rows.Next() {
		var r SyncRun
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Status, &r.Phase, &r.Trigger,
			&r.MailboxesTotal, &r.MailboxesDone, &r.MessagesNew, &r.BodiesFetched,
			&r.BytesFetched, &r.CommandsIssued, &r.Error,
			&r.StartedAt, &r.HeartbeatAt, &r.FinishedAt); err != nil {
			return nil, fmt.Errorf("scanning sync run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
