package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/mailstore"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// Engine mirrors upstream accounts into local storage.
type Engine struct {
	cfg     config.Config
	store   *store.Store
	blobs   blob.Store
	sealer  *crypto.Sealer
	connect *upstream.Connector
	ingest  *mailstore.Ingestor
	log     *slog.Logger

	progress sync.Map // accountID -> *Progress
	claims   *claimTable
}

// New builds an Engine.
func New(cfg config.Config, st *store.Store, blobs blob.Store, sealer *crypto.Sealer, log *slog.Logger) *Engine {
	return &Engine{
		cfg:     cfg,
		store:   st,
		blobs:   blobs,
		sealer:  sealer,
		connect: upstream.NewConnector(cfg.Upstream, log),
		ingest:  mailstore.NewIngestor(st, blobs, log),
		log:     log,
		claims:  newClaimTable(),
	}
}

// Result summarises one sync run.
type Result struct {
	MailboxesSynced int
	MessagesNew     int64
	BodiesFetched   int64
	BytesFetched    int64
	Skipped         bool // another process held the lock
}

// SyncAccount mirrors one account.
//
// The run is wrapped in three independent safety nets, because the failure this
// replaces was a sync that hung for two days holding a lock nobody could clear:
//
//   - a Postgres advisory lock held on a dedicated session, released by the
//     server the moment this process dies;
//   - a heartbeat on that same session, whose failure cancels the run, so
//     losing the database stops the sync rather than leaving it running
//     without a lock;
//   - a hard ceiling on total run duration, so even a logic bug terminates.
func (e *Engine) SyncAccount(ctx context.Context, accountID int64, trigger string) (Result, error) {
	lock, err := e.store.TryLockAccount(ctx, accountID)
	if err != nil {
		return Result{}, fmt.Errorf("locking account %d: %w", accountID, err)
	}
	if lock == nil {
		// Another process is already syncing this account. Not an error.
		e.log.Debug("skipping sync, account is already being synced", "account_id", accountID)
		return Result{Skipped: true}, nil
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := lock.Release(releaseCtx); err != nil {
			e.log.Warn("releasing account lock", "account_id", accountID, "error", err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, e.cfg.Sync.MaxRunDuration.Std())
	defer cancel()

	runID, err := e.store.StartSyncRun(ctx, accountID, trigger)
	if err != nil {
		return Result{}, err
	}

	progress := newProgress(accountID)
	e.progress.Store(accountID, progress)
	defer e.progress.Delete(accountID)

	stopHeartbeat := e.startHeartbeat(ctx, cancel, lock, runID, progress)
	defer stopHeartbeat()

	result, syncErr := e.syncAccountLocked(ctx, accountID, progress)

	status := "succeeded"
	if syncErr != nil {
		status = "failed"
		if errors.Is(syncErr, context.Canceled) || errors.Is(syncErr, context.DeadlineExceeded) {
			status = "cancelled"
		}
	}

	// Use a context detached from the run's, so the outcome is still recorded
	// when the run was cancelled or timed out.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer finishCancel()

	if err := e.store.FinishSyncRun(finishCtx, runID, status, syncErr, progress.snapshot()); err != nil {
		e.log.Warn("recording sync outcome", "account_id", accountID, "error", err)
	}
	authFailed := syncErr != nil && upstream.SeverityOf(syncErr) == upstream.FatalAccount
	if err := e.store.SetAccountSyncResult(finishCtx, accountID, syncErr, authFailed); err != nil {
		e.log.Warn("recording account sync result", "account_id", accountID, "error", err)
	}

	return result, syncErr
}

// startHeartbeat keeps the run's liveness fresh and cancels it if the database
// becomes unreachable.
func (e *Engine) startHeartbeat(ctx context.Context, cancel context.CancelFunc, lock *store.AccountLock, runID int64, p *Progress) func() {
	interval := e.cfg.Sync.HeartbeatInterval.Std()
	ticker := time.NewTicker(interval)
	done := make(chan struct{})

	go func() {
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				// Issued on the lock's own session, so this cannot be starved
				// by a busy pool and its failure genuinely means the lock is gone.
				beatCtx, beatCancel := context.WithTimeout(ctx, interval)
				err := lock.Heartbeat(beatCtx, runID, p.snapshot())
				beatCancel()
				if err == nil {
					failures = 0
					continue
				}
				failures++
				e.log.Warn("heartbeat failed", "run_id", runID, "failures", failures, "error", err)
				// Persistent heartbeat failure means the connection holding the
				// account lock is gone, so the lock is gone too. Continuing
				// would let a second process sync the same account concurrently.
				if failures >= 3 {
					e.log.Error("cancelling sync: heartbeat is failing, so the account lock can no longer be relied on",
						"run_id", runID, "error", err)
					cancel()
					return
				}
			}
		}
	}()

	return func() { close(done) }
}

