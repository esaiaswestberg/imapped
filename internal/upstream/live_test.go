//go:build live

// Live tests run against a real IMAP account.
//
// They are strictly read-only: mailboxes are selected with readOnly set and no
// command that mutates state is ever issued. They live in this package rather
// than internal/syncer so that no live test can reach the mutation replay path
// and write to someone's real mailbox — that is a structural guarantee, not a
// convention.
//
// Run with: make test-live
package upstream_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// credentialsFile is read from the repository root and is gitignored.
const credentialsFile = ".testing-credentials"

// liveAccount loads credentials, skipping the test when they are absent.
//
// The password is never logged, printed or included in a failure message.
func liveAccount(t *testing.T) upstream.Account {
	t.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", credentialsFile)

	file, err := os.Open(path)
	if err != nil {
		t.Skipf("no %s present; live tests skipped", credentialsFile)
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.ToUpper(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading %s: %v", credentialsFile, err)
	}

	// HOST may carry the port, as it does in the form a provider hands out.
	host, port := values["HOST"], 993
	if h, p, ok := strings.Cut(host, ":"); ok {
		if parsed, err := strconv.Atoi(p); err == nil {
			host, port = h, parsed
		}
	}

	account := upstream.Account{
		Host:     host,
		Port:     port,
		TLSMode:  upstream.TLSModeTLS,
		Username: values["USERNAME"],
		Password: values["PASSWORD"],
	}
	if explicit := values["PORT"]; explicit != "" {
		if parsed, err := strconv.Atoi(explicit); err == nil {
			account.Port = parsed
		}
	}
	if mode := values["TLS_MODE"]; mode != "" {
		account.TLSMode = upstream.TLSMode(mode)
	}

	if account.Host == "" || account.Username == "" || account.Password == "" {
		t.Skipf("%s is missing HOST, USERNAME or PASSWORD", credentialsFile)
	}
	return account
}

func liveConfig() config.Upstream {
	cfg := config.Default().Upstream
	// Generous, because the point of these tests is to measure how long real
	// operations actually take rather than to enforce a limit.
	cfg.CommandTimeout = config.Duration(2 * time.Minute)
	cfg.FetchMetadataTimeout = config.Duration(15 * time.Minute)
	cfg.FetchBodyTimeout = config.Duration(15 * time.Minute)
	cfg.IOIdleTimeout = config.Duration(2 * time.Minute)
	return cfg
}

func liveConnect(t *testing.T) *upstream.Client {
	t.Helper()

	account := liveAccount(t)
	connector := upstream.NewConnector(liveConfig(), logging.Discard())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := connector.Connect(ctx, account)
	if err != nil {
		// Report the host but never the credentials.
		t.Fatalf("connecting to %s:%d as %s: %v",
			account.Host, account.Port, maskUser(account.Username), err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// maskUser keeps enough of an address to identify it without publishing it.
func maskUser(username string) string {
	local, domain, ok := strings.Cut(username, "@")
	if !ok || len(local) == 0 {
		return "***"
	}
	return local[:1] + "***@" + domain
}

// TestLiveCapabilities records what the server actually supports, which decides
// which sync paths are available.
func TestLiveCapabilities(t *testing.T) {
	client := liveConnect(t)

	var names []string
	for capability := range client.Caps() {
		names = append(names, string(capability))
	}
	t.Logf("server capabilities: %s", strings.Join(names, " "))

	for _, want := range []imap.Cap{imap.CapCondStore, imap.CapQResync, imap.CapMove, imap.CapESearch} {
		t.Logf("  %-12s %v", want, client.Caps().Has(want))
	}
}

// TestLiveInboxShape reports the size of the mailbox under test.
func TestLiveInboxShape(t *testing.T) {
	client := liveConnect(t)
	ctx := context.Background()

	boxes, err := client.ListMailboxes(ctx)
	if err != nil {
		t.Fatalf("listing mailboxes: %v", err)
	}
	t.Logf("%d mailboxes", len(boxes))
	for _, box := range boxes {
		t.Logf("  %s", box.Name)
	}

	selected, err := client.Select(ctx, "INBOX", true)
	if err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}
	t.Logf("INBOX: %d messages, uidnext %d, uidvalidity %d, highestmodseq %d",
		selected.NumMessages, selected.UIDNext, selected.UIDValidity, selected.HighestModSeq)
}

// TestLiveBodyStructureCostDelta measures the hypothesis behind this fix: that
// asking for BODYSTRUCTURE forces the server to open and parse every message,
// while the same window without it is answered from the index cache.
func TestLiveBodyStructureCostDelta(t *testing.T) {
	client := liveConnect(t)
	ctx := context.Background()

	selected, err := client.Select(ctx, "INBOX", true)
	if err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}
	if selected.NumMessages == 0 {
		t.Skip("INBOX is empty")
	}

	const window = 200
	upper := selected.UIDNext - 1
	lower := imap.UID(1)
	if upper > window {
		lower = upper - window + 1
	}

	var set imap.UIDSet
	set.AddRange(lower, upper)
	t.Logf("measuring UID window %d:%d", lower, upper)

	// Without BODYSTRUCTURE: the field set this fix moves to.
	start := time.Now()
	lean, err := client.FetchMetadataFields(ctx, set, 0, upstream.MetadataFieldsLean)
	leanElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("lean metadata fetch: %v", err)
	}

	// With BODYSTRUCTURE: what the code does today.
	start = time.Now()
	full, err := client.FetchMetadataFields(ctx, set, 0, upstream.MetadataFieldsWithBodyStructure)
	fullElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("metadata fetch including BODYSTRUCTURE: %v", err)
	}

	t.Logf("without BODYSTRUCTURE: %d messages in %s", len(lean), leanElapsed.Round(time.Millisecond))
	t.Logf("with    BODYSTRUCTURE: %d messages in %s", len(full), fullElapsed.Round(time.Millisecond))

	if leanElapsed > 0 {
		t.Logf("BODYSTRUCTURE costs %.1fx", float64(fullElapsed)/float64(leanElapsed))
	}

	// Extrapolate to the real mailbox, which is what actually timed out.
	perMessage := fullElapsed / time.Duration(max(len(full), 1))
	t.Logf("extrapolated to %d messages with BODYSTRUCTURE: %s",
		selected.NumMessages, (perMessage * time.Duration(selected.NumMessages)).Round(time.Second))

	perMessageLean := leanElapsed / time.Duration(max(len(lean), 1))
	t.Logf("extrapolated to %d messages without:            %s",
		selected.NumMessages, (perMessageLean * time.Duration(selected.NumMessages)).Round(time.Second))
}

