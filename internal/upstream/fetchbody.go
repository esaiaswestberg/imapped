package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/emersion/go-imap/v2"
)

// ErrMessageTooLarge is reported when a body exceeds the configured ceiling.
var ErrMessageTooLarge = errors.New("message exceeds the maximum size")

// BodyHandler receives one message body as a stream.
//
// The reader is only valid for the duration of the call and must be consumed
// fully; the connection cannot proceed to the next message until it is drained.
type BodyHandler func(uid imap.UID, size int64, body io.Reader) error

// FetchBodies downloads the bodies for a UID set, invoking handler per message.
//
// Bodies are streamed rather than buffered. go-imap offers Collect() which
// materialises every message in the response, but a 40MB message would then
// cost 40MB of resident memory per worker, and a batch of them multiplies that
// by the pool size. Handing the caller an io.Reader keeps peak memory at one
// buffer regardless of message size, which is what makes it safe to run several
// fetch workers in parallel.
//
// maxSize bounds what will be read from any single message; a larger one is
// reported to the handler as ErrMessageTooLarge without consuming the whole
// body into memory.
func (c *Client) FetchBodies(ctx context.Context, uids imap.NumSet, maxSize int64, handler BodyHandler) error {
	if err := c.checkUsable(); err != nil {
		return err
	}

	// Scale the deadline with the batch: a fixed timeout would abort a
	// legitimately large batch on a slow link, while the per-read inactivity
	// window still catches a genuinely stalled transfer within seconds.
	timeout := c.cfg.FetchBodyTimeout.Std()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	options := &imap.FetchOptions{
		UID:        true,
		RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{
			// Peek so the server does not implicitly set \Seen: mirroring mail
			// must not change its read state.
			{Peek: true},
		},
	}

	return c.withDeadline(ctx, func() error {
		cmd := c.imap.Fetch(uids, options)
		defer cmd.Close()

		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			if err := c.handleBodyMessage(msg, maxSize, handler); err != nil {
				return err
			}
		}
		if err := cmd.Close(); err != nil {
			return fmt.Errorf("fetching bodies: %w", classify(err))
		}
		return nil
	})
}

// handleBodyMessage consumes exactly one message's response items.
//
// Every item must be drained even if the caller does not want it, because the
// items share one connection: an unread literal leaves unconsumed octets in the
// stream, and the next command's responses would then be parsed from the middle
// of this message's body. That is precisely the class of desynchronisation that
// produces an unexplained hang, so draining is unconditional.
func (c *Client) handleBodyMessage(msg fetchMessage, maxSize int64, handler BodyHandler) error {
	var (
		uid      imap.UID
		size     int64
		handled  bool
		handlErr error
	)

	for {
		item := msg.Next()
		if item == nil {
			break
		}
		switch data := item.(type) {
		case imapFetchUID:
			uid = data.UID
		case imapFetchRFC822Size:
			size = data.Size
		case imapFetchBodySection:
			if data.Literal == nil {
				continue
			}
			if maxSize > 0 && data.Literal.Size() > maxSize {
				// Drain and discard: the message stays on the server and is
				// recorded locally as too large, rather than blocking the batch.
				_, _ = io.Copy(io.Discard, data.Literal)
				handled = true
				handlErr = fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, data.Literal.Size())
				continue
			}
			if size == 0 {
				size = data.Literal.Size()
			}
			// LimitReader is a second line of defence against a server that
			// announces one literal size and sends a different one.
			limited := io.LimitReader(data.Literal, data.Literal.Size())
			err := handler(uid, data.Literal.Size(), limited)
			// Drain whatever the handler left, so the connection stays in sync
			// even when the handler stops early or fails.
			_, _ = io.Copy(io.Discard, data.Literal)
			handled = true
			if err != nil {
				handlErr = err
			}
		}
	}

	if !handled && handlErr == nil {
		// A UID with no body section means the message vanished between the
		// metadata pass and now, which is normal and not an error.
		return nil
	}
	return handlErr
}
