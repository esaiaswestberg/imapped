package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/upstream"
	"golang.org/x/sync/errgroup"
)

type mailboxResult struct {
	MessagesNew   int64
	BodiesFetched int64
	BytesFetched  int64
}

// syncMailbox mirrors one mailbox in two passes.
//
// Pass one enumerates metadata and writes rows immediately; pass two downloads
// bodies for whatever those rows say is missing. Splitting them is what makes
// the sync resumable and parallel: the metadata pass is cheap and strictly
// ordered, while bodies are independent units of work that several connections
// can fetch at once and that survive an interruption.
func (e *Engine) syncMailbox(
	ctx context.Context,
	client *upstream.Client,
	account store.Account,
	box upstream.MailboxInfo,
	p *Progress,
) (mailboxResult, error) {
	var result mailboxResult

	p.setMailbox(box.Name)
	p.setPhase("metadata: " + box.Name)

	delimiter := string(box.Delimiter)
	specialUse := box.SpecialUse()
	params := store.UpsertMailboxParams{
		AccountID:     account.ID,
		Name:          box.Name,
		CanonicalName: strings.ToLower(box.Name),
		Attributes:    attributeNames(box.Attributes),
	}
	if delimiter != "" && box.Delimiter != 0 {
		params.Delimiter = &delimiter
	}
	if specialUse != "" {
		params.SpecialUse = &specialUse
	}

	mailbox, err := e.store.UpsertMailbox(ctx, params)
	if err != nil {
		return result, err
	}

	p.addCommands(1)
	selected, err := client.Select(ctx, box.Name, true)
	if err != nil {
		return result, err
	}

	// A changed UIDVALIDITY means the server renumbered the mailbox and every
	// UID we hold is meaningless. Messages are kept and re-linked by content
	// rather than deleted and re-downloaded.
	if mailbox.UIDValidity != nil && *mailbox.UIDValidity != int64(selected.UIDValidity) {
		e.log.Info("mailbox was renumbered upstream, re-enumerating without re-downloading",
			"mailbox", box.Name,
			"old_uidvalidity", *mailbox.UIDValidity,
			"new_uidvalidity", selected.UIDValidity)
		if err := e.store.ResetForUIDValidityChange(ctx, mailbox.ID, int64(selected.UIDValidity)); err != nil {
			return result, err
		}
		mailbox, err = e.store.GetMailbox(ctx, mailbox.ID)
		if err != nil {
			return result, err
		}
	}

	// The two passes run concurrently.
	//
	// They share nothing but the database: the metadata pass writes rows marked
	// pending, and the body pass claims them. Running them in sequence meant no
	// message became readable until the last one had been enumerated — on a
	// mailbox of a hundred thousand messages, hours of a completely empty
	// interface while the work was in fact proceeding.
	//
	// Overlapping them means mail appears within seconds, newest first, while
	// enumeration continues behind it.
	metadataDone := make(chan struct{})

	var (
		bodies, bytes int64
		bodyErr       error
		wg            sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		bodies, bytes, bodyErr = e.bodyPass(ctx, account, mailbox, p, metadataDone)

		// Reported the moment it happens. The error is not returned to the
		// caller until the metadata pass also finishes, which on a large
		// mailbox is hours away — so a body pass that died in the first minute
		// would otherwise leave the download counter frozen with nothing
		// anywhere to say why.
		if bodyErr != nil && !errors.Is(bodyErr, context.Canceled) {
			e.log.Error("downloading message bodies stopped early",
				"mailbox", box.Name, "downloaded", bodies, "error", bodyErr)
		}
	}()

	newMessages, metaErr := e.metadataPass(ctx, client, account, mailbox, selected, p)

	// Tell the body pass no further work is coming, whether the metadata pass
	// succeeded or not: bodies already recorded are still worth downloading,
	// and leaving the producer waiting would hang the mailbox.
	p.recordMetadataComplete()
	close(metadataDone)
	p.setPhase("bodies: " + box.Name)
	wg.Wait()

	result.MessagesNew = newMessages
	result.BodiesFetched = bodies
	result.BytesFetched = bytes

	if metaErr != nil {
		return result, metaErr
	}
	if bodyErr != nil {
		return result, bodyErr
	}

	if err := e.store.RefreshMailboxCounts(ctx, mailbox.ID); err != nil {
		e.log.Warn("refreshing mailbox counts", "mailbox", box.Name, "error", err)
	}
	return result, nil
}

