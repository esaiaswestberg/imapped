// Package mailstore is the single read/write interface over mirrored mail.
//
// Both the IMAP frontend and the web UI go through here, so quota accounting,
// deduplication, search indexing and reference counting happen in exactly one
// place. In the Rust implementation this logic lived inside a 7,000-line IMAP
// server file, which is why adding a second consumer meant duplicating it.
package mailstore

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers non-UTF-8 charset decoders
	"github.com/emersion/go-message/mail"
)

// ParsedMessage is the result of parsing a raw RFC 5322 message.
type ParsedMessage struct {
	Subject   string
	MessageID string
	InReplyTo string
	Refs      []string

	From    []string
	To      []string
	Cc      []string
	Bcc     []string
	ReplyTo []string

	Date time.Time

	// BodyText is the complete extracted text, used for search and display.
	BodyText string
	// Preview is a short snippet for message lists.
	Preview string

	Parts []Part

	// Failed records that parsing did not complete. The raw message is still
	// stored: a message we cannot parse must never be a message we lose.
	Failed bool
	Error  string
}

// Part is one MIME part of a message.
type Part struct {
	Path        string // RFC 3501 body part number, e.g. "1.2"
	ContentType string
	Charset     string
	Disposition string
	Filename    string
	ContentID   string
	Encoding    string
	Size        int64
	Content     []byte
}

// IsAttachment reports whether a part should be offered as a download rather
// than rendered inline.
func (p Part) IsAttachment() bool {
	return strings.EqualFold(p.Disposition, "attachment") || p.Filename != ""
}

const (
	previewLength = 300
	// maxBodyText bounds what is kept for search and display. A message with a
	// multi-megabyte text part is almost always machine-generated, and indexing
	// all of it costs far more than it returns.
	maxBodyText = 1 << 20
)

// Parse extracts structure and text from a raw message.
//
// Parsing never returns an error for malformed input. Two decades of real mail
// contains MIME that no parser fully agrees on, and refusing such a message
// would mean losing it; instead the failure is recorded on the result and
// whatever was extracted is kept.
func Parse(raw []byte) ParsedMessage {
	var out ParsedMessage

	// go-message can panic on sufficiently malformed input. An ingest worker
	// dying would take the whole sync with it, so this is one of the few places
	// where recovering is the right call.
	defer func() {
		if r := recover(); r != nil {
			out.Failed = true
			out.Error = fmt.Sprintf("panic while parsing message: %v", r)
		}
	}()

	entity, err := message.Read(strings.NewReader(string(raw)))
	if err != nil && entity == nil {
		out.Failed = true
		out.Error = err.Error()
		return out
	}
	if err != nil {
		// message.Read reports unknown encodings and charsets as errors while
		// still returning a usable entity, so keep going and note it.
		out.Error = err.Error()
	}

	header := mail.Header{Header: entity.Header}
	out.Subject = headerText(header, "Subject")
	out.MessageID = strings.Trim(headerRaw(header, "Message-Id"), "<>")
	out.InReplyTo = strings.Trim(headerRaw(header, "In-Reply-To"), "<>")
	out.Refs = splitReferences(headerRaw(header, "References"))

	if date, err := header.Date(); err == nil {
		out.Date = date
	}

	out.From = addresses(header, "From")
	out.To = addresses(header, "To")
	out.Cc = addresses(header, "Cc")
	out.Bcc = addresses(header, "Bcc")
	out.ReplyTo = addresses(header, "Reply-To")

	var text strings.Builder
	walk(entity, "1", &out.Parts, &text, 0)

	out.BodyText = sanitise(truncate(text.String(), maxBodyText))
	out.Preview = makePreview(out.BodyText)

	out.Subject = sanitise(out.Subject)
	out.MessageID = sanitise(out.MessageID)
	out.InReplyTo = sanitise(out.InReplyTo)

	return out
}

// maxDepth bounds MIME nesting. Deeply nested multiparts occur naturally in
// forwarded chains, but unbounded recursion on hostile input is a denial of
// service.
const maxDepth = 20

