package mailstore_test

import (
	"strings"
	"testing"

	"github.com/esaiaswestberg/imapped/internal/mailstore"
)

func TestParsePlainTextMessage(t *testing.T) {
	raw := []byte("From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: Lunch tomorrow\r\n" +
		"Message-ID: <abc123@example.com>\r\n" +
		"Date: Mon, 05 Jan 2026 10:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Shall we meet at the usual place?\r\n")

	parsed := mailstore.Parse(raw)

	if parsed.Failed {
		t.Fatalf("parsing failed: %s", parsed.Error)
	}
	if parsed.Subject != "Lunch tomorrow" {
		t.Errorf("Subject = %q", parsed.Subject)
	}
	if parsed.MessageID != "abc123@example.com" {
		t.Errorf("MessageID = %q", parsed.MessageID)
	}
	if len(parsed.From) != 1 || !strings.Contains(parsed.From[0], "alice@example.com") {
		t.Errorf("From = %v", parsed.From)
	}
	if !strings.Contains(parsed.BodyText, "usual place") {
		t.Errorf("BodyText = %q", parsed.BodyText)
	}
	if parsed.Date.IsZero() {
		t.Error("Date was not parsed")
	}
}

// RFC 2047 encoded headers are ubiquitous; a subject shown as raw
// =?utf-8?B?...?= is useless in a message list and unsearchable.
func TestParseDecodesEncodedHeaders(t *testing.T) {
	raw := []byte("From: sender@example.com\r\n" +
		"Subject: =?utf-8?B?SGVsbG8gV8O2cmxk?=\r\n" +
		"\r\n" +
		"body\r\n")

	parsed := mailstore.Parse(raw)
	if parsed.Subject != "Hello Wörld" {
		t.Errorf("Subject = %q, want %q", parsed.Subject, "Hello Wörld")
	}
}

func TestParseMultipartAlternative(t *testing.T) {
	raw := []byte("From: sender@example.com\r\n" +
		"Subject: Newsletter\r\n" +
		"Content-Type: multipart/alternative; boundary=BOUND\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"The plain text version.\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><p>The HTML version.</p></body></html>\r\n" +
		"--BOUND--\r\n")

	parsed := mailstore.Parse(raw)
	if parsed.Failed {
		t.Fatalf("parsing failed: %s", parsed.Error)
	}
	if len(parsed.Parts) != 2 {
		t.Fatalf("found %d parts, want 2: %+v", len(parsed.Parts), parsed.Parts)
	}
	if !strings.Contains(parsed.BodyText, "plain text version") {
		t.Errorf("plain text is missing from the body: %q", parsed.BodyText)
	}
	// HTML must be reduced to text, not indexed as markup.
	if strings.Contains(parsed.BodyText, "<p>") {
		t.Errorf("HTML markup leaked into the body text: %q", parsed.BodyText)
	}
	if !strings.Contains(parsed.BodyText, "HTML version") {
		t.Errorf("HTML text is missing from the body: %q", parsed.BodyText)
	}
}

// Part numbering must follow RFC 3501, or a client asking for BODY[2] gets the
// wrong part.
func TestParseNumbersPartsPerRFC3501(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nfirst\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nsecond\r\n" +
		"--B--\r\n")

	parsed := mailstore.Parse(raw)
	if len(parsed.Parts) != 2 {
		t.Fatalf("found %d parts, want 2", len(parsed.Parts))
	}
	for i, want := range []string{"1", "2"} {
		if parsed.Parts[i].Path != want {
			t.Errorf("part %d has path %q, want %q", i, parsed.Parts[i].Path, want)
		}
	}
}

func TestParseIdentifiesAttachments(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nSee attached.\r\n" +
		"--B\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n\r\n" +
		"%PDF-1.4 fake\r\n" +
		"--B--\r\n")

	parsed := mailstore.Parse(raw)

	var attachments []mailstore.Part
	for _, part := range parsed.Parts {
		if part.IsAttachment() {
			attachments = append(attachments, part)
		}
	}
	if len(attachments) != 1 {
		t.Fatalf("found %d attachments, want 1", len(attachments))
	}
	if attachments[0].Filename != "invoice.pdf" {
		t.Errorf("filename = %q, want invoice.pdf", attachments[0].Filename)
	}
	// Attachment contents must not pollute the searchable body.
	if strings.Contains(parsed.BodyText, "%PDF") {
		t.Errorf("attachment content leaked into the body text: %q", parsed.BodyText)
	}
}

// Postgres rejects NUL bytes and invalid UTF-8 outright. A message containing
// either previously aborted an entire sync.
func TestParseStripsBytesPostgresRejects(t *testing.T) {
	raw := []byte("Subject: has a \x00 nul\r\n" +
		"From: sender@example.com\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"body with \x00 nul and invalid \xff\xfe utf-8\r\n")

	parsed := mailstore.Parse(raw)

	for name, value := range map[string]string{
		"Subject":  parsed.Subject,
		"BodyText": parsed.BodyText,
		"Preview":  parsed.Preview,
	} {
		if strings.ContainsRune(value, 0) {
			t.Errorf("%s still contains a NUL byte: %q", name, value)
		}
		if !isValidUTF8(value) {
			t.Errorf("%s is not valid UTF-8: %q", name, value)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' && !strings.Contains(s, "�") {
			return false
		}
	}
	return strings.ToValidUTF8(s, "") == s
}

// Malformed input must never panic or lose the message.
func TestParseSurvivesMalformedInput(t *testing.T) {
	for name, raw := range map[string][]byte{
		"empty":              {},
		"headers only":       []byte("Subject: nothing else\r\n"),
		"no headers":         []byte("just some text with no headers at all"),
		"truncated boundary": []byte("Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\ntrunc"),
		"bad encoding":       []byte("Content-Transfer-Encoding: base64\r\n\r\n!!!not base64!!!"),
		"unknown charset":    []byte("Content-Type: text/plain; charset=definitely-not-a-charset\r\n\r\nhello"),
		"binary":             {0x00, 0x01, 0x02, 0xff, 0xfe},
	} {
		t.Run(name, func(t *testing.T) {
			// The requirement is simply that this returns.
			parsed := mailstore.Parse(raw)
			_ = parsed.Subject
		})
	}
}

// Nesting must be bounded, or hostile input becomes a denial of service.
func TestParseBoundsNestingDepth(t *testing.T) {
	var b strings.Builder
	const depth = 60
	for i := range depth {
		b.WriteString("Content-Type: multipart/mixed; boundary=B")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\r\n\r\n--B")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\r\n")
	}
	b.WriteString("Content-Type: text/plain\r\n\r\ndeep\r\n")

	done := make(chan struct{})
	go func() {
		mailstore.Parse([]byte(b.String()))
		close(done)
	}()

	select {
	case <-done:
	case <-timeoutAfterSeconds(10):
		t.Fatal("parsing deeply nested input did not terminate")
	}
}
