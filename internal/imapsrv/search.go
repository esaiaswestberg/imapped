package imapsrv

import (
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/esaiaswestberg/imapped/internal/store"
)

// Search evaluates a client's SEARCH criteria against the selected mailbox.
//
// Evaluated in memory against the snapshot rather than translated into SQL.
// The criteria form a recursive tree with negation and alternation, and
// building that into a query would be a large amount of code for a command
// clients issue rarely and over a mailbox whose metadata is already loaded.
func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria,
	options *imap.SearchOptions) (*imap.SearchData, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireMailbox(); err != nil {
		return nil, err
	}

	data := &imap.SearchData{}

	var (
		seqNums []uint32
		uids    []imap.UID
	)
	for i, message := range s.snapshot {
		if !matchesCriteria(message, uint32(i+1), criteria) {
			continue
		}
		seqNums = append(seqNums, uint32(i+1))
		uids = append(uids, imap.UID(message.LocalUID))
	}

	if kind == imapserver.NumKindUID {
		var set imap.UIDSet
		for _, uid := range uids {
			set.AddNum(uid)
		}
		data.All = set
	} else {
		var set imap.SeqSet
		for _, seqNum := range seqNums {
			set.AddNum(seqNum)
		}
		data.All = set
	}

	data.Count = uint32(len(seqNums))
	if len(seqNums) > 0 {
		data.Min = seqNums[0]
		data.Max = seqNums[len(seqNums)-1]
	}
	return data, nil
}

// matchesCriteria evaluates one message against a criteria tree.
//
// IMAP criteria are conjunctive at each level: every listed condition must
// hold, with Not and Or providing negation and alternation.
func matchesCriteria(m store.MessageSummary, seqNum uint32, c *imap.SearchCriteria) bool {
	if c == nil {
		return true
	}

	for _, set := range c.SeqNum {
		if !set.Contains(seqNum) {
			return false
		}
	}
	for _, set := range c.UID {
		if !set.Contains(imap.UID(m.LocalUID)) {
			return false
		}
	}

	for _, flag := range c.Flag {
		if !hasFlag(m.Flags, string(flag)) {
			return false
		}
	}
	for _, flag := range c.NotFlag {
		if hasFlag(m.Flags, string(flag)) {
			return false
		}
	}

	if c.Larger > 0 && m.Size <= c.Larger {
		return false
	}
	if c.Smaller > 0 && m.Size >= c.Smaller {
		return false
	}

	if !c.Since.IsZero() && !dateAtLeast(m.InternalDate, c.Since) {
		return false
	}
	if !c.Before.IsZero() && !dateBefore(m.InternalDate, c.Before) {
		return false
	}

	// Header and body text searches use the summary's subject, sender and
	// preview. A full-body match belongs in the search index, which the web
	// interface exposes; replicating it here would mean loading every message.
	for _, header := range c.Header {
		if !headerMatches(m, header.Key, header.Value) {
			return false
		}
	}
	for _, text := range c.Body {
		if !containsFold(m.Preview, text) {
			return false
		}
	}
	for _, text := range c.Text {
		if !containsFold(m.Subject, text) && !containsFold(m.Preview, text) &&
			!containsFold(strings.Join(m.From, " "), text) {
			return false
		}
	}

	for _, not := range c.Not {
		if matchesCriteria(m, seqNum, &not) {
			return false
		}
	}
	for _, or := range c.Or {
		if !matchesCriteria(m, seqNum, &or[0]) && !matchesCriteria(m, seqNum, &or[1]) {
			return false
		}
	}
	return true
}

func headerMatches(m store.MessageSummary, key, value string) bool {
	switch strings.ToLower(key) {
	case "subject":
		return containsFold(m.Subject, value)
	case "from":
		return containsFold(strings.Join(m.From, " "), value)
	default:
		// An unindexed header cannot be matched without loading the message;
		// treating it as a match would return wrong results, so it does not.
		return false
	}
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func hasFlag(flags []string, want string) bool {
	return slices.ContainsFunc(flags, func(flag string) bool {
		return strings.EqualFold(flag, want)
	})
}

func dateAtLeast(date *time.Time, bound time.Time) bool {
	return date != nil && !date.Before(bound)
}

func dateBefore(date *time.Time, bound time.Time) bool {
	return date != nil && date.Before(bound)
}
