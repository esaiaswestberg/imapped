// Package store holds the typed database queries.
//
// Queries are written by hand against pgx rather than generated. The
// performance-critical paths here are batch inserts and claim-style updates
// whose SQL wants to be read and tuned directly, and avoiding a code generator
// keeps the build to a single `go build`.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/esaiaswestberg/imapped/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store provides database access.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps a pool.
func New(pool *db.Pool) *Store { return &Store{pool: pool} }

// Pool exposes the underlying pool for callers needing a dedicated connection,
// such as the advisory lock holder.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// InTx runs fn inside a transaction, committing on success and rolling back on
// error or panic.
func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op, so this is safe
		// unconditionally and also covers the panic path.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// notFound maps pgx's no-rows sentinel onto the package's own.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// isNoRows reports whether err is pgx's no-rows sentinel.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