// metadataPass enumerates messages and records what it learns.
//
// The command shape depends on what the server supports:
//
//   - With CONDSTORE, an unchanged mailbox is detected from SELECT alone and
//     costs nothing further; changed flags come back via CHANGEDSINCE, and only
//     genuinely new UIDs are enumerated in full.
//   - Without it, the whole mailbox is enumerated, but still in one command per
//     chunk rather than one per message.
//
// In every case the cost is proportional to what changed, not to how many
// messages the mailbox holds.
func (e *Engine) metadataPass(
	ctx context.Context,
	client *upstream.Client,
	account store.Account,
	mailbox store.Mailbox,
	selected *upstream.SelectedMailbox,
	p *Progress,
) (int64, error) {
	condStore := e.cfg.Upstream.PreferCondStore &&
		client.Caps().Has(imap.CapCondStore) &&
		selected.HighestModSeq > 0

	storedModSeq := int64(0)
	if mailbox.HighestModSeq != nil {
		storedModSeq = *mailbox.HighestModSeq
	}
	storedUIDNext := int64(0)
	if mailbox.UIDNext != nil {
		storedUIDNext = *mailbox.UIDNext
	}

	// Nothing has changed at all: no new messages and no flag updates. One
	// SELECT settles it, where the previous implementation would still have
	// issued a FETCH for every message in the mailbox.
	//
	// Holding fewer messages than the server reports is treated as proof that
	// we are not caught up, whatever the modification sequence says. Without
	// that check a mailbox whose enumeration was interrupted could look
	// complete — and it did: an interrupted pass left the stored sequence equal
	// to the server's, so the next run skipped ninety thousand messages. It
	// also repairs a database already in that state, with no manual step.
	if condStore &&
		storedModSeq == int64(selected.HighestModSeq) &&
		storedUIDNext == int64(selected.UIDNext) &&
		mailbox.MetadataSyncedThroughUID > 0 {

		localCount, err := e.store.CountMailboxMessages(ctx, mailbox.ID)
		if err != nil {
			return 0, err
		}
		if localCount >= int64(selected.NumMessages) {
			e.log.Debug("mailbox is unchanged since the last sync",
				"mailbox", mailbox.Name, "highestmodseq", storedModSeq, "messages", localCount)
			return 0, nil
		}
		e.log.Info("mailbox reports as unchanged but holds fewer messages than the server, re-enumerating",
			"mailbox", mailbox.Name, "local", localCount, "upstream", selected.NumMessages)
	}

	var newMessages int64

	// Flag changes on messages already known, when the server can tell us
	// exactly which ones moved.
	if condStore && storedModSeq > 0 {
		p.addCommands(1)
		changed, err := client.FetchMetadata(ctx, UIDRange(1), uint64(storedModSeq), false)
		if err != nil {
			return newMessages, err
		}
		for _, meta := range changed {
			if err := e.store.UpdateFlags(ctx, mailbox.ID, int64(meta.UID),
				flagNames(meta.Flags), int64(meta.ModSeq)); err != nil {
				return newMessages, err
			}
		}
		if len(changed) > 0 {
			e.log.Debug("applied flag changes", "mailbox", mailbox.Name, "count", len(changed))
		}
	}

	// Enumerate the exact UIDs present, then chunk by message count.
	//
	// Asking the server which UIDs exist costs one cheap command and removes
	// all guesswork: UID spans say nothing about how many messages they cover
	// once deletions have left holes, and UIDNEXT can lag on some servers.
	p.addCommands(1)
	allUIDs, err := client.SearchAllUIDs(ctx)
	if err != nil {
		return newMessages, err
	}

	known, err := e.store.ExistingUpstreamUIDs(ctx, mailbox.ID)
	if err != nil {
		return newMessages, err
	}

	// Skip what is already recorded, so a re-sync only asks about the rest.
	pending := make([]imap.UID, 0, len(allUIDs))
	for _, uid := range allUIDs {
		if _, exists := known[int64(uid)]; exists && condStore {
			continue
		}
		pending = append(pending, uid)
	}

	highestSeen := mailbox.MetadataSyncedThroughUID
	chunks := PlanMetadataChunks(pending, e.cfg.Sync.MetadataBatchMessages)

	e.log.Debug("metadata pass planned",
		"mailbox", mailbox.Name, "present", len(allUIDs),
		"to_fetch", len(pending), "chunks", len(chunks))

	batch := make([]store.MessageMeta, 0, e.cfg.Sync.MetadataBatchMessages)

	for i, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return newMessages, err
		}

		// Counted before the command, not after: a command that ran for
		// minutes and then failed still happened, and a run that reports fewer
		// commands than it issued is misleading precisely when it matters.
		p.addCommands(1)
		metas, err := client.FetchMetadata(ctx, chunk, 0, true)
		if err != nil {
			return newMessages, err
		}

		batch = batch[:0]
		for _, meta := range metas {
			uid := int64(meta.UID)
			if uid > highestSeen {
				highestSeen = uid
			}
			if _, exists := known[uid]; exists {
				// Already recorded; only the flags may have moved, and the
				// CONDSTORE pass above handles that when available.
				if !condStore {
					if err := e.store.UpdateFlags(ctx, mailbox.ID, uid,
						flagNames(meta.Flags), int64(meta.ModSeq)); err != nil {
						return newMessages, err
					}
				}
				continue
			}
			batch = append(batch, buildMeta(account.ID, mailbox.ID, meta))
		}

		// One transaction per chunk rather than one per message: several
		// thousand commits would make ingest, not the network, the bottleneck.
		created, err := e.store.UpsertMetadataBatch(ctx, batch)
		if err != nil {
			return newMessages, fmt.Errorf("recording metadata chunk %d: %w", i+1, err)
		}
		newMessages += created

		// Credited per chunk so the interface moves during a long pass, and so
		// a pass that fails partway still reports what it committed.
		p.addMessages(created)
		p.setPhase(fmt.Sprintf("metadata: %s (%d/%d)",
			mailbox.Name, min((i+1)*e.cfg.Sync.MetadataBatchMessages, len(pending)), len(pending)))

		// Checkpoint after each chunk, so an interruption costs one chunk
		// rather than the whole pass.
		if err := e.store.SaveCheckpoint(ctx, mailbox.ID, store.Checkpoint{
			UIDValidity:              int64(selected.UIDValidity),
			UIDNext:                  int64(selected.UIDNext),
			HighestModSeq:            int64(selected.HighestModSeq),
			MetadataSyncedThroughUID: highestSeen,
		}); err != nil {
			return newMessages, err
		}
	}

	if err := e.reconcileDeletions(ctx, client, mailbox, selected, condStore); err != nil {
		e.log.Warn("reconciling deletions", "mailbox", mailbox.Name, "error", err)
	}

	// The modification sequence is advanced only now, after every change it
	// covers has been committed. Advancing it earlier would mean the server
	// never reports those changes again, silently losing them.
	if err := e.store.SaveCheckpoint(ctx, mailbox.ID, store.Checkpoint{
		UIDValidity:              int64(selected.UIDValidity),
		UIDNext:                  int64(selected.UIDNext),
		HighestModSeq:            int64(selected.HighestModSeq),
		MetadataSyncedThroughUID: highestSeen,
		FullScanCompleted:        true,
	}); err != nil {
		return newMessages, err
	}

	return newMessages, nil
}

