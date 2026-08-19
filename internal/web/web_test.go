//go:build integration

package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/search"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/syncer"
	"github.com/esaiaswestberg/imapped/internal/testutil/pgtest"
	"github.com/esaiaswestberg/imapped/internal/web"
)

const (
	testEmail    = "admin@example.com"
	testPassword = "a sufficiently long test password"
	testKey      = "a test master key that is long enough"
)

type harness struct {
	server *httptest.Server
	client *http.Client
	store  *store.Store
	csrf   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	pool := pgtest.New(t)
	st := store.New(pool)
	sealer, err := crypto.NewSealer(testKey)
	if err != nil {
		t.Fatalf("creating sealer: %v", err)
	}

	cfg := config.Default()
	cfg.EncryptionMasterKey = testKey
	// httptest serves plaintext, so a Secure cookie would never be sent back.
	cfg.Web.SecureCookies = false

	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}
	if _, err := st.CreateUser(context.Background(), testEmail, hash, true); err != nil {
		t.Fatalf("creating user: %v", err)
	}

	ui, err := web.New(web.Options{
		Config: cfg, Store: st, Blobs: blob.NewMemStore(),
		Search: search.NewPostgres(pool, "english"),
		Engine: syncer.New(cfg, st, blob.NewMemStore(), sealer, logging.Discard()),
		Sealer: sealer, Logger: logging.Discard(),
		Provenance: []config.Field{
			{TOMLPath: "db.url", Value: "sha256:abcd1234", Source: config.SourceEnv,
				EnvVar: "DATABASE_URL", Secret: true},
			{TOMLPath: "sync.interval", Value: "15m0s", Source: config.SourceDefault},
		},
	})
	if err != nil {
		t.Fatalf("building the web server: %v", err)
	}

	mux := http.NewServeMux()
	ui.Mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	jar := newJar()
	client := &http.Client{
		Jar: jar,
		// Redirects are asserted on explicitly rather than followed.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &harness{server: server, client: client, store: st}
}

func (h *harness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) body(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(data)
}

func (h *harness) signIn(t *testing.T) {
	t.Helper()
	resp, err := h.client.PostForm(h.server.URL+"/login", url.Values{
		"email":    {testEmail},
		"password": {testPassword},
	})
	if err != nil {
		t.Fatalf("signing in: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in returned %d, want 303", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "imapped_csrf" {
			h.csrf = cookie.Value
		}
	}
	if h.csrf == "" {
		t.Fatal("no CSRF cookie was issued")
	}
}

func TestUnauthenticatedRequestsRedirectToLogin(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/", "/accounts", "/search", "/sync", "/settings", "/runs"} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s returned %d, want a redirect to the login page", path, resp.StatusCode)
			continue
		}
		if location := resp.Header.Get("Location"); location != "/login" {
			t.Errorf("GET %s redirected to %q, want /login", path, location)
		}
	}
}

func TestSignInAndOut(t *testing.T) {
	h := newHarness(t)
	h.signIn(t)

	resp := h.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard returned %d after signing in", resp.StatusCode)
	}
	if body := h.body(t, resp); !strings.Contains(body, "Dashboard") {
		t.Errorf("dashboard did not render: %.200s", body)
	}

	// Signing out must actually invalidate the session server-side, not merely
	// clear the cookie.
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/logout", nil)
	req.Header.Set("X-CSRF-Token", h.csrf)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("signing out: %v", err)
	}
	resp.Body.Close()

	if resp := h.get(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("dashboard returned %d after signing out, want a redirect", resp.StatusCode)
	}
}

func TestWrongPasswordIsRejected(t *testing.T) {
	h := newHarness(t)

	resp, err := h.client.PostForm(h.server.URL+"/login", url.Values{
		"email":    {testEmail},
		"password": {"not the password"},
	})
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("returned %d for a wrong password, want 401", resp.StatusCode)
	}
	// The message must not distinguish a wrong password from a missing account,
	// or it becomes a way to enumerate which addresses exist.
	body := h.body(t, resp)
	if strings.Contains(strings.ToLower(body), "no such user") ||
		strings.Contains(strings.ToLower(body), "unknown account") {
		t.Error("the error message reveals whether the account exists")
	}
}

// A state-changing request without the CSRF token must be refused, or any site
// could trigger actions on behalf of a signed-in user.
func TestStateChangingRequestsRequireCSRF(t *testing.T) {
	h := newHarness(t)
	h.signIn(t)

	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/accounts/1/sync", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a request without a CSRF token returned %d, want 403", resp.StatusCode)
	}
}

func TestSettingsRedactsSecrets(t *testing.T) {
	h := newHarness(t)
	h.signIn(t)

	body := h.body(t, h.get(t, "/settings"))
	if !strings.Contains(body, "sha256:abcd1234") {
		t.Error("the settings page does not show a fingerprint for the secret")
	}
	if !strings.Contains(body, "DATABASE_URL") {
		t.Error("the settings page does not name the environment variable a value came from")
	}
	if !strings.Contains(body, "sync.interval") {
		t.Error("the settings page is missing a defaulted setting")
	}
}

// Every page must work both as a full navigation and as an htmx fragment.
func TestHTMXRequestsReceiveFragments(t *testing.T) {
	h := newHarness(t)
	h.signIn(t)

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/accounts", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}
	defer resp.Body.Close()

	fragment := h.body(t, resp)
	if strings.Contains(fragment, "<!doctype html>") {
		t.Error("an htmx request received the full page shell instead of a fragment")
	}
	if !strings.Contains(fragment, "Accounts") {
		t.Errorf("the fragment does not contain the page content: %.200s", fragment)
	}

	// The same URL loaded normally must still be a complete page.
	full := h.body(t, h.get(t, "/accounts"))
	if !strings.Contains(full, "<!doctype html>") {
		t.Errorf("a normal navigation did not receive a complete page: %.300s", full)
	}
}

// Guessing another user's account id must not reveal that it exists.
func TestAccountsAreScopedToTheirOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	otherHash, _ := crypto.HashPassword("another password entirely")
	other, err := h.store.CreateUser(ctx, "someone.else@example.com", otherHash, false)
	if err != nil {
		t.Fatalf("creating the other user: %v", err)
	}
	account, err := h.store.CreateAccount(ctx, store.CreateAccountParams{
		UserID: other.ID, EmailAddress: "private@example.com",
		UpstreamHost: "imap.example.com", UpstreamPort: 993,
		UpstreamTLSMode: "tls", EncryptedUsername: []byte{1}, EncryptedSecret: []byte{2},
	})
	if err != nil {
		t.Fatalf("creating the other account: %v", err)
	}

	// Sign in as the admin's non-owning peer: this harness's user is an admin,
	// so demote to prove ownership scoping rather than admin override.
	if _, err := h.store.Pool().Exec(ctx,
		`UPDATE users SET is_admin = FALSE WHERE email = $1`, testEmail); err != nil {
		t.Fatalf("demoting the test user: %v", err)
	}
	h.signIn(t)

	resp := h.get(t, "/accounts/"+itoa(account.ID))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("another user's account returned %d, want 404", resp.StatusCode)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