// TestLiveChunkSizeSweep measures how response time scales with chunk size, so
// the default is chosen from measurement rather than guesswork.
func TestLiveChunkSizeSweep(t *testing.T) {
	client := liveConnect(t)
	ctx := context.Background()

	selected, err := client.Select(ctx, "INBOX", true)
	if err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}
	if selected.NumMessages == 0 {
		t.Skip("INBOX is empty")
	}
	upper := selected.UIDNext - 1

	for _, size := range []imap.UID{50, 100, 200, 400, 800} {
		lower := imap.UID(1)
		if upper > size {
			lower = upper - size + 1
		}
		var set imap.UIDSet
		set.AddRange(lower, upper)

		start := time.Now()
		metas, err := client.FetchMetadataFields(ctx, set, 0, upstream.MetadataFieldsLean)
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("chunk of %d: %v", size, err)
			continue
		}
		perMessage := elapsed / time.Duration(max(len(metas), 1))
		t.Logf("chunk %4d: %d messages in %-10s (%s per message)",
			size, len(metas), elapsed.Round(time.Millisecond), perMessage.Round(time.Microsecond))
	}
}

// TestLiveFullInboxMetadata streams the entire INBOX with the production field
// set. This is the operation that failed in production; it must complete, and
// its wall time is what validates the stall default.
func TestLiveFullInboxMetadata(t *testing.T) {
	if os.Getenv("IMAPPED_LIVE_FULL") == "" {
		t.Skip("set IMAPPED_LIVE_FULL=1 to stream the whole mailbox")
	}

	client := liveConnect(t)
	ctx := context.Background()

	selected, err := client.Select(ctx, "INBOX", true)
	if err != nil {
		t.Fatalf("selecting INBOX: %v", err)
	}

	var set imap.UIDSet
	set.AddRange(1, 0)

	start := time.Now()
	metas, err := client.FetchMetadataFields(ctx, set, 0, upstream.MetadataFieldsLean)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("streaming the whole INBOX after %s: %v", elapsed.Round(time.Second), err)
	}

	t.Logf("streamed %d of %d messages in %s",
		len(metas), selected.NumMessages, elapsed.Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "full INBOX metadata: %d messages in %s\n",
		len(metas), elapsed.Round(time.Millisecond))
}
