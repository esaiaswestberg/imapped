package syncer_test

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/syncer"
)

func sized(sizes ...int64) []syncer.SizedUID {
	msgs := make([]syncer.SizedUID, len(sizes))
	for i, size := range sizes {
		msgs[i] = syncer.SizedUID{UID: imap.UID(i + 1), Size: size}
	}
	return msgs
}

func totalBytes(batch []syncer.SizedUID) int64 {
	var n int64
	for _, m := range batch {
		n += m.Size
	}
	return n
}

func TestPlanBatchesGroupsByByteBudget(t *testing.T) {
	budget := syncer.BatchBudget{MaxBytes: 1000, MaxMessages: 100}
	// Four messages of 400 bytes: 1000 bytes fits two, so expect two batches.
	batches := syncer.PlanBatches(sized(400, 400, 400, 400), budget)

	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2: %v", len(batches), batches)
	}
	for i, batch := range batches {
		if got := totalBytes(batch); got > budget.MaxBytes {
			t.Errorf("batch %d holds %d bytes, exceeding the %d budget", i, got, budget.MaxBytes)
		}
	}
}

func TestPlanBatchesRespectsMessageCount(t *testing.T) {
	// Tiny messages: the count limit binds before the byte budget does.
	budget := syncer.BatchBudget{MaxBytes: 1 << 20, MaxMessages: 3}
	batches := syncer.PlanBatches(sized(10, 10, 10, 10, 10, 10, 10), budget)

	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3", len(batches))
	}
	for i, batch := range batches {
		if len(batch) > 3 {
			t.Errorf("batch %d holds %d messages, exceeding the limit of 3", i, len(batch))
		}
	}
}

// A single large attachment must not delay the small messages queued behind it.
func TestPlanBatchesIsolatesOversizedMessages(t *testing.T) {
	budget := syncer.BatchBudget{MaxBytes: 10_000, MaxMessages: 50, SoloAbove: 5_000}
	batches := syncer.PlanBatches(sized(100, 100, 50_000, 100, 100), budget)

	var solo []syncer.SizedUID
	for _, batch := range batches {
		if len(batch) == 1 && batch[0].Size == 50_000 {
			solo = batch
		}
		if len(batch) > 1 {
			for _, msg := range batch {
				if msg.Size > budget.SoloAbove {
					t.Errorf("oversized message %d was batched with %d others",
						msg.UID, len(batch)-1)
				}
			}
		}
	}
	if solo == nil {
		t.Error("the oversized message did not get a batch of its own")
	}
}

// A message larger than the whole budget still has to be fetched; it goes out
// alone rather than being dropped or splitting the message across commands.
func TestPlanBatchesHandlesMessageLargerThanBudget(t *testing.T) {
	budget := syncer.BatchBudget{MaxBytes: 1000, MaxMessages: 10}
	batches := syncer.PlanBatches(sized(5000), budget)

	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("expected one batch of one message, got %v", batches)
	}
}

func TestPlanBatchesPreservesOrder(t *testing.T) {
	budget := syncer.BatchBudget{MaxBytes: 250, MaxMessages: 2}
	batches := syncer.PlanBatches(sized(100, 100, 100, 100, 100), budget)

	var seen []imap.UID
	for _, batch := range batches {
		for _, msg := range batch {
			seen = append(seen, msg.UID)
		}
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("batching reordered messages: %v", seen)
		}
	}
	if len(seen) != 5 {
		t.Errorf("planned %d messages, want 5", len(seen))
	}
}

func TestPlanBatchesLosesNothing(t *testing.T) {
	sizes := make([]int64, 500)
	for i := range sizes {
		sizes[i] = int64(100 + (i%17)*300)
	}
	msgs := sized(sizes...)

	budget := syncer.BatchBudget{MaxBytes: 4 << 20, MaxMessages: 50, SoloAbove: 32 << 20}
	batches := syncer.PlanBatches(msgs, budget)

	seen := make(map[imap.UID]int)
	for _, batch := range batches {
		if len(batch) == 0 {
			t.Error("an empty batch was produced, which would send a FETCH for nothing")
		}
		for _, msg := range batch {
			seen[msg.UID]++
		}
	}
	if len(seen) != len(msgs) {
		t.Errorf("planned %d distinct messages, want %d", len(seen), len(msgs))
	}
	for uid, count := range seen {
		if count != 1 {
			t.Errorf("UID %d appears in %d batches; a duplicate fetch wastes bandwidth", uid, count)
		}
	}
}

func TestPlanBatchesEmptyInput(t *testing.T) {
	if batches := syncer.PlanBatches(nil, syncer.BatchBudget{MaxBytes: 100, MaxMessages: 10}); batches != nil {
		t.Errorf("expected no batches for no input, got %v", batches)
	}
}

// Realistic mailbox shape: the batch count is what determines how many round
// trips a full sync costs, and it must stay far below one-per-message.
func TestPlanBatchesKeepsCommandCountLow(t *testing.T) {
	const messageCount = 8300

	sizes := make([]int64, messageCount)
	for i := range sizes {
		sizes[i] = 60_000 // roughly the average size of a real message
		if i%50 == 49 {
			sizes[i] = 2 << 20
		}
	}

	budget := syncer.BatchBudget{MaxBytes: 4 << 20, MaxMessages: 50, SoloAbove: 32 << 20}
	batches := syncer.PlanBatches(sized(sizes...), budget)

	// The previous implementation issued two commands per message: 16,600 for
	// this mailbox, every sync. Anything near that is a regression.
	if len(batches) > 400 {
		t.Errorf("planning %d messages produced %d batches, want at most 400",
			messageCount, len(batches))
	}
	t.Logf("%d messages planned into %d batches (%.1f messages per command)",
		messageCount, len(batches), float64(messageCount)/float64(len(batches)))
}

func TestUIDSetCollapsesConsecutiveUIDs(t *testing.T) {
	batch := make([]syncer.SizedUID, 0, 50)
	for uid := 100; uid < 150; uid++ {
		batch = append(batch, syncer.SizedUID{UID: imap.UID(uid), Size: 10})
	}

	set := syncer.UIDSet(batch)
	// Contiguous UIDs should render as a range, not fifty comma-separated
	// numbers, which keeps the command line short on large batches.
	if got := set.String(); got != "100:149" {
		t.Errorf("UIDSet rendered %q, want %q", got, "100:149")
	}
}

func TestChunkUIDRange(t *testing.T) {
	chunks := syncer.ChunkUIDRange(1, 100, 30)
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4: %v", len(chunks), chunks)
	}

	// Chunks must tile the range exactly: no gaps (messages would be missed)
	// and no overlaps (messages would be fetched twice).
	covered := make(map[imap.UID]int)
	for _, chunk := range chunks {
		for uid := imap.UID(1); uid <= 100; uid++ {
			if chunk.Contains(uid) {
				covered[uid]++
			}
		}
	}
	for uid := imap.UID(1); uid <= 100; uid++ {
		switch covered[uid] {
		case 1:
		case 0:
			t.Fatalf("UID %d is not covered by any chunk", uid)
		default:
			t.Fatalf("UID %d is covered by %d chunks", uid, covered[uid])
		}
	}
}
