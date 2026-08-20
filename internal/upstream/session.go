package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/esaiaswestberg/imapped/internal/config"
)

// Session is a self-healing handle to one upstream connection.
//
// A connection dies for ordinary reasons: the peer closes it, a network blip
// interrupts it, a command is abandoned at its deadline and the protocol stream
// can no longer be trusted. Detecting that was never the hard part; recovering
// was, and every caller that held a raw Client had to solve it separately —
// which in practice meant none of them did, and a single fault ended the work.
//
// A Session reconnects on demand and restores the mailbox it had open, so
// callers write the command they want and stop reasoning about connections.
type Session struct {
	connector *Connector
	account   Account
	cfg       config.Upstream
	log       *slog.Logger

	mu       sync.Mutex
	client   *Client
	selected string
	readOnly bool
}

// NewSession creates a session for an account. No connection is opened until
// the first command runs.
func (c *Connector) NewSession(account Account) *Session {
	return &Session{
		connector: c,
		account:   account,
		cfg:       c.cfg,
		log:       c.log,
	}
}

// Select opens a mailbox, and remembers it so a later reconnection restores it.
func (s *Session) Select(ctx context.Context, name string, readOnly bool) (*SelectedMailbox, error) {
	var selected *SelectedMailbox

	err := s.Do(ctx, "select "+name, func(ctx context.Context, client *Client) error {
		data, err := client.Select(ctx, name, readOnly)
		if err != nil {
			return err
		}
		selected = data
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.selected, s.readOnly = name, readOnly
	s.mu.Unlock()

	return selected, nil
}

// Do runs a command, reconnecting and retrying if the connection fails.
//
// Only idempotent operations may be passed here. Every command the sync engine
// issues qualifies: fetches and searches are reads, and a flag update sends an
// absolute set rather than a delta, so applying it twice is indistinguishable
// from applying it once.
func (s *Session) Do(ctx context.Context, op string, fn func(context.Context, *Client) error) error {
	attempts := s.cfg.RetryMaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		client, err := s.ensureConnected(ctx)
		if err != nil {
			lastErr = err
			// An authentication failure will not fix itself, and retrying it
			// risks the provider locking the account.
			if SeverityOf(err) == FatalAccount {
				return err
			}
			if !s.waitBeforeRetry(ctx, attempt, op, err) {
				break
			}
			continue
		}

		err = fn(ctx, client)
		if err == nil {
			return nil
		}
		lastErr = err

		if !s.recoverable(err) {
			return err
		}

		// The connection cannot carry another command, so discard it. The next
		// attempt dials afresh and re-opens the mailbox.
		s.discard()

		if !s.waitBeforeRetry(ctx, attempt, op, err) {
			break
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", op, attempts, lastErr)
}

// recoverable reports whether a fresh connection could plausibly succeed.
func (s *Session) recoverable(err error) bool {
	switch {
	case errors.Is(err, context.Canceled):
		// The caller is shutting down; a new connection would not help.
		return false
	case SeverityOf(err) == FatalAccount, SeverityOf(err) == FatalMailbox:
		return false
	case errors.Is(err, ErrPoisoned),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, context.DeadlineExceeded):
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}

// waitBeforeRetry sleeps between attempts, reporting whether to continue.
func (s *Session) waitBeforeRetry(ctx context.Context, attempt int, op string, cause error) bool {
	delay := Backoff(attempt, s.cfg.RetryBaseDelay.Std(), s.cfg.RetryMaxDelay.Std())
	s.log.Warn("upstream command failed, reconnecting",
		"operation", op, "attempt", attempt, "retry_in", delay, "error", cause)

	select {
	case <-ctx.Done():
		return false
	case <-timeAfter(delay):
		return true
	}
}

// ensureConnected returns a live client, dialling and re-selecting if needed.
func (s *Session) ensureConnected(ctx context.Context) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil && !s.client.Poisoned() {
		return s.client, nil
	}
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}

	client, err := s.connector.Connect(ctx, s.account)
	if err != nil {
		return nil, err
	}

	// Restore the mailbox the caller had open, so a reconnection is invisible
	// to code that selected once and then issued many commands.
	if s.selected != "" {
		if _, err := client.Select(ctx, s.selected, s.readOnly); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("reopening %s after reconnecting: %w", s.selected, err)
		}
	}

	s.client = client
	return client, nil
}

// discard drops the current connection so the next command dials again.
func (s *Session) discard() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
}

// Close releases the connection, if one is open.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil {
		return nil
	}
	err := s.client.Close()
	s.client = nil
	return err
}

// Caps reports the capabilities of the current connection, if any.
func (s *Session) Caps() imapCapSet {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil {
		return nil
	}
	return s.client.Caps()
}
