package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// maxMutationAttempts bounds how often a change is retried before it is parked.
const maxMutationAttempts = 8

// ReplayMutations pushes queued local changes to the upstream server.
//
// Changes are applied locally first and replayed here, so a client sees its
// action take effect immediately and an upstream outage delays the push rather
// than blocking the user. Replay is idempotent: setting flags to a known state
// produces the same result however many times it runs, which is what makes a
// crash between "applied" and "recorded as applied" harmless.
func (e *Engine) ReplayMutations(ctx context.Context, accountID int64) (int, error) {
	pending, err := e.store.CountPendingMutations(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if pending == 0 {
		return 0, nil
	}

	account, err := e.store.GetAccount(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if !account.Active() {
		return 0, nil
	}

	creds, err := e.credentials(account)
	if err != nil {
		return 0, err
	}
	client, err := e.connect.Connect(ctx, creds)
	if err != nil {
		return 0, fmt.Errorf("connecting to replay changes: %w", err)
	}
	defer client.Close()

	worker := workerName()
	applied := 0
	selectedMailbox := ""

	for {
		if err := ctx.Err(); err != nil {
			return applied, err
		}

		batch, err := e.store.ClaimDueMutations(ctx, accountID, 50, worker)
		if err != nil {
			return applied, err
		}
		if len(batch) == 0 {
			return applied, nil
		}

		for _, mutation := range batch {
			err := e.applyMutation(ctx, client, mutation, &selectedMailbox)
			if err == nil {
				if err := e.store.MutationSucceeded(ctx, mutation.ID); err != nil {
					return applied, err
				}
				applied++
				continue
			}

			// One failing change must not stop the queue: the rest are still
			// worth attempting, and this one is rescheduled with backoff.
			e.log.Warn("replaying a change upstream failed",
				"account_id", accountID, "mutation_id", mutation.ID, "error", err)

			delay := upstream.Backoff(mutation.Attempts,
				e.cfg.Upstream.RetryBaseDelay.Std(), e.cfg.Upstream.RetryMaxDelay.Std())
			if markErr := e.store.MutationFailed(ctx, mutation.ID, err, delay, maxMutationAttempts); markErr != nil {
				return applied, markErr
			}

			// An authentication failure will not fix itself; stop rather than
			// working through the queue failing every one.
			if upstream.SeverityOf(err) == upstream.FatalAccount {
				return applied, err
			}
		}
	}
}

func (e *Engine) applyMutation(ctx context.Context, client *upstream.Client,
	mutation store.Mutation, selectedMailbox *string) error {

	switch mutation.Type {
	case store.MutationStoreFlags:
		var payload store.FlagPayload
		if err := json.Unmarshal(mutation.Payload, &payload); err != nil {
			return fmt.Errorf("decoding the change: %w", err)
		}
		if payload.UpstreamUID == 0 {
			// A message that has not been pushed upstream has no UID there to
			// act on. Nothing to do, and nothing that will change that.
			return nil
		}

		// SELECT is stateful, so only re-issue it when the target changes.
		if *selectedMailbox != payload.Mailbox {
			if _, err := client.Select(ctx, payload.Mailbox, false); err != nil {
				return err
			}
			*selectedMailbox = payload.Mailbox
		}

		flags := make([]imap.Flag, 0, len(payload.Flags))
		for _, name := range payload.Flags {
			flags = append(flags, imap.Flag(name))
		}
		return client.StoreFlags(ctx, imap.UID(payload.UpstreamUID), flags)

	default:
		return fmt.Errorf("unsupported change type %q", mutation.Type)
	}
}

// workerName identifies which process holds a claim, for diagnosis.
func workerName() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// ReleaseStaleMutations returns claims abandoned by a dead process.
func (e *Engine) ReleaseStaleMutations(ctx context.Context, olderThan time.Duration) (int64, error) {
	return e.store.ReleaseStaleMutations(ctx, olderThan)
}
