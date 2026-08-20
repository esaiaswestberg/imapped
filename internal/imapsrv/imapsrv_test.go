//go:build integration

package imapsrv_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/imapsrv"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/testutil/pgtest"
)

const (
	testUser = "reader@example.com"
	testPass = "a password long enough for a test"
)

type harness struct {
	addr      string
	store     *store.Store
	mailboxID int64
}

// mailboxID is exposed so a test can seed rows the normal path would not
// produce, such as a message recorded but not yet downloaded.

// newHarness stands up the IMAP server over a seeded database.
//
// The client driving it is go-imap's own, which means these are genuine
// protocol round trips rather than assertions about internal state.
func newHarness(t *testing.T, messages int) *harness {
	t.Helper()
	ctx := context.Background()

	pool := pgtest.New(t)
	st := store.New(pool)
	blobs := blob.NewMemStore()

	hash, err := crypto.HashPassword(testPass)
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}
	user, err := st.CreateUser(ctx, testUser, hash, true)
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	account, err := st.CreateAccount(ctx, store.CreateAccountParams{
		UserID: user.ID, EmailAddress: testUser,
		UpstreamHost: "imap.example.com", UpstreamPort: 993, UpstreamTLSMode: "tls",
		EncryptedUsername: []byte{1}, EncryptedSecret: []byte{2},
	})
	if err != nil {
		t.Fatalf("creating account: %v", err)
	}

	mailbox, err := st.UpsertMailbox(ctx, store.UpsertMailboxParams{
		AccountID: account.ID, Name: "INBOX", CanonicalName: "inbox",
	})
	if err != nil {
		t.Fatalf("creating mailbox: %v", err)
	}

	for i := range messages {
		raw := []byte("From: sender" + itoa(i) + "@example.com\r\n" +
			"To: " + testUser + "\r\n" +
			"Subject: Message " + itoa(i) + "\r\n" +
			"Message-ID: <msg" + itoa(i) + "@example.com>\r\n" +
			"\r\n" +
			"Body of message " + itoa(i) + ".\r\n")

		info, err := blobs.Put(ctx, blob.TypeRFC822, strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("storing blob: %v", err)
		}

		var messageID int64
		// Alternate seen and unseen, so flag-dependent assertions have both.
		flags := []string{}
		if i%2 == 0 {
			flags = []string{`\Seen`}
		}
		err = pool.QueryRow(ctx,
			`INSERT INTO messages (account_id, rfc822_sha256, rfc822_size, blob_key,
			   subject, body_text, preview, addrs, internal_date)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now()) RETURNING id`,
			account.ID, info.SHA256, info.Size, info.Key.String(),
			"Message "+itoa(i), "Body of message "+itoa(i),
			"Body of message "+itoa(i),
			`{"from":["sender`+itoa(i)+`@example.com"]}`).Scan(&messageID)
		if err != nil {
			t.Fatalf("inserting message: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO mailbox_messages (mailbox_id, message_id, local_uid, upstream_uid,
			   body_state, flags)
			 VALUES ($1,$2,$3,$4,'stored',$5)`,
			mailbox.ID, messageID, i+1, i+1, flags); err != nil {
			t.Fatalf("linking message: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE mailboxes SET local_uidnext = $2 WHERE id = $1`,
		mailbox.ID, messages+1); err != nil {
		t.Fatalf("updating uidnext: %v", err)
	}
	if err := st.RefreshMailboxCounts(ctx, mailbox.ID); err != nil {
		t.Fatalf("refreshing counts: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	cfg := config.Default()
	backend := imapsrv.NewBackend(cfg, st, blobs, logging.Discard())
	server := imapsrv.NewServer("imap", addr, backend, nil, logging.Discard())

	serverCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = server.Serve(serverCtx) }()

	// Wait for the listener to come up.
	for range 100 {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	return &harness{addr: addr, store: st, mailboxID: mailbox.ID}
}

func (h *harness) connect(t *testing.T) *imapclient.Client {
	t.Helper()
	client, err := imapclient.DialInsecure(h.addr, nil)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func (h *harness) login(t *testing.T) *imapclient.Client {
	t.Helper()
	client := h.connect(t)
	if err := client.Login(testUser, testPass).Wait(); err != nil {
		t.Fatalf("signing in: %v", err)
	}
	return client
}

func TestLogin(t *testing.T) {
	h := newHarness(t, 3)

	client := h.connect(t)
	if err := client.Login(testUser, "the wrong password").Wait(); err == nil {
		t.Error("a wrong password was accepted")
	}
	if err := client.Login(testUser, testPass).Wait(); err != nil {
		t.Errorf("the correct password was rejected: %v", err)
	}
}

func TestListMailboxes(t *testing.T) {
	h := newHarness(t, 3)
	client := h.login(t)

	boxes, err := client.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(boxes) != 1 || boxes[0].Mailbox != "INBOX" {
		t.Errorf("listed %+v, want a single INBOX", boxes)
	}
}

func TestSelectReportsCounts(t *testing.T) {
	h := newHarness(t, 10)
	client := h.login(t)

	data, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("selecting: %v", err)
	}
	if data.NumMessages != 10 {
		t.Errorf("NumMessages = %d, want 10", data.NumMessages)
	}
	if data.UIDValidity == 0 {
		t.Error("UIDVALIDITY must not be zero")
	}
	if data.UIDNext != 11 {
		t.Errorf("UIDNEXT = %d, want 11", data.UIDNext)
	}
}

// The essential read path: a client fetches envelopes to build a message list.
func TestFetchEnvelopes(t *testing.T) {
	h := newHarness(t, 5)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	var set imap.SeqSet
	set.AddRange(1, 0)
	messages, err := client.Fetch(set, &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, RFC822Size: true,
	}).Collect()
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}

	if len(messages) != 5 {
		t.Fatalf("fetched %d messages, want 5", len(messages))
	}
	for i, msg := range messages {
		if msg.UID != imap.UID(i+1) {
			t.Errorf("message %d has UID %d, want %d", i, msg.UID, i+1)
		}
		if msg.Envelope == nil || msg.Envelope.Subject == "" {
			t.Errorf("message %d has no subject", i)
			continue
		}
		if !strings.HasPrefix(msg.Envelope.Subject, "Message ") {
			t.Errorf("message %d subject = %q", i, msg.Envelope.Subject)
		}
		if len(msg.Envelope.From) == 0 {
			t.Errorf("message %d has no sender", i)
		}
		if msg.RFC822Size == 0 {
			t.Errorf("message %d has no size", i)
		}
	}
}