// reconcileDeletions removes messages that no longer exist upstream.
//
// CONDSTORE reports flag changes but says nothing about expunges, so detecting
// deletions needs an explicit UID enumeration. That is the one remaining
// operation whose cost scales with mailbox size, so it runs only when the local
// and remote message counts actually disagree.
func (e *Engine) reconcileDeletions(
	ctx context.Context,
	client *upstream.Client,
	mailbox store.Mailbox,
	selected *upstream.SelectedMailbox,
	condStore bool,
) error {
	localCount, err := e.store.CountMailboxMessages(ctx, mailbox.ID)
	if err != nil {
		return err
	}
	if localCount == int64(selected.NumMessages) {
		return nil
	}

	e.log.Debug("message counts disagree, scanning for deletions",
		"mailbox", mailbox.Name, "local", localCount, "upstream", selected.NumMessages)

	uids, err := client.SearchAllUIDs(ctx)
	if err != nil {
		return err
	}

	present := make([]int64, 0, len(uids))
	for _, uid := range uids {
		present = append(present, int64(uid))
	}

	removed, err := e.store.DeleteMissingUIDs(ctx, mailbox.ID, present)
	if err != nil {
		return err
	}
	if removed > 0 {
		e.log.Info("removed messages deleted upstream",
			"mailbox", mailbox.Name, "count", removed)
	}
	return nil
}

