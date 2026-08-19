package upstream_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// Misclassifying a failure is costly in both directions: treating a permanent
// authentication failure as retryable hammers the provider until it locks the
// account, while treating a transient network blip as fatal stops an account
// syncing until someone notices.
func TestSeverityClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want upstream.Severity
	}{
		{
			name: "bad credentials are fatal for the account",
			err:  &imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "invalid credentials"},
			want: upstream.FatalAccount,
		},
		{
			name: "expired credentials are fatal for the account",
			err:  &imap.Error{Code: imap.ResponseCodeExpired, Text: "password expired"},
			want: upstream.FatalAccount,
		},
		{
			name: "a missing mailbox is fatal only for that mailbox",
			err:  &imap.Error{Code: imap.ResponseCodeNonExistent, Text: "no such mailbox"},
			want: upstream.FatalMailbox,
		},
		{
			name: "server unavailable is retryable",
			err:  &imap.Error{Code: imap.ResponseCodeUnavailable, Text: "try again later"},
			want: upstream.Retryable,
		},
		{
			name: "a server bug is retryable",
			err:  &imap.Error{Code: imap.ResponseCodeServerBug, Text: "internal error"},
			want: upstream.Retryable,
		},
		{
			name: "prose about a password is fatal even without a response code",
			err:  errors.New("NO application-specific password required"),
			want: upstream.FatalAccount,
		},
		{
			name: "an unrecognised failure defaults to retryable",
			err:  errors.New("something entirely unexpected"),
			want: upstream.Retryable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Severity is attached by the client wrapper, so exercise it the way
			// production code does: through a Retry that surfaces the verdict.
			err := upstream.Retry(context.Background(), 1, time.Millisecond, time.Millisecond,
				func(context.Context) error { return tc.err })
			if got := upstream.SeverityOf(err); got != tc.want {
				t.Errorf("severity = %v, want %v (error: %v)", got, tc.want, err)
			}
		})
	}
}

// Retrying a permanent failure cannot help and risks a provider lockout, so it
// must stop on the first attempt.
func TestRetryStopsOnFatalFailures(t *testing.T) {
	attempts := 0
	err := upstream.Retry(context.Background(), 5, time.Millisecond, time.Millisecond,
		func(context.Context) error {
			attempts++
			return &imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "nope"}
		})

	if attempts != 1 {
		t.Errorf("made %d attempts against a fatal failure, want 1", attempts)
	}
	if upstream.SeverityOf(err) != upstream.FatalAccount {
		t.Errorf("severity = %v, want FatalAccount", upstream.SeverityOf(err))
	}
}

func TestRetryRetriesTransientFailures(t *testing.T) {
	attempts := 0
	err := upstream.Retry(context.Background(), 5, time.Millisecond, 2*time.Millisecond,
		func(context.Context) error {
			attempts++
			if attempts < 3 {
				return errors.New("temporary network glitch")
			}
			return nil
		})

	if err != nil {
		t.Errorf("expected eventual success, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("made %d attempts, want 3", attempts)
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	attempts := 0
	err := upstream.Retry(context.Background(), 3, time.Millisecond, time.Millisecond,
		func(context.Context) error {
			attempts++
			return errors.New("still down")
		})

	if attempts != 3 {
		t.Errorf("made %d attempts, want 3", attempts)
	}
	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}
}

// A cancelled context must stop the retry loop promptly rather than sleeping
// out the remaining backoff.
func TestRetryHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := upstream.Retry(ctx, 10, time.Second, 10*time.Second,
		func(context.Context) error { return errors.New("down") })
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("retry loop took %s to notice cancellation", elapsed)
	}
}

// Full jitter spreads reconnection attempts out, so a server coming back after
// an outage is not immediately knocked over by every worker retrying in unison.
func TestBackoffGrowsAndStaysBounded(t *testing.T) {
	const (
		base = 100 * time.Millisecond
		max  = 2 * time.Second
	)

	for attempt := 1; attempt <= 10; attempt++ {
		for range 50 {
			delay := upstream.Backoff(attempt, base, max)
			if delay <= 0 {
				t.Fatalf("attempt %d produced a non-positive delay %s", attempt, delay)
			}
			if delay > max {
				t.Fatalf("attempt %d produced %s, exceeding the %s ceiling", attempt, delay, max)
			}
		}
	}

	// Later attempts should be able to reach delays earlier ones cannot.
	var maxEarly, maxLate time.Duration
	for range 200 {
		if d := upstream.Backoff(1, base, max); d > maxEarly {
			maxEarly = d
		}
		if d := upstream.Backoff(6, base, max); d > maxLate {
			maxLate = d
		}
	}
	if maxLate <= maxEarly {
		t.Errorf("backoff does not grow: attempt 1 reached %s, attempt 6 reached %s",
			maxEarly, maxLate)
	}
}
