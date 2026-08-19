package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// renderer holds the parsed template set.
type renderer struct {
	templates *template.Template
}

func newRenderer() (*renderer, error) {
	r := &renderer{}

	// The layout renders the page body through the "partial" function rather
	// than through a cloned template set. html/template refuses to Clone a set
	// that has already been executed, so the clone approach works exactly once
	// and then fails for every subsequent page — a bug that only appears after
	// the first request.
	funcs := templateFuncs()
	funcs["partial"] = func(name string, data any) (template.HTML, error) {
		var sb strings.Builder
		if err := r.templates.ExecuteTemplate(&sb, name, data); err != nil {
			return "", err
		}
		return template.HTML(sb.String()), nil //nolint:gosec // rendered by our own templates, which escape their inputs
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing templates: %w", err)
	}
	r.templates = tmpl
	return r, nil
}

// pageData is the envelope every page template receives.
type pageData struct {
	Title string
	User  any
	CSRF  string
	Flash string
	Error string
	// Content is the page's own data.
	Content any
	// Fragment names the template the layout should render as the page body.
	Fragment string
}

// render writes a template.
//
// An htmx request receives just the named fragment; a normal navigation gets it
// wrapped in the page shell. Handlers therefore need no knowledge of which kind
// of request they are serving, and every URL remains a working bookmark.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data pageData) {
	if user, ok := userFrom(r.Context()); ok {
		data.User = user
	}
	if cookie, err := r.Cookie(csrfCookie); err == nil {
		data.CSRF = cookie.Value
	}

	// An htmx request gets the bare fragment; a normal navigation gets it
	// wrapped in the page shell.
	target := name
	if r.Header.Get("HX-Request") != "true" {
		target = "layout.html"
		data.Fragment = name
	}

	// Rendered to a buffer first: a template error midway through would
	// otherwise emit a half-written page under a 200 status.
	var buf bytes.Buffer
	if err := s.renderer.templates.ExecuteTemplate(&buf, target, data); err != nil {
		s.log.Error("rendering template", "template", name, "error", err)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatBytes": formatBytes,
		"formatTime":  formatTime,
		"formatAgo":   formatAgo,
		"join":        func(sep string, values []string) string { return strings.Join(values, sep) },
		"firstOr": func(fallback string, values []string) string {
			if len(values) == 0 || values[0] == "" {
				return fallback
			}
			return values[0]
		},
		"percent": func(done, total int) int {
			if total <= 0 {
				return 0
			}
			return done * 100 / total
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"hasFlag": func(flags []string, flag string) bool {
			for _, f := range flags {
				if strings.EqualFold(f, flag) {
					return true
				}
			}
			return false
		},
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func formatTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Local().Format("2 Jan 2006, 15:04")
}

// formatAgo renders a relative time, which is what someone watching a sync
// actually wants to know.
func formatAgo(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
