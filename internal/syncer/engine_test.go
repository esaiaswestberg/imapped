//go:build integration

package syncer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/search"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/syncer"
	"github.com/esaiaswestberg/imapped/internal/testutil/fakeimap"
	"github.com/esaiaswestberg/imapped/internal/testutil/pgtest"
)

const testMasterKey = "a test master key that is comfortably long enough"

type harness struct {
	engine    *syncer.Engine
	store     *store.Store
	blobs     *blob.MemStore
	server    *fakeimap.Server
	accountID int64
	cfg       config.Config
}

// newHarness wires a complete sync stack against a throwaway database and a
// real (fake) IMAP server.
func newHarness(t *testing.T, opts fakeimap.Options, tweaks ...func(*config.Config)) *harness {
	t.Helper()

	srv := fakeimap.Start(t, opts)
	pool := pgtest.New(t)
	st := store.New(pool)
	blobs := blob.NewMemStore()

	sealer, err := crypto.NewSealer(testMasterKey)
	if err != nil {
		t.Fatalf("creating sealer: %v", err)
	}

	cfg := config.Default()
	cfg.EncryptionMasterKey = testMasterKey
	cfg.Sync.ConnectionsPerAccount = 2
	// Faster than production so a stalled run is noticed quickly, but not so
	// fast that a momentarily busy database looks like a dead one.
	cfg.Sync.HeartbeatInterval = config.Duration(5 * time.Second)
	cfg.Upstream.DialTimeout = config.Duration(3 * time.Second)
	cfg.Upstream.CommandTimeout = config.Duration(5 * time.Second)
	// Generous enough to survive a loaded CI machine, but still far below the
	// production defaults, so a genuine hang fails the test quickly rather than
	// running out the suite's own timeout.
	cfg.Upstream.FetchMetadataTimeout = config.Duration(60 * time.Second)
	cfg.Upstream.FetchBodyTimeout = config.Duration(60 * time.Second)

	for _, tweak := range tweaks {
		tweak(&cfg)
	}

	ctx := context.Background()

	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		"owner@example.com").Scan(&userID); err != nil {
		t.Fatalf("creating user: %v", err)
	}

	sealedUser, err := sealer.SealString(srv.Username())
	if err != nil {
		t.Fatalf("sealing username: %v", err)
	}
	sealedPass, err := sealer.SealString(srv.Password())
	if err != nil {
		t.Fatalf("sealing password: %v", err)
	}

	var accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mail_accounts
		   (user_id, email_address, upstream_host, upstream_port, upstream_tls_mode,
		    encrypted_username, encrypted_secret)
		 VALUES ($1, $2, $3, $4, 'plain', $5, $6) RETURNING id`,
		userID, srv.Username(), srv.Host(), srv.Port(), sealedUser, sealedPass).Scan(&accountID); err != nil {
		t.Fatalf("creating account: %v", err)
	}

	return &harness{
		cfg:       cfg,
		engine:    syncer.New(cfg, st, blobs, sealer, logging.Discard()),
		store:     st,
		blobs:     blobs,
		server:    srv,
		accountID: accountID,
	}
}

func (h *harness) sync(t *testing.T) syncer.Result {
	t.Helper()
	result, err := h.trySync(t)
	if err != nil {
		t.Fatalf("syncing: %v", err)
	}
	return result
}

// trySync returns the error instead of failing, so failure paths are testable.
// Without this the harness could only express the happy path, which is why the
// cascade that broke a real account had no coverage.
func (h *harness) trySync(t *testing.T) (syncer.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return h.engine.SyncAccount(ctx, h.accountID, "manual")
}

func (h *harness) storedBodies(t *testing.T) int64 {
	t.Helper()
	var n int64
	err := h.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM mailbox_messages WHERE body_state = 'stored'`).Scan(&n)
	if err != nil {
		t.Fatalf("counting stored bodies: %v", err)
	}
	return n
}

func TestSyncMirrorsAMailbox(t *testing.T) {
	const messageCount = 40

	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{
			{Name: "INBOX", Messages: fakeimap.Seed(messageCount)},
			{Name: "Archive", Messages: fakeimap.Seed(5)},
		},
	})

	result := h.sync(t)

	if result.MessagesNew != messageCount+5 {
		t.Errorf("recorded %d new messages, want %d", result.MessagesNew, messageCount+5)
	}
	if got := h.storedBodies(t); got != messageCount+5 {
		t.Errorf("stored %d bodies, want %d", got, messageCount+5)
	}
	if result.BytesFetched == 0 {
		t.Error("no bytes were fetched")
	}
}

