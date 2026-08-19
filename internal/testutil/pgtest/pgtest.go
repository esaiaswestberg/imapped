// Package pgtest provides throwaway Postgres databases for tests.
//
// The model is one server per test binary, one *database* per test. Migrations
// are applied once into a template database, and each test clones it with
// CREATE DATABASE ... TEMPLATE, which takes single-digit milliseconds. Tests
// therefore get complete isolation without paying migration cost per test and
// without the cross-test interference that comes from sharing a schema.
//
// The server comes from IMAPPED_TEST_PG_URL when set (fast local iteration
// against an already-running Postgres, and what CI uses via a service
// container). Otherwise a container is started automatically, so `go test`
// works on a clean checkout with no setup.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/db"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL names the environment variable holding a Postgres URL to reuse.
const EnvURL = "IMAPPED_TEST_PG_URL"

// templateDBName is unique per test binary.
//
// `go test ./...` runs each package's tests in a separate process, and those
// processes run concurrently. A single shared template name meant one package
// would drop and recreate the template while another was cloning from it,
// producing failures that looked like schema bugs but were pure interference.
var templateDBName = fmt.Sprintf("imapped_template_%d", os.Getpid())

var (
	setupOnce sync.Once
	adminURL  string
	setupErr  error
	// Guards CREATE DATABASE: Postgres refuses to use a template while another
	// session is connected to it, so clones must not race each other.
	cloneMu sync.Mutex
	dbSeq   int
)

// New returns a pool connected to a fresh, fully-migrated database that is
// dropped when the test finishes.
//
// Tests calling this must be built with the `integration` tag, so the default
// `go test ./...` stays hermetic and fast.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	setupOnce.Do(func() { adminURL, setupErr = setup() })
	if setupErr != nil {
		t.Fatalf("preparing test database server: %v", setupErr)
	}

	name := uniqueDBName()
	ctx := context.Background()

	cloneMu.Lock()
	err := withAdminConn(ctx, adminURL, func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, fmt.Sprintf(
			"CREATE DATABASE %s TEMPLATE %s", quoteIdent(name), quoteIdent(templateDBName)))
		return err
	})
	cloneMu.Unlock()
	if err != nil {
		t.Fatalf("creating test database %s: %v", name, err)
	}

	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = withAdminConn(dropCtx, adminURL, func(conn *pgx.Conn) error {
			_, err := conn.Exec(dropCtx,
				fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name)))
			return err
		})
	})

	// Configure the pool explicitly rather than taking pgx's default, which
	// scales with CPU count: on a many-core machine several test pools can
	// otherwise exhaust the server's connection limit and produce failures that
	// look like application bugs.
	poolCfg, err := pgxpool.ParseConfig(replaceDBName(adminURL, name))
	if err != nil {
		t.Fatalf("parsing test database URL: %v", err)
	}
	poolCfg.MaxConns = 10
	poolCfg.MinConns = 0

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("connecting to test database %s: %v", name, err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pinging test database %s: %v", name, err)
	}
	return pool
}

// URL returns a connection URL for a fresh migrated database, for code paths
// that build their own pool from configuration.
func URL(t *testing.T) string {
	t.Helper()
	pool := New(t)
	return pool.Config().ConnString()
}

// setup ensures a reachable server and a migrated template database.
func setup() (string, error) {
	baseURL := os.Getenv(EnvURL)
	if baseURL == "" {
		var err error
		baseURL, err = startContainer()
		if err != nil {
			return "", err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := waitForServer(ctx, baseURL); err != nil {
		return "", err
	}

	// Rebuild the template from scratch so a schema change in the working tree
	// is never masked by a stale template left over from an earlier run.
	//
	// Only this process's own template is touched. Sweeping others looks tidy
	// but is wrong: concurrent test binaries each own a live template, and
	// removing one out from under its owner breaks it. Abandoned templates are
	// harmless and disappear with the throwaway server.
	err := withAdminConn(ctx, baseURL, func(conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, fmt.Sprintf(
			"DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(templateDBName))); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, "CREATE DATABASE "+quoteIdent(templateDBName))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("creating template database: %w", err)
	}

	templateURL := replaceDBName(baseURL, templateDBName)
	cfg := config.Default()
	cfg.DB.URL = templateURL

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("connecting to template database: %w", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, logging.Discard()); err != nil {
		return "", fmt.Errorf("migrating template database: %w", err)
	}
	return baseURL, nil
}

func waitForServer(ctx context.Context, dsn string) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		err := withAdminConn(ctx, dsn, func(conn *pgx.Conn) error {
			return conn.Ping(ctx)
		})
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("database at %s never became ready: %w", redact(dsn), lastErr)
}

func withAdminConn(ctx context.Context, dsn string, fn func(*pgx.Conn) error) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return fn(conn)
}

func uniqueDBName() string {
	cloneMu.Lock()
	dbSeq++
	seq := dbSeq
	cloneMu.Unlock()
	return fmt.Sprintf("imapped_test_%d_%d", os.Getpid(), seq)
}

// quoteIdent quotes a SQL identifier. Database names cannot be parameterised,
// and these are constructed from a fixed prefix plus integers, but quoting
// keeps the habit correct.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func replaceDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

func redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable)"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}
