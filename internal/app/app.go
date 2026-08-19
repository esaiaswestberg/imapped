// Package app wires the subsystems together.
//
// Construction lives here rather than in the CLI so that the server, the admin
// commands and the tests all build the same object graph in the same way.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/esaiaswestberg/imapped/internal/blob"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/db"
	"github.com/esaiaswestberg/imapped/internal/search"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/esaiaswestberg/imapped/internal/syncer"
)

// App holds the constructed subsystems.
type App struct {
	Config config.Config
	Log    *slog.Logger

	Pool   *db.Pool
	Store  *store.Store
	Blobs  blob.Store
	Search search.Searcher
	Sealer *crypto.Sealer
	Engine *syncer.Engine
}

// Open builds the application from configuration.
func Open(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}

	if cfg.DB.AutoMigrate {
		if err := db.Migrate(ctx, pool, log); err != nil {
			pool.Close()
			return nil, err
		}
	}

	blobs, err := openBlobStore(cfg, log)
	if err != nil {
		pool.Close()
		return nil, err
	}

	// A missing master key is tolerated outside production so the process can
	// still start and report the problem through the interface, rather than
	// refusing to boot and leaving the operator with only a log line.
	var sealer *crypto.Sealer
	if cfg.EncryptionMasterKey != "" {
		sealer, err = crypto.NewSealer(cfg.EncryptionMasterKey)
		if err != nil {
			pool.Close()
			return nil, err
		}
	} else {
		log.Warn("no encryption master key is configured; accounts cannot be added until one is set")
	}

	st := store.New(pool)

	return &App{
		Config: cfg,
		Log:    log,
		Pool:   pool,
		Store:  st,
		Blobs:  blobs,
		Search: search.NewPostgres(pool, cfg.Search.Language),
		Sealer: sealer,
		Engine: syncer.New(cfg, st, blobs, sealer, log),
	}, nil
}

// Close releases resources.
func (a *App) Close() {
	if a.Pool != nil {
		a.Pool.Close()
	}
}

func openBlobStore(cfg config.Config, log *slog.Logger) (blob.Store, error) {
	if cfg.UseS3() {
		return nil, fmt.Errorf("S3 blob storage is configured but not implemented yet; " +
			"clear the s3_* settings to use local disk at storage.path")
	}
	log.Info("using local blob storage", "path", cfg.Storage.Path)

	store, err := blob.NewFSStore(cfg.Storage.Path)
	if err != nil && errors.Is(err, fs.ErrPermission) {
		// Overwhelmingly this means a container volume mounted at this path is
		// owned by root while the process runs as someone else. The bare
		// "permission denied" sends people looking for a bug in the software.
		return nil, fmt.Errorf("%w\n\n"+
			"The blob directory could not be created. If this is running in a container, "+
			"the volume mounted at %s is probably owned by root while the process runs as "+
			"a non-root user. Fix the ownership with:\n"+
			"  docker run --rm -v <volume-name>:/data alpine chown -R 65532:65532 /data",
			err, cfg.Storage.Path)
	}
	return store, err
}
