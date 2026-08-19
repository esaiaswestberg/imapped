package upstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

// Severity says how a failure should be handled.
type Severity int

const (
	// Retryable failures are transient: reconnect and try again.
	Retryable Severity = iota
	// FatalAccount failures will recur until a human changes something, so
	// retrying only burns quota and risks the provider blocking the address.
	FatalAccount
	// FatalMailbox failures affect one mailbox; other mailboxes still sync.
	FatalMailbox
)

func (s Severity) String() string {
	switch s {
	case FatalAccount:
		return "fatal-account"
	case FatalMailbox:
		return "fatal-mailbox"
	default:
		return "retryable"
	}
}

// Error wraps an upstream failure with its severity.
type Error struct {
	Severity Severity
	Code     imap.ResponseCode
	Err      error
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s [%s]", e.Err, e.Code)
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// SeverityOf reports how err should be handled, defaulting to retryable for
// anything unrecognised. Defaulting the other way would let one unfamiliar
// server response permanently stop an account from syncing.
func SeverityOf(err error) Severity {
	var upstreamErr *Error
	if errors.As(err, &upstreamErr) {
		return upstreamErr.Severity
	}
	return Retryable
}

// classify inspects an IMAP error and attaches a severity.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return err
	}

	// Cancellation is our own doing, not the server's.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Severity: Retryable, Err: err}
	}

	var imapErr *imap.Error
	if errors.As(err, &imapErr) {
		return &Error{Severity: severityForCode(imapErr.Code, imapErr.Text), Code: imapErr.Code, Err: err}
	}

	// Network-level failures are transient by nature.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return &Error{Severity: Retryable, Err: err}
	}

	return &Error{Severity: severityForText(err.Error()), Err: err}
}

func severityForCode(code imap.ResponseCode, text string) Severity {
	switch code {
	case imap.ResponseCodeAuthenticationFailed,
		imap.ResponseCodeAuthorizationFailed,
		imap.ResponseCodeExpired,
		imap.ResponseCodePrivacyRequired:
		// These recur until credentials or server settings change. Retrying
		// risks the provider locking the account for repeated bad logins.
		return FatalAccount
	case imap.ResponseCodeNonExistent, imap.ResponseCodeTryCreate:
		// One mailbox is gone; the rest of the account is unaffected.
		return FatalMailbox
	case imap.ResponseCodeUnavailable,
		imap.ResponseCodeInUse,
		imap.ResponseCodeServerBug,
		imap.ResponseCodeCannot,
		imap.ResponseCodeLimit:
		return Retryable
	}
	return severityForText(text)
}

// severityForText is the fallback for servers that report a condition in prose
// rather than a response code, which is common on older or bespoke servers.
func severityForText(text string) Severity {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "authentication failed"),
		strings.Contains(lower, "invalid credentials"),
		strings.Contains(lower, "login failed"),
		strings.Contains(lower, "password"),
		strings.Contains(lower, "application-specific password"):
		return FatalAccount
	case strings.Contains(lower, "no such mailbox"),
		strings.Contains(lower, "mailbox does not exist"),
		strings.Contains(lower, "nonexistent"):
		return FatalMailbox
	}
	return Retryable
}

// IsTooManyConnections reports whether the server is refusing further parallel
// connections, which the sync engine uses to reduce its worker pool rather than
// hammering a server that has already said no.
func IsTooManyConnections(err error) bool {
	if err == nil {
		return false
	}
	var imapErr *imap.Error
	if errors.As(err, &imapErr) && imapErr.Code == imap.ResponseCodeLimit {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "too many connections") ||
		strings.Contains(lower, "maximum number of connections") ||
		strings.Contains(lower, "concurrent connections")
}

// Backoff computes the delay before retry attempt n (1-based).
//
// Full jitter rather than plain exponential: when a server comes back after an
// outage, every worker would otherwise retry in lockstep and knock it over
// again. Randomising the whole interval spreads the reconnection storm out.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := base
	for i := 1; i < attempt && backoff < max; i++ {
		backoff *= 2
	}
	if backoff > max {
		backoff = max
	}
	if backoff <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(backoff)) + 1)
}

// Retry runs fn until it succeeds, its failure is fatal, or attempts run out.
func Retry(ctx context.Context, attempts int, base, max time.Duration, fn func(ctx context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = classify(err)

		// Retrying a fatal failure cannot help and may do harm.
		if SeverityOf(lastErr) != Retryable {
			return lastErr
		}
		if attempt == attempts {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(Backoff(attempt, base, max)):
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", attempts, lastErr)
}