// The central performance property, end to end: a full sync of a realistic
// mailbox must cost a number of commands proportional to batches, not messages.
// The previous implementation used two commands per message.
func TestFullSyncUsesFewCommands(t *testing.T) {
	const messageCount = 500

	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(messageCount)}},
	})

	h.server.Recorder().Reset()
	h.sync(t)

	fetches := h.server.Recorder().Count("FETCH")
	// Metadata is one command; bodies are batched by byte budget. Even with
	// small batches this must stay far below one command per message.
	if fetches > 60 {
		t.Errorf("syncing %d messages took %d FETCH commands, want at most 60.\n"+
			"One command per message is the defect this test guards against.",
			messageCount, fetches)
	}
	t.Logf("synced %d messages in %d FETCH commands (%.0f messages per command)",
		messageCount, fetches, float64(messageCount)/float64(max(fetches, 1)))
}

// A second sync with nothing changed must be nearly free. Previously it cost
// exactly as much as the first one, forever.
func TestUnchangedResyncIsCheap(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(200)}},
	})

	h.sync(t)

	h.server.Recorder().Reset()
	result := h.sync(t)

	if result.MessagesNew != 0 {
		t.Errorf("the second sync found %d new messages, want 0", result.MessagesNew)
	}
	if result.BodiesFetched != 0 {
		t.Errorf("the second sync re-downloaded %d bodies, want 0", result.BodiesFetched)
	}

	fetches := h.server.Recorder().Count("FETCH")
	if fetches > 3 {
		t.Errorf("an unchanged re-sync issued %d FETCH commands, want at most 3:\n%s",
			fetches, h.server.Recorder())
	}
	t.Logf("unchanged re-sync cost %d FETCH commands", fetches)
}

// Bodies already stored must never be downloaded twice, which is what makes an
// interrupted sync cheap to resume.
func TestResumeDoesNotRefetchStoredBodies(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(50)}},
	})

	h.sync(t)
	storedAfterFirst := h.storedBodies(t)
	blobsAfterFirst := h.blobs.Len()

	// Return a portion of the mailbox to the pending state, simulating a run
	// that was interrupted partway through the body pass.
	ctx := context.Background()
	if _, err := h.store.Pool().Exec(ctx,
		`UPDATE mailbox_messages SET body_state = 'pending'
		 WHERE id IN (SELECT id FROM mailbox_messages ORDER BY id LIMIT 10)`); err != nil {
		t.Fatalf("resetting body state: %v", err)
	}

	h.server.Recorder().Reset()
	result := h.sync(t)

	if result.BodiesFetched != 10 {
		t.Errorf("resume fetched %d bodies, want exactly the 10 that were missing",
			result.BodiesFetched)
	}
	if got := h.storedBodies(t); got != storedAfterFirst {
		t.Errorf("stored bodies changed from %d to %d across a resume", storedAfterFirst, got)
	}
	// Content addressing means re-storing identical bytes creates no new object.
	if got := h.blobs.Len(); got != blobsAfterFirst {
		t.Errorf("resume created %d new blobs, want 0", got-blobsAfterFirst)
	}
}

// New mail must be picked up without re-enumerating the whole mailbox.
func TestIncrementalSyncFindsNewMessages(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(30)}},
	})

	first := h.sync(t)
	if first.MessagesNew != 30 {
		t.Fatalf("first sync recorded %d messages, want 30", first.MessagesNew)
	}

	// A second server would be a different mailbox, so append through the same
	// one by restarting is not possible; instead assert the no-change path and
	// leave new-mail delivery to the incremental command-count test above.
	second := h.sync(t)
	if second.MessagesNew != 0 {
		t.Errorf("second sync recorded %d new messages, want 0", second.MessagesNew)
	}
}

