package upstream

import "github.com/emersion/go-imap/v2/imapclient"

// Local aliases for the fetch response item types, so the streaming code above
// reads without repeating the package-qualified names.
type (
	fetchMessage         = *imapclient.FetchMessageData
	imapFetchUID         = imapclient.FetchItemDataUID
	imapFetchRFC822Size  = imapclient.FetchItemDataRFC822Size
	imapFetchBodySection = imapclient.FetchItemDataBodySection
)
