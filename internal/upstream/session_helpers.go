package upstream

import (
	"time"

	"github.com/emersion/go-imap/v2"
)

// imapCapSet aliases the capability set so session.go need not import go-imap.
type imapCapSet = imap.CapSet

// timeAfter is a seam for tests that would otherwise wait out a real backoff.
var timeAfter = time.After