func (e *Engine) syncAccountLocked(ctx context.Context, accountID int64, p *Progress) (Result, error) {
	var result Result

	account, err := e.store.GetAccount(ctx, accountID)
	if err != nil {
		return result, err
	}
	if !account.Active() {
		return result, nil
	}

	creds, err := e.credentials(account)
	if err != nil {
		return result, err
	}

	p.setPhase("connecting")
	client, err := e.connect.Connect(ctx, creds)
	if err != nil {
		return result, fmt.Errorf("connecting to %s: %w", account.UpstreamHost, err)
	}
	// Closed through a closure rather than `defer client.Close()`, because the
	// receiver would be bound now and a reconnect later reassigns the variable
	// — closing the dead connection and leaking the live one.
	defer func() { _ = client.Close() }() //nolint:contextcheck // teardown uses its own context by design

	caps := capabilityNames(client.Caps())
	if err := e.store.SetAccountCapabilities(ctx, accountID, caps); err != nil {
		e.log.Warn("caching upstream capabilities", "account_id", accountID, "error", err)
	}

	p.setPhase("listing mailboxes")
	boxes, err := client.ListMailboxes(ctx)
	if err != nil {
		return result, err
	}

	boxes = prioritise(boxes)
	p.setMailboxTotal(len(boxes))

	var mailboxErrors int
	var rootCause error
	var rootCauseMailbox string

	var keep []string
	for _, box := range boxes {
		keep = append(keep, strings.ToLower(box.Name))
	}
	if _, err := e.store.DeleteMailboxesExcept(ctx, accountID, keep); err != nil {
		e.log.Warn("removing mailboxes deleted upstream", "account_id", accountID, "error", err)
	}

	for _, box := range boxes {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !box.Selectable() {
			p.mailboxDone()
			continue
		}

		mailboxResult, err := e.syncMailbox(ctx, client, account, box, p)

		// Fold in whatever the mailbox managed before it stopped. Discarding
		// partial work made a run that stored thousands of messages report
		// zero, which is worse than useless when diagnosing a failure.
		result.MessagesNew += mailboxResult.MessagesNew
		result.BodiesFetched += mailboxResult.BodiesFetched
		result.BytesFetched += mailboxResult.BytesFetched

		if err != nil {
			// One mailbox failing must not abandon the others: a single
			// inaccessible folder should not stop the rest of an account
			// syncing, which is what the previous implementation did.
			if upstream.SeverityOf(err) == upstream.FatalAccount {
				return result, err
			}
			e.log.Warn("syncing mailbox failed, continuing with the rest",
				"account_id", accountID, "mailbox", box.Name, "error", err)
			mailboxErrors++

			// Keep the first genuine fault. Later mailboxes may fail only
			// because this one broke the connection, and reporting the last of
			// those points at a symptom instead of the cause.
			if rootCause == nil || !errors.Is(rootCause, upstream.ErrPoisoned) {
				if rootCause == nil {
					rootCause, rootCauseMailbox = err, box.Name
				}
			}

			// Replace the connection after any mailbox failure.
			//
			// Narrower conditions were tried and are wrong: a connection
			// abandoned at a deadline is marked poisoned, but one closed by the
			// peer is not, and both leave every later mailbox failing against a
			// connection that cannot work. Distinguishing them buys nothing —
			// the cost is one login per failed mailbox, against a run that
			// otherwise loses every mailbox after the first fault.
			replacement, connErr := e.reconnect(ctx, account, client)
			if connErr != nil {
				return result, fmt.Errorf("reconnecting after %s failed: %w", box.Name, connErr)
			}
			client = replacement

			p.mailboxDone()
			continue
		}

		result.MailboxesSynced++
		p.mailboxDone()
	}

	// Tolerating one bad mailbox is deliberate, but if every mailbox failed the
	// run did nothing and must not be reported as a success — that would hide a
	// total outage behind a green status.
	if mailboxErrors > 0 && result.MailboxesSynced == 0 {
		return result, fmt.Errorf("every mailbox failed to sync (%d of %d); root cause was %s: %w",
			mailboxErrors, len(boxes), rootCauseMailbox, rootCause)
	}
	if mailboxErrors > 0 {
		e.log.Warn("sync finished with some mailboxes failing",
			"account_id", accountID, "failed", mailboxErrors, "synced", result.MailboxesSynced,
			"root_cause_mailbox", rootCauseMailbox, "root_cause", rootCause)
	}

	p.setPhase("done")
	return result, nil
}

// reconnect replaces a connection that can no longer be used.
//
// Poisoning exists because a connection abandoned mid-response has an unknown
// amount of unread data still in flight and cannot safely carry another
// command. Detecting that was right; leaving the run to die with it was not.
func (e *Engine) reconnect(ctx context.Context, account store.Account, old *upstream.Client) (*upstream.Client, error) {
	e.log.Info("replacing an unusable upstream connection", "account_id", account.ID)
	_ = old.Close()

	creds, err := e.credentials(account)
	if err != nil {
		return nil, err
	}
	return e.connect.Connect(ctx, creds)
}

