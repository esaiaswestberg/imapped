//go:build live

package upstream_test

import (
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

// TestLiveBodyThroughputByWorkerCount measures whether downloading bodies goes
// faster with more connections, or whether the provider throttles total
// bandwidth and parallelism buys nothing.
//
// Read-only: bodies are fetched with BODY.PEEK and discarded.
//
// Run this only when no sync is active. It opens up to eight connections and
// pulls real message bodies, so against a provider that is already working hard
// it competes with the thing it is meant to be measuring — and reports the
// contention rather than the capacity. An attempt during a live sync could not
// even complete a login inside two minutes.
func TestLiveBodyThroughputByWorkerCount(t *testing.T) {
	if os.Getenv("IMAPPED_LIVE_THROUGHPUT") == "" {
		t.Skip("set IMAPPED_LIVE_THROUGHPUT=1, and only when no sync is running")
	}

	account := liveAccount(t)
	connector := upstream.NewConnector(liveConfig(), logging.Discard())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Find a window of UIDs to read repeatedly.
	probe, err := connector.Connect(ctx, account)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	selected, err := probe.Select(ctx, "INBOX", true)
	if err != nil {
		t.Fatalf("selecting: %v", err)
	}
	_ = probe.Close()

	if selected.UIDNext < 200 {
		t.Skip("mailbox too small to measure")
	}

	// A different slice per worker count, so server-side caching of one window
	// does not flatter a later run.
	const perWorkerSpan = 40

	for _, workers := range []int{1, 2, 4, 8} {
		start := time.Now()

		var (
			messages atomic.Int64
			bytes    atomic.Int64
			wg       sync.WaitGroup
			failures atomic.Int64
		)

		base := selected.UIDNext - imap.UID(workers*perWorkerSpan) - 1
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()

				client, err := connector.Connect(ctx, account)
				if err != nil {
					failures.Add(1)
					return
				}
				defer client.Close()

				if _, err := client.Select(ctx, "INBOX", true); err != nil {
					failures.Add(1)
					return
				}

				lo := base + imap.UID(w*perWorkerSpan)
				var set imap.UIDSet
				set.AddRange(lo, lo+perWorkerSpan-1)

				err = client.FetchBodies(ctx, set, 0, func(_ imap.UID, _ int64, body io.Reader) error {
					n, err := io.Copy(io.Discard, body)
					if err != nil {
						return err
					}
					messages.Add(1)
					bytes.Add(n)
					return nil
				})
				if err != nil {
					failures.Add(1)
				}
			}()
		}
		wg.Wait()

		elapsed := time.Since(start)
		got, transferred := messages.Load(), bytes.Load()
		if got == 0 {
			t.Logf("%d worker(s): no messages fetched (%d failures)", workers, failures.Load())
			continue
		}

		perSecond := float64(got) / elapsed.Seconds()
		kib := float64(transferred) / 1024 / elapsed.Seconds()
		t.Logf("%d worker(s): %3d messages in %-8s => %5.2f msg/s, %6.1f KiB/s (%d failures)",
			workers, got, elapsed.Round(time.Millisecond), perSecond, kib, failures.Load())
	}
}
