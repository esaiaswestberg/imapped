package httpsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/obs"
)

func testConfig() config.Config {
	cfg := config.Default()
	cfg.DB.URL = "postgres://localhost/imapped"
	return cfg
}

func get(t *testing.T, handler http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return rec.Code, string(body)
}

// Liveness must not depend on downstream health: a database outage should
// remove an instance from rotation, not have the orchestrator kill it.
func TestHealthzIgnoresSubsystemFailures(t *testing.T) {
	health := obs.NewHealth("db")
	health.Set("db", errors.New("connection refused"))
	handler := OperationalHandler(testConfig(), obs.NewMetrics("test", "abc"), health)

	if code, body := get(t, handler, "/healthz"); code != http.StatusOK || body != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 \"ok\"", code, body)
	}
}

func TestReadyzReflectsSubsystemState(t *testing.T) {
	health := obs.NewHealth("db", "storage")
	metrics := obs.NewMetrics("test", "abc")
	handler := OperationalHandler(testConfig(), metrics, health)

	// Nothing has reported in yet, so readiness must be false.
	code, _ := get(t, handler, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz before startup = %d, want 503", code)
	}

	var body string

	health.Set("db", nil)
	code, body = get(t, handler, "/readyz")
	_ = body
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with one subsystem down = %d, want 503", code)
	}
	if !strings.Contains(body, "storage") {
		t.Errorf("readyz body should name the failing subsystem, got %q", body)
	}

	health.Set("storage", nil)
	code, body = get(t, handler, "/readyz")
	if code != http.StatusOK {
		t.Errorf("GET /readyz when all healthy = %d, want 200; body %q", code, body)
	}
	for _, want := range []string{"db: ok", "storage: ok"} {
		if !strings.Contains(body, want) {
			t.Errorf("readyz body missing %q, got %q", want, body)
		}
	}
}

// A failing subsystem must say which one it is; "not ready" alone sends the
// operator digging through logs.
func TestReadyzNamesTheFailure(t *testing.T) {
	health := obs.NewHealth("db")
	health.Set("db", errors.New("connection refused"))
	handler := OperationalHandler(testConfig(), nil, health)

	code, body := get(t, handler, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want 503", code)
	}
	if !strings.Contains(body, "connection refused") {
		t.Errorf("readyz should include the failure reason, got %q", body)
	}
}

func TestMetricsExposesBuildInfo(t *testing.T) {
	handler := OperationalHandler(testConfig(), obs.NewMetrics("1.2.3", "deadbeef"), obs.NewHealth())

	code, body := get(t, handler, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", code)
	}
	if !strings.Contains(body, `imapped_build_info{commit="deadbeef",version="1.2.3"} 1`) {
		t.Errorf("build info missing from /metrics output")
	}
	// The Go collector should be registered too.
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("runtime metrics missing from /metrics output")
	}
}

func TestPProfIsOptIn(t *testing.T) {
	cfg := testConfig()

	handler := OperationalHandler(cfg, nil, obs.NewHealth())
	if code, _ := get(t, handler, "/debug/pprof/"); code != http.StatusNotFound {
		t.Errorf("pprof should be absent by default, got %d", code)
	}

	cfg.Web.PProf = true
	handler = OperationalHandler(cfg, nil, obs.NewHealth())
	if code, _ := get(t, handler, "/debug/pprof/"); code != http.StatusOK {
		t.Errorf("pprof should be served when enabled, got %d", code)
	}
}

// Binding must fail loudly at startup rather than leaving a listener silently
// missing, which is how a port conflict turns into a support ticket.
func TestServeReportsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer occupied.Close()

	server := New(Options{
		Name: "http", Addr: occupied.Addr().String(),
		Handler: http.NewServeMux(), Logger: logging.Discard(), Config: testConfig(),
	})

	err = server.Serve(context.Background())
	if err == nil {
		t.Fatal("expected a bind conflict to be reported")
	}
	if !strings.Contains(err.Error(), "binding") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestServeShutsDownOnContextCancel(t *testing.T) {
	server := New(Options{
		Name: "http", Addr: "127.0.0.1:0",
		Handler: OperationalHandler(testConfig(), nil, obs.NewHealth()),
		Logger:  logging.Discard(), Config: testConfig(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()

	// Give Serve a moment to bind, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of cancellation")
	}
}

// End-to-end over a real socket, mirroring how the process actually runs.
func TestServeHandlesRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close() // reuse the address we just proved is free

	server := New(Options{
		Name: "http", Addr: addr,
		Handler: OperationalHandler(testConfig(), obs.NewMetrics("t", "c"), obs.NewHealth()),
		Logger:  logging.Discard(), Config: testConfig(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()

	url := fmt.Sprintf("http://%s/healthz", addr)
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = http.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want \"ok\"", body)
	}
}
