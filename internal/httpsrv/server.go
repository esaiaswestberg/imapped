// Package httpsrv serves the operational HTTP endpoints. The web UI mounts its
// own routes onto the same mux in a later phase; what lives here is the part
// that must work even when nothing else does.
package httpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"sort"
	"time"

	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/obs"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server wraps an http.Server with the lifecycle the run command expects.
type Server struct {
	name     string
	http     *http.Server
	log      *slog.Logger
	shutdown time.Duration
}

// Options configures a Server.
type Options struct {
	Name    string // used in log lines, e.g. "http" or "metrics"
	Addr    string
	Handler http.Handler
	Logger  *slog.Logger
	Config  config.Config
}

// New builds a Server. Note that WriteTimeout is deliberately left at whatever
// the configuration says (zero by default): server-sent events for live sync
// progress are long-lived responses, and a write deadline would sever them
// mid-stream.
func New(opts Options) *Server {
	cfg := opts.Config
	return &Server{
		name: opts.Name,
		log:  opts.Logger,
		http: &http.Server{
			Addr:              opts.Addr,
			Handler:           opts.Handler,
			ReadHeaderTimeout: cfg.HTTP.ReadTimeout.Std(),
			ReadTimeout:       cfg.HTTP.ReadTimeout.Std(),
			WriteTimeout:      cfg.HTTP.WriteTimeout.Std(),
			ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
		},
		shutdown: cfg.HTTP.ShutdownTimeout.Std(),
	}
}

// Serve listens and serves until ctx is cancelled, then drains gracefully.
// It binds before returning control, so a port conflict surfaces at startup
// rather than as a silently missing listener.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("binding %s listener on %s: %w", s.name, s.http.Addr, err)
	}
	s.log.Info("listening", "service", s.name, "addr", listener.Addr().String())

	errs := make(chan error, 1)
	go func() {
		err := s.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdown)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			// Drain deadline exceeded: close hard rather than hang forever.
			s.log.Warn("graceful shutdown timed out, closing connections",
				"service", s.name, "error", err)
			return s.http.Close()
		}
		s.log.Info("stopped", "service", s.name)
		return nil
	}
}

// OperationalHandler builds the always-available endpoints: liveness,
// readiness, metrics, and optionally pprof.
func OperationalHandler(cfg config.Config, metrics *obs.Metrics, health *obs.Health) *http.ServeMux {
	mux := http.NewServeMux()
	Mount(mux, cfg, metrics, health)
	return mux
}

// Mount attaches the operational endpoints to an existing mux.
func Mount(mux *http.ServeMux, cfg config.Config, metrics *obs.Metrics, health *obs.Health) {
	// Liveness: the process is up and the mux is answering. Deliberately does
	// not consult dependencies, so a database outage does not cause the
	// orchestrator to kill an otherwise healthy process.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writePlain(w, http.StatusOK, "ok")
	})

	// Readiness: every subsystem reports healthy. A failing dependency here
	// removes the instance from rotation without restarting it.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		ready, detail := health.Ready()
		names := make([]string, 0, len(detail))
		for name := range detail {
			names = append(names, name)
		}
		sort.Strings(names)

		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		body := ""
		for _, name := range names {
			if err := detail[name]; err != nil {
				body += fmt.Sprintf("%s: %s\n", name, err)
			} else {
				body += name + ": ok\n"
			}
		}
		writePlain(w, status, body)
	})

	if metrics != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{
			Registry:          metrics.Registry,
			EnableOpenMetrics: true,
		}))
	}

	if cfg.Web.PProf {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
