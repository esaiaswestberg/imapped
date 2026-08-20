package imapsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/store"
)

// selected pairs a message with its sequence number.
type selected struct {
	seqNum  uint32
	message store.MessageSummary
}

// resolve turns a client's number set into the messages it refers to.
//
// A sequence set indexes into the snapshot captured at SELECT; a UID set is
// matched against local UIDs. Getting this wrong means a client silently acts
// on the wrong messages, so both forms go through one place.
func (s *session) resolve(numSet imap.NumSet) []selected {
	var out []selected

	switch set := numSet.(type) {
	case imap.SeqSet:
		for i, message := range s.snapshot {
			seqNum := uint32(i + 1)
			if set.Contains(seqNum) {
				out = append(out, selected{seqNum: seqNum, message: message})
			}
		}
	case imap.UIDSet:
		for i, message := range s.snapshot {
			if set.Contains(imap.UID(message.LocalUID)) {
				out = append(out, selected{seqNum: uint32(i + 1), message: message})
			}
		}
	}
	return out
}

// Fetch returns the requested items for a set of messages.
func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireMailbox(); err != nil {
		return err
	}
	ctx, cancel := s.ctx()
	defer cancel()

	for _, item := range s.resolve(numSet) {
		if err := s.writeMessage(ctx, w, item, options); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) writeMessage(ctx context.Context, w *imapserver.FetchWriter,
	item selected, options *imap.FetchOptions) error {

	writer := w.CreateMessage(item.seqNum)

	if options.UID {
		writer.WriteUID(imap.UID(item.message.LocalUID))
	}
	if options.Flags {
		writer.WriteFlags(toFlags(item.message.Flags))
	}
	if options.InternalDate {
		// Falls back to the message's own Date header when the mirrored server
		// gave us no INTERNALDATE, since a zero timestamp is worse than an
		// approximate one for a client that sorts on it.
		writer.WriteInternalDate(headerDate(item.message))
	}
	if options.RFC822Size {
		writer.WriteRFC822Size(item.message.Size)
	}

	// Envelope, body structure and body sections all need the stored message,
	// so it is loaded once and reused rather than fetched per item.
	needsRaw := options.Envelope || options.BodyStructure != nil || len(options.BodySection) > 0
	var raw []byte
	if needsRaw {
		var err error
		raw, err = s.loadRaw(ctx, item.message.MailboxMessageID)
		if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, blob.ErrNotFound) {
			return err
		}
	}

	if options.Envelope {
		writer.WriteEnvelope(envelopeFor(item.message, raw))
	}
	if options.BodyStructure != nil {
		writer.WriteBodyStructure(bodyStructureFor(raw, item.message.Size))
	}

	for _, section := range options.BodySection {
		if err := writeSection(writer, section, raw, item.message); err != nil {
			return err
		}
	}

	return writer.Close()
}

