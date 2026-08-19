// Package search provides full-text search over stored messages.
package search

import (
	"context"
	"time"
)

// Query describes a search request.
type Query struct {
	// Text is the user's query, in whatever syntax they typed.
	Text string

	AccountID int64
	MailboxID int64 // zero searches every mailbox

	Limit  int
	Offset int
}

// Result is one matching message.
type Result struct {
	MessageID        int64
	MailboxMessageID int64
	MailboxID        int64
	MailboxName      string
	LocalUID         int64

	Subject      string
	From         []string
	Preview      string
	InternalDate time.Time
	Size         int64
	Flags        []string

	// Rank orders results; higher is a better match.
	Rank float32
	// Highlight is a snippet with the matching terms marked.
	Highlight string
}

// Searcher runs queries.
//
// This is an interface so the backend can change without touching callers.
// Postgres full-text search is what ships; if it proves insufficient at scale a
// dedicated engine can be substituted here.
type Searcher interface {
	Search(ctx context.Context, q Query) ([]Result, int, error)
}
