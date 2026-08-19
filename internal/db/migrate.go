package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationFS re-roots the embedded files so the migrations sit at the root of
// the filesystem goose walks, which is where it looks for them.
func migrationFS() (fs.FS, error) {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("locating embedded migrations: %w", err)
	}
	return sub, nil
}

// MigrationStatus describes one migration and whether it has been applied.
type MigrationStatus struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

// Migrate applies every pending migration.
//
// Migrations run under a Postgres session advisory lock, so two replicas
// starting simultaneously cannot both try to create the same table: the loser
// waits and then finds the schema already current. A session lock (rather than
// a lock table) is released by the server when the connection dies, so a
// process killed mid-migration cannot leave the lock held.
func Migrate(ctx context.Context, pool *Pool, log *slog.Logger) error {
	fsys, err := migrationFS()
	if err != nil {
		return err
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("creating migration lock: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys,
		goose.WithSessionLocker(locker),
		goose.WithSlog(log),
	)
	if err != nil {
		return fmt.Errorf("preparing migrations: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	for _, r := range results {
		log.Info("applied migration", "version", r.Source.Version, "name", r.Source.Path)
	}
	if len(results) == 0 {
		log.Debug("database schema is up to date")
	}
	return nil
}

// Status returns every migration and whether it has been applied.
func Status(ctx context.Context, pool *Pool) ([]MigrationStatus, error) {
	fsys, err := migrationFS()
	if err != nil {
		return nil, err
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
	if err != nil {
		return nil, fmt.Errorf("preparing migrations: %w", err)
	}
	sources, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading migration status: %w", err)
	}

	out := make([]MigrationStatus, 0, len(sources))
	for _, s := range sources {
		out = append(out, MigrationStatus{
			Version:   s.Source.Version,
			Name:      s.Source.Path,
			Applied:   s.State == goose.StateApplied,
			AppliedAt: s.AppliedAt,
		})
	}
	return out, nil
}