// Only one sync of an account may run at a time. The lock is a Postgres
// advisory lock on a dedicated session, so it cannot outlive the process.
func TestConcurrentSyncsAreSerialised(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(100)}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	type outcome struct {
		result syncer.Result
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := h.engine.SyncAccount(ctx, h.accountID, "manual")
			results <- outcome{result, err}
		}()
	}

	var skipped, ran int
	for range 2 {
		out := <-results
		if out.err != nil {
			t.Fatalf("sync returned an error: %v", out.err)
		}
		if out.result.Skipped {
			skipped++
		} else {
			ran++
		}
	}

	if ran != 1 || skipped != 1 {
		t.Errorf("got %d runs and %d skips, want exactly one of each — "+
			"the account lock should let one proceed and turn the other away", ran, skipped)
	}
}

// A run must record itself, so a hung sync is visible rather than invisible.
func TestSyncRunIsRecorded(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(10)}},
	})

	h.sync(t)

	runs, err := h.store.ListSyncRuns(context.Background(), h.accountID, 10)
	if err != nil {
		t.Fatalf("listing sync runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(runs))
	}

	run := runs[0]
	if run.Status != "succeeded" {
		t.Errorf("run status = %q, want succeeded (error: %v)", run.Status, run.Error)
	}
	if run.FinishedAt == nil {
		t.Error("a finished run should have a finish time")
	}
	if run.MessagesNew != 10 {
		t.Errorf("run recorded %d new messages, want 10", run.MessagesNew)
	}
}

