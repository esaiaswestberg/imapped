package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/store"
)

const (
	sessionCookie = "imapped_session"
	csrfCookie    = "imapped_csrf"
	csrfField     = "csrf_token"
	csrfHeader    = "X-CSRF-Token"
)

type contextKey string

const userContextKey contextKey = "user"

// userFrom returns the signed-in user, if any.
func userFrom(ctx context.Context) (store.User, bool) {
	user, ok := ctx.Value(userContextKey).(store.User)
	return user, ok
}

// newToken generates a URL-safe random token.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken derives the value stored in the database.
//
// Storing only a hash means a leaked database does not hand an attacker live
// sessions, the way storing the token itself would.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// signIn verifies credentials and issues a session cookie.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, email, password string) error {
	user, err := s.store.UserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Hash anyway, so a missing account and a wrong password take
			// comparable time and the response cannot be used to enumerate
			// which addresses exist.
			_, _ = crypto.HashPassword(password)
			return errInvalidCredentials
		}
		return err
	}
	if !user.Active() {
		return errInvalidCredentials
	}

	ok, err := crypto.VerifyPassword(password, user.PasswordHash)
	if err != nil || !ok {
		return errInvalidCredentials
	}

	token, err := newToken()
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(s.cfg.Web.SessionTTL.Std())

	if err := s.store.CreateSession(r.Context(), user.ID, hashToken(token), "web",
		r.UserAgent(), r.RemoteAddr, expiresAt); err != nil {
		return err
	}

	http.SetCookie(w, s.cookie(sessionCookie, token, expiresAt))

	csrf, err := newToken()
	if err != nil {
		return err
	}
	// Readable by script so htmx can attach it to requests; its security comes
	// from being unguessable and same-origin, not from being hidden.
	csrfCookieValue := s.cookie(csrfCookie, csrf, expiresAt)
	csrfCookieValue.HttpOnly = false
	http.SetCookie(w, csrfCookieValue)

	return nil
}

func (s *Server) cookie(name, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.cfg.Web.SecureCookies,
		// Lax rather than Strict: Strict would drop the session on any
		// cross-site navigation into the UI, including a link from a chat app.
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.RevokeSession(r.Context(), hashToken(cookie.Value))
	}
	expired := time.Unix(0, 0)
	http.SetCookie(w, s.cookie(sessionCookie, "", expired))
	http.SetCookie(w, s.cookie(csrfCookie, "", expired))
}

// requireUser is middleware that rejects unauthenticated requests.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			s.redirectToLogin(w, r)
			return
		}
		user, err := s.store.UserForSession(r.Context(), hashToken(cookie.Value))
		if err != nil || !user.Active() {
			s.redirectToLogin(w, r)
			return
		}

		// CSRF: any state-changing request must present the token from the
		// cookie. Combined with SameSite=Lax this closes both the classic
		// form-post and the fetch-from-another-origin cases.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.csrfValid(r) {
				http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
				return
			}
		}

		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	}
}

func (s *Server) csrfValid(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	presented := r.Header.Get(csrfHeader)
	if presented == "" {
		presented = r.FormValue(csrfField)
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(presented)) == 1
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		// htmx follows redirects inside the swapped fragment, which would nest
		// the login page inside the app shell; this makes the browser navigate.
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

var errInvalidCredentials = errors.New("that email address and password do not match")
