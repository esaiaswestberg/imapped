package imapsrv

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/esaiaswestberg/imapped/internal/config"
)

// Server listens for mail clients.
type Server struct {
	name string
	imap *imapserver.Server
	addr string
	log  *slog.Logger
	tls  *tls.Config
}

// capabilities advertises what this server actually implements.
//
// SORT and THREAD are deliberately absent. go-imap's server does not implement
// them, and clients fall back to sorting and threading locally when they are
// not advertised — which is the common case against most IMAP servers. Claiming
// support and then failing would be far worse than not claiming it.
func capabilities() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1:    {},
		imap.CapNamespace:    {},
		imap.CapUIDPlus:      {},
		imap.CapESearch:      {},
		imap.CapIdle:         {},
		imap.CapLiteralMinus: {},
	}
}

// NewServer builds a listener for the given bind address.
func NewServer(name, addr string, backend *Backend, tlsConfig *tls.Config, log *slog.Logger) *Server {
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return backend.NewSession(), &imapserver.GreetingData{PreAuth: false}, nil
		},
		Caps:      capabilities(),
		TLSConfig: tlsConfig,
		// Plaintext authentication is allowed because deployments commonly
		// terminate TLS at a reverse proxy or keep this listener on a private
		// network. Validate warns when this is exposed in production.
		InsecureAuth: tlsConfig == nil,
		Logger:       serverLogger{log: log},
	})
	return &Server{name: name, imap: srv, addr: addr, log: log, tls: tlsConfig}
}

// Serve listens until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	var (
		listener net.Listener
		err      error
	)
	if s.tls != nil {
		listener, err = tls.Listen("tcp", s.addr, s.tls)
	} else {
		listener, err = net.Listen("tcp", s.addr)
	}
	if err != nil {
		return fmt.Errorf("binding %s listener on %s: %w", s.name, s.addr, err)
	}
	s.log.Info("listening", "service", s.name, "addr", listener.Addr().String())

	errs := make(chan error, 1)
	go func() { errs <- s.imap.Serve(listener) }()

	select {
	case err := <-errs:
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		_ = s.imap.Close()
		_ = listener.Close()
		s.log.Info("stopped", "service", s.name)
		return nil
	}
}

// LoadTLS builds a TLS configuration from the configured certificate.
func LoadTLS(cfg config.Config) (*tls.Config, error) {
	if cfg.IMAP.TLSCertPath == "" || cfg.IMAP.TLSKeyPath == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.IMAP.TLSCertPath, cfg.IMAP.TLSKeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading the IMAP TLS certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// serverLogger adapts slog to go-imap's logging interface.
type serverLogger struct{ log *slog.Logger }

func (l serverLogger) Printf(format string, args ...any) {
	l.log.Debug(fmt.Sprintf(format, args...), "component", "imap-server")
}

// Advertising a capability the session cannot serve makes go-imap panic when a
// client uses it, which surfaces as a dropped connection rather than a clear
// error. These assertions tie the capability set to the implementation, so the
// mismatch is a compile error instead.
var (
	_ imapserver.Session          = (*session)(nil)
	_ imapserver.SessionNamespace = (*session)(nil)
)
