package search

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/esaiaswestberg/imapped/internal/db"
)

// PostgresSearcher searches using Postgres full-text search.
type PostgresSearcher struct {
	pool     *db.Pool
	language string
}

// NewPostgres builds a searcher over an existing pool.
func NewPostgres(pool *db.Pool, language string) *PostgresSearcher {
	if language == "" {
		language = "english"
	}
	return &PostgresSearcher{pool: pool, language: language}
}

const defaultLimit = 50

// Search runs a full-text query.
//
// User input goes through websearch_to_tsquery, which accepts the syntax people
// actually type — quoted phrases, OR, leading minus to exclude — and, crucially,
// never errors on malformed input. to_tsquery raises an error on so much as an
// unbalanced quote, which would turn a typo into a 500.
func (s *PostgresSearcher) Search(ctx context.Context, q Query) ([]Result, int, error) {
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return nil, 0, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = defaultLimit
	}

	args := []any{s.language, text, q.AccountID, limit, q.Offset}
	mailboxFilter := ""
	if q.MailboxID > 0 {
		mailboxFilter = " AND mm.mailbox_id = $6"
		args = append(args, q.MailboxID)
	}

	// ts_rank_cd accounts for how close the matched terms are to each other, so
	// a message containing the words together outranks one that merely mentions
	// them in unrelated places.
	query := `
		WITH q AS (SELECT websearch_to_tsquery($1::regconfig, $2) AS tsq)
		SELECT m.id, mm.id, mm.mailbox_id, mb.name, mm.local_uid,
		       COALESCE(m.subject, ''), COALESCE(m.addrs->'from', '[]'::jsonb),
		       COALESCE(m.preview, ''), m.internal_date, m.rfc822_size, mm.flags,
		       ts_rank_cd(m.search_tsv, q.tsq) AS rank,
		       ts_headline($1::regconfig, COALESCE(m.body_text, ''), q.tsq,
		                   'MaxWords=30, MinWords=10, ShortWord=3, MaxFragments=2'),
		       count(*) OVER () AS total
		FROM messages m
		JOIN mailbox_messages mm ON mm.message_id = m.id
		JOIN mailboxes mb ON mb.id = mm.mailbox_id
		CROSS JOIN q
		WHERE m.account_id = $3
		  AND m.search_tsv @@ q.tsq
		  AND mm.expunged_at IS NULL` + mailboxFilter + `
		ORDER BY rank DESC, m.internal_date DESC NULLS LAST
		LIMIT $4 OFFSET $5`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("searching messages: %w", err)
	}
	defer rows.Close()

	var (
		results []Result
		total   int
	)
	for rows.Next() {
		var (
			r        Result
			fromJSON []byte
			date     *time.Time
		)
		if err := rows.Scan(&r.MessageID, &r.MailboxMessageID, &r.MailboxID, &r.MailboxName,
			&r.LocalUID, &r.Subject, &fromJSON, &r.Preview, &date, &r.Size, &r.Flags,
			&r.Rank, &r.Highlight, &total); err != nil {
			return nil, 0, fmt.Errorf("scanning search result: %w", err)
		}
		if date != nil {
			r.InternalDate = *date
		}
		r.From = decodeStringArray(fromJSON)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading search results: %w", err)
	}
	return results, total, nil
}

var _ Searcher = (*PostgresSearcher)(nil)
