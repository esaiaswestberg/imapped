package fakeimap

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"
)

// Render builds an RFC 5322 message from the fields set on m, filling in
// defaults so a zero-value Message is still a valid message.
func (m Message) Render() []byte {
	subject := m.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	from := m.From
	if from == "" {
		from = "sender@example.com"
	}
	to := m.To
	if to == "" {
		to = DefaultUsername
	}
	date := m.Date
	if date.IsZero() {
		date = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	}
	body := m.Body
	if body == "" {
		body = "This is a test message.\r\n"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", date.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@example.com>\r\n", messageIDFor(subject, date))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(normaliseNewlines(body))

	return []byte(b.String())
}

// messageIDFor derives a stable identifier, so the same seed produces the same
// corpus across runs and a test asserting deduplication is reproducible.
func messageIDFor(subject string, date time.Time) string {
	cleaned := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, subject)
	return fmt.Sprintf("%s.%d", strings.ToLower(cleaned), date.Unix())
}

// normaliseNewlines converts bare LF to CRLF, since IMAP literals are measured
// in octets and a mismatch corrupts the byte count.
func normaliseNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// Seed generates n messages with predictable, varied content.
//
// Sizes deliberately vary: the body-fetch planner batches by byte budget, so a
// corpus of identically-sized messages would not exercise it meaningfully.
func Seed(n int) []Message {
	msgs := make([]Message, 0, n)
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	for i := range n {
		// A repeating cycle of sizes, with an occasional much larger message to
		// exercise batch splitting.
		bodySize := 200 + (i%7)*350
		if i%50 == 49 {
			bodySize = 60_000
		}
		msgs = append(msgs, Message{
			Subject: fmt.Sprintf("Test message %d", i+1),
			From:    fmt.Sprintf("sender%d@example.com", i%13),
			To:      DefaultUsername,
			Date:    base.Add(time.Duration(i) * time.Minute),
			Body:    strings.Repeat("word ", bodySize/5),
			Flags:   seedFlags(i),
		})
	}
	return msgs
}

func seedFlags(i int) []imapFlag {
	// Roughly two thirds read, a tenth flagged: enough variety that flag
	// synchronisation has something to actually synchronise.
	var flags []imapFlag
	if i%3 != 0 {
		flags = append(flags, seenFlag)
	}
	if i%10 == 0 {
		flags = append(flags, flaggedFlag)
	}
	return flags
}

// literal adapts a byte slice to the reader interface APPEND expects.
type literal struct {
	r    *bytes.Reader
	size int64
}

func newLiteral(b []byte) *literal {
	return &literal{r: bytes.NewReader(b), size: int64(len(b))}
}

func (l *literal) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *literal) Size() int64                { return l.size }

var _ io.Reader = (*literal)(nil)
