package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/search"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/upstream"
)

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login.html", pageData{Title: "Sign in"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")

	if err := s.signIn(w, r, email, password); err != nil {
		if !errors.Is(err, errInvalidCredentials) {
			s.log.Error("signing in", "email", email, "error", err)
		}
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", pageData{
			Title: "Sign in",
			Error: "That email address and password do not match.",
		})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.signOut(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type dashboardAccount struct {
	Account  store.Account
	Stats    store.AccountStats
	Running  bool
	Progress *syncProgressView
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())

	accounts, err := s.store.ListAccountsForUser(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, "loading accounts", err)
		return
	}

	view := make([]dashboardAccount, 0, len(accounts))
	for _, account := range accounts {
		stats, err := s.store.StatsForAccount(r.Context(), account.ID)
		if err != nil {
			s.log.Warn("loading account statistics", "account_id", account.ID, "error", err)
		}
		entry := dashboardAccount{Account: account, Stats: stats}
		if progress, ok := s.engine.ProgressFor(account.ID); ok {
			entry.Running = true
			entry.Progress = newSyncProgressView(account, progress)
		}
		view = append(view, entry)
	}

	s.render(w, r, "dashboard.html", pageData{Title: "Dashboard", Content: view})
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	accounts, err := s.store.ListAccountsForUser(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, "loading accounts", err)
		return
	}
	s.render(w, r, "accounts.html", pageData{Title: "Accounts", Content: accounts})
}

func (s *Server) handleNewAccountForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "account_new.html", pageData{Title: "Add an account"})
}

// handleTestAccount probes credentials before they are saved, so a typo is
// caught while the form is still on screen rather than as a failed sync later.
func (s *Server) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	account, err := accountFromForm(r)
	if err != nil {
		s.render(w, r, "account_test_result.html", pageData{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	connector := upstream.NewConnector(s.cfg.Upstream, s.log)
	client, err := connector.Connect(ctx, account)
	if err != nil {
		s.render(w, r, "account_test_result.html", pageData{
			Error: fmt.Sprintf("Could not connect: %v", err),
		})
		return
	}
	defer client.Close()

	boxes, err := client.ListMailboxes(ctx)
	if err != nil {
		s.render(w, r, "account_test_result.html", pageData{
			Error: fmt.Sprintf("Connected, but listing mailboxes failed: %v", err),
		})
		return
	}

	s.render(w, r, "account_test_result.html", pageData{
		Flash: fmt.Sprintf("Connected successfully and found %d mailboxes.", len(boxes)),
	})
}

func accountFromForm(r *http.Request) (upstream.Account, error) {
	port, err := strconv.Atoi(r.FormValue("port"))
	if err != nil || port < 1 || port > 65535 {
		return upstream.Account{}, errors.New("the port must be a number between 1 and 65535")
	}
	host := r.FormValue("host")
	if host == "" {
		return upstream.Account{}, errors.New("a server hostname is required")
	}
	username := r.FormValue("username")
	if username == "" {
		return upstream.Account{}, errors.New("a username is required")
	}
	return upstream.Account{
		Host:     host,
		Port:     port,
		TLSMode:  upstream.TLSMode(r.FormValue("tls_mode")),
		Username: username,
		Password: r.FormValue("password"),
	}, nil
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	user, _ := userFrom(r.Context())

	creds, err := accountFromForm(r)
	if err != nil {
		s.render(w, r, "account_new.html", pageData{Title: "Add an account", Error: err.Error()})
		return
	}
	if s.sealer == nil {
		s.render(w, r, "account_new.html", pageData{
			Title: "Add an account",
			Error: "No encryption master key is configured, so credentials cannot be stored securely.",
		})
		return
	}

	sealedUser, err := s.sealer.SealString(creds.Username)
	if err != nil {
		s.fail(w, r, "encrypting the username", err)
		return
	}
	sealedSecret, err := s.sealer.SealString(creds.Password)
	if err != nil {
		s.fail(w, r, "encrypting the password", err)
		return
	}

	email := r.FormValue("email")
	if email == "" {
		email = creds.Username
	}

	account, err := s.store.CreateAccount(r.Context(), store.CreateAccountParams{
		UserID:            user.ID,
		DisplayName:       r.FormValue("display_name"),
		EmailAddress:      email,
		UpstreamHost:      creds.Host,
		UpstreamPort:      creds.Port,
		UpstreamTLSMode:   string(creds.TLSMode),
		EncryptedUsername: sealedUser,
		EncryptedSecret:   sealedSecret,
	})
	if err != nil {
		s.render(w, r, "account_new.html", pageData{
			Title: "Add an account",
			Error: fmt.Sprintf("Could not save the account: %v", err),
		})
		return
	}

	// Start mirroring straight away: the point of adding an account is to sync it.
	s.startSync(account.ID, "manual")

	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", account.ID), http.StatusSeeOther)
}

type accountView struct {
	Account   store.Account
	Stats     store.AccountStats
	Mailboxes []store.Mailbox
	Runs      []store.SyncRun
	Running   bool
	Progress  *syncProgressView
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r)
	if !ok {
		return
	}

	stats, _ := s.store.StatsForAccount(r.Context(), account.ID)
	mailboxes, err := s.store.ListMailboxes(r.Context(), account.ID)
	if err != nil {
		s.fail(w, r, "loading mailboxes", err)
		return
	}
	runs, err := s.store.ListSyncRuns(r.Context(), account.ID, 10)
	if err != nil {
		s.log.Warn("loading sync runs", "account_id", account.ID, "error", err)
	}

	view := accountView{Account: account, Stats: stats, Mailboxes: mailboxes, Runs: runs}
	if progress, ok := s.engine.ProgressFor(account.ID); ok {
		view.Running = true
		view.Progress = newSyncProgressView(account, progress)
	}

	s.render(w, r, "account.html", pageData{Title: account.EmailAddress, Content: view})
}

func (s *Server) handleSyncAccount(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r)
	if !ok {
		return
	}
	s.startSync(account.ID, "manual")
	http.Redirect(w, r, "/sync", http.StatusSeeOther)
}