// bodyPass downloads message bodies in parallel.
//
// Work is claimed from the database rather than held in memory, which is what
// makes an interrupted sync cheap to resume: whatever was not stored is still
// marked pending and gets picked up on the next run, with no re-downloading of
// what already succeeded.
func (e *Engine) bodyPass(
	ctx context.Context,
	account store.Account,
	mailbox store.Mailbox,
	p *Progress,
	metadataDone <-chan struct{},
) (int64, int64, error) {
	if pending, err := e.store.CountPendingBodiesForAccount(ctx, account.ID); err == nil {
		p.setPendingBodies(pending)
	}

	budget := BatchBudget{
		MaxBytes:    e.cfg.Sync.BodyBatchBytes.Int64(),
		MaxMessages: e.cfg.Sync.BodyBatchMaxMsgs,
		SoloAbove:   e.cfg.Sync.BodyMaxInlineBytes.Int64(),
	}

	// Every worker opens its own connection. Reusing the account's control
	// connection for worker zero saved one SELECT, but that connection is now
	// busy enumerating metadata — and sharing it also meant a body-worker
	// failure could damage the connection every later mailbox depends on.
	//
	// One slot is left for the metadata pass, so the total stays within the
	// per-account budget a provider will tolerate.
	workers := e.cfg.Sync.ConnectionsPerAccount - 1
	if workers < 1 {
		workers = 1
	}

	batches := make(chan []SizedUID)
	var fetched, bytes atomic.Int64

	// A plain group, not errgroup.WithContext.
	//
	// With a shared cancelling context, one worker hitting a deadline cancelled
	// every sibling and ended the download for the whole mailbox — turning a
	// single slow batch into a total stop. Bodies are claimed from the database
	// and retried on the next run, so one failed worker is not a reason to
	// abandon the rest; the pass fails only if every worker did.
	group := new(errgroup.Group)
	groupCtx := ctx

	var (
		workerMu   sync.Mutex
		workerErrs []error
		workersRun int
	)
	recordWorker := func(err error) {
		workerMu.Lock()
		defer workerMu.Unlock()
		workersRun++
		if err != nil {
			workerErrs = append(workerErrs, err)
		}
	}

	// Producer: claim work and plan it into batches.
	//
	// Finding nothing to claim no longer means the pass is over — the metadata
	// pass may still be discovering messages. The producer waits for more work
	// and only stops once metadata has finished and a final sweep comes back
	// empty, so a message committed moments before that signal is not missed.
	group.Go(func() error {
		defer close(batches)

		const idlePoll = 2 * time.Second
		metadataFinished := false

		for {
			if err := groupCtx.Err(); err != nil {
				return err
			}
			claimed, err := e.store.ClaimPendingBodies(groupCtx, mailbox.ID,
				e.cfg.Sync.BodyBatchMaxMsgs*workers*2, e.cfg.Sync.BodyMaxAttempts)
			if err != nil {
				return err
			}
			if len(claimed) == 0 {
				if metadataFinished {
					return nil
				}
				select {
				case <-metadataDone:
					// Sweep once more: metadata may have committed rows between
					// the claim above and this signal.
					metadataFinished = true
				case <-groupCtx.Done():
					return groupCtx.Err()
				case <-time.After(idlePoll):
				}
				continue
			}
			if remaining, err := e.store.CountPendingBodiesForAccount(groupCtx, account.ID); err == nil {
				p.setPendingBodies(remaining)
			}

			sized := make([]SizedUID, 0, len(claimed))
			byUID := make(map[imap.UID]store.PendingBody, len(claimed))
			for _, c := range claimed {
				sized = append(sized, SizedUID{UID: imap.UID(c.UpstreamUID), Size: c.Size})
				byUID[imap.UID(c.UpstreamUID)] = c
			}
			e.rememberClaims(mailbox.ID, byUID)

			for _, batch := range PlanBatches(sized, budget) {
				select {
				case batches <- batch:
				case <-groupCtx.Done():
					return groupCtx.Err()
				}
			}
		}
	})

	for worker := range workers {
		group.Go(func() error {
			creds, err := e.credentials(account)
			if err != nil {
				return err
			}
			workerClient, err := e.connect.Connect(groupCtx, creds)
			if err != nil {
				// A server refusing further connections is a reason to run with
				// fewer workers, not to fail the sync. Warn rather than inform:
				// losing a worker silently is how a download queue ends up
				// making no progress with nothing to explain it.
				if upstream.IsTooManyConnections(err) {
					e.log.Warn("upstream refused another connection; this worker will not run. "+
						"Lower sync.connections_per_account if downloads stall",
						"account_id", account.ID, "worker", worker, "error", err)
					recordWorker(nil)
					return nil
				}
				e.log.Warn("a body-fetch worker could not connect",
					"account_id", account.ID, "worker", worker, "error", err)
				recordWorker(err)
				return nil
			}
			defer workerClient.Close()

			if _, err := workerClient.Select(groupCtx, mailbox.Name, true); err != nil {
				e.log.Warn("a body-fetch worker could not open the mailbox",
					"mailbox", mailbox.Name, "worker", worker, "error", err)
				recordWorker(err)
				return nil
			}

			// Batches keep being drained even after a failure, so one bad batch
			// does not strand the queue behind a worker that has stopped
			// reading from it.
			var workerErr error
			for batch := range batches {
				if err := e.fetchBatch(groupCtx, workerClient, mailbox, batch, p, &fetched, &bytes); err != nil {
					e.log.Warn("a batch of message bodies failed; the messages stay queued for the next run",
						"mailbox", mailbox.Name, "worker", worker, "size", len(batch), "error", err)
					workerErr = err
					break
				}
			}
			// Drain whatever is left so the producer is never blocked writing
			// to a channel nobody is reading.
			for range batches { //nolint:revive // deliberately discarding
			}
			recordWorker(workerErr)
			return nil
		})
	}

	err := group.Wait()

	// Only a total failure is worth failing the mailbox for. Anything less
	// leaves messages queued, and the next run picks them up.
	workerMu.Lock()
	failed, ran := len(workerErrs), workersRun
	var firstErr error
	if failed > 0 {
		firstErr = workerErrs[0]
	}
	workerMu.Unlock()

	if err == nil && failed > 0 && failed == ran {
		err = fmt.Errorf("every body-fetch worker failed (%d of %d): %w", failed, ran, firstErr)
	} else if failed > 0 {
		e.log.Warn("some body-fetch workers failed; their messages remain queued",
			"mailbox", mailbox.Name, "failed", failed, "workers", ran)
	}

	return fetched.Load(), bytes.Load(), err
}

