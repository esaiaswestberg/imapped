//go:build integration

package search_test

import (
	"context"
	"strings"
	"testing"

	"github.com/esaiaswestberg/imapped/internal/search"
	"github.com/esaiaswestberg/imapped/internal/testutil/pgtest"
)

func TestPostgresSearch(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()

	var userID, accountID, mailboxID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('o@e.com','x') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO mail_accounts (user_id, email_address, upstream_host, upstream_port,
		   encrypted_username, encrypted_secret)
		 VALUES ($1,'o@e.com','imap.example.com',993,'\x00','\x00') RETURNING id`,
		userID).Scan(&accountID); err != nil {
		t.Fatalf("creating account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (account_id, name, canonical_name)
		 VALUES ($1,'INBOX','inbox') RETURNING id`, accountID).Scan(&mailboxID); err != nil {
		t.Fatalf("creating mailbox: %v", err)
	}

	messages := []struct {
		subject string
		body    string
	}{
		{"Quarterly invoice", "Please find the invoice for the third quarter attached."},
		{"Lunch plans", "Shall we meet at the usual place near the station?"},
		{"Invoice reminder", "This is a reminder about the outstanding invoice payment."},
		{"Holiday photos", "Here are the photographs from the trip to the coast."},
	}

	for i, msg := range messages {
		digest := make([]byte, 32)
		digest[0] = byte(i + 1)

		var messageID int64
		err := pool.QueryRow(ctx,
			`INSERT INTO messages (account_id, rfc822_sha256, rfc822_size, subject, body_text, preview,
			   addrs, internal_date)
			 VALUES ($1,$2,$3,$4,$5,$6,$7, now()) RETURNING id`,
			accountID, digest, len(msg.body), msg.subject, msg.body, msg.body,
			`{"from": ["sender@example.com"]}`).Scan(&messageID)
		if err != nil {
			t.Fatalf("inserting message %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO mailbox_messages (mailbox_id, message_id, local_uid, upstream_uid, body_state)
			 VALUES ($1,$2,$3,$4,'stored')`, mailboxID, messageID, i+1, i+1); err != nil {
			t.Fatalf("linking message %d: %v", i, err)
		}
	}

	searcher := search.NewPostgres(pool, "english")

	t.Run("finds matches in the body", func(t *testing.T) {
		results, total, err := searcher.Search(ctx, search.Query{Text: "invoice", AccountID: accountID})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("found %d results for \"invoice\", want 2", len(results))
		}
		if total != 2 {
			t.Errorf("total = %d, want 2", total)
		}
		for _, r := range results {
			if !strings.Contains(strings.ToLower(r.Subject+r.Preview), "invoice") {
				t.Errorf("result %q does not appear to match", r.Subject)
			}
		}
	})

	// Body text must be indexed in full, not merely a truncated preview as the
	// Rust implementation did.
	t.Run("searches the whole body, not just a preview", func(t *testing.T) {
		results, _, err := searcher.Search(ctx, search.Query{Text: "photographs", AccountID: accountID})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("found %d results, want 1", len(results))
		}
		if results[0].Subject != "Holiday photos" {
			t.Errorf("matched %q, want \"Holiday photos\"", results[0].Subject)
		}
	})

	// Stemming is what makes "photo" find "photographs".
	t.Run("stems words", func(t *testing.T) {
		results, _, err := searcher.Search(ctx, search.Query{Text: "reminders", AccountID: accountID})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) == 0 {
			t.Error("stemming did not match \"reminder\" for the query \"reminders\"")
		}
	})

	t.Run("supports quoted phrases", func(t *testing.T) {
		results, _, err := searcher.Search(ctx,
			search.Query{Text: `"usual place"`, AccountID: accountID})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) != 1 {
			t.Errorf("found %d results for a quoted phrase, want 1", len(results))
		}
	})

	t.Run("supports exclusion", func(t *testing.T) {
		results, _, err := searcher.Search(ctx,
			search.Query{Text: "invoice -reminder", AccountID: accountID})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("found %d results, want 1", len(results))
		}
		if results[0].Subject != "Quarterly invoice" {
			t.Errorf("matched %q, want \"Quarterly invoice\"", results[0].Subject)
		}
	})

	// Malformed input is inevitable from a live search box. websearch_to_tsquery
	// tolerates it; to_tsquery would raise an error and return a 500.
	t.Run("tolerates malformed queries", func(t *testing.T) {
		for _, text := range []string{`"unbalanced`, `& | !`, `((()))`, `:*`, `-`} {
			if _, _, err := searcher.Search(ctx, search.Query{Text: text, AccountID: accountID}); err != nil {
				t.Errorf("query %q returned an error: %v", text, err)
			}
		}
	})

	t.Run("returns a highlighted snippet", func(t *testing.T) {
		results, _, err := searcher.Search(ctx, search.Query{Text: "quarter", AccountID: accountID})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("no results")
		}
		if !strings.Contains(results[0].Highlight, "<b>") {
			t.Errorf("highlight has no marked terms: %q", results[0].Highlight)
		}
	})

	t.Run("scopes to the account", func(t *testing.T) {
		results, _, err := searcher.Search(ctx, search.Query{Text: "invoice", AccountID: accountID + 999})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("found %d results for another account, want 0", len(results))
		}
	})

	t.Run("an empty query returns nothing", func(t *testing.T) {
		results, _, err := searcher.Search(ctx, search.Query{Text: "   ", AccountID: accountID})
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("an empty query returned %d results", len(results))
		}
	})
}
