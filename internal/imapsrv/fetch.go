package imapsrv

import (
	"context"
	"errors"
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
		date := time.Time{}
		if item.message.InternalDate != nil {
			date = *item.message.InternalDate
		}
		writer.WriteInternalDate(date)
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
		if err := writeSection(writer, section, raw); err != nil {
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
func writeSection(writer *imapserver.FetchResponseWriter, section *imap.FetchItemBodySection, raw []byte) error {
	content := raw

	// A header-only request must not transfer the whole message.
	switch section.Specifier {
	case imap.PartSpecifierHeader:
		content = headerBytes(raw)
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
// Changes are local only in this release. The upstream replay queue exists in
// the schema but is not wired up, so telling a client the change succeeded
// while it never reaches the server would be a lie; it is accepted locally and
// reported honestly as such.
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
