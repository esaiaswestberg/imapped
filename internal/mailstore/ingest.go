package mailstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/store"
)

// Ingestor turns a stored raw message into searchable, displayable content.
type Ingestor struct {
	store *store.Store
	blobs blob.Store
	log   *slog.Logger

	// storeParts controls whether individual MIME parts get their own blobs.
	// Parts can always be re-derived from the raw message, so storing them is a
	// read-latency optimisation rather than a correctness requirement.
	storeParts bool
}

// NewIngestor builds an Ingestor.
func NewIngestor(st *store.Store, blobs blob.Store, log *slog.Logger) *Ingestor {
	return &Ingestor{store: st, blobs: blobs, log: log, storeParts: true}
}

// Ingest parses a raw message and records what it finds.
//
// A parse failure is recorded rather than returned: the raw message is already
// stored, and refusing to record it would turn "we cannot display this message"
// into "we lost this message".
func (i *Ingestor) Ingest(ctx context.Context, messageID int64, raw []byte) error {
	parsed := Parse(raw)

	content := store.ParsedContent{
		References:  parsed.Refs,
		ParseFailed: parsed.Failed,
	}
	if parsed.Subject != "" {
		content.Subject = &parsed.Subject
	}
	if parsed.MessageID != "" {
		content.MessageID = &parsed.MessageID
	}
	if parsed.InReplyTo != "" {
		content.InReplyTo = &parsed.InReplyTo
	}
	if parsed.BodyText != "" {
		content.BodyText = &parsed.BodyText
	}
	if parsed.Preview != "" {
		content.Preview = &parsed.Preview
	}
	if !parsed.Date.IsZero() {
		date := parsed.Date
		content.SentDate = &date
	}
	if parsed.Error != "" {
		content.ParseError = &parsed.Error
	}

	addrs := map[string][]string{
		"from": parsed.From, "to": parsed.To, "cc": parsed.Cc,
		"bcc": parsed.Bcc, "reply_to": parsed.ReplyTo,
	}
	if encoded, err := json.Marshal(addrs); err == nil {
		content.Addrs = encoded
	}

	parts := make([]store.MIMEPart, 0, len(parsed.Parts))
	for _, part := range parsed.Parts {
		record := store.MIMEPart{
			Path:        part.Path,
			ContentType: part.ContentType,
			Size:        part.Size,
		}
		record.Charset = optional(part.Charset)
		record.Disposition = optional(part.Disposition)
		record.Filename = optional(part.Filename)
		record.ContentID = optional(part.ContentID)
		record.Encoding = optional(part.Encoding)

		// Only attachments are worth storing separately: they are what a client
		// requests on its own, whereas inline text is served from the already
		// parsed body.
		if i.storeParts && part.IsAttachment() && len(part.Content) > 0 {
			info, err := i.blobs.Put(ctx, blob.TypeMIMEPart, bytes.NewReader(part.Content))
			if err != nil {
				// Losing a part blob is recoverable — it can be re-derived from
				// the raw message — so note it and carry on.
				i.log.Warn("storing MIME part failed, it can be re-derived from the raw message",
					"message_id", messageID, "part", part.Path, "error", err)
			} else {
				key := info.Key.String()
				record.BlobKey = &key
			}
		}
		parts = append(parts, record)
	}

	if err := i.store.SaveParsedContent(ctx, messageID, content, parts); err != nil {
		return fmt.Errorf("recording parsed message %d: %w", messageID, err)
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
