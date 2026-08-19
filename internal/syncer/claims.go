package syncer

import (
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/esaiaswestberg/imapped/internal/store"
)

// claimTable remembers which database row a claimed UID belongs to.
//
// FETCH responses identify messages by UID, but the row that was claimed is
// identified by its own id, and a UID is only unique within a mailbox. Rather
// than query per response, the producer records the mapping when it claims work
// and the workers look it up as bodies arrive.
type claimTable struct {
	mu     sync.Mutex
	claims map[claimKey]store.PendingBody
}

type claimKey struct {
	mailboxID int64
	uid       imap.UID
}

func newClaimTable() *claimTable {
	return &claimTable{claims: make(map[claimKey]store.PendingBody)}
}

func (t *claimTable) remember(mailboxID int64, claims map[imap.UID]store.PendingBody) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for uid, claim := range claims {
		t.claims[claimKey{mailboxID, uid}] = claim
	}
}

// take returns and removes a claim, so a UID cannot be processed twice.
func (t *claimTable) take(mailboxID int64, uid imap.UID) (store.PendingBody, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := claimKey{mailboxID, uid}
	claim, ok := t.claims[key]
	if ok {
		delete(t.claims, key)
	}
	return claim, ok
}

func (e *Engine) rememberClaims(mailboxID int64, claims map[imap.UID]store.PendingBody) {
	e.claims.remember(mailboxID, claims)
}

func (e *Engine) lookupClaim(mailboxID int64, uid imap.UID) (store.PendingBody, bool) {
	return e.claims.take(mailboxID, uid)
}