// loadRaw reads a message's stored bytes.
func (s *session) loadRaw(ctx context.Context, mailboxMessageID int64) ([]byte, error) {
	key, _, err := s.backend.store.RawMessage(ctx, mailboxMessageID)
	if err != nil {
		return nil, err
	}
	reader, err := s.backend.blobs.Get(ctx, blob.Key(key))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// writeSection serves a BODY[...] request.
//
// Mail clients build their message list from BODY[HEADER.FIELDS (...)] rather
// than from ENVELOPE, so this is the path that decides whether a mailbox looks
// populated or blank. Serving it purely by slicing the stored message meant
// that a mailbox still downloading showed thousands of entries with no subject,
// no sender and today's date — while every one of those values sat in the
// database, unused.
func writeSection(writer *imapserver.FetchResponseWriter, section *imap.FetchItemBodySection,
	raw []byte, summary store.MessageSummary) error {

	content := raw

	switch section.Specifier {
	case imap.PartSpecifierHeader:
		content = headerBytes(raw)
		if len(content) == 0 {
			// The message itself has not been downloaded yet, so build the
			// headers from what was recorded when it was enumerated.
			content = synthesiseHeader(summary)
		}
		if len(section.HeaderFields) > 0 {
			content = selectHeaderFields(content, section.HeaderFields, false)
		} else if len(section.HeaderFieldsNot) > 0 {
			content = selectHeaderFields(content, section.HeaderFieldsNot, true)
		}
	case imap.PartSpecifierText:
		content = textBytes(raw)
	}

	if section.Partial != nil {
		content = applyPartial(content, section.Partial)
	}

	sectionWriter := writer.WriteBodySection(section, int64(len(content)))
	if _, err := sectionWriter.Write(content); err != nil {
		sectionWriter.Close()
		return err
	}
	return sectionWriter.Close()
}

// applyPartial serves a byte range, which clients use to stream large messages.
func applyPartial(content []byte, partial *imap.SectionPartial) []byte {
	if partial.Offset >= int64(len(content)) {
		return nil
	}
	content = content[partial.Offset:]
	if partial.Size > 0 && partial.Size < int64(len(content)) {
		content = content[:partial.Size]
	}
	return content
}

// headerBytes returns the message headers including the terminating blank line.
func headerBytes(raw []byte) []byte {
	if idx := strings.Index(string(raw), "\r\n\r\n"); idx >= 0 {
		return raw[:idx+4]
	}
	if idx := strings.Index(string(raw), "\n\n"); idx >= 0 {
		return raw[:idx+2]
	}
	return raw
}

// textBytes returns the body after the headers.
func textBytes(raw []byte) []byte {
	if idx := strings.Index(string(raw), "\r\n\r\n"); idx >= 0 {
		return raw[idx+4:]
	}
	if idx := strings.Index(string(raw), "\n\n"); idx >= 0 {
		return raw[idx+2:]
	}
	return nil
}

// synthesiseHeader builds an RFC 5322 header block from stored metadata.
//
// It is a faithful subset rather than the original: the fields a client needs
// to list a message, drawn from what the metadata pass recorded. Once the body
// arrives the real headers are served instead.
func synthesiseHeader(m store.MessageSummary) []byte {
	var b strings.Builder

	writeAddressList := func(name string, values []string) {
		if len(values) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s: %s\r\n", name, strings.Join(values, ", "))
	}

	writeAddressList("From", m.From)
	writeAddressList("To", m.To)
	writeAddressList("Cc", m.Cc)

	if m.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\r\n", m.Subject)
	}
	if m.MessageIDHdr != "" {
		fmt.Fprintf(&b, "Message-ID: %s\r\n", angled(m.MessageIDHdr))
	}
	// The message's own Date header first, falling back to the date the server
	// filed it under. A client that finds no Date at all stamps the message
	// with the time it listed it, which is how a mailbox of old mail ends up
	// looking like it all arrived this afternoon.
	if date := headerDate(m); !date.IsZero() {
		fmt.Fprintf(&b, "Date: %s\r\n", date.Format(time.RFC1123Z))
	}

	// Named so it is obvious to anyone reading a raw message that this header
	// block was reconstructed rather than received.
	b.WriteString("X-Imapped-Body-State: " + m.BodyState + "\r\n")
	b.WriteString("\r\n")

	return []byte(b.String())
}

// headerDate picks the timestamp to serve as a message's Date header.
func headerDate(m store.MessageSummary) time.Time {
	if m.SentDate != nil && !m.SentDate.IsZero() {
		return *m.SentDate
	}
	if m.InternalDate != nil && !m.InternalDate.IsZero() {
		return *m.InternalDate
	}
	return time.Time{}
}

// angled wraps a Message-ID in the brackets RFC 5322 requires, tolerating a
// stored value that already carries them.
func angled(id string) string {
	if strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") {
		return id
	}
	return "<" + id + ">"
}

