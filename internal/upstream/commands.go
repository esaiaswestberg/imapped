package upstream

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// MailboxInfo describes a mailbox as the server reports it in LIST.
type MailboxInfo struct {
	Name       string
	Delimiter  rune
	Attributes []imap.MailboxAttr
}

// SpecialUse returns the mailbox's role attribute, if any.
func (m MailboxInfo) SpecialUse() string {
	for _, attr := range m.Attributes {
		switch attr {
		case imap.MailboxAttrSent, imap.MailboxAttrDrafts, imap.MailboxAttrTrash,
			imap.MailboxAttrJunk, imap.MailboxAttrArchive, imap.MailboxAttrAll,
			imap.MailboxAttrFlagged:
			return string(attr)
		}
	}
	return ""
}

// Selectable reports whether the mailbox can hold messages. Some servers expose
// pure container nodes that cannot be selected.
func (m MailboxInfo) Selectable() bool {
	for _, attr := range m.Attributes {
		if attr == imap.MailboxAttrNoSelect || attr == imap.MailboxAttrNonExistent {
			return false
		}
	}
	return true
}

// ListMailboxes enumerates every mailbox on the server.
func (c *Client) ListMailboxes(ctx context.Context) ([]MailboxInfo, error) {
	if err := c.checkUsable(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CommandTimeout.Std())
	defer cancel()

	var out []MailboxInfo
	err := c.withDeadline(ctx, func() error {
		entries, err := c.imap.List("", "*", &imap.ListOptions{}).Collect()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			out = append(out, MailboxInfo{
				Name:       entry.Mailbox,
				Delimiter:  entry.Delim,
				Attributes: entry.Attrs,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing mailboxes: %w", classify(err))
	}
	return out, nil
}

// SelectedMailbox is the state reported by SELECT.
type SelectedMailbox struct {
	Name          string
	NumMessages   uint32
	UIDValidity   uint32
	UIDNext       imap.UID
	HighestModSeq uint64
	ReadOnly      bool
}

// Select opens a mailbox for reading.
func (c *Client) Select(ctx context.Context, name string, readOnly bool) (*SelectedMailbox, error) {
	if err := c.checkUsable(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CommandTimeout.Std())
	defer cancel()

	var out *SelectedMailbox
	err := c.withDeadline(ctx, func() error {
		data, err := c.imap.Select(name, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
		if err != nil {
			return err
		}
		out = &SelectedMailbox{
			Name:        name,
			NumMessages: data.NumMessages,
			UIDValidity: data.UIDValidity,
			UIDNext:     data.UIDNext,
			ReadOnly:    readOnly,
		}
		if data.HighestModSeq != 0 {
			out.HighestModSeq = data.HighestModSeq
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("selecting mailbox %q: %w", name, classify(err))
	}
	return out, nil
}

// MessageMeta is everything the metadata pass learns about a message without
// downloading its body.
type MessageMeta struct {
	UID           imap.UID
	Flags         []imap.Flag
	Size          int64
	InternalDate  timeValue
	Envelope      *imap.Envelope
	BodyStructure imap.BodyStructure
	ModSeq        uint64
}

// FetchMetadata retrieves metadata for a UID set in a single command.
//
// This is the heart of the performance fix. The previous implementation issued
// one FETCH per message and did so on every sync, giving O(mailbox size)
// round trips forever. Here an entire mailbox — or an entire changed subset —
// is one command whose untagged responses are consumed as they stream in.
//
// changedSince, when non-zero, adds CONDSTORE's CHANGEDSINCE modifier so the
// server sends only messages whose flags moved since that modification
// sequence. On an unchanged mailbox that reduces the whole pass to nothing.
func (c *Client) FetchMetadata(ctx context.Context, uids imap.NumSet, changedSince uint64, full bool) ([]MessageMeta, error) {
	if err := c.checkUsable(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.FetchMetadataTimeout.Std())
	defer cancel()

	options := &imap.FetchOptions{
		UID:   true,
		Flags: true,
	}
	if full {
		// A first sighting needs everything needed to build a database row.
		options.RFC822Size = true
		options.InternalDate = true
		options.Envelope = true
		options.BodyStructure = &imap.FetchItemBodyStructure{}
	}
	if changedSince > 0 {
		options.ChangedSince = changedSince
		options.ModSeq = true
	}

	var out []MessageMeta
	err := c.withDeadline(ctx, func() error {
		cmd := c.imap.Fetch(uids, options)
		// Close drains any unread response data. Leaving even one literal
		// half-read desynchronises the connection, so this must happen on every
		// path out of the function, including errors.
		defer cmd.Close()

		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			meta, err := collectMeta(msg)
			if err != nil {
				return err
			}
			out = append(out, meta)
		}
		return cmd.Close()
	})
	if err != nil {
		return nil, fmt.Errorf("fetching metadata: %w", classify(err))
	}
	return out, nil
}

func collectMeta(msg *imapclient.FetchMessageData) (MessageMeta, error) {
	buf, err := msg.Collect()
	if err != nil {
		return MessageMeta{}, err
	}
	meta := MessageMeta{
		UID:           buf.UID,
		Flags:         buf.Flags,
		Size:          buf.RFC822Size,
		Envelope:      buf.Envelope,
		BodyStructure: buf.BodyStructure,
		ModSeq:        buf.ModSeq,
	}
	if !buf.InternalDate.IsZero() {
		meta.InternalDate = timeValue{Time: buf.InternalDate, Valid: true}
	}
	return meta, nil
}

// SearchAllUIDs returns every UID in the selected mailbox.
//
// Used only for deletion detection on servers without QRESYNC, and only when
// the local message count disagrees with the server's, since it is the one
// remaining operation whose cost scales with mailbox size.
func (c *Client) SearchAllUIDs(ctx context.Context) ([]imap.UID, error) {
	if err := c.checkUsable(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.FetchMetadataTimeout.Std())
	defer cancel()

	var uids []imap.UID
	err := c.withDeadline(ctx, func() error {
		data, err := c.imap.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true}).Wait()
		if err != nil {
			return err
		}
		uids = data.AllUIDs()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("searching for all UIDs: %w", classify(err))
	}
	return uids, nil
}

// StoreFlags replaces the flags on a message upstream.
//
// FLAGS.SILENT rather than FLAGS: we already know the resulting state, and
// suppressing the untagged response avoids a round trip's worth of data we
// would only discard.
func (c *Client) StoreFlags(ctx context.Context, uid imap.UID, flags []imap.Flag) error {
	if err := c.checkUsable(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CommandTimeout.Std())
	defer cancel()

	var set imap.UIDSet
	set.AddNum(uid)

	err := c.withDeadline(ctx, func() error {
		cmd := c.imap.Store(set, &imap.StoreFlags{
			Op:     imap.StoreFlagsSet,
			Flags:  flags,
			Silent: true,
		}, nil)
		// Even a silent STORE returns a command completion that must be
		// consumed before the connection can be reused.
		return cmd.Close()
	})
	if err != nil {
		return fmt.Errorf("setting flags on UID %d: %w", uid, classify(err))
	}
	return nil
}
