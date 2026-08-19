package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/esaiaswestberg/imapped/internal/app"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/db"
	"github.com/esaiaswestberg/imapped/internal/httpsrv"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/obs"
	"github.com/esaiaswestberg/imapped/internal/web"
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

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		"version", Version, "commit", Commit,
		"app_env", cfg.AppEnv, "config_file", configFileLabel(res))
	warnOnRiskyConfig(log, cfg)

	application, err := app.Open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	if err := bootstrapUser(ctx, application); err != nil {
		return err
	}

	// A run still marked as running at startup belonged to a process that died.
	// Marking it failed is what turns an invisible hang into a visible one.
	if n, err := application.Store.MarkOrphanedRuns(ctx, cfg.Sync.MaxRunDuration.Std()); err != nil {
		log.Warn("marking orphaned sync runs", "error", err)
	} else if n > 0 {
		log.Warn("found sync runs left behind by a previous process", "count", n)
	}

	metrics := obs.NewMetrics(Version, Commit)
	health := obs.NewHealth("database")

	group, ctx := errgroup.WithContext(ctx)

	if cfg.HTTP.Bind != "" {
		mux := http.NewServeMux()
		httpsrv.Mount(mux, cfg, metrics, health)

		if cfg.Web.Enabled {
			ui, err := web.New(web.Options{
				Config: cfg, Store: application.Store, Blobs: application.Blobs,
				Search: application.Search, Engine: application.Engine,
				Sealer: application.Sealer, Logger: log, Provenance: res.Fields,
			})
			if err != nil {
				return err
			}
			ui.Mount(mux)
			log.Info("web interface enabled", "url", cfg.AppBaseURL)
		}

		server := httpsrv.New(httpsrv.Options{
			Name: "http", Addr: cfg.HTTP.Bind, Handler: mux, Logger: log, Config: cfg,
		})
		group.Go(func() error { return server.Serve(ctx) })
	}

	if cfg.HTTP.MetricsBind != "" && cfg.HTTP.MetricsBind != cfg.HTTP.Bind {
		server := httpsrv.New(httpsrv.Options{
			Name: "metrics", Addr: cfg.HTTP.MetricsBind,
			Handler: httpsrv.OperationalHandler(cfg, metrics, health),
			Logger:  log, Config: cfg,
		})
		group.Go(func() error { return server.Serve(ctx) })
	}

	group.Go(func() error { return watchDatabase(ctx, application, health) })

	if cfg.Sync.Enabled {
		group.Go(func() error { return syncLoop(ctx, application) })
		group.Go(func() error { return maintenanceLoop(ctx, application) })
	} else {
		log.Info("background syncing is disabled; syncs must be started from the interface")
	}

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// watchDatabase keeps readiness in step with database reachability.
func watchDatabase(ctx context.Context, a *app.App, health *obs.Health) error {
	check := func() {
		if err := db.Ping(ctx, a.Pool); err != nil {
			health.Set("database", err)
			return
		}
		health.Set("database", nil)
	}
	check()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			check()
		}
	}
}

// syncLoop syncs every eligible account on a schedule.
func syncLoop(ctx context.Context, a *app.App) error {
	// A short initial delay lets the listeners bind first, so the interface is
	// reachable while the first sync is running.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(5 * time.Second):
	}

	ticker := time.NewTicker(a.Config.Sync.Interval.Std())
	defer ticker.Stop()

	syncAll(ctx, a, "startup")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			syncAll(ctx, a, "scheduled")
		}
	}
}

func syncAll(ctx context.Context, a *app.App, trigger string) {
	accounts, err := a.Store.ListSyncableAccounts(ctx)
	if err != nil {
		a.Log.Error("listing accounts to sync", "error", err)
		return
	}

	// Accounts sync concurrently up to the configured limit. One account
	// failing must not delay the others, so each runs independently.
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(a.Config.Sync.AccountsConcurrent)

	for _, account := range accounts {
		group.Go(func() error {
			if _, err := a.Engine.SyncAccount(groupCtx, account.ID, trigger); err != nil {
				a.Log.Error("syncing account failed",
					"account_id", account.ID, "address", account.EmailAddress, "error", err)
			}
			// Never propagated: one bad account must not cancel the rest.
			return nil
		})
	}
	_ = group.Wait()
}

// maintenanceLoop performs periodic housekeeping.
func maintenanceLoop(ctx context.Context, a *app.App) error {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Messages claimed by a process that died would otherwise stay in
			// the fetching state and never be downloaded.
			if n, err := a.Store.ReapStaleClaims(ctx, a.Config.Sync.ClaimReapAfter.Std()); err != nil {
				a.Log.Warn("reaping abandoned message claims", "error", err)
			} else if n > 0 {
				a.Log.Info("returned abandoned messages to the download queue", "count", n)
			}
			if _, err := a.Store.DeleteExpiredSessions(ctx); err != nil {
				a.Log.Warn("removing expired sessions", "error", err)
			}
		}
	}
}

// bootstrapUser creates the first administrator when configured and no users
// exist, so a fresh deployment can reach the interface without shell access.
func bootstrapUser(ctx context.Context, a *app.App) error {
	if a.Config.Bootstrap.Email == "" || a.Config.Bootstrap.Password == "" {
		return nil
	}
	count, err := a.Store.CountUsers(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	hash, err := crypto.HashPassword(a.Config.Bootstrap.Password)
	if err != nil {
		return err
	}
	user, err := a.Store.CreateUser(ctx, a.Config.Bootstrap.Email, hash, true)
	if err != nil {
		return err
	}
	a.Log.Info("created the first administrator from bootstrap configuration",
		"email", user.Email)
	return nil
}

// warnOnRiskyConfig surfaces settings that are legal but deserve attention.
func warnOnRiskyConfig(log *slog.Logger, cfg config.Config) {
	if cfg.EncryptionMasterKey == "" {
		log.Warn("no encryption_master_key set; upstream credentials cannot be stored")
	}
	if cfg.Web.Enabled && !cfg.Web.SecureCookies && cfg.IsProduction() {
		log.Warn("web.secure_cookies is disabled in production; session cookies will travel over plaintext HTTP")
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