// startSync launches a sync in the background.
//
// The request must not wait for it: a first sync of a large mailbox takes
// minutes, and holding an HTTP request open for that would time out somewhere
// between the browser and any reverse proxy in between.
func (s *Server) startSync(accountID int64, trigger string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Sync.MaxRunDuration.Std())
		defer cancel()
		if _, err := s.engine.SyncAccount(ctx, accountID, trigger); err != nil {
			s.log.Error("sync failed", "account_id", accountID, "error", err)
		}
	}()
}

func (s *Server) handlePauseAccount(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, true)
}

func (s *Server) handleResumeAccount(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, false)
}

func (s *Server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	account, ok := s.accountForRequest(w, r)
	if !ok {
		return
	}
	if err := s.store.SetAccountPaused(r.Context(), account.ID, paused); err != nil {
		s.fail(w, r, "updating the account", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", account.ID), http.StatusSeeOther)
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteAccount(r.Context(), account.ID); err != nil {
		s.fail(w, r, "deleting the account", err)
		return
	}
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

type mailboxView struct {
	Mailbox  store.Mailbox
	Messages []store.MessageSummary
	Total    int64
	Page     int
	PageSize int
	HasNext  bool
	HasPrev  bool
}

func (s *Server) handleMailbox(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mailbox, err := s.store.GetMailbox(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.ownsAccount(r, mailbox.AccountID) {
		http.NotFound(w, r)
		return
	}

	const pageSize = 50
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			page = parsed
		}
	}

	messages, total, err := s.store.ListMessages(r.Context(), mailbox.ID, pageSize, (page-1)*pageSize)
	if err != nil {
		s.fail(w, r, "loading messages", err)
		return
	}

	s.render(w, r, "mailbox.html", pageData{
		Title: mailbox.Name,
		Content: mailboxView{
			Mailbox: mailbox, Messages: messages, Total: total,
			Page: page, PageSize: pageSize,
			HasNext: int64(page*pageSize) < total,
			HasPrev: page > 1,
		},
	})
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	message, ok := s.messageForRequest(w, r)
	if !ok {
		return
	}
	s.render(w, r, "message.html", pageData{Title: message.Subject, Content: message})
}

// handleRawMessage serves the original RFC 5322 bytes.
func (s *Server) handleRawMessage(w http.ResponseWriter, r *http.Request) {
	message, ok := s.messageForRequest(w, r)
	if !ok {
		return
	}
	if message.BlobKey == "" {
		http.Error(w, "this message has not been downloaded yet", http.StatusNotFound)
		return
	}

	reader, err := s.blobs.Get(r.Context(), blob.Key(message.BlobKey))
	if err != nil {
		s.fail(w, r, "reading the message", err)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("message-%d.eml", message.LocalUID)))
	_, _ = io.Copy(w, reader)
}

// handleMessagePart serves one MIME part.
//
// Always as an attachment, never inline: rendering attacker-supplied HTML or
// SVG from the application's own origin would give it access to the session.
func (s *Server) handleMessagePart(w http.ResponseWriter, r *http.Request) {
	message, ok := s.messageForRequest(w, r)
	if !ok {
		return
	}
	path := r.PathValue("path")

	for _, part := range message.Parts {
		if part.Path != path {
			continue
		}
		if part.BlobKey == "" {
			http.Error(w, "this part was not stored separately", http.StatusNotFound)
			return
		}
		reader, err := s.blobs.Get(r.Context(), blob.Key(part.BlobKey))
		if err != nil {
			s.fail(w, r, "reading the attachment", err)
			return
		}
		defer reader.Close()

		filename := part.Filename
		if filename == "" {
			filename = "part-" + part.Path
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.Copy(w, reader)
		return
	}
	http.NotFound(w, r)
}

type searchView struct {
	Query   string
	Results []search.Result
	Total   int
	Ran     bool
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	query := r.URL.Query().Get("q")

	view := searchView{Query: query}
	if query != "" {
		accounts, err := s.store.ListAccountsForUser(r.Context(), user.ID)
		if err != nil {
			s.fail(w, r, "loading accounts", err)
			return
		}
		for _, account := range accounts {
			results, total, err := s.search.Search(r.Context(), search.Query{
				Text: query, AccountID: account.ID, Limit: 50,
			})
			if err != nil {
				s.log.Warn("searching", "account_id", account.ID, "error", err)
				continue
			}
			view.Results = append(view.Results, results...)
			view.Total += total
		}
		view.Ran = true
	}

	name := "search.html"
	if r.Header.Get("HX-Request") == "true" {
		name = "search_results.html"
	}
	s.render(w, r, name, pageData{Title: "Search", Content: view})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	accounts, err := s.store.ListAccountsForUser(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, "loading accounts", err)
		return
	}

	type runRow struct {
		Account store.Account
		Run     store.SyncRun
		Stale   bool
	}
	var rows []runRow
	for _, account := range accounts {
		runs, err := s.store.ListSyncRuns(r.Context(), account.ID, 20)
		if err != nil {
			continue
		}
		for _, run := range runs {
			rows = append(rows, runRow{
				Account: account,
				Run:     run,
				// A run claiming to be alive whose heartbeat stopped belongs to
				// a process that died. Surfacing that is the whole point of
				// recording runs.
				Stale: run.Stale(2 * s.cfg.Sync.HeartbeatInterval.Std()),
			})
		}
	}

	s.render(w, r, "runs.html", pageData{Title: "Sync history", Content: rows})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings.html", pageData{Title: "Settings", Content: s.provenance})
}

// accountForRequest loads the account named in the path, enforcing ownership.
func (s *Server) accountForRequest(w http.ResponseWriter, r *http.Request) (store.Account, bool) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return store.Account{}, false
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return store.Account{}, false
	}
	user, _ := userFrom(r.Context())
	// Not found rather than forbidden: confirming an account exists would leak
	// its existence to anyone who guesses an id.
	if account.UserID != user.ID && !user.IsAdmin {
		http.NotFound(w, r)
		return store.Account{}, false
	}
	return account, true
}

func (s *Server) messageForRequest(w http.ResponseWriter, r *http.Request) (store.MessageDetail, bool) {
	id, err := pathID(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return store.MessageDetail{}, false
	}
	message, err := s.store.GetMessage(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return store.MessageDetail{}, false
	}
	if !s.ownsAccount(r, message.AccountID) {
		http.NotFound(w, r)
		return store.MessageDetail{}, false
	}
	return message, true
}

func (s *Server) ownsAccount(r *http.Request, accountID int64) bool {
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		return false
	}
	user, _ := userFrom(r.Context())
	return account.UserID == user.ID || user.IsAdmin
}

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

// fail logs the underlying cause and shows the user a generic message, so
// internal detail is not exposed in the browser.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, doing string, err error) {
	s.log.Error("request failed", "doing", doing, "path", r.URL.Path, "error", err)
	w.WriteHeader(http.StatusInternalServerError)
	s.render(w, r, "error.html", pageData{
		Title: "Something went wrong",
		Error: "Something went wrong while " + doing + ".",
	})
}
