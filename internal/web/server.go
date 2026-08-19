// Package web serves the browser interface.
//
// Pages are rendered server-side with html/template and progressively enhanced
// with htmx, so there is no JavaScript build step and the whole interface ships
// inside the binary. Every handler works both as a full page load and as an
// htmx fragment, which keeps the UI usable without scripting and makes the
// handlers straightforward to test.
package web

import (
	"embed"
	"log/slog"
	"net/http"

	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/search"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/syncer"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server serves the web interface.
type Server struct {
	cfg      config.Config
	store    *store.Store
	blobs    blob.Store
	search   search.Searcher
	engine   *syncer.Engine
	sealer   *crypto.Sealer
	log      *slog.Logger
	renderer *renderer

	// provenance backs the settings page.
	provenance []config.Field
}

// Options configures a Server.
type Options struct {
	Config     config.Config
	Store      *store.Store
	Blobs      blob.Store
	Search     search.Searcher
	Engine     *syncer.Engine
	Sealer     *crypto.Sealer
	Logger     *slog.Logger
	Provenance []config.Field
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	renderer, err := newRenderer()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:        opts.Config,
		store:      opts.Store,
		blobs:      opts.Blobs,
		search:     opts.Search,
		engine:     opts.Engine,
		sealer:     opts.Sealer,
		log:        opts.Logger,
		renderer:   renderer,
		provenance: opts.Provenance,
	}, nil
}

// Mount attaches the web interface to a mux.
func (s *Server) Mount(mux *http.ServeMux) {
	static := http.FileServerFS(staticFS)
	mux.Handle("GET /static/", static)

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /{$}", s.requireUser(s.handleDashboard))

	mux.HandleFunc("GET /accounts", s.requireUser(s.handleAccounts))
	mux.HandleFunc("GET /accounts/new", s.requireUser(s.handleNewAccountForm))
	mux.HandleFunc("POST /accounts", s.requireUser(s.handleCreateAccount))
	mux.HandleFunc("POST /accounts/test", s.requireUser(s.handleTestAccount))
	mux.HandleFunc("GET /accounts/{id}", s.requireUser(s.handleAccount))
	mux.HandleFunc("POST /accounts/{id}/sync", s.requireUser(s.handleSyncAccount))
	mux.HandleFunc("POST /accounts/{id}/pause", s.requireUser(s.handlePauseAccount))
	mux.HandleFunc("POST /accounts/{id}/resume", s.requireUser(s.handleResumeAccount))
	mux.HandleFunc("POST /accounts/{id}/delete", s.requireUser(s.handleDeleteAccount))

	mux.HandleFunc("GET /mailboxes/{id}", s.requireUser(s.handleMailbox))
	mux.HandleFunc("GET /messages/{id}", s.requireUser(s.handleMessage))
	mux.HandleFunc("GET /messages/{id}/raw", s.requireUser(s.handleRawMessage))
	mux.HandleFunc("GET /messages/{id}/parts/{path}", s.requireUser(s.handleMessagePart))

	mux.HandleFunc("GET /search", s.requireUser(s.handleSearch))
	mux.HandleFunc("GET /sync", s.requireUser(s.handleSyncPage))
	mux.HandleFunc("GET /sync/events", s.requireUser(s.handleSyncEvents))
	mux.HandleFunc("GET /runs", s.requireUser(s.handleRuns))
	mux.HandleFunc("GET /settings", s.requireUser(s.handleSettings))
}