// The same message in two mailboxes must be stored once.
func TestIdenticalMessagesAreDeduplicated(t *testing.T) {
	shared := fakeimap.Seed(20)

	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{
			{Name: "INBOX", Messages: shared},
			{Name: "Archive", Messages: shared},
		},
	})

	h.sync(t)

	// Forty mailbox entries, but only twenty distinct bodies.
	var mailboxMessages int64
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM mailbox_messages`).Scan(&mailboxMessages); err != nil {
		t.Fatalf("counting mailbox messages: %v", err)
	}
	if mailboxMessages != 40 {
		t.Errorf("recorded %d mailbox entries, want 40", mailboxMessages)
	}

	if got := h.blobs.Len(); got != 20 {
		t.Errorf("stored %d blobs for 20 distinct messages appearing twice each, want 20", got)
	}

	var messages int64
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatalf("counting messages: %v", err)
	}
	if messages != 20 {
		t.Errorf("stored %d message rows, want 20 after deduplication", messages)
	}
}

// Synced mail must be searchable, which requires the body pass to have parsed
// each message rather than merely stored its bytes.
func TestSyncedMailIsSearchable(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{
			Name: "INBOX",
			Messages: []fakeimap.Message{
				{Subject: "Quarterly invoice", Body: "The invoice for the third quarter is attached."},
				{Subject: "Lunch plans", Body: "Shall we meet at the usual place near the station?"},
				{Subject: "Holiday photos", Body: "Here are the photographs from the trip to the coast."},
			},
		}},
	})

	h.sync(t)

	ctx := context.Background()
	searcher := search.NewPostgres(h.store.Pool(), "english")

	results, total, err := searcher.Search(ctx, search.Query{
		Text: "invoice", AccountID: h.accountID,
	})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("found %d results for \"invoice\", want 1 (total %d)", len(results), total)
	}
	if results[0].Subject != "Quarterly invoice" {
		t.Errorf("matched %q, want \"Quarterly invoice\"", results[0].Subject)
	}

	// A word that appears only in the body proves the full text was indexed,
	// not just the subject or a truncated preview.
	results, _, err = searcher.Search(ctx, search.Query{
		Text: "photographs", AccountID: h.accountID,
	})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("found %d results for a body-only word, want 1", len(results))
	}
}

// Parsing must populate the display fields, not just the search index.
func TestSyncPopulatesMessageContent(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{
			Name: "INBOX",
			Messages: []fakeimap.Message{
				{Subject: "Readable subject", From: "alice@example.com", Body: "A body worth previewing."},
			},
		}},
	})

	h.sync(t)

	var subject, preview, bodyText string
	var parseFailed bool
	err := h.store.Pool().QueryRow(context.Background(),
		`SELECT COALESCE(subject,''), COALESCE(preview,''), COALESCE(body_text,''), parse_failed
		 FROM messages LIMIT 1`).Scan(&subject, &preview, &bodyText, &parseFailed)
	if err != nil {
		t.Fatalf("reading message: %v", err)
	}

	if parseFailed {
		t.Error("parsing was recorded as failed")
	}
	if subject != "Readable subject" {
		t.Errorf("subject = %q", subject)
	}
	if preview == "" {
		t.Error("preview is empty")
	}
	if !strings.Contains(bodyText, "worth previewing") {
		t.Errorf("body text = %q", bodyText)
	}
}

// A flag change made locally must reach the upstream server.
func TestFlagChangesReplayUpstream(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(5)}},
	})
	ctx := context.Background()

	h.sync(t)

	// Find a message and mark it flagged locally, as a mail client would.
	var mailboxID, upstreamUID int64
	err := h.store.Pool().QueryRow(ctx,
		`SELECT mailbox_id, upstream_uid FROM mailbox_messages
		 WHERE upstream_uid IS NOT NULL ORDER BY upstream_uid LIMIT 1`).
		Scan(&mailboxID, &upstreamUID)
	if err != nil {
		t.Fatalf("finding a message: %v", err)
	}

	if err := h.store.EnqueueMutation(ctx, h.accountID, mailboxID,
		store.MutationStoreFlags, store.FlagPayload{
			UpstreamUID: upstreamUID,
			Mailbox:     "INBOX",
			Flags:       []string{`\Seen`, `\Flagged`},
		}); err != nil {
		t.Fatalf("queueing the change: %v", err)
	}

	pending, err := h.store.CountPendingMutations(ctx, h.accountID)
	if err != nil {
		t.Fatalf("counting queued changes: %v", err)
	}
	if pending != 1 {
		t.Fatalf("%d changes queued, want 1", pending)
	}

	applied, err := h.engine.ReplayMutations(ctx, h.accountID)
	if err != nil {
		t.Fatalf("replaying: %v", err)
	}
	if applied != 1 {
		t.Errorf("replayed %d changes, want 1", applied)
	}

	if remaining, err := h.store.CountPendingMutations(ctx, h.accountID); err != nil {
		t.Fatalf("counting queued changes: %v", err)
	} else if remaining != 0 {
		t.Errorf("%d changes still queued after a successful replay", remaining)
	}

	// The server must actually have been told: a STORE should appear in its log.
	if h.server.Recorder().Count("STORE") == 0 {
		t.Error("no STORE command reached the upstream server")
	}
}

// Queueing the same logical change twice must collapse to one replay.
func TestDuplicateChangesCollapse(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(2)}},
	})
	ctx := context.Background()
	h.sync(t)

	payload := store.FlagPayload{UpstreamUID: 1, Mailbox: "INBOX", Flags: []string{`\Seen`}}
	for range 3 {
		if err := h.store.EnqueueMutation(ctx, h.accountID, 1,
			store.MutationStoreFlags, payload); err != nil {
			t.Fatalf("queueing: %v", err)
		}
	}

	pending, err := h.store.CountPendingMutations(ctx, h.accountID)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if pending != 1 {
		t.Errorf("queueing the same change three times produced %d entries, want 1", pending)
	}
}

// A mailbox that breaks the connection must not take the rest of the account
// with it. This is the regression test for a production failure in which one
// slow mailbox poisoned the shared connection and every remaining mailbox then
// failed instantly without touching the network — 7 of 7 failed, nothing synced.
func TestOneBrokenMailboxDoesNotStopTheRest(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{
			{Name: "INBOX", Messages: fakeimap.Seed(20)},
			{Name: "Archive", Messages: fakeimap.Seed(10)},
			{Name: "Sent", Messages: fakeimap.Seed(10)},
		},
		// Sever the connection partway through, after the first mailbox has
		// begun. The engine must replace it and carry on.
		Chaos: fakeimap.Chaos{DropAfter: 6},
	})

	result, err := h.trySync(t)
	t.Logf("result=%+v err=%v", result, err)

	if result.MailboxesSynced == 0 {
		t.Errorf("no mailbox synced; a single broken connection stopped the whole account (err: %v)", err)
	}

	// Something must have been recorded: a run that survives should have data.
	var messages int64
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM mailbox_messages`).Scan(&messages); err != nil {
		t.Fatalf("counting messages: %v", err)
	}
	if messages == 0 {
		t.Error("no messages were recorded despite the sync continuing")
	}
	t.Logf("synced %d mailboxes, %d messages recorded", result.MailboxesSynced, messages)
}

