// Package imapsrv serves mirrored mail to mail clients over IMAP.
//
// It is a thin adaptation of go-imap's server onto the local store: all the
// mailbox logic lives in the store and is shared with the web interface, so
// the two cannot disagree about what a mailbox contains.
package imapsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/store"
)

// Backend builds sessions for incoming connections.
type Backend struct {
	cfg   config.Config
	store *store.Store
	blobs blob.Store
	log   *slog.Logger
}

// NewBackend builds a Backend.
func NewBackend(cfg config.Config, st *store.Store, blobs blob.Store, log *slog.Logger) *Backend {
	return &Backend{cfg: cfg, store: st, blobs: blobs, log: log}
}

// session is one client connection.
type session struct {
	backend *Backend

	mu       sync.Mutex
	user     *store.User
	account  *store.Account
	mailbox  *store.Mailbox
	readOnly bool

	// snapshot is the ordered message list backing sequence numbers. IMAP
	// sequence numbers are positions in this list, and they must not shift
	// under the client mid-command, so it is captured at SELECT and refreshed
	// only at defined points.
	snapshot []store.MessageSummary
}

// NewSession is called by the server for each connection.
func (b *Backend) NewSession() imapserver.Session {
	return &session{backend: b}
}

func (s *session) Close() error { return nil }

// ctx bounds a command. Every database call the session makes is subject to it,
// so a stuck query cannot pin a client connection indefinitely.
func (s *session) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.backend.cfg.IMAP.CommandTimeout.Std())
}

// Login authenticates a mail client.
//
// Clients authenticate with the same credentials they use for the web
// interface, not with the upstream mail server's — the whole point is that the
// upstream password stays sealed in the database and is never handed out.
func (s *session) Login(username, password string) error {
	ctx, cancel := s.ctx()
	defer cancel()

	user, err := s.backend.store.UserByEmail(ctx, username)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.backend.log.Error("looking up user during IMAP login", "error", err)
		}
		// Hash regardless, so a missing account and a wrong password take
		// comparable time.
		_, _ = crypto.HashPassword(password)
		return authFailed()
	}
	if !user.Active() {
		return authFailed()
	}

	ok, err := crypto.VerifyPassword(password, user.PasswordHash)
	if err != nil || !ok {
		return authFailed()
	}

	accounts, err := s.backend.store.ListAccountsForUser(ctx, user.ID)
	if err != nil {
		return internalError(err)
	}
	if len(accounts) == 0 {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "this user has no mail accounts configured yet",
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.user = &user
	// One account per session. Serving several accounts through one connection
	// would need a namespace prefix scheme that no client expects by default.
	s.account = &accounts[0]

	s.backend.log.Info("IMAP client signed in",
		"user", user.Email, "account", s.account.EmailAddress)
	return nil
}

func authFailed() error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeAuthenticationFailed,
		Text: "invalid credentials",
	}
}

func internalError(err error) error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeServerBug,
		Text: err.Error(),
	}
}

func (s *session) requireAuth() error {
	if s.user == nil || s.account == nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "not authenticated"}
	}
	return nil
}

func (s *session) requireMailbox() error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if s.mailbox == nil {
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "no mailbox is selected"}
	}
	return nil
}

// Select opens a mailbox and captures the sequence-number snapshot.
func (s *session) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireAuth(); err != nil {
		return nil, err
	}

	ctx, cancel := s.ctx()
	defer cancel()

	boxes, err := s.backend.store.ListMailboxes(ctx, s.account.ID)
	if err != nil {
		return nil, internalError(err)
	}

	var found *store.Mailbox
	for i := range boxes {
		if strings.EqualFold(boxes[i].Name, mailbox) {
			found = &boxes[i]
			break
		}
	}
	if found == nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "no such mailbox",
		}
	}

	s.mailbox = found
	s.readOnly = options != nil && options.ReadOnly
	if err := s.refreshSnapshot(ctx); err != nil {
		return nil, internalError(err)
	}

	data := &imap.SelectData{
		NumMessages: uint32(len(s.snapshot)),
		UIDNext:     imap.UID(found.LocalUIDNext),
		UIDValidity: uidValidity(*found),
		Flags:       standardFlags(),
		// Custom keywords cannot be created, since a flag set locally would
		// have to be replayed upstream and not every server accepts arbitrary
		// keywords.
		PermanentFlags: standardFlags(),
	}
	return data, nil
}

