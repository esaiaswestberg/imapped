package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/syncer"
)

// syncProgressView is a running sync as the UI shows it.
type syncProgressView struct {
	AccountID    int64
	AccountEmail string
	Phase        string
	Mailbox      string

	MailboxesDone  int
	MailboxesTotal int
	MessagesNew    int64
	BodiesFetched  int64
	BytesFetched   int64
	PendingBodies  int64

	Elapsed string
	Percent int
	ETA     string
}

func newSyncProgressView(account store.Account, p *syncer.Progress) *syncProgressView {
	done, total, messages, bodies, bytes, pending := p.Counts()

	view := &syncProgressView{
		AccountID:      account.ID,
		AccountEmail:   account.EmailAddress,
		Phase:          p.Phase(),
		Mailbox:        p.CurrentMailbox(),
		MailboxesDone:  done,
		MailboxesTotal: total,
		MessagesNew:    messages,
		BodiesFetched:  bodies,
		BytesFetched:   bytes,
		PendingBodies:  pending,
		Elapsed:        p.Elapsed().Round(time.Second).String(),
	}
	if total > 0 {
		view.Percent = done * 100 / total
	}
	view.ETA = estimateRemaining(p.Elapsed(), bodies, pending)
	return view
}

// estimateRemaining projects a finish time from the rate so far.
//
// Deliberately coarse: an estimate that swings wildly is worse than none, so a
// figure is only offered once enough work has completed for the rate to mean
// something.
func estimateRemaining(elapsed time.Duration, done, pending int64) string {
	if pending <= 0 {
		return ""
	}
	if done < 20 || elapsed < 5*time.Second {
		return "estimating…"
	}
	rate := float64(done) / elapsed.Seconds()
	if rate <= 0 {
		return ""
	}
	remaining := time.Duration(float64(pending)/rate) * time.Second
	return remaining.Round(time.Second).String() + " remaining"
}

func (s *Server) handleSyncPage(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	accounts, err := s.store.ListAccountsForUser(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, "loading accounts", err)
		return
	}
	s.render(w, r, "sync.html", pageData{
		Title:   "Sync",
		Content: s.progressViews(accounts),
	})
}

func (s *Server) progressViews(accounts []store.Account) []*syncProgressView {
	var views []*syncProgressView
	for _, account := range accounts {
		if progress, ok := s.engine.ProgressFor(account.ID); ok {
			views = append(views, newSyncProgressView(account, progress))
		}
	}
	return views
}

// handleSyncEvents streams live progress.
//
// Rendered HTML fragments are sent rather than JSON, so htmx can swap them
// directly into the page and no client-side rendering code is needed at all.
func (s *Server) handleSyncEvents(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which would hold the stream
	// until it ended — precisely defeating the point.
	w.Header().Set("X-Accel-Buffering", "no")

	controller := http.NewResponseController(w)

	// Coarse enough that a fast sync cannot flood the browser, frequent enough
	// to feel live.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// A heartbeat keeps intermediaries from closing an idle connection.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// The closure reads from the request context on every call; the linter
	// cannot see that through the indirection.
	//nolint:contextcheck // r.Context() is used inside
	send := func() bool {
		accounts, err := s.store.ListAccountsForUser(r.Context(), user.ID)
		if err != nil {
			return false
		}
		html, err := s.renderFragment("sync_progress.html", pageData{
			Content: s.progressViews(accounts),
		})
		if err != nil {
			s.log.Warn("rendering sync progress", "error", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "event: progress\ndata: %s\n\n", flatten(html)); err != nil {
			return false
		}
		return controller.Flush() == nil
	}

	if !send() {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			// The browser navigated away or closed the tab.
			return
		case <-ticker.C:
			if !send() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

// renderFragment renders a template to a string without the page shell.
func (s *Server) renderFragment(name string, data pageData) (string, error) {
	var sb strings.Builder
	if err := s.renderer.templates.ExecuteTemplate(&sb, name, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// flatten collapses HTML onto one line.
//
// Server-sent events are newline-delimited, so a multi-line payload would be
// parsed as several separate data fields and arrive at the browser mangled.
func flatten(html string) string {
	replaced := strings.NewReplacer("\r", "", "\n", " ").Replace(html)
	return strings.Join(strings.Fields(replaced), " ")
}