// fetchBatch downloads one batch and stores each body.
func (e *Engine) fetchBatch(
	ctx context.Context,
	client *upstream.Client,
	mailbox store.Mailbox,
	batch []SizedUID,
	p *Progress,
	fetched, bytesTotal *atomic.Int64,
) error {
	maxSize := e.cfg.Limits.MaxMessageSize.Int64()

	err := client.FetchBodies(ctx, UIDSet(batch), maxSize,
		func(uid imap.UID, size int64, body io.Reader) error {
			claim, ok := e.lookupClaim(mailbox.ID, uid)
			if !ok {
				// The message was claimed by another worker or vanished; drain
				// and move on rather than failing the batch.
				_, _ = io.Copy(io.Discard, body)
				return nil
			}

			// Buffered rather than streamed straight to storage, because the
			// message must also be parsed and the parser needs the whole thing.
			// The batch planner keeps this bounded: batches are capped by byte
			// budget, and anything oversized is fetched alone.
			raw, err := io.ReadAll(body)
			if err != nil {
				return e.store.MarkBodyFailed(ctx, claim.MailboxMessageID, err, e.cfg.Sync.BodyMaxAttempts)
			}

			info, err := e.blobs.Put(ctx, blob.TypeRFC822, bytes.NewReader(raw))
			if err != nil {
				return e.store.MarkBodyFailed(ctx, claim.MailboxMessageID, err, e.cfg.Sync.BodyMaxAttempts)
			}

			if err := e.store.AttachBody(ctx, claim.MailboxMessageID,
				info.SHA256, info.Size, info.Key.String()); err != nil {
				return err
			}

			// Parsing populates the searchable text and part list. A failure
			// here must not discard the message: the body is already stored and
			// the failure is recorded against the row.
			messageID, err := e.store.MessageIDForMailboxMessage(ctx, claim.MailboxMessageID)
			if err != nil {
				return err
			}
			if err := e.ingest.Ingest(ctx, messageID, raw); err != nil {
				e.log.Warn("parsing message failed, the raw message is still stored",
					"mailbox_message_id", claim.MailboxMessageID, "error", err)
			}

			fetched.Add(1)
			bytesTotal.Add(info.Size)
			p.addBody(info.Size)
			return nil
		})
	p.addCommands(1)

	if err == nil {
		return nil
	}

	// An oversized message is expected and must not fail the batch.
	if errors.Is(err, upstream.ErrMessageTooLarge) {
		for _, msg := range batch {
			if claim, ok := e.lookupClaim(mailbox.ID, msg.UID); ok {
				if markErr := e.store.MarkBodyTooLarge(ctx, claim.MailboxMessageID); markErr != nil {
					return markErr
				}
			}
		}
		return nil
	}

	// Release the batch's claims so the messages are retried rather than left
	// stuck in the fetching state until the reaper notices them.
	for _, msg := range batch {
		if claim, ok := e.lookupClaim(mailbox.ID, msg.UID); ok {
			_ = e.store.MarkBodyFailed(ctx, claim.MailboxMessageID, err, e.cfg.Sync.BodyMaxAttempts)
		}
	}
	return err
}

