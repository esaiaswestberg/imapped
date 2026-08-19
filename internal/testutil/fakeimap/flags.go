package fakeimap

import "github.com/emersion/go-imap/v2"

type imapFlag = imap.Flag

const (
	seenFlag    = imap.FlagSeen
	flaggedFlag = imap.FlagFlagged
)
