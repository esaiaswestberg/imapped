package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// TLSMode selects how the transport is secured.
type TLSMode string

const (
	TLSModePlain    TLSMode = "plain"    // no encryption, test servers only
	TLSModeTLS      TLSMode = "tls"      // implicit TLS, usually port 993
	TLSModeStartTLS TLSMode = "starttls" // upgrade in-band, usually port 143
)

// dialer builds TCP connections with every layer bounded by a deadline.
//
// The connection is dialled here rather than via imapclient.DialTLS so that the
// dial, the TLS handshake and the per-read inactivity window are all under our
// control. Handing go-imap an already-established net.Conn is the only way to
// guarantee that.
func (c *Connector) dial(ctx context.Context, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout: c.cfg.DialTimeout.Std(),
		// Keepalives are the first line of defence against a peer that has gone
		// away without sending a FIN, e.g. a NAT dropping an idle mapping.
		KeepAlive: c.cfg.TCPKeepAlive.Std(),
		Control:   c.socketControl,
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dialling %s: %w", addr, err)
	}
	return conn, nil
}

// socketControl sets TCP_USER_TIMEOUT on the raw socket.
//
// Linux retransmits unacknowledged data for roughly 11 minutes before giving
// up, which is far too long to notice a black-holed connection. TCP_USER_TIMEOUT
// caps that at a value we choose, so a dead peer surfaces as an error in
// seconds rather than minutes — independent of anything the application does.
func (c *Connector) socketControl(network, address string, rawConn syscall.RawConn) error {
	timeout := c.cfg.TCPUserTimeout.Std()
	if timeout <= 0 {
		return nil
	}
	var setErr error
	err := rawConn.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP,
			unix.TCP_USER_TIMEOUT, int(timeout.Milliseconds()))
	})
	if err != nil {
		return err
	}
	// A kernel that does not support the option should not prevent connecting;
	// we simply fall back to keepalives and the application-level idle timeout.
	if setErr != nil && !isUnsupportedSockopt(setErr) {
		return setErr
	}
	return nil
}

func isUnsupportedSockopt(err error) bool {
	return errors.Is(err, unix.ENOPROTOOPT) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTSUP)
}

// tlsConfig builds the client TLS configuration for a host.
func (c *Connector) tlsConfig(host string) *tls.Config {
	return &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: c.cfg.InsecureSkipVerify, //nolint:gosec // guarded by config validation, refused in production
		MinVersion:         tls.VersionTLS12,
	}
}

// handshakeTLS wraps conn in TLS with a bounded handshake.
//
// A server that accepts the TCP connection but never completes the handshake
// would otherwise block forever, which is the same failure mode as a stalled
// read but earlier in the connection's life.
func (c *Connector) handshakeTLS(ctx context.Context, conn net.Conn, host string) (net.Conn, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, c.cfg.TLSHandshakeTimeout.Std())
	defer cancel()

	tlsConn := tls.Client(conn, c.tlsConfig(host))
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("TLS handshake with %s: %w", host, err)
	}
	return tlsConn, nil
}
