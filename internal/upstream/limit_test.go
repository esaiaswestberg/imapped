//go:build live

package upstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// TestLiveConcurrentConnectionLimit measures how many simultaneous connections
// the provider actually permits, so sync.connections_per_account can be set
// from a measurement rather than a guess.
//
// Read-only: each connection logs in, selects INBOX read-only, and waits.
func TestLiveConcurrentConnectionLimit(t *testing.T) {
	account := liveAccount(t)
	connector := upstream.NewConnector(liveConfig(), logging.Discard())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var open []*upstream.Client
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()

	const ceiling = 12
	for i := 1; i <= ceiling; i++ {
		client, err := connector.Connect(ctx, account)
		if err != nil {
			t.Logf("connection %d refused: %v", i, err)
			if upstream.IsTooManyConnections(err) {
				t.Logf("provider reports a connection limit at %d simultaneous connections", i-1)
			}
			break
		}
		if _, err := client.Select(ctx, "INBOX", true); err != nil {
			t.Logf("connection %d opened but could not select INBOX: %v", i, err)
			_ = client.Close()
			break
		}
		open = append(open, client)
		t.Logf("connection %d: open and usable", i)
	}

	t.Logf("held %d simultaneous connections", len(open))
	if len(open) >= 2 {
		t.Logf("sync.connections_per_account can be set to %d "+
			"(one is used for enumeration, the rest download bodies)", len(open))
	}
	time.Sleep(2 * time.Second)
}