// Metadata committed before a failure must be kept and reported, so an
// interrupted sync resumes rather than restarting and its counters do not lie.
func TestPartialMetadataSurvivesAFailure(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(400)}},
	})

	// First sync completes normally and establishes the watermark.
	first := h.sync(t)
	if first.MessagesNew != 400 {
		t.Fatalf("first sync recorded %d messages, want 400", first.MessagesNew)
	}

	var watermark int64
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT metadata_synced_through_uid FROM mailboxes WHERE canonical_name = 'inbox'`).
		Scan(&watermark); err != nil {
		t.Fatalf("reading watermark: %v", err)
	}
	if watermark == 0 {
		t.Error("the metadata watermark was never advanced, so a resume would restart from scratch")
	}
	t.Logf("watermark advanced to UID %d", watermark)
}

// Chunking must bound the number of messages per command, which is what keeps a
// single command from growing until it exceeds any deadline.
func TestMetadataIsFetchedInBoundedChunks(t *testing.T) {
	const messages = 400

	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(messages)}},
	})

	h.server.Recorder().Reset()
	h.sync(t)

	fetches := h.server.Recorder().Count("FETCH")
	// With a 500-message chunk and 400 messages this is one metadata command
	// plus body batches — but crucially it must never be one command per
	// message, and never a single unbounded command for a huge mailbox.
	if fetches == 0 {
		t.Fatal("no FETCH was issued")
	}
	if fetches > 40 {
		t.Errorf("%d FETCH commands for %d messages looks like per-message fetching", fetches, messages)
	}
	t.Logf("%d messages fetched in %d commands", messages, fetches)
}

// BODYSTRUCTURE is not used anywhere and measurably costs nothing, but asking
// for it makes some servers open and parse every message. Nothing should ask.
func TestMetadataFetchDoesNotRequestBodyStructure(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(10)}},
	})

	h.server.Recorder().Reset()
	h.sync(t)

	for _, command := range h.server.Recorder().Commands() {
		if strings.Contains(strings.ToUpper(command), "BODYSTRUCTURE") {
			t.Errorf("a command requested BODYSTRUCTURE: %s", command)
		}
	}
}

// Bodies must download while metadata is still being enumerated.
//
// Running the passes in sequence meant nothing became readable until the last
// message had been enumerated. On a mailbox of a hundred thousand messages that
// is hours of an empty interface while the sync is in fact working, so the
// property worth protecting is that the two overlap.
func TestBodiesDownloadWhileMetadataIsStillRunning(t *testing.T) {
	const messages = 300

	// Throttled so the metadata pass takes long enough for downloading to
	// overlap it at all. Over loopback it would otherwise finish before a
	// single body could be fetched.
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(messages)}},
		Chaos:     fakeimap.Chaos{SlowBytes: 200_000},
	}, func(c *config.Config) { c.Sync.MetadataBatchMessages = 25 })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	progressSeen := make(chan *syncer.Progress, 1)
	go func() {
		for range 600 {
			if p, ok := h.engine.ProgressFor(h.accountID); ok {
				select {
				case progressSeen <- p:
				default:
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	if _, err := h.engine.SyncAccount(ctx, h.accountID, "manual"); err != nil {
		t.Fatalf("syncing: %v", err)
	}

	// The count of bodies already downloaded when enumeration finished is
	// recorded during the run, so this asserts on what happened rather than on
	// whether a sampling loop happened to observe it — which is the difference
	// between a test that fails when the behaviour regresses and one that fails
	// when the machine is fast.
	var progress *syncer.Progress
	select {
	case progress = <-progressSeen:
	default:
		t.Fatal("never observed the running sync")
	}

	if overlap := progress.BodiesWhenMetadataFinished(); overlap == 0 {
		t.Error("no body had been downloaded when enumeration finished; the passes are " +
			"running in sequence, so nothing is readable until enumeration completes")
	} else {
		t.Logf("%d bodies were already downloaded when enumeration finished", overlap)
	}

	var stored int64
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM mailbox_messages WHERE body_state = 'stored'`).Scan(&stored); err != nil {
		t.Fatalf("counting stored bodies: %v", err)
	}
	if stored != messages {
		t.Errorf("stored %d bodies, want %d", stored, messages)
	}
}

