// Package db owns the Postgres connection pool and schema migrations.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the shared connection pool. It is an alias rather than a wrapper so
// callers can use pgx directly; the value of this package is construction,
// tuning and migration, not another layer of indirection over queries.
type Pool = pgxpool.Pool

// Open builds a configured pool and verifies it can reach the database.
func Open(ctx context.Context, cfg config.Config) (*Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DB.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}

	poolCfg.MaxConns = cfg.DB.MaxConns
	poolCfg.MinConns = cfg.DB.MinConns
	poolCfg.MaxConnLifetime = cfg.DB.ConnMaxLifetime.Std()
	poolCfg.ConnConfig.ConnectTimeout = cfg.DB.ConnectTimeout.Std()

	// A server-side statement timeout is the backstop for a query that would
	// otherwise pin a connection indefinitely. Context deadlines cover the
	// common case, but this also catches a caller that forgot one.
	if timeout := cfg.DB.StatementTimeout.Std(); timeout > 0 {
		if poolCfg.ConnConfig.RuntimeParams == nil {
			poolCfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] =
			fmt.Sprintf("%d", timeout.Milliseconds())
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "imapped"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.DB.ConnectTimeout.Std())
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return pool, nil
}

// Ping reports whether the database is reachable, for the readiness endpoint.
func Ping(ctx context.Context, pool *Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return pool.Ping(ctx)
}
