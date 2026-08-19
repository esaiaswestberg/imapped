package imapsrv

import (
	"path"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/esaiaswestberg/imapped/internal/mailstore"
	"github.com/esaiaswestberg/imapped/internal/store"
)

// delimiter is the hierarchy separator reported to clients.
const delimiter = '/'

// List enumerates mailboxes matching the client's patterns.
func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireAuth(); err != nil {
		return err
	}
	ctx, cancel := s.ctx()
	defer cancel()

	boxes, err := s.backend.store.ListMailboxes(ctx, s.account.ID)
	if err != nil {
		return internalError(err)
	}

	for _, box := range boxes {
		if !matchesAny(box.Name, ref, patterns) {
			continue
		}
		data := &imap.ListData{
			Mailbox: box.Name,
			Delim:   delimiter,
			Attrs:   attributesFor(box),
		}
		if options != nil && options.ReturnStatus != nil {
			data.Status = statusFor(box, options.ReturnStatus)
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// matchesAny reports whether a mailbox name matches any client pattern.
func matchesAny(name, ref string, patterns []string) bool {
	full := name
	if ref != "" {
		full = strings.TrimSuffix(ref, string(delimiter)) + string(delimiter) + name
	}
	for _, pattern := range patterns {
		if matchPattern(full, pattern) {
			return true
		}
	}
	return false
}

// matchPattern implements IMAP wildcards.
//
// '*' matches anything including the hierarchy delimiter; '%' matches anything
// except it, which is how a client asks for one level of the tree at a time.
func matchPattern(name, pattern string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*%") {
		return strings.EqualFold(name, pattern)
	}
	if !strings.Contains(pattern, "*") {
		// Only '%' present: fall back to path matching, which stops at the
		// separator exactly as '%' should.
		matched, err := path.Match(strings.ReplaceAll(pattern, "%", "*"), name)
		if err != nil {
			return false
		}
		return matched && strings.Count(name, string(delimiter)) ==
			strings.Count(pattern, string(delimiter))
	}
	matched, err := path.Match(strings.ReplaceAll(pattern, "*", "*"), name)
	if err != nil {
		return false
	}
	return matched
}

func attributesFor(box store.Mailbox) []imap.MailboxAttr {
	var attrs []imap.MailboxAttr
	if box.SpecialUse != nil && *box.SpecialUse != "" {
		attrs = append(attrs, imap.MailboxAttr(*box.SpecialUse))
	}
	// Every mirrored mailbox is a leaf that can hold messages.
	attrs = append(attrs, imap.MailboxAttrHasNoChildren)
	return attrs
}

// Status answers a client's STATUS request from the cached counts.
func (s *session) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	ctx, cancel := s.ctx()
	defer cancel()

	boxes, err := s.backend.store.ListMailboxes(ctx, s.account.ID)
	if err != nil {
		return nil, internalError(err)
	}
	for _, box := range boxes {
		if strings.EqualFold(box.Name, mailbox) {
			return statusFor(box, options), nil
		}
	}
	return nil, &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeNonExistent,
		Text: "no such mailbox",
	}
}

func statusFor(box store.Mailbox, options *imap.StatusOptions) *imap.StatusData {
	data := &imap.StatusData{Mailbox: box.Name}
	if options == nil {
		return data
	}
	if options.NumMessages {
		count := uint32(box.ExistsCount)
		data.NumMessages = &count
	}
	if options.NumUnseen {
		unseen := uint32(box.UnseenCount)
		data.NumUnseen = &unseen
	}
	if options.UIDNext {
		data.UIDNext = imap.UID(box.LocalUIDNext)
	}
	if options.UIDValidity {
		data.UIDValidity = uidValidity(box)
	}
	return data
}

// envelopeFor builds the envelope a client expects.
//
// Parsed from the stored message where available so the values match exactly
// what the client would compute itself; the summary provides a fallback for a
// message whose body has not been downloaded.
func envelopeFor(summary store.MessageSummary, raw []byte) *imap.Envelope {
	envelope := &imap.Envelope{Subject: summary.Subject}
	if summary.InternalDate != nil {
		envelope.Date = *summary.InternalDate
	}

	if len(raw) > 0 {
		parsed := mailstore.Parse(raw)
		if parsed.Subject != "" {
			envelope.Subject = parsed.Subject
		}
		if !parsed.Date.IsZero() {
			envelope.Date = parsed.Date
		}
		envelope.From = toAddresses(parsed.From)
		envelope.To = toAddresses(parsed.To)
		envelope.Cc = toAddresses(parsed.Cc)
		envelope.ReplyTo = toAddresses(parsed.ReplyTo)
		if parsed.MessageID != "" {
			envelope.MessageID = "<" + parsed.MessageID + ">"
		}
		if parsed.InReplyTo != "" {
			envelope.InReplyTo = []string{"<" + parsed.InReplyTo + ">"}
		}
		return envelope
	}

	envelope.From = toAddresses(summary.From)
	return envelope
}

// toAddresses converts display strings back into structured addresses.
func toAddresses(values []string) []imap.Address {
	out := make([]imap.Address, 0, len(values))
	for _, value := range values {
		name, addr := splitAddress(value)
		mailbox, host, found := strings.Cut(addr, "@")
		if !found {
			mailbox, host = addr, ""
		}
		out = append(out, imap.Address{Name: name, Mailbox: mailbox, Host: host})
	}
	return out
}

func splitAddress(value string) (name, addr string) {
	if start := strings.LastIndex(value, "<"); start >= 0 {
		if end := strings.LastIndex(value, ">"); end > start {
			return strings.TrimSpace(value[:start]), value[start+1 : end]
		}
	}
	return "", strings.TrimSpace(value)
}

// bodyStructureFor describes the message structure.
//
// A single text part is reported rather than the true MIME tree. Clients use
// the structure to decide what to request; reporting one text part means they
// ask for the whole message, which is already stored locally and therefore
// costs nothing extra to serve. A faithful tree would be better but is only
// worth building once selective part fetching is worth optimising.
func bodyStructureFor(raw []byte, size int64) imap.BodyStructure {
	if size <= 0 {
		size = int64(len(raw))
	}
	lines := int64(strings.Count(string(raw), "\n"))
	return &imap.BodyStructureSinglePart{
		Type:    "text",
		Subtype: "plain",
		Params:  map[string]string{"charset": "utf-8"},
		Size:    uint32(size),
		Text:    &imap.BodyStructureText{NumLines: lines},
	}
}

// Namespace reports the mailbox namespaces available to the client.
//
// One personal namespace with no prefix: every mirrored mailbox sits at the top
// level, and there are no shared or public namespaces to expose.
func (s *session) Namespace() (*imap.NamespaceData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: delimiter}},
	}, nil
}