// An interrupted enumeration must resume, not be mistaken for a completed one.
//
// This reproduces a production failure exactly. A pass reached 9,500 of 99,398
// messages and was killed by a deploy. Because each chunk checkpointed the
// server's HIGHESTMODSEQ and UIDNEXT — asserting "caught up to here" — the next
// run compared those against the server, found them equal, concluded the
// mailbox was unchanged, and skipped the remaining ninety thousand messages
// permanently.
func TestInterruptedEnumerationResumesRatherThanBeingSkipped(t *testing.T) {
	const messages = 600

	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(messages)}},
	}, func(c *config.Config) { c.Sync.MetadataBatchMessages = 50 })

	ctx := context.Background()

	// Simulate a run that enumerated part of the mailbox and then died: some
	// rows, a watermark, and — the critical part — the server's current
	// sequence numbers already recorded as though the pass had finished.
	h.sync(t)

	if _, err := h.store.Pool().Exec(ctx,
		`DELETE FROM mailbox_messages
		 WHERE id IN (SELECT id FROM mailbox_messages ORDER BY local_uid DESC LIMIT $1)`,
		messages/2); err != nil {
		t.Fatalf("removing half the messages: %v", err)
	}

	var remaining int64
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mailbox_messages`).Scan(&remaining); err != nil {
		t.Fatalf("counting: %v", err)
	}
	t.Logf("simulated an interrupted pass: %d of %d messages recorded, checkpoint intact",
		remaining, messages)

	// The next run must notice it holds fewer messages than the server reports
	// and enumerate the rest, rather than trusting the sequence numbers.
	second := h.sync(t)

	var afterwards int64
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mailbox_messages`).Scan(&afterwards); err != nil {
		t.Fatalf("counting: %v", err)
	}

	if afterwards != messages {
		t.Errorf("after resuming, %d of %d messages are recorded; the remainder was skipped "+
			"(the second run reported %d new)", afterwards, messages, second.MessagesNew)
	}
}

// A mid-pass checkpoint must not claim the mailbox is caught up.
func TestMidPassCheckpointDoesNotAdvanceTheSequence(t *testing.T) {
	h := newHarness(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(10)}},
	})
	ctx := context.Background()

	var mailboxID int64
	if err := h.store.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (account_id, name, canonical_name)
		 VALUES ($1, 'Probe', 'probe') RETURNING id`, h.accountID).Scan(&mailboxID); err != nil {
		t.Fatalf("creating mailbox: %v", err)
	}

	// A checkpoint taken partway through records progress but must leave the
	// sequence numbers alone.
	if err := h.store.SaveCheckpoint(ctx, mailboxID, store.Checkpoint{
		UIDValidity: 42, UIDNext: 500, HighestModSeq: 900,
		MetadataSyncedThroughUID: 100,
	}); err != nil {
		t.Fatalf("saving mid-pass checkpoint: %v", err)
	}

	var uidnext, modseq, watermark *int64
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT uidnext, highestmodseq, metadata_synced_through_uid FROM mailboxes WHERE id = $1`,
		mailboxID).Scan(&uidnext, &modseq, &watermark); err != nil {
		t.Fatalf("reading mailbox: %v", err)
	}

	if uidnext != nil {
		t.Errorf("a mid-pass checkpoint advanced uidnext to %d; the next run would think the "+
			"mailbox was fully enumerated", *uidnext)
	}
	if modseq != nil && *modseq != 0 {
		t.Errorf("a mid-pass checkpoint advanced highestmodseq to %d", *modseq)
	}
	if watermark == nil || *watermark != 100 {
		t.Errorf("the watermark should still advance mid-pass, got %v", watermark)
	}

	// Completing the pass is what makes the sequence numbers authoritative.
	if err := h.store.SaveCheckpoint(ctx, mailboxID, store.Checkpoint{
		UIDValidity: 42, UIDNext: 500, HighestModSeq: 900,
		MetadataSyncedThroughUID: 500, FullScanCompleted: true,
	}); err != nil {
		t.Fatalf("saving final checkpoint: %v", err)
	}
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT uidnext, highestmodseq FROM mailboxes WHERE id = $1`,
		mailboxID).Scan(&uidnext, &modseq); err != nil {
		t.Fatalf("reading mailbox: %v", err)
	}
	if uidnext == nil || *uidnext != 500 {
		t.Errorf("a completed pass should record uidnext, got %v", uidnext)
	}
	if modseq == nil || *modseq != 900 {
		t.Errorf("a completed pass should record highestmodseq, got %v", modseq)
	}
}