// uidValidity derives the value reported to clients.
//
// The local UID space is ours, not the upstream server's, so a change in the
// upstream UIDVALIDITY does not invalidate a client's cache: local UIDs are
// never reused or renumbered. The generation counter exists so that if we ever
// do have to renumber, clients are told.
func uidValidity(m store.Mailbox) uint32 {
	return uint32(m.ID<<8) ^ uint32(m.SyncGeneration) ^ 0x9E3779B9
}

func standardFlags() []imap.Flag {
	return []imap.Flag{
		imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged,
		imap.FlagDeleted, imap.FlagDraft,
	}
}

// refreshSnapshot reloads the ordered message list.
//
// Messages are ordered by local UID ascending, which is the order IMAP
// sequence numbers must follow.
func (s *session) refreshSnapshot(ctx context.Context) error {
	messages, _, err := s.backend.store.ListMessagesByUID(ctx, s.mailbox.ID, 0, 0)
	if err != nil {
		return err
	}
	s.snapshot = messages
	return nil
}

func (s *session) Unselect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailbox = nil
	s.snapshot = nil
	return nil
}

// Poll reports changes that happened since the last snapshot.
func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mailbox == nil {
		return nil
	}
	ctx, cancel := s.ctx()
	defer cancel()

	before := len(s.snapshot)
	if err := s.refreshSnapshot(ctx); err != nil {
		return internalError(err)
	}
	if len(s.snapshot) != before {
		return w.WriteNumMessages(uint32(len(s.snapshot)))
	}
	return nil
}

// Idle holds the connection open and reports new mail as it arrives.
//
// Changes are detected by polling the database rather than by subscribing to a
// notification channel. A poll every few seconds is a negligible query against
// an indexed table, and it cannot miss an update the way an at-most-once
// notification can — correctness here does not depend on delivery.
func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	timeout := time.NewTimer(s.backend.cfg.IMAP.IdleTimeout.Std())
	defer timeout.Stop()

	for {
		select {
		case <-stop:
			return nil
		case <-timeout.C:
			// RFC 2177 expects the client to re-issue IDLE periodically;
			// returning lets it do so.
			return nil
		case <-ticker.C:
			if err := s.Poll(w, true); err != nil {
				return err
			}
		}
	}
}

var _ imapserver.Session = (*session)(nil)

// unsupported reports a command this server does not implement.
//
// Mirrored mail is read-only from a client's perspective in this release:
// creating or renaming a mailbox locally would have to be replayed upstream,
// and doing that safely is a larger piece of work than pretending to succeed.
func unsupported(command string) error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Text: fmt.Sprintf("%s is not supported: this mailbox mirrors an upstream server and is read-only", command),
	}
}

func (s *session) Create(string, *imap.CreateOptions) error         { return unsupported("CREATE") }
func (s *session) Delete(string) error                              { return unsupported("DELETE") }
func (s *session) Rename(string, string, *imap.RenameOptions) error { return unsupported("RENAME") }
func (s *session) Subscribe(string) error                           { return nil }
func (s *session) Unsubscribe(string) error                         { return nil }

func (s *session) Append(string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, unsupported("APPEND")
}

func (s *session) Expunge(*imapserver.ExpungeWriter, *imap.UIDSet) error {
	return unsupported("EXPUNGE")
}

func (s *session) Copy(imap.NumSet, string) (*imap.CopyData, error) {
	return nil, unsupported("COPY")
}