func walk(entity *message.Entity, path string, parts *[]Part, text *strings.Builder, depth int) {
	if entity == nil || depth > maxDepth {
		return
	}

	multipart := entity.MultipartReader()
	if multipart != nil {
		index := 1
		for {
			child, err := multipart.NextPart()
			if err != nil {
				// io.EOF ends the loop; anything else means the remaining parts
				// are unreadable, and stopping keeps what was parsed so far.
				return
			}
			childPath := fmt.Sprintf("%s.%d", path, index)
			if depth == 0 {
				// RFC 3501 numbers the children of a top-level multipart 1, 2,
				// ... rather than 1.1, 1.2, so clients ask for the right part.
				childPath = fmt.Sprintf("%d", index)
			}
			walk(child, childPath, parts, text, depth+1)
			index++
		}
	}

	content, err := io.ReadAll(io.LimitReader(entity.Body, maxBodyText*4))
	if err != nil {
		return
	}

	contentType, params, _ := entity.Header.ContentType()
	disposition, dispositionParams, _ := entity.Header.ContentDisposition()

	part := Part{
		Path:        path,
		ContentType: contentType,
		Charset:     params["charset"],
		Disposition: disposition,
		Filename:    firstNonEmpty(dispositionParams["filename"], params["name"]),
		ContentID:   strings.Trim(entity.Header.Get("Content-Id"), "<>"),
		Encoding:    entity.Header.Get("Content-Transfer-Encoding"),
		Size:        int64(len(content)),
		Content:     content,
	}
	*parts = append(*parts, part)

	// Only inline text contributes to the searchable body. An attached text
	// file is retrievable but is not what someone means when they search for
	// words "in a message".
	if strings.HasPrefix(contentType, "text/") && !part.IsAttachment() && text.Len() < maxBodyText {
		body := string(content)
		if strings.EqualFold(contentType, "text/html") {
			body = stripHTML(body)
		}
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString(body)
	}
}

func headerText(h mail.Header, key string) string {
	value, err := h.Text(key)
	if err != nil {
		// Undecodable RFC 2047 encoding: the raw value is more useful than none.
		return h.Get(key)
	}
	return value
}

func headerRaw(h mail.Header, key string) string { return h.Get(key) }

func addresses(h mail.Header, key string) []string {
	list, err := h.AddressList(key)
	if err != nil || len(list) == 0 {
		// Malformed address headers are common; fall back to the raw value so
		// the information is not lost entirely.
		if raw := h.Get(key); raw != "" {
			return []string{sanitise(raw)}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, addr := range list {
		if addr.Name != "" {
			out = append(out, sanitise(fmt.Sprintf("%s <%s>", addr.Name, addr.Address)))
		} else {
			out = append(out, sanitise(addr.Address))
		}
	}
	return out
}

func splitReferences(value string) []string {
	if value == "" {
		return nil
	}
	fields := strings.Fields(value)
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.Trim(field, "<>,"); trimmed != "" {
			out = append(out, sanitise(trimmed))
		}
	}
	return out
}

// stripHTML reduces markup to readable text.
//
// This is for indexing and previews, not rendering, so a tag-stripping pass is
// enough; it does not need to understand the document.
func stripHTML(html string) string {
	var out strings.Builder
	out.Grow(len(html))

	inTag := false
	skipUntil := ""

	for i := 0; i < len(html); i++ {
		if skipUntil != "" {
			if strings.HasPrefix(strings.ToLower(html[i:]), skipUntil) {
				i += len(skipUntil) - 1
				skipUntil = ""
			}
			continue
		}
		switch {
		case html[i] == '<':
			lower := strings.ToLower(html[i:])
			// Script and style contents are not prose and would pollute both
			// the search index and the preview.
			switch {
			case strings.HasPrefix(lower, "<script"):
				skipUntil = "</script>"
				continue
			case strings.HasPrefix(lower, "<style"):
				skipUntil = "</style>"
				continue
			}
			inTag = true
		case html[i] == '>':
			inTag = false
			out.WriteByte(' ')
		case !inTag:
			out.WriteByte(html[i])
		}
	}

	return strings.Join(strings.Fields(decodeEntities(out.String())), " ")
}

var htmlEntities = strings.NewReplacer(
	"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
	"&quot;", `"`, "&#39;", "'", "&apos;", "'",
)

func decodeEntities(s string) string { return htmlEntities.Replace(s) }

func makePreview(body string) string {
	cleaned := strings.Join(strings.Fields(body), " ")
	return truncate(cleaned, previewLength)
}

// truncate cuts to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// sanitise makes text safe to store in Postgres.
//
// NUL bytes are rejected outright by text columns, and invalid UTF-8 too. Real
// mail contains both; the previous implementation aborted an entire sync on the
// first message that did.
func sanitise(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.ToValidUTF8(s, "")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
