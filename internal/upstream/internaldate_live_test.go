//go:build live

package upstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestLiveInternalDateIsReturned checks that the real server answers
// INTERNALDATE for the metadata field set the sync pass uses. A missing date is
// what leaves a mail client stamping every message with the time it was listed.
func TestLiveInternalDateIsReturned(t *testing.T) {
	client := liveConnect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	boxes, err := client.ListMailboxes(ctx)
	if err != nil {
		t.Fatalf("listing mailboxes: %v", err)
	}
	// The smallest mailbox, because selecting a large one on this server takes
	// minutes and the question here is about the shape of the response, not
	// about how long a mailbox takes to open.
	name := ""
	for _, box := range boxes {
		if box.Selectable() && (name == "" || box.Name < name) {
			name = box.Name
		}
	}
	if name == "" {
		t.Skip("no selectable mailbox")
	}
	t.Logf("using mailbox %q", name)
	if _, err := client.Select(ctx, name, true); err != nil {
		t.Fatalf("selecting %s: %v", name, err)
	}

	var set imap.UIDSet
	set.AddRange(1, 20)

	metas, err := client.FetchMetadata(ctx, set, 0, true)
	if err != nil {
		t.Fatalf("fetching metadata: %v", err)
	}
	t.Logf("fetched %d messages", len(metas))

	missing := 0
	for _, meta := range metas {
		internal := meta.InternalDate.Or(time.Time{})
		var envelopeDate time.Time
		if meta.Envelope != nil {
			envelopeDate = meta.Envelope.Date
		}
		if internal.IsZero() {
			missing++
		}
		t.Logf("uid=%d internaldate=%q envelope.date=%q size=%d",
			meta.UID, internal.Format(time.RFC1123Z), envelopeDate.Format(time.RFC1123Z), meta.Size)
	}
	if missing > 0 {
		t.Errorf("%d of %d messages came back with no INTERNALDATE", missing, len(metas))
	}
}
