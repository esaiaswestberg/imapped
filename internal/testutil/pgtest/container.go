package pgtest

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// containerImage pins the Postgres version tests run against. It must match the
// version used in production, since this project depends on version-specific
// behaviour: generated columns, FOR UPDATE SKIP LOCKED, and pg_trgm.
const containerImage = "postgres:17-alpine"

// startContainer boots a throwaway Postgres and returns its connection URL.
// The container is reaped by testcontainers' Ryuk sidecar when the test process
// exits, including when it exits abnormally.
func startContainer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        containerImage,
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "imapped",
			"POSTGRES_PASSWORD": "imapped",
			"POSTGRES_DB":       "postgres",
		},
		// Postgres restarts itself once during first-time initialisation, so
		// waiting for a single "ready" line races the restart. Requiring two
		// occurrences waits for the real one.
		WaitingFor: wait.ForAll(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			wait.ForListeningPort("5432/tcp"),
		).WithDeadline(2 * time.Minute),
		// Durability is pointless for a database that lives for one test run,
		// and turning it off is a large speedup on the write-heavy sync tests.
		Cmd: []string{"postgres", "-c", "fsync=off", "-c", "full_page_writes=off",
			"-c", "synchronous_commit=off"},
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return "", fmt.Errorf("starting postgres container (is Docker running? "+
			"set %s to use an existing server instead): %w", EnvURL, err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("resolving container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return "", fmt.Errorf("resolving container port: %w", err)
	}

	return fmt.Sprintf("postgres://imapped:imapped@%s:%s/postgres?sslmode=disable",
		host, port.Port()), nil
}
