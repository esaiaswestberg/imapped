package cli

import (
	"context"
	"errors"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/httpsrv"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/obs"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

func newRunCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the server",
		Long: "Starts the configured listeners and background workers, and runs until\n" +
			"interrupted. Shutdown is graceful: in-flight requests are drained and any\n" +
			"running sync is cancelled cleanly so no account lock is left held.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := config.MustLoad(opts.configPath)
			if err != nil {
				return err
			}
			return run(cmd.Context(), res)
		},
	}
}

func run(ctx context.Context, res *config.Result) error {
	cfg := res.Config
	log := logging.New(cfg)

	// SIGINT/SIGTERM cancel the root context, which every subsystem observes.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		"version", Version,
		"commit", Commit,
		"app_env", cfg.AppEnv,
		"config_file", configFileLabel(res),
	)
	warnOnRiskyConfig(log, cfg)

	metrics := obs.NewMetrics(Version, Commit)
	// Subsystems register here as they come up; readiness stays false until all
	// of them report healthy.
	health := obs.NewHealth("http")

	group, ctx := errgroup.WithContext(ctx)

	if cfg.HTTP.Bind != "" {
		handler := httpsrv.OperationalHandler(cfg, metrics, health)
		server := httpsrv.New(httpsrv.Options{
			Name: "http", Addr: cfg.HTTP.Bind, Handler: handler,
			Logger: log, Config: cfg,
		})
		group.Go(func() error { return server.Serve(ctx) })
		health.Set("http", nil)
	}

	// A separate metrics listener lets the web UI be exposed publicly while
	// metrics stay on an internal interface.
	if cfg.HTTP.MetricsBind != "" && cfg.HTTP.MetricsBind != cfg.HTTP.Bind {
		handler := httpsrv.OperationalHandler(cfg, metrics, health)
		server := httpsrv.New(httpsrv.Options{
			Name: "metrics", Addr: cfg.HTTP.MetricsBind, Handler: handler,
			Logger: log, Config: cfg,
		})
		group.Go(func() error { return server.Serve(ctx) })
	}

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// warnOnRiskyConfig surfaces settings that are legal but deserve attention.
// Validate rejects what is outright wrong; this covers what merely deserves a
// second look before it becomes a 2am incident.
func warnOnRiskyConfig(log *slog.Logger, cfg config.Config) {
	if cfg.EncryptionMasterKey == "" {
		log.Warn("no encryption_master_key set; upstream credentials cannot be stored securely")
	}
	if !cfg.UseS3() {
		log.Info("using local filesystem blob storage", "path", cfg.Storage.Path)
	}
	if cfg.Web.Enabled && !cfg.Web.SecureCookies && cfg.IsProduction() {
		log.Warn("web.secure_cookies is disabled in production; session cookies will be sent over plaintext HTTP")
	}
	if cfg.Web.PProf {
		log.Warn("pprof endpoints are enabled; do not expose this listener publicly")
	}
}

func configFileLabel(res *config.Result) string {
	if res.FileLoaded {
		return res.FilePath
	}
	return "(none)"
}
