package syncer

import (
	"encoding/json"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// A client shows the display name, so it has to survive the metadata pass. It
// is the only name available for a message whose body has not arrived.
func TestBuildMetaKeepsDisplayNames(t *testing.T) {
	meta := buildMeta(1, 2, upstream.MessageMeta{
		UID: 7,
		Envelope: &imap.Envelope{
			From: []imap.Address{{Name: "YouTube", Mailbox: "no-reply", Host: "youtube.com"}},
			To: []imap.Address{
				{Name: "Westberg, Esaias", Mailbox: "esaias", Host: "example.com"},
				{Mailbox: "plain", Host: "example.com"},
			},
		},
	})

	var addrs map[string][]string
	if err := json.Unmarshal(meta.Addrs, &addrs); err != nil {
		t.Fatalf("decoding addresses: %v", err)
	}

	if got, want := addrs["from"][0], "YouTube <no-reply@youtube.com>"; got != want {
		t.Errorf("from is %q, want %q", got, want)
	}
	// A comma in a display name must be quoted, or it reads as two recipients.
	if got, want := addrs["to"][0], `"Westberg, Esaias" <esaias@example.com>`; got != want {
		t.Errorf("to[0] is %q, want %q", got, want)
	}
	if got, want := addrs["to"][1], "plain@example.com"; got != want {
		t.Errorf("to[1] is %q, want %q", got, want)
	}
}
