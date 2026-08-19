// Package fakeimap provides a real IMAP server for tests, plus the ability to
// make it misbehave in specific ways.
//
// The server is go-imap's own imapserver/imapmemserver, so it is
// protocol-correct in ways a hand-rolled fake never is. The Rust suite this
// replaces had roughly forty separately hand-written fake servers, each
// string-matching commands and emitting canned responses; they agreed with each
// other only by accident and none of them modelled a server that simply stops
// responding, which is the failure that mattered most.
//
// Two things are layered on top, both at the net.Conn level so they are
// independent of the library's internals:
//
//   - Recorder captures every command the client sends. Assertions on that log
//     are how the "one FETCH, not eight thousand" property is enforced.
//   - Chaos injects transport-level misbehaviour: hanging, dropping, trickling
//     bytes. These are the regression tests for the two-day hang.
package fakeimap

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// DefaultUsername and DefaultPassword are the credentials the server accepts
// unless Options says otherwise.
const (
	DefaultUsername = "tester@example.com"
	DefaultPassword = "correct-horse-battery-staple"
)

// Options configures a fake server.
type Options struct {
	Username string
	Password string

	// Caps overrides the advertised capability set. Nil advertises a sensible
	// modern default; use this to simulate an older server that lacks CONDSTORE
	// or QRESYNC and force the sync engine down its fallback paths.
	Caps imap.CapSet

	// Mailboxes to create, in order. INBOX is always created first.
	Mailboxes []Mailbox

	// Chaos makes the transport misbehave. Zero value behaves normally.
	Chaos Chaos
}

// Mailbox describes a mailbox to pre-populate.
type Mailbox struct {
	Name string
	// Messages to append. Use Seed to generate them.
	Messages []Message
}

// Message is one message to seed into a mailbox.
type Message struct {
	Subject string
	From    string
	To      string
	Body    string
	Date    time.Time
	Flags   []imap.Flag
}

// Server is a running fake IMAP server.
type Server struct {
	t        *testing.T
	listener net.Listener
	imap     *imapserver.Server
	recorder *Recorder

	username string
	password string

	closeOnce sync.Once
}

// defaultCaps advertises the extensions a capable modern server would.
func defaultCaps() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1:    {},
		imap.CapIMAP4rev2:    {},
		imap.CapUIDPlus:      {},
		imap.CapMove:         {},
		imap.CapESearch:      {},
		imap.CapNamespace:    {},
		imap.CapListExtended: {},
		imap.CapCondStore:    {},
	}
}

// Start boots a server on a loopback port and registers cleanup with t.
func Start(t *testing.T, opts Options) *Server {
	t.Helper()

	if opts.Username == "" {
		opts.Username = DefaultUsername
	}
	if opts.Password == "" {
		opts.Password = DefaultPassword
	}
	if opts.Caps == nil {
		opts.Caps = defaultCaps()
	}

	user := imapmemserver.NewUser(opts.Username, opts.Password)
	memServer := imapmemserver.New()
	memServer.AddUser(user)

	// INBOX must exist before anything else; some clients assume it.
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("fakeimap: creating INBOX: %v", err)
	}
	for _, mbox := range opts.Mailboxes {
		if !strings.EqualFold(mbox.Name, "INBOX") {
			if err := user.Create(mbox.Name, nil); err != nil {
				t.Fatalf("fakeimap: creating mailbox %s: %v", mbox.Name, err)
			}
		}
		for i, msg := range mbox.Messages {
			if err := appendMessage(user, mbox.Name, msg); err != nil {
				t.Fatalf("fakeimap: appending message %d to %s: %v", i, mbox.Name, err)
			}
		}
	}

	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeimap: listening: %v", err)
	}

	recorder := newRecorder()
	listener := &wrappedListener{
		Listener: base,
		recorder: recorder,
		chaos:    opts.Chaos,
	}

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), &imapserver.GreetingData{PreAuth: false}, nil
		},
		Caps: opts.Caps,
		// Tests speak plaintext to loopback; requiring TLS would add noise
		// without testing anything the transport layer does not already cover.
		InsecureAuth: true,
		Logger:       discardLogger{},
	})

	s := &Server{
		t:        t,
		listener: listener,
		imap:     srv,
		recorder: recorder,
		username: opts.Username,
		password: opts.Password,
	}

	go func() {
		// Serve returns when the listener closes during cleanup.
		_ = srv.Serve(listener)
	}()

	t.Cleanup(s.Close)
	return s
}

// Addr is the host:port clients should connect to.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Host and Port split Addr for callers that need them separately.
func (s *Server) Host() string {
	host, _, _ := net.SplitHostPort(s.Addr())
	return host
}

func (s *Server) Port() int {
	_, port, _ := net.SplitHostPort(s.Addr())
	var n int
	_, _ = fmt.Sscanf(port, "%d", &n)
	return n
}

// Username and Password are the accepted credentials.
func (s *Server) Username() string { return s.username }
func (s *Server) Password() string { return s.password }

// Recorder exposes the log of commands the server received.
func (s *Server) Recorder() *Recorder { return s.recorder }

// Close shuts the server down. Safe to call more than once.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		_ = s.imap.Close()
		_ = s.listener.Close()
	})
}

func appendMessage(user *imapmemserver.User, mailbox string, msg Message) error {
	raw := msg.Render()
	opts := &imap.AppendOptions{Flags: msg.Flags, Time: msg.Date}
	if opts.Time.IsZero() {
		opts.Time = time.Now()
	}
	_, err := user.Append(mailbox, newLiteral(raw), opts)
	return err
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}
