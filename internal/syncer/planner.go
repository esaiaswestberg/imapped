// Package syncer mirrors upstream mailboxes into local storage.
package syncer

import (
	"github.com/emersion/go-imap/v2"
)

// SizedUID pairs a message UID with its known size.
type SizedUID struct {
	UID  imap.UID
	Size int64
}

// BatchBudget bounds how much work one FETCH command may carry.
type BatchBudget struct {
	// MaxBytes is the preferred ceiling on the total size of a batch.
	MaxBytes int64
	// MaxMessages caps the number of messages regardless of their size.
	MaxMessages int
	// SoloAbove sends any message larger than this in a batch of its own, so a
	// single large attachment cannot hold up the small messages behind it.
	SoloAbove int64
}

// PlanBatches groups messages into FETCH batches.
//
// This is deliberately a pure function over sizes: the hardest logic in the
// body-fetch path is decided here, and keeping it free of connections and
// databases means it can be tested exhaustively without either.
//
// Batching by byte budget rather than message count is what keeps memory and
// latency predictable across wildly different mailboxes. A thousand 2KB
// notifications and ten 4MB attachments both produce sensibly sized batches,
// whereas a fixed count would either waste round trips on the former or buffer
// far too much for the latter.
//
// Input order is preserved, so a caller that supplies newest-first UIDs gets
// newest-first batches and recent mail lands before an old archive completes.
func PlanBatches(msgs []SizedUID, budget BatchBudget) [][]SizedUID {
	if len(msgs) == 0 {
		return nil
	}
	if budget.MaxMessages < 1 {
		budget.MaxMessages = 1
	}

	var (
		batches [][]SizedUID
		current []SizedUID
		bytes   int64
	)

	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current, bytes = nil, 0
		}
	}

	for _, msg := range msgs {
		// An outsized message gets its own command, so the rest of the queue is
		// not stuck behind its transfer.
		if budget.SoloAbove > 0 && msg.Size > budget.SoloAbove {
			flush()
			batches = append(batches, []SizedUID{msg})
			continue
		}

		// Adding this message would breach the budget, so close the batch first.
		// A message is never split across batches, and a batch is never empty:
		// one that exceeds the budget on its own still goes out alone.
		if len(current) > 0 {
			overBytes := budget.MaxBytes > 0 && bytes+msg.Size > budget.MaxBytes
			overCount := len(current) >= budget.MaxMessages
			if overBytes || overCount {
				flush()
			}
		}

		current = append(current, msg)
		bytes += msg.Size
	}
	flush()

	return batches
}

// UIDSet converts a batch into the set to send in one FETCH command.
//
// go-imap collapses consecutive UIDs into ranges when rendering, so a
// contiguous batch becomes "100:149" rather than fifty comma-separated numbers.
func UIDSet(batch []SizedUID) imap.UIDSet {
	var set imap.UIDSet
	for _, msg := range batch {
		set.AddNum(msg.UID)
	}
	return set
}

// UIDRange builds the open-ended range from start upwards, used by the metadata
// pass to enumerate everything at or above a UID in a single command.
func UIDRange(start imap.UID) imap.UIDSet {
	var set imap.UIDSet
	set.AddRange(start, 0) // 0 means "*", i.e. the highest UID in the mailbox
	return set
}

// ChunkUIDRange splits a large enumeration into bounded ranges.
//
// A mailbox with hundreds of thousands of messages produces a response too
// large to treat as one unit of work: chunking means a failure costs one chunk
// rather than the whole pass, and lets the checkpoint advance as it goes.
func ChunkUIDRange(start, end imap.UID, chunk int) []imap.UIDSet {
	if chunk < 1 || end < start {
		return []imap.UIDSet{UIDRange(start)}
	}

	var sets []imap.UIDSet
	for from := start; from <= end; from += imap.UID(chunk) {
		to := from + imap.UID(chunk) - 1
		if to > end {
			to = end
		}
		var set imap.UIDSet
		set.AddRange(from, to)
		sets = append(sets, set)
		if to == end {
			break
		}
	}
	return sets
}

// PlanMetadataChunks groups a UID list into fetch-sized sets.
//
// Chunking by message count rather than by UID span is what makes the size of
// each command predictable. A UID range says nothing about how many messages it
// contains once deletions have left holes — a mailbox numbered to 8000 might
// hold 200 messages or 8000 — so a span-based chunk can be arbitrarily large or
// pointlessly small.
//
// Bounded commands matter for three reasons: a checkpoint after each one, so an
// interrupted pass loses at most one chunk; progress the user can see; and a
// response that completes even when the server is slow.
func PlanMetadataChunks(uids []imap.UID, size int) []imap.UIDSet {
	if len(uids) == 0 {
		return nil
	}
	if size < 1 {
		size = 1
	}

	var sets []imap.UIDSet
	for start := 0; start < len(uids); start += size {
		end := min(start+size, len(uids))

		// AddNum collapses consecutive values, so a contiguous chunk goes on
		// the wire as "1:500" rather than five hundred comma-separated numbers.
		var set imap.UIDSet
		for _, uid := range uids[start:end] {
			set.AddNum(uid)
		}
		sets = append(sets, set)
	}
	return sets
}
