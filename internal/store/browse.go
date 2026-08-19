package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// MessageSummary is a message as it appears in a list.
type MessageSummary struct {
	MailboxMessageID int64
	MessageID        int64
	LocalUID         int64
	Subject          string
	From             []string
	Preview          string
	InternalDate     *time.Time
	Size             int64
	Flags            []string
	BodyState        string
	HasAttachments   bool
}

// Seen reports whether the message has been read.
func (m MessageSummary) Seen() bool {
	for _, flag := range m.Flags {
		if flag == `\Seen` {
			return true
		}
	}
	return false
}

// ListMessages returns a page of messages in a mailbox, newest first.
func (s *Store) ListMessages(ctx context.Context, mailboxID int64, limit, offset int) ([]MessageSummary, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := s.pool.Query(ctx,
		`SELECT mm.id, m.id, mm.local_uid, COALESCE(m.subject, '(no subject)'),
		        COALESCE(m.addrs->'from', '[]'::jsonb), COALESCE(m.preview, ''),
		        m.internal_date, m.rfc822_size, mm.flags, mm.body_state,
		        EXISTS (SELECT 1 FROM mime_parts p
		                WHERE p.message_id = m.id AND p.filename IS NOT NULL),
		        count(*) OVER () AS total
		 FROM mailbox_messages mm
		 JOIN messages m ON m.id = mm.message_id
		 WHERE mm.mailbox_id = $1 AND mm.expunged_at IS NULL
		 ORDER BY m.internal_date DESC NULLS LAST, mm.local_uid DESC
		 LIMIT $2 OFFSET $3`, mailboxID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing messages: %w", err)
	}
	defer rows.Close()

	var (
		out   []MessageSummary
		total int64
	)
	for rows.Next() {
		var (
			m        MessageSummary
			fromJSON []byte
		)
		if err := rows.Scan(&m.MailboxMessageID, &m.MessageID, &m.LocalUID, &m.Subject,
			&fromJSON, &m.Preview, &m.InternalDate, &m.Size, &m.Flags, &m.BodyState,
			&m.HasAttachments, &total); err != nil {
			return nil, 0, fmt.Errorf("scanning message: %w", err)
		}
		m.From = decodeStrings(fromJSON)
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// MessageDetail is a full message for display.
type MessageDetail struct {
	MessageSummary

	To         []string
	Cc         []string
	MessageID_ string // the RFC 5322 Message-ID header
	BodyText   string
	BlobKey    string

	ParseFailed bool
	ParseError  string

	MailboxID   int64
	MailboxName string
	AccountID   int64

	Parts []MIMEPartRow
}

// MIMEPartRow is a stored part as shown in the UI.
type MIMEPartRow struct {
	Path        string
	ContentType string
	Filename    string
	Size        int64
	BlobKey     string
}

// GetMessage loads one message for display.
func (s *Store) GetMessage(ctx context.Context, mailboxMessageID int64) (MessageDetail, error) {
	var (
		d        MessageDetail
		addrs    []byte
		blobKey  *string
		bodyText *string
		msgID    *string
		errText  *string
	)

	err := s.pool.QueryRow(ctx,
		`SELECT mm.id, m.id, mm.local_uid, COALESCE(m.subject,'(no subject)'),
		        COALESCE(m.addrs,'{}'::jsonb), COALESCE(m.preview,''),
		        m.internal_date, m.rfc822_size, mm.flags, mm.body_state,
		        m.body_text, m.blob_key, m.message_id_hdr, m.parse_failed, m.parse_error,
		        mm.mailbox_id, mb.name, m.account_id
		 FROM mailbox_messages mm
		 JOIN messages m ON m.id = mm.message_id
		 JOIN mailboxes mb ON mb.id = mm.mailbox_id
		 WHERE mm.id = $1`, mailboxMessageID).
		Scan(&d.MailboxMessageID, &d.MessageID, &d.LocalUID, &d.Subject,
			&addrs, &d.Preview, &d.InternalDate, &d.Size, &d.Flags, &d.BodyState,
			&bodyText, &blobKey, &msgID, &d.ParseFailed, &errText,
			&d.MailboxID, &d.MailboxName, &d.AccountID)
	if err != nil {
		return MessageDetail{}, notFound(err)
	}

	var parsed map[string][]string
	_ = json.Unmarshal(addrs, &parsed)
	d.From, d.To, d.Cc = parsed["from"], parsed["to"], parsed["cc"]

	if bodyText != nil {
		d.BodyText = *bodyText
	}
	if blobKey != nil {
		d.BlobKey = *blobKey
	}
	if msgID != nil {
		d.MessageID_ = *msgID
	}
	if errText != nil {
		d.ParseError = *errText
	}

	rows, err := s.pool.Query(ctx,
		`SELECT part_path, content_type, COALESCE(filename,''), size_octets, COALESCE(blob_key,'')
		 FROM mime_parts WHERE message_id = $1 ORDER BY part_path`, d.MessageID)
	if err != nil {
		return d, fmt.Errorf("loading MIME parts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p MIMEPartRow
		if err := rows.Scan(&p.Path, &p.ContentType, &p.Filename, &p.Size, &p.BlobKey); err != nil {
			return d, err
		}
		d.Parts = append(d.Parts, p)
	}
	return d, rows.Err()
}

// AccountStats summarises an account for the dashboard.
type AccountStats struct {
	Mailboxes     int64
	Messages      int64
	PendingBodies int64
	FailedBodies  int64
	Bytes         int64
}

// StatsForAccount computes dashboard figures.
func (s *Store) StatsForAccount(ctx context.Context, accountID int64) (AccountStats, error) {
	var st AccountStats
	err := s.pool.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM mailboxes WHERE account_id = $1),
		   (SELECT count(*) FROM mailbox_messages mm
		      JOIN mailboxes mb ON mb.id = mm.mailbox_id WHERE mb.account_id = $1),
		   (SELECT count(*) FROM mailbox_messages mm
		      JOIN mailboxes mb ON mb.id = mm.mailbox_id
		      WHERE mb.account_id = $1 AND mm.body_state IN ('pending','fetching')),
		   (SELECT count(*) FROM mailbox_messages mm
		      JOIN mailboxes mb ON mb.id = mm.mailbox_id
		      WHERE mb.account_id = $1 AND mm.body_state = 'failed'),
		   (SELECT COALESCE(sum(rfc822_size), 0) FROM messages WHERE account_id = $1)`,
		accountID).Scan(&st.Mailboxes, &st.Messages, &st.PendingBodies, &st.FailedBodies, &st.Bytes)
	if err != nil {
		return AccountStats{}, fmt.Errorf("computing account statistics: %w", err)
	}
	return st, nil
}

func decodeStrings(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