// Reading a message: the client asks for the full body and must get the exact
// bytes that were stored.
func TestFetchBody(t *testing.T) {
	h := newHarness(t, 3)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	var set imap.SeqSet
	set.AddNum(1)
	messages, err := client.Fetch(set, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(messages))
	}

	var body []byte
	for _, contents := range messages[0].BodySection {
		body = contents.Bytes
	}
	if !strings.Contains(string(body), "Subject: Message 0") {
		t.Errorf("body does not contain the expected headers: %q", body)
	}
	if !strings.Contains(string(body), "Body of message 0") {
		t.Errorf("body does not contain the expected text: %q", body)
	}
}

// A header-only fetch must not return the whole message; clients rely on this
// to build lists cheaply.
func TestFetchHeadersOnly(t *testing.T) {
	h := newHarness(t, 2)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	var set imap.SeqSet
	set.AddNum(1)
	messages, err := client.Fetch(set, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierHeader}},
	}).Collect()
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}

	var headers []byte
	for _, contents := range messages[0].BodySection {
		headers = contents.Bytes
	}
	if !strings.Contains(string(headers), "Subject:") {
		t.Errorf("headers missing: %q", headers)
	}
	if strings.Contains(string(headers), "Body of message") {
		t.Error("a header-only fetch returned the message body as well")
	}
}

func TestUIDFetch(t *testing.T) {
	h := newHarness(t, 5)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	var set imap.UIDSet
	set.AddNum(3)
	messages, err := client.Fetch(set, &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil {
		t.Fatalf("fetching by UID: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(messages))
	}
	if messages[0].UID != 3 {
		t.Errorf("fetched UID %d, want 3", messages[0].UID)
	}
}

// Marking a message read is the most common thing a client does.
func TestStoreFlags(t *testing.T) {
	h := newHarness(t, 3)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	// Message 2 starts unseen (odd index).
	var set imap.SeqSet
	set.AddNum(2)
	if _, err := client.Store(set, &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen},
	}, nil).Collect(); err != nil {
		t.Fatalf("storing flags: %v", err)
	}

	messages, err := client.Fetch(set, &imap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatalf("fetching flags: %v", err)
	}
	var seen bool
	for _, flag := range messages[0].Flags {
		if flag == imap.FlagSeen {
			seen = true
		}
	}
	if !seen {
		t.Errorf("the \\Seen flag was not applied: %v", messages[0].Flags)
	}

	// And removing it again must work.
	if _, err := client.Store(set, &imap.StoreFlags{
		Op: imap.StoreFlagsDel, Flags: []imap.Flag{imap.FlagSeen},
	}, nil).Collect(); err != nil {
		t.Fatalf("removing flags: %v", err)
	}
	messages, err = client.Fetch(set, &imap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatalf("fetching flags: %v", err)
	}
	for _, flag := range messages[0].Flags {
		if flag == imap.FlagSeen {
			t.Error("the \\Seen flag was not removed")
		}
	}
}