func attributeNames(attrs []imap.MailboxAttr) []string {
	names := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		names = append(names, string(attr))
	}
	return names
}

func flagNames(flags []imap.Flag) []string {
	names := make([]string, 0, len(flags))
	for _, flag := range flags {
		names = append(names, string(flag))
	}
	return names
}

// buildMeta converts an upstream metadata record into a database row.
func buildMeta(accountID, mailboxID int64, meta upstream.MessageMeta) store.MessageMeta {
	out := store.MessageMeta{
		AccountID:      accountID,
		MailboxID:      mailboxID,
		UpstreamUID:    int64(meta.UID),
		UpstreamModSeq: int64(meta.ModSeq),
		Flags:          flagNames(meta.Flags),
		Size:           meta.Size,
	}

	if envelope := meta.Envelope; envelope != nil {
		if envelope.Subject != "" {
			subject := sanitiseText(envelope.Subject)
			out.Subject = &subject
		}
		if envelope.MessageID != "" {
			id := sanitiseText(envelope.MessageID)
			out.MessageID = &id
		}
		if len(envelope.InReplyTo) > 0 {
			ref := sanitiseText(envelope.InReplyTo[0])
			out.InReplyTo = &ref
		}
		if !envelope.Date.IsZero() {
			date := envelope.Date
			out.SentDate = &date
		}
		out.Addrs = marshalJSON(addressMap(envelope))
		out.Envelope = marshalJSON(envelope)
	}
	if meta.BodyStructure != nil {
		out.BodyStructure = marshalJSON(meta.BodyStructure)
	}
	if internal := meta.InternalDate.Or(time.Time{}); !internal.IsZero() {
		out.InternalDate = &internal
	}
	return out
}

func addressMap(envelope *imap.Envelope) map[string][]string {
	render := func(addrs []imap.Address) []string {
		out := make([]string, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, sanitiseText(a.Addr()))
		}
		return out
	}
	return map[string][]string{
		"from":     render(envelope.From),
		"to":       render(envelope.To),
		"cc":       render(envelope.Cc),
		"bcc":      render(envelope.Bcc),
		"reply_to": render(envelope.ReplyTo),
	}
}

// sanitiseText makes a header value safe to store.
//
// Postgres rejects NUL bytes in text columns outright, and invalid UTF-8 too.
// Real-world mail contains both, and the previous implementation aborted an
// entire sync on the first message that did — 763 messages into an 8,300
// message mailbox.
func sanitiseText(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.ToValidUTF8(s, "")
}
