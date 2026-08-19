// Package upstream talks to the remote IMAP server being mirrored.
//
// Everything here is bounded by a deadline. The Rust implementation this
// replaces had no timeouts at all: a read on a silently-dropped connection
// blocked forever, wedging a sync for two days at zero CPU while holding the
// account lock. The rule in this package is that no operation may block
// without a deadline that will eventually fire.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/esaiaswestberg/imapped/internal/config"
)

// Account describes how to reach and authenticate against an upstream server.
type Account struct {
	Host     string
	Port     int
	TLSMode  TLSMode
	Username string
	Password string
}

// Addr renders the host:port to dial.
func (a Account) Addr() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// Connector opens authenticated connections to upstream servers.
type Connector struct {
	cfg config.Upstream
	log *slog.Logger
}

// NewConnector builds a Connector from configuration.
func NewConnector(cfg config.Upstream, log *slog.Logger) *Connector {
	return &Connector{cfg: cfg, log: log}
}

// Client is an authenticated connection to an upstream server.
//
// It is not safe for concurrent use: the sync engine gives each worker its own
// Client rather than sharing one, because IMAP command pipelining across
// goroutines makes response attribution ambiguous.
type Client struct {
	imap *imapclient.Client
	conn net.Conn
	cfg  config.Upstream
	log  *slog.Logger

	caps    imap.CapSet
	account Account

	// Set when a command is abandoned at its deadline. Such a connection has an
	// unknown amount of unread response data still in flight, so it can never be
	// reused: the next command's replies would interleave with the abandoned
	// ones. Reconnecting costs a round trip; a desynchronised connection costs
	// silent data corruption.
	poisoned bool
}

// Connect dials, secures, greets and authenticates in one bounded operation.
func (c *Connector) Connect(ctx context.Context, account Account) (*Client, error) {
	conn, err := c.dial(ctx, account.Addr())
	if err != nil {
		return nil, err
	}

	if account.TLSMode == TLSModeTLS {
		if conn, err = c.handshakeTLS(ctx, conn, account.Host); err != nil {
			return nil, err
		}
	}

	// From here on every read is subject to the inactivity window.
	conn = newDeadlineConn(conn, c.cfg.IOIdleTimeout.Std())

	client := &Client{conn: conn, cfg: c.cfg, log: c.log, account: account}

	// Reading the greeting is the first thing that can hang, so bound it
	// separately and more tightly than ordinary commands.
	greetCtx, cancelGreet := context.WithTimeout(ctx, c.cfg.GreetingTimeout.Std())
	defer cancelGreet()

	options := &imapclient.Options{}

	if account.TLSMode == TLSModeStartTLS {
		// NewStartTLS performs the greeting, CAPABILITY and STARTTLS exchange.
		err = client.withDeadline(greetCtx, func() error {
			var startErr error
			client.imap, startErr = imapclient.NewStartTLS(conn, &imapclient.Options{
				TLSConfig: c.tlsConfig(account.Host),
			})
			return startErr
		})
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("starting TLS with %s: %w", account.Host, err)
		}
	} else {
		client.imap = imapclient.New(conn, options)
		if err := client.withDeadline(greetCtx, func() error {
			return client.imap.WaitGreeting()
		}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("waiting for greeting from %s: %w", account.Host, err)
		}
	}

	if err := client.authenticate(ctx, account); err != nil {
		client.Close()
		return nil, err
	}

	if err := client.refreshCaps(ctx); err != nil {
		client.Close()
		return nil, err
	}

	return client, nil
}

// authenticate logs in using the simplest mechanism the server accepts.
func (c *Client) authenticate(ctx context.Context, account Account) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CommandTimeout.Std())
	defer cancel()

	err := c.withDeadline(ctx, func() error {
		return c.imap.Login(account.Username, account.Password).Wait()
	})
	if err != nil {
		return fmt.Errorf("authenticating as %s: %w", account.Username, classify(err))
	}
	return nil
}

func (c *Client) refreshCaps(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CommandTimeout.Std())
	defer cancel()

	// go-imap caches capabilities from the greeting and post-login responses;
	// only ask explicitly when it has none.
	if caps := c.imap.Caps(); len(caps) > 0 {
		c.caps = caps
		return nil
	}
	return c.withDeadline(ctx, func() error {
		caps, err := c.imap.Capability().Wait()
		if err != nil {
			return err
		}
		c.caps = caps
		return nil
	})
}

// withDeadline runs a blocking go-imap call under a context deadline.
//
// go-imap's command methods block and do not take a context, so cancellation
// has to be imposed from outside. Closing the underlying connection is the only
// thing that reliably unblocks a goroutine parked in Read, so that is what
// happens when the context fires — and the connection is marked poisoned so it
// is never reused afterwards.
func (c *Client) withDeadline(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	stop := context.AfterFunc(ctx, func() {
		c.poisoned = true
		_ = c.conn.Close()
	})
	defer stop()

	err := fn()
	if err != nil && ctx.Err() != nil {
		// The failure is a consequence of us closing the connection, so report
		// the cause rather than the resulting "use of closed connection".
		return fmt.Errorf("%w after %s", ctx.Err(), describeDeadline(ctx))
	}
	return err
}

func describeDeadline(ctx context.Context) string {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline).Round(time.Millisecond).String() + " remaining"
	}
	return "cancellation"
}

// Caps reports the server's advertised capabilities.
func (c *Client) Caps() imap.CapSet { return c.caps }

// Poisoned reports whether this connection was abandoned mid-command and must
// be discarded rather than reused.
func (c *Client) Poisoned() bool { return c.poisoned }

// Close logs out and closes the connection. A poisoned connection is closed
// without a LOGOUT, since the protocol state is already unknown.
func (c *Client) Close() error {
	if c.imap == nil {
		return c.conn.Close()
	}
	if c.poisoned {
		return c.conn.Close()
	}

	// LOGOUT is a courtesy; never let it delay teardown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.withDeadline(ctx, func() error { return c.imap.Logout().Wait() })

	return c.conn.Close()
}

// ErrPoisoned is returned when a command is attempted on a connection that was
// abandoned at a deadline.
var ErrPoisoned = errors.New("connection was abandoned at a deadline and cannot be reused")

func (c *Client) checkUsable() error {
	if c.poisoned {
		return ErrPoisoned
	}
	return nil
}