func TestSearchUnseen(t *testing.T) {
	h := newHarness(t, 10)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	data, err := client.Search(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	// Without ESEARCH RETURN options the results arrive as a plain set rather
	// than a count, which is what a client asking a bare SEARCH receives.
	found := data.AllSeqNums()
	// Odd-indexed messages were seeded unseen: five of ten.
	if len(found) != 5 {
		t.Errorf("found %d unseen messages, want 5: %v", len(found), found)
	}
}

func TestSearchBySubject(t *testing.T) {
	h := newHarness(t, 5)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	data, err := client.Search(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "Message 3"}},
	}, nil).Wait()
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if found := data.AllSeqNums(); len(found) != 1 {
		t.Errorf("found %d matches for a subject search, want 1: %v", len(found), found)
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t, 8)
	client := h.login(t)

	data, err := client.Status("INBOX", &imap.StatusOptions{
		NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true,
	}).Wait()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if data.NumMessages == nil || *data.NumMessages != 8 {
		t.Errorf("NumMessages = %v, want 8", data.NumMessages)
	}
	if data.NumUnseen == nil || *data.NumUnseen != 4 {
		t.Errorf("NumUnseen = %v, want 4", data.NumUnseen)
	}
}

// Mirrored mail is read-only, and the server must say so rather than pretend a
// change succeeded and silently drop it.
func TestWritesAreRefusedHonestly(t *testing.T) {
	h := newHarness(t, 2)
	client := h.login(t)

	if err := client.Create("New Folder", nil).Wait(); err == nil {
		t.Error("CREATE was accepted, but mirrored mailboxes are read-only")
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}
	// The literal must be written even when the command will be refused, or the
	// client blocks waiting to send it.
	raw := []byte("Subject: nope\r\n\r\nbody\r\n")
	appendCmd := client.Append("INBOX", int64(len(raw)), nil)
	_, _ = appendCmd.Write(raw)
	_ = appendCmd.Close()
	if _, err := appendCmd.Wait(); err == nil {
		t.Error("APPEND was accepted, but mirrored mailboxes are read-only")
	}
}

// Sequence numbers must remain stable and one-based across a selection.
func TestSequenceNumbersAreStable(t *testing.T) {
	h := newHarness(t, 6)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	var set imap.SeqSet
	set.AddRange(1, 0)
	messages, err := client.Fetch(set, &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}
	for i, msg := range messages {
		if msg.SeqNum != uint32(i+1) {
			t.Errorf("message at index %d reported sequence number %d", i, msg.SeqNum)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// A message whose body has not been downloaded must still show its headers.
//
// Mail clients build their message list from BODY[HEADER.FIELDS (...)], not
// from ENVELOPE. Serving that only by slicing the stored message meant a
// mailbox mid-download appeared as thousands of entries with no subject, no
// sender and today's date — even though every one of those values was sitting
// in the database.
func TestHeaderFieldsAreServedBeforeTheBodyArrives(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()

	var messageID int64
	err := h.store.Pool().QueryRow(ctx,
		`INSERT INTO messages (account_id, rfc822_sha256, rfc822_size, subject, addrs, internal_date)
		 SELECT mb.account_id, $1, 4096, 'A subject from metadata',
		        '{"from":["Alice <alice@example.com>"],"to":["bob@example.com"]}'::jsonb,
		        '2026-03-04 05:06:07+00'
		 FROM mailboxes mb WHERE mb.id = $2
		 RETURNING id`, make([]byte, 32), h.mailboxID).Scan(&messageID)
	if err != nil {
		t.Fatalf("inserting message: %v", err)
	}
	if _, err := h.store.Pool().Exec(ctx,
		`INSERT INTO mailbox_messages (mailbox_id, message_id, local_uid, upstream_uid, body_state, flags)
		 VALUES ($1, $2, 1, 1, 'pending', '{}')`, h.mailboxID, messageID); err != nil {
		t.Fatalf("linking message: %v", err)
	}
	if _, err := h.store.Pool().Exec(ctx,
		`UPDATE mailboxes SET local_uidnext = 2 WHERE id = $1`, h.mailboxID); err != nil {
		t.Fatalf("updating uidnext: %v", err)
	}

	client := h.login(t)
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	var set imap.SeqSet
	set.AddNum(1)
	messages, err := client.Fetch(set, &imap.FetchOptions{
		UID: true, Flags: true, RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"From", "To", "Subject", "Date", "Message-ID"},
		}},
	}).Collect()
	if err != nil {
		t.Fatalf("fetching headers: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(messages))
	}

	var headers string
	for _, contents := range messages[0].BodySection {
		headers = string(contents.Bytes)
	}
	t.Logf("headers served:\n%s", headers)

	for _, want := range []string{"A subject from metadata", "alice@example.com", "2026"} {
		if !strings.Contains(headers, want) {
			t.Errorf("headers do not contain %q; a client would show this message blank", want)
		}
	}
}

// A client asking for a few header fields must not receive the whole block.
func TestHeaderFieldsAreFiltered(t *testing.T) {
	h := newHarness(t, 3)
	client := h.login(t)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	var set imap.SeqSet
	set.AddNum(1)
	messages, err := client.Fetch(set, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"Subject"},
		}},
	}).Collect()
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}

	var headers string
	for _, contents := range messages[0].BodySection {
		headers = string(contents.Bytes)
	}

	if !strings.Contains(headers, "Subject:") {
		t.Errorf("the requested field is missing: %q", headers)
	}
	if strings.Contains(headers, "Message-ID:") || strings.Contains(headers, "MIME-Version:") {
		t.Errorf("fields that were not asked for were returned: %q", headers)
	}
}