// selectHeaderFields keeps or drops the named header fields.
//
// A client asking for a handful of fields should not be sent the whole block:
// it asked precisely to avoid that.
func selectHeaderFields(header []byte, fields []string, exclude bool) []byte {
	wanted := make(map[string]bool, len(fields))
	for _, field := range fields {
		wanted[strings.ToLower(field)] = true
	}

	var (
		out  strings.Builder
		keep bool
	)
	for _, line := range strings.Split(string(header), "\r\n") {
		if line == "" {
			continue
		}
		// A line starting with whitespace continues the previous field.
		if line[0] == ' ' || line[0] == '\t' {
			if keep {
				out.WriteString(line)
				out.WriteString("\r\n")
			}
			continue
		}

		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		keep = wanted[strings.ToLower(strings.TrimSpace(name))] != exclude
		if keep {
			out.WriteString(line)
			out.WriteString("\r\n")
		}
	}
	out.WriteString("\r\n")

	return []byte(out.String())
}

func toFlags(names []string) []imap.Flag {
	flags := make([]imap.Flag, 0, len(names))
	for _, name := range names {
		flags = append(flags, imap.Flag(name))
	}
	return flags
}

func fromFlags(flags []imap.Flag) []string {
	names := make([]string, 0, len(flags))
	for _, flag := range flags {
		names = append(names, string(flag))
	}
	return names
}

// Store applies a flag change from a client.
//
// The change is applied locally and queued for replay upstream. Acting locally
// first means the client sees its action take effect immediately and an
// upstream outage delays the push rather than blocking the user.
func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet,
	flags *imap.StoreFlags, options *imap.StoreOptions) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireMailbox(); err != nil {
		return err
	}
	if s.readOnly {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "the mailbox is open read-only"}
	}

	ctx, cancel := s.ctx()
	defer cancel()

	for _, item := range s.resolve(numSet) {
		updated := applyFlagChange(item.message.Flags, flags)
		if err := s.backend.store.SetFlags(ctx, item.message.MailboxMessageID, updated); err != nil {
			return internalError(err)
		}
		item.message.Flags = updated

		// Queue the change for the upstream server. Failing to queue it would
		// leave the local and remote states silently divergent, so it is
		// reported rather than ignored.
		if err := s.backend.store.EnqueueMutation(ctx, s.account.ID, s.mailbox.ID,
			store.MutationStoreFlags, store.FlagPayload{
				UpstreamUID: upstreamUIDOf(item.message),
				Mailbox:     s.mailbox.Name,
				Flags:       updated,
			}); err != nil {
			return internalError(err)
		}

		if !flags.Silent {
			writer := w.CreateMessage(item.seqNum)
			writer.WriteUID(imap.UID(item.message.LocalUID))
			writer.WriteFlags(toFlags(updated))
			if err := writer.Close(); err != nil {
				return err
			}
		}
	}

	return s.refreshSnapshot(ctx)
}

// applyFlagChange computes the new flag set for one message.
func applyFlagChange(current []string, change *imap.StoreFlags) []string {
	requested := fromFlags(change.Flags)

	switch change.Op {
	case imap.StoreFlagsSet:
		return dedupe(requested)
	case imap.StoreFlagsAdd:
		return dedupe(append(slices.Clone(current), requested...))
	case imap.StoreFlagsDel:
		var kept []string
		for _, flag := range current {
			if !slices.ContainsFunc(requested, func(r string) bool {
				return strings.EqualFold(r, flag)
			}) {
				kept = append(kept, flag)
			}
		}
		return kept
	}
	return current
}

// upstreamUIDOf returns the message's UID on the upstream server.
//
// Locally-visible UIDs are ours and mean nothing remotely; for messages
// mirrored from upstream the two happen to be seeded alike, but the mapping is
// what the replay needs.
func upstreamUIDOf(m store.MessageSummary) int64 {
	return m.UpstreamUID
}

func dedupe(values []string) []string {
	var out []string
	for _, value := range values {
		if !slices.ContainsFunc(out, func(existing string) bool {
			return strings.EqualFold(existing, value)
		}) {
			out = append(out, value)
		}
	}
	return out
}