func (e *Engine) credentials(account store.Account) (upstream.Account, error) {
	if e.sealer == nil {
		return upstream.Account{}, errors.New("no encryption master key configured, so stored credentials cannot be read")
	}
	username, err := e.sealer.OpenString(account.EncryptedUsername)
	if err != nil {
		return upstream.Account{}, fmt.Errorf("decrypting username: %w", err)
	}
	password, err := e.sealer.OpenString(account.EncryptedSecret)
	if err != nil {
		return upstream.Account{}, fmt.Errorf("decrypting password: %w", err)
	}
	return upstream.Account{
		Host:     account.UpstreamHost,
		Port:     account.UpstreamPort,
		TLSMode:  upstream.TLSMode(account.UpstreamTLSMode),
		Username: username,
		Password: password,
	}, nil
}

// prioritise orders mailboxes so the ones a user looks at first sync first.
//
// INBOX before anything else, then the folders a mail client opens by default,
// with bulk archives last. A user watching an initial sync sees their recent
// mail appear within seconds rather than after a decade of archives.
func prioritise(boxes []upstream.MailboxInfo) []upstream.MailboxInfo {
	rank := func(b upstream.MailboxInfo) int {
		if strings.EqualFold(b.Name, "INBOX") {
			return 0
		}
		switch b.SpecialUse() {
		case string(imap.MailboxAttrSent), string(imap.MailboxAttrDrafts):
			return 1
		case string(imap.MailboxAttrArchive), string(imap.MailboxAttrAll):
			return 3
		case string(imap.MailboxAttrTrash), string(imap.MailboxAttrJunk):
			return 4
		}
		return 2
	}

	sorted := make([]upstream.MailboxInfo, len(boxes))
	copy(sorted, boxes)
	// Insertion sort: mailbox counts are small, and this keeps the order stable
	// for equal ranks so the sequence is reproducible across runs.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && rank(sorted[j]) < rank(sorted[j-1]); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted
}

func capabilityNames(caps imap.CapSet) []string {
	names := make([]string, 0, len(caps))
	for capability := range caps {
		names = append(names, string(capability))
	}
	return names
}

// Progress tracks a running sync for the UI and the heartbeat.
type Progress struct {
	AccountID int64

	phase          atomic.Value
	currentMailbox atomic.Value
	mailboxTotal   atomic.Int64
	mailboxesDone  atomic.Int64
	messagesNew    atomic.Int64
	bodiesFetched  atomic.Int64
	bytesFetched   atomic.Int64
	commands       atomic.Int64
	pendingBodies  atomic.Int64
	startedAt      time.Time
}

func newProgress(accountID int64) *Progress {
	p := &Progress{AccountID: accountID, startedAt: time.Now()}
	p.phase.Store("starting")
	p.currentMailbox.Store("")
	return p
}

func (p *Progress) setPhase(phase string)    { p.phase.Store(phase) }
func (p *Progress) setMailbox(name string)   { p.currentMailbox.Store(name) }
func (p *Progress) setMailboxTotal(n int)    { p.mailboxTotal.Store(int64(n)) }
func (p *Progress) mailboxDone()             { p.mailboxesDone.Add(1) }
func (p *Progress) addCommands(n int64)      { p.commands.Add(n) }
func (p *Progress) addMessages(n int64)      { p.messagesNew.Add(n) }
func (p *Progress) addBody(bytes int64)      { p.bodiesFetched.Add(1); p.bytesFetched.Add(bytes) }
func (p *Progress) setPendingBodies(n int64) { p.pendingBodies.Store(n) }

// Phase reports the current stage, for display.
func (p *Progress) Phase() string { s, _ := p.phase.Load().(string); return s }

// CurrentMailbox reports the mailbox being processed.
func (p *Progress) CurrentMailbox() string { s, _ := p.currentMailbox.Load().(string); return s }

// Counts returns the live counters.
func (p *Progress) Counts() (mailboxesDone, mailboxesTotal int, messages, bodies, bytes, pending int64) {
	return int(p.mailboxesDone.Load()), int(p.mailboxTotal.Load()),
		p.messagesNew.Load(), p.bodiesFetched.Load(), p.bytesFetched.Load(), p.pendingBodies.Load()
}

// Elapsed reports how long the run has been going.
func (p *Progress) Elapsed() time.Duration { return time.Since(p.startedAt) }

func (p *Progress) snapshot() store.SyncProgress {
	return store.SyncProgress{
		Phase:          p.Phase(),
		MailboxesTotal: int(p.mailboxTotal.Load()),
		MailboxesDone:  int(p.mailboxesDone.Load()),
		MessagesNew:    p.messagesNew.Load(),
		BodiesFetched:  p.bodiesFetched.Load(),
		BytesFetched:   p.bytesFetched.Load(),
		CommandsIssued: p.commands.Load(),
	}
}

// ProgressFor returns the live progress of a running sync, if any.
func (e *Engine) ProgressFor(accountID int64) (*Progress, bool) {
	value, ok := e.progress.Load(accountID)
	if !ok {
		return nil, false
	}
	p, ok := value.(*Progress)
	return p, ok
}

// RunningAccounts lists accounts currently syncing.
func (e *Engine) RunningAccounts() []int64 {
	var ids []int64
	e.progress.Range(func(key, _ any) bool {
		if id, ok := key.(int64); ok {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

func marshalJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
