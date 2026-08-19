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

// testConfig returns upstream settings with short timeouts, so a test that
// exercises a timeout finishes in seconds rather than the production minute.
func testConfig() config.Upstream {
	cfg := config.Default().Upstream
	cfg.DialTimeout = config.Duration(2 * time.Second)
	cfg.GreetingTimeout = config.Duration(2 * time.Second)
	cfg.IOIdleTimeout = config.Duration(2 * time.Second)
	cfg.CommandTimeout = config.Duration(2 * time.Second)
	cfg.FetchMetadataTimeout = config.Duration(3 * time.Second)
	cfg.FetchBodyTimeout = config.Duration(3 * time.Second)
	return cfg
}

func connect(t *testing.T, srv *fakeimap.Server, cfg config.Upstream) *upstream.Client {
	t.Helper()
	connector := upstream.NewConnector(cfg, logging.Discard())
	client, err := connector.Connect(context.Background(), upstream.Account{
		Host:     srv.Host(),
		Port:     srv.Port(),
		TLSMode:  upstream.TLSModePlain,
		Username: srv.Username(),
		Password: srv.Password(),
	})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestConnectAndListMailboxes(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{
			{Name: "INBOX", Messages: fakeimap.Seed(3)},
			{Name: "Archive"},
		},
	})
	client := connect(t, srv, testConfig())

	boxes, err := client.ListMailboxes(context.Background())
	if err != nil {
		t.Fatalf("listing mailboxes: %v", err)
	}

	names := make(map[string]bool)
	for _, b := range boxes {
		names[b.Name] = true
	}
	for _, want := range []string{"INBOX", "Archive"} {
		if !names[want] {
			t.Errorf("mailbox %q missing from %v", want, names)
		}
	}
}

func TestSelectReportsMailboxState(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(5)}},
	})
	client := connect(t, srv, testConfig())

	sel, err := client.Select(context.Background(), "INBOX", true)
	if err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}
	if sel.NumMessages != 5 {
		t.Errorf("NumMessages = %d, want 5", sel.NumMessages)
	}
	if sel.UIDValidity == 0 {
		t.Error("UIDValidity should be non-zero")
	}
}

// This is the regression test for the performance defect that motivated the
// rewrite. The previous implementation issued one FETCH per message on every
// sync; here the entire mailbox is enumerated in a single command.
func TestMetadataForWholeMailboxIsOneCommand(t *testing.T) {
	const messageCount = 500

	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(messageCount)}},
	})
	client := connect(t, srv, testConfig())
	ctx := context.Background()

	if _, err := client.Select(ctx, "INBOX", true); err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}

	srv.Recorder().Reset()

	metas, err := client.FetchMetadata(ctx, allUIDs(), 0, true)
	if err != nil {
		t.Fatalf("fetching metadata: %v", err)
	}

	if len(metas) != messageCount {
		t.Errorf("got metadata for %d messages, want %d", len(metas), messageCount)
	}

	fetches := srv.Recorder().Count("FETCH")
	if fetches > 2 {
		t.Errorf("enumerating %d messages took %d FETCH commands, want at most 2.\n"+
			"A per-message fetch loop is the defect this test exists to catch.\ncommands:\n%s",
			messageCount, fetches, srv.Recorder())
	}
	t.Logf("%d messages enumerated in %d FETCH command(s)", len(metas), fetches)

	// Metadata must be complete enough to build a database row without a
	// second round trip.
	for _, m := range metas[:min(3, len(metas))] {
		if m.UID == 0 {
			t.Error("metadata is missing the UID")
		}
		if m.Size == 0 {
			t.Error("metadata is missing RFC822.SIZE")
		}
		if m.Envelope == nil {
			t.Error("metadata is missing the envelope")
		}
	}
}

// allUIDs is the open-ended range 1:*, which asks for every message in the
// selected mailbox in a single command.
func allUIDs() imap.NumSet {
	set := imap.UIDSet{}
	set.AddRange(1, 0)
	return set
}

func TestFetchBodiesStreamsContent(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(10)}},
	})
	client := connect(t, srv, testConfig())
	ctx := context.Background()

	if _, err := client.Select(ctx, "INBOX", true); err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}

	srv.Recorder().Reset()

	bodies := make(map[imap.UID]int)
	err := client.FetchBodies(ctx, allUIDs(), 0, func(uid imap.UID, size int64, body io.Reader) error {
		data, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		bodies[uid] = len(data)
		if !strings.Contains(string(data), "Subject:") {
			t.Errorf("body for UID %d does not look like a message", uid)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fetching bodies: %v", err)
	}

	if len(bodies) != 10 {
		t.Errorf("received %d bodies, want 10", len(bodies))
	}
	// Ten bodies in a single batched command, not ten commands.
	if fetches := srv.Recorder().Count("FETCH"); fetches != 1 {
		t.Errorf("fetching 10 bodies took %d commands, want 1:\n%s", fetches, srv.Recorder())
	}
}

// Oversized messages must be skipped without being buffered, and without
// aborting the rest of the batch.
func TestFetchBodiesRejectsOversizedMessages(t *testing.T) {
	srv := fakeimap.Start(t, fakeimap.Options{
		Mailboxes: []fakeimap.Mailbox{{Name: "INBOX", Messages: fakeimap.Seed(1)}},
	})
	client := connect(t, srv, testConfig())
	ctx := context.Background()

	if _, err := client.Select(ctx, "INBOX", true); err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}

	err := client.FetchBodies(ctx, allUIDs(), 10, func(imap.UID, int64, io.Reader) error {
		t.Error("handler should not be called for an oversized message")
		return nil
	})
	if !errors.Is(err, upstream.ErrMessageTooLarge) {
		t.Errorf("got %v, want ErrMessageTooLarge", err)
	}
}
