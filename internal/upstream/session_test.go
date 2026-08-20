package upstream_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/testutil/fakeimap"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

func sessionFor(t *testing.T, srv *fakeimap.Server, cfg config.Upstream) *upstream.Session {
	t.Helper()
	connector := upstream.NewConnector(cfg, logging.Discard())
	session := connector.NewSession(upstream.Account{
		Host: srv.Host(), Port: srv.Port(), TLSMode: upstream.TLSModePlain,
		Username: srv.Username(), Password: srv.Password(),
	})
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// A session must survive losing its connection, reconnect, and restore the
// mailbox it had open — without the caller knowing anything happened.
func TestSessionReconnectsAndRestoresTheMailbox(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(20)}},
		// Drop each connection after a handful of commands, so a session that
		// cannot reconnect makes no progress at all.
		Chaos: fakeimap.Chaos{DropAfter: 5},
	})

	cfg := testConfig()
	cfg.RetryMaxAttempts = 6
	cfg.RetryBaseDelay = config.Duration(10 * time.Millisecond)
	cfg.RetryMaxDelay = config.Duration(50 * time.Millisecond)

	session := sessionFor(t, srv, cfg)
	ctx := context.Background()

	if _, err := session.Select(ctx, "INBOX", true); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	// Enough commands that the connection is certainly dropped partway.
	succeeded := 0
	for range 8 {
		err := session.Do(ctx, "fetch", func(ctx context.Context, client *upstream.Client) error {
			_, err := client.FetchMetadata(ctx, allUIDs(), 0, false)
			return err
		})
		if err != nil {
			t.Fatalf("command failed despite the session being able to reconnect: %v", err)
		}
		succeeded++
	}

	if succeeded != 8 {
		t.Errorf("%d of 8 commands succeeded", succeeded)
	}

	// More than one login proves a reconnection happened rather than the
	// connection simply surviving.
	if logins := srv.Recorder().Count("LOGIN"); logins < 2 {
		t.Errorf("only %d LOGIN commands were issued, so no reconnection occurred", logins)
	} else {
		t.Logf("session reconnected: %d logins across %d commands", logins, succeeded)
	}

	// And the mailbox must have been reopened each time, or the fetches would
	// have failed for want of a selected mailbox.
	if selects := srv.Recorder().Count("EXAMINE") + srv.Recorder().Count("SELECT"); selects < 2 {
		t.Errorf("the mailbox was opened %d times; a reconnection should reopen it", selects)
	}
}

// A wrong password must not be retried: repeating it risks the provider
// locking the account, and no number of attempts will make it work.
func TestSessionDoesNotRetryAuthenticationFailure(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX"}},
	})

	cfg := testConfig()
	cfg.RetryMaxAttempts = 5
	cfg.RetryBaseDelay = config.Duration(10 * time.Millisecond)

	connector := upstream.NewConnector(cfg, logging.Discard())
	session := connector.NewSession(upstream.Account{
		Host: srv.Host(), Port: srv.Port(), TLSMode: upstream.TLSModePlain,
		Username: srv.Username(), Password: "definitely not the password",
	})
	defer session.Close()

	err := session.Do(context.Background(), "select", func(ctx context.Context, client *upstream.Client) error {
		_, err := client.Select(ctx, "INBOX", true)
		return err
	})
	if err == nil {
		t.Fatal("expected authentication to fail")
	}

	if logins := srv.Recorder().Count("LOGIN"); logins != 1 {
		t.Errorf("%d LOGIN attempts were made; a rejected password must be tried once", logins)
	}
}

// A cancelled context must stop immediately rather than working through the
// retry budget.
func TestSessionStopsOnCancellation(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(5)}},
	})

	cfg := testConfig()
	cfg.RetryMaxAttempts = 10
	cfg.RetryBaseDelay = config.Duration(2 * time.Second)
	cfg.RetryMaxDelay = config.Duration(10 * time.Second)

	session := sessionFor(t, srv, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := session.Do(ctx, "fetch", func(context.Context, *upstream.Client) error {
		return errors.New("should not be reached")
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if elapsed > time.Second {
		t.Errorf("took %s to notice cancellation", elapsed)
	}
}

// A mailbox that does not exist is not a connection problem, so reconnecting
// cannot help and must not be attempted.
func TestSessionDoesNotRetryAMissingMailbox(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX"}},
	})

	cfg := testConfig()
	cfg.RetryMaxAttempts = 5
	cfg.RetryBaseDelay = config.Duration(10 * time.Millisecond)

	session := sessionFor(t, srv, cfg)

	_, err := session.Select(context.Background(), "No Such Folder", true)
	if err == nil {
		t.Fatal("expected selecting a missing mailbox to fail")
	}
	if logins := srv.Recorder().Count("LOGIN"); logins > 1 {
		t.Errorf("%d logins for a missing mailbox; reconnecting cannot conjure one", logins)
	}
}

// Bodies must keep downloading across a reconnection.
func TestSessionFetchesBodiesAcrossAReconnection(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(10)}},
		Chaos:     fakeimap.Chaos{DropAfter: 6},
	})

	cfg := testConfig()
	cfg.RetryMaxAttempts = 6
	cfg.RetryBaseDelay = config.Duration(10 * time.Millisecond)
	cfg.RetryMaxDelay = config.Duration(50 * time.Millisecond)

	session := sessionFor(t, srv, cfg)
	ctx := context.Background()

	if _, err := session.Select(ctx, "INBOX", true); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	seen := map[imap.UID]bool{}
	for round := range 4 {
		err := session.Do(ctx, "fetch bodies", func(ctx context.Context, client *upstream.Client) error {
			return client.FetchBodies(ctx, allUIDs(), 0, func(uid imap.UID, _ int64, body io.Reader) error {
				data, err := io.ReadAll(body)
				if err != nil {
					return err
				}
				if strings.Contains(string(data), "Subject:") {
					seen[uid] = true
				}
				return nil
			})
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	if len(seen) != 10 {
		t.Errorf("received bodies for %d messages, want 10", len(seen))
	}
}
