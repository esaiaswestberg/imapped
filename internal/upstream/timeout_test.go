package upstream_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/testutil/fakeimap"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// These are the regression tests for the defect that made this rewrite
// necessary: a sync that ran for two days at zero CPU with no error, because a
// read on a silently-dropped connection had no deadline and blocked forever.
//
// Every one of them asserts a bound on elapsed time. A test that merely checks
// "an error is returned" would pass even if the operation took an hour, which
// is the failure mode being guarded against.

// mustFinishWithin runs fn and fails if it has not returned by limit.
//
// The goroutine is deliberately abandoned on timeout rather than waited for: if
// the code under test really has hung, waiting would hang the test process too,
// turning a clear failure into a CI timeout with no diagnosis.
func mustFinishWithin(t *testing.T, limit time.Duration, what string, fn func() error) error {
	t.Helper()

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		err := fn()
		done <- result{err: err, elapsed: time.Since(start)}
	}()

	select {
	case r := <-done:
		t.Logf("%s returned after %s: %v", what, r.elapsed.Round(time.Millisecond), r.err)
		return r.err
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s — it is hung, which is the exact "+
			"defect this test guards against", what, limit)
		return nil
	}
}

// A server that accepts a command and then never responds must not hang us.
// The connection stays open and TCP-healthy; only the application-level
// inactivity deadline can break the deadlock.
func TestHungServerDoesNotBlockForever(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(5)}},
		// Go silent as soon as the client speaks, i.e. immediately after LOGIN
		// is sent but before any response comes back.
		Chaos: fakeimap.Chaos{HangAfter: 1},
	})

	cfg := testConfig()
	cfg.IOIdleTimeout = config.Duration(1500 * time.Millisecond)
	cfg.CommandTimeout = config.Duration(1500 * time.Millisecond)
	cfg.GreetingTimeout = config.Duration(1500 * time.Millisecond)

	connector := upstream.NewConnector(cfg, logging.Discard())

	err := mustFinishWithin(t, 15*time.Second, "Connect against a hung server", func() error {
		client, err := connector.Connect(context.Background(), upstream.Account{
			Host: srv.Host(), Port: srv.Port(), TLSMode: upstream.TLSModePlain,
			Username: srv.Username(), Password: srv.Password(),
		})
		if client != nil {
			_ = client.Close()
		}
		return err
	})

	if err == nil {
		t.Fatal("expected an error from a server that stopped responding")
	}
}

// A server that goes silent mid-FETCH is the shape of the original failure:
// the sync had already connected and selected a mailbox before it stalled.
func TestHungFetchIsBoundedByIdleTimeout(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(50)}},
	})

	cfg := testConfig()
	cfg.IOIdleTimeout = config.Duration(1 * time.Second)
	cfg.FetchMetadataTimeout = config.Duration(2 * time.Second)

	client := connect(t, srv, cfg)
	ctx := context.Background()
	if _, err := client.Select(ctx, "INBOX", true); err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}

	// Sever the network underneath an established session.
	srv.Close()

	err := mustFinishWithin(t, 15*time.Second, "FetchMetadata after the server vanished", func() error {
		_, err := client.FetchMetadata(ctx, allUIDs(), 0, true)
		return err
	})
	if err == nil {
		t.Fatal("expected an error once the server was gone")
	}
}

// Cancelling the context must actually unblock a parked read. go-imap's
// commands do not take a context, so this only works because the client closes
// the underlying connection when the context fires.
func TestContextCancellationUnblocksAFetch(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(20)}},
		Chaos:     fakeimap.Chaos{SlowBytes: 32}, // 32 B/s: far slower than the test's patience
	})

	cfg := testConfig()
	cfg.IOIdleTimeout = config.Duration(30 * time.Second) // deliberately generous
	cfg.FetchMetadataTimeout = config.Duration(30 * time.Second)
	cfg.GreetingTimeout = config.Duration(10 * time.Second)
	cfg.CommandTimeout = config.Duration(10 * time.Second)

	connector := upstream.NewConnector(cfg, logging.Discard())
	client, err := connector.Connect(context.Background(), upstream.Account{
		Host: srv.Host(), Port: srv.Port(), TLSMode: upstream.TLSModePlain,
		Username: srv.Username(), Password: srv.Password(),
	})
	if err != nil {
		t.Skipf("could not connect through the throttled transport: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = mustFinishWithin(t, 15*time.Second, "FetchMetadata with a 1s context", func() error {
		_, err := client.FetchMetadata(ctx, allUIDs(), 0, true)
		return err
	})
	if err == nil {
		t.Fatal("expected the cancelled context to abort the fetch")
	}
	if !client.Poisoned() {
		t.Error("a connection abandoned at a deadline must be marked poisoned so " +
			"it is never reused with unread response data still in flight")
	}
}

// A connection severed mid-response must surface as an error promptly.
func TestDroppedConnectionIsReportedPromptly(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(5)}},
		Chaos:     fakeimap.Chaos{DropAfter: 1},
	})

	cfg := testConfig()
	connector := upstream.NewConnector(cfg, logging.Discard())

	err := mustFinishWithin(t, 15*time.Second, "Connect against a server that drops", func() error {
		client, err := connector.Connect(context.Background(), upstream.Account{
			Host: srv.Host(), Port: srv.Port(), TLSMode: upstream.TLSModePlain,
			Username: srv.Username(), Password: srv.Password(),
		})
		if err != nil {
			return err
		}
		defer client.Close()
		_, err = client.Select(context.Background(), "INBOX", true)
		return err
	})
	if err == nil {
		t.Fatal("expected an error from a connection that was dropped")
	}
}

// Dialling a port that nothing is listening on must fail within the dial
// timeout rather than the operating system's much longer default.
func TestDialTimeoutIsRespected(t *testing.T) {
	// Bind and immediately close, so the port is almost certainly unused.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	listener.Close()

	cfg := testConfig()
	cfg.DialTimeout = config.Duration(500 * time.Millisecond)
	connector := upstream.NewConnector(cfg, logging.Discard())

	err = mustFinishWithin(t, 10*time.Second, "Connect to a closed port", func() error {
		_, err := connector.Connect(context.Background(), upstream.Account{
			Host: "127.0.0.1", Port: addr.Port, TLSMode: upstream.TLSModePlain,
			Username: "u", Password: "p",
		})
		return err
	})
	if err == nil {
		t.Fatal("expected connecting to a closed port to fail")
	}
}

// A body fetch against a vanished server must not hang either; this is the same
// property as the metadata case but on the path that moves the most bytes.
func TestBodyFetchAgainstDeadServerTerminates(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(30)}},
	})

	cfg := testConfig()
	cfg.IOIdleTimeout = config.Duration(1 * time.Second)
	cfg.FetchBodyTimeout = config.Duration(2 * time.Second)

	client := connect(t, srv, cfg)
	ctx := context.Background()
	if _, err := client.Select(ctx, "INBOX", true); err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}

	srv.Close()

	err := mustFinishWithin(t, 15*time.Second, "FetchBodies after the server vanished", func() error {
		return client.FetchBodies(ctx, allUIDs(), 0, func(imap.UID, int64, io.Reader) error {
			return nil
		})
	})
	if err == nil {
		t.Fatal("expected an error once the server was gone")
	}
}
