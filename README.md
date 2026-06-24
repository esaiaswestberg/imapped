# imapped

`imapped` is a Rust IMAP caching proxy and mirror service.

It sits between downstream mail clients and one or more upstream IMAP servers. The server mirrors mailboxes into local storage, serves cached bodies and metadata locally, indexes parsed message content for search, and pushes local mutations back upstream when required.

## Table of contents

- [Get started](#get-started)
- [Overview](#overview)
- [Repository layout](#repository-layout)
- [Configuration](#configuration)
  - [Runtime config](#runtime-config)
  - [Environment variables](#environment-variables)
  - [Bootstrap authentication](#bootstrap-authentication)
  - [Upstream IMAP accounts](#upstream-imap-accounts)
  - [HTTP and metrics](#http-and-metrics)
  - [TLS certificates](#tls-certificates)
- [Production deployment](#production-deployment)
  - [Docker Compose](#docker-compose)
  - [Portainer](#portainer)
  - [Operational checklist](#operational-checklist)
- [Administration](#administration)
- [Architecture](#architecture)
- [Protocol surface](#protocol-surface)
- [Local development](#local-development)
- [Testing](#testing)
- [Troubleshooting](#troubleshooting)

## Get started

This is the fastest path to a working production-style deployment.

1. Prepare a production environment file.

   Start from [`.env.example`](./.env.example) and create something like `.env.production`. At minimum, set:

   - `APP_ENV=production`
   - `APP_BASE_URL=https://mail.example.com`
   - `ENCRYPTION_MASTER_KEY=<random-secret>`
   - `DATABASE_URL=<production-postgres-url>`
   - `REDIS_URL=<production-redis-url>`
   - `R2_ENDPOINT=<s3-compatible-endpoint>`
   - `R2_BUCKET=<bucket-name>`
   - `R2_ACCESS_KEY_ID=<access-key>`
   - `R2_SECRET_ACCESS_KEY=<secret-key>`
   - `IMAP_TLS_CERT_PATH=/certs/imap.crt`
   - `IMAP_TLS_KEY_PATH=/certs/imap.key`
   - `OBJECT_STORE_PATH=/app/data/blob`
   - `SEARCH_INDEX_PATH=/app/data/search`

2. Mount a TLS certificate and key.

   The application expects PEM files. Use a CA-issued certificate for a public deployment, or a self-signed certificate if your clients trust it.

3. Start the stack.

   The repository includes [`docker-compose.prod.yml`](./docker-compose.prod.yml). It runs the app container plus PostgreSQL and Redis, while assuming an external S3-compatible object store.

   ```bash
   docker compose -f docker-compose.prod.yml up -d
   ```

4. Run migrations.

   ```bash
   docker compose -f docker-compose.prod.yml run --rm imap-cache run-migrations
   ```

5. Create a local application user.

   ```bash
   docker compose -f docker-compose.prod.yml run --rm imap-cache create-user \
     --username-email user@example.test \
     --password 'change-me'
   ```

6. Add an upstream IMAP account.

   Upstream settings are per account, not global.

   ```bash
   docker compose -f docker-compose.prod.yml run --rm imap-cache add-account \
     --user-email user@example.test \
     --display-name "Primary Mail" \
     --email-address user@example.test \
     --upstream-host imap.provider.example \
     --upstream-port 993 \
     --upstream-tls-mode tls \
     --upstream-auth-method login \
     --upstream-username user@example.test \
     --upstream-secret 'upstream-password'
   ```

7. Point your mail client at the IMAP ports exposed by the stack.

   The service listens internally on `1143` for plaintext IMAP and `1993` for IMAPS/TLS in the default compose file.

## Overview

`imapped` is a production-shaped IMAP mirror built as a Rust workspace with separate crates for protocol handling, upstream access, sync, storage, search, coordination, and admin workflows.

The runtime behavior is:

1. A mail client connects to the IMAP frontend.
2. The frontend authenticates the session and routes commands through repository-backed state.
3. Reads are served from PostgreSQL metadata and cached object storage where possible.
4. Cache misses and sync gaps are filled from the upstream IMAP server configured for that account.
5. Mutations are written locally first, then replayed upstream through the mutation queue.
6. Redis and the in-process event hub fan mailbox changes out to active sessions and background workers.

There is currently no browser-based web UI. The HTTP listener is for health and metrics style endpoints, not end-user mail access.

## Repository layout

This repository is a Rust workspace. The main pieces are:

- `crates/core/` for shared domain types, errors, and security helpers.
- `crates/config/` for configuration loading.
- `crates/auth/` for authentication and account bootstrap logic.
- `crates/db/` for SQLx repositories and migrations.
- `crates/imap-server/` for the IMAP frontend and HTTP endpoints.
- `crates/upstream/` for the upstream IMAP client.
- `crates/sync/` for mailbox synchronization and mutation replay.
- `crates/storage/` for object storage abstractions and backends.
- `crates/search/` for Tantivy indexing and search.
- `crates/notifications/` for mailbox events and Redis relay hooks.
- `crates/test-support/` for shared live-test helpers.
- `src/` for the top-level binary and admin wiring.

## Configuration

Configuration can be supplied either through environment variables or a TOML file.

- Use `--config /path/to/config.toml` to point the binary at a config file.
- Alternatively set `APP_CONFIG_PATH=/path/to/config.toml`.
- See [`config.example.toml`](./config.example.toml) for a config-file example.
- See [`.env.example`](./.env.example) for an environment-variable example.

### Runtime config

The important runtime settings include:

- listener bind addresses for IMAP, HTTP, and metrics
- `DATABASE_URL` for PostgreSQL
- `REDIS_URL` for Redis fanout and coordination
- `R2_*` values for the external S3-compatible object store endpoint and credentials
- `IMAP_TLS_CERT_PATH` and `IMAP_TLS_KEY_PATH` for TLS
- `ENCRYPTION_MASTER_KEY` for secret handling

`APP_BASE_URL` is not a web UI. The current HTTP server exposes operational endpoints, not a browser application for mail clients.

`ENCRYPTION_MASTER_KEY` is a plain passphrase string. Do not treat it as base64 or base32 unless you are deliberately encoding a string yourself. Keep it stable across restarts.

### Environment variables

The production compose file and `.env.example` use these key groups:

- `IMAP_PLAINTEXT_BIND` and `IMAP_TLS_BIND` for the downstream mail client listeners
- `HTTP_BIND` and `METRICS_BIND` for operational endpoints
- `DATABASE_URL` for PostgreSQL
- `REDIS_URL` for Redis
- `R2_ENDPOINT`, `R2_BUCKET`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, and `R2_REGION` for blob storage
- `OBJECT_STORE_PATH` and `SEARCH_INDEX_PATH` for local runtime data
- `MAX_LITERAL_SIZE_BYTES`, `MAX_MESSAGE_SIZE_BYTES`, `DEFAULT_ACCOUNT_QUOTA_BYTES`, `SYNC_CONCURRENCY`, `UPSTREAM_CONNECTION_LIMIT_PER_ACCOUNT`, `LOGIN_RATE_LIMIT_FAILURES`, and `LOGIN_RATE_LIMIT_LOCKOUT_SECONDS` for safety tuning

### Bootstrap authentication

The service supports a bootstrap IMAP login path when `BOOTSTRAP_IMAP_USERNAME` is set together with either `BOOTSTRAP_IMAP_PASSWORD` or `BOOTSTRAP_IMAP_PASSWORD_HASH`.

Use this for local development, quick smoke tests, or a controlled bootstrap user. For normal production use, create application users in the database with the admin CLI instead.

### Upstream IMAP accounts

Upstream IMAP configuration is stored per account in the database. There is no separate global upstream host setting.

The `add-account` command stores:

- upstream host
- upstream port
- TLS mode
- authentication method
- upstream username
- upstream secret

Those values are then used by the upstream client when the account syncs or when you run `test-upstream` or `force-sync`.

### HTTP and metrics

The HTTP and metrics bind settings are optional. They do not help downstream IMAP clients connect.

- Use `HTTP_BIND` if you want the operational HTTP listener exposed.
- Use `METRICS_BIND` if you want a separate metrics listener.

If you do not need them, leave them unset and do not expose those ports.

### TLS certificates

The IMAP listener expects a certificate and private key on disk, mounted into the container at the paths referenced by `IMAP_TLS_CERT_PATH` and `IMAP_TLS_KEY_PATH`.

- Certificates should be PEM encoded.
- The key should be the matching private key for that certificate.
- For a public deployment, a CA-issued certificate such as one from Let's Encrypt is recommended.
- For a private deployment, a self-signed certificate is fine if your clients trust the issuing CA or the certificate directly.

## Production deployment

The recommended production shape is:

- one Docker container for `imapped`
- PostgreSQL as the canonical metadata store
- Redis for pub/sub, coordination, and short-lived cache/workers
- an external S3-compatible object store for raw message and MIME blobs
- TLS certificates mounted into the container for IMAP STARTTLS and implicit TLS

If you are using MinIO locally, treat that as a development or compatibility substitute, not as the production object store.

The release workflow publishes the Docker image to `ghcr.io/esaiaswestberg/imapped` with both the release tag and `latest`. Production Compose should normally pull `latest`; use a tag-specific image if you want to pin a deployment to a particular release.

### Docker Compose

The repository includes [`docker-compose.prod.yml`](./docker-compose.prod.yml). It pulls `ghcr.io/esaiaswestberg/imapped:latest`, provisions PostgreSQL and Redis locally, and leaves the object store external and S3-compatible.

Start it with:

```bash
docker compose -f docker-compose.prod.yml up -d
```

Run migrations before opening the service to clients:

```bash
docker compose -f docker-compose.prod.yml run --rm imap-cache run-migrations
```

Bootstrap users and accounts with the same compose file:

```bash
docker compose -f docker-compose.prod.yml run --rm imap-cache create-user \
  --username-email user@example.test \
  --password 'change-me'

docker compose -f docker-compose.prod.yml run --rm imap-cache add-account \
  --user-email user@example.test \
  --display-name "Primary Mail" \
  --email-address user@example.test \
  --upstream-host imap.provider.example \
  --upstream-port 993 \
  --upstream-tls-mode tls \
  --upstream-auth-method login \
  --upstream-username user@example.test \
  --upstream-secret 'upstream-password'
```

Notes:

- The container listens on `1143` and `1993` internally by default. Port mapping exposes standard IMAP ports on the host.
- The data volume holds the search index and other runtime data. Keep it persistent.
- The compose file is intentionally small and expects the S3-compatible object store to be provided externally.
- Do not set `command: ["run"]` in Portainer. The image already starts the binary by default, and overriding the command with `run` makes Docker try to execute a nonexistent `run` program.

If you want to build locally for testing or inspection, you can still do:

```bash
docker build -t imapped:latest .
```

### Portainer

Portainer works well with the same compose file, with one important caveat: do not add an explicit `command: ["run"]` override.

Use the image entrypoint as shipped by the container. If you want to be explicit for some reason, set an entrypoint to the binary path and then pass `run`, but that is not required for the normal deployment.

### Operational checklist

- `APP_ENV=production`
- `DATABASE_URL` points to production PostgreSQL
- `REDIS_URL` points to production Redis
- S3-compatible object store endpoint, bucket, and credentials are valid
- TLS certificate and key are mounted read-only
- persistent volume exists for local cache and search index data
- migrations have been run successfully
- at least one administrative user has been created
- one or more upstream mail accounts have been added

When those items are in place, the container is ready for normal IMAP client traffic.

## Administration

The binary exposes admin subcommands for common operational tasks.

Examples:

```bash
cargo run --bin imap-cache-rs -- --help
cargo run --bin imap-cache-rs -- run-migrations
cargo run --bin imap-cache-rs -- create-user --username-email user@example.test --password 'change-me'
cargo run --bin imap-cache-rs -- add-account \
  --user-email user@example.test \
  --display-name "Primary Mail" \
  --email-address user@example.test \
  --upstream-host imap.provider.example \
  --upstream-port 993 \
  --upstream-tls-mode tls \
  --upstream-auth-method login \
  --upstream-username user@example.test \
  --upstream-secret 'upstream-password'
cargo run --bin imap-cache-rs -- list-accounts --user-email user@example.test
cargo run --bin imap-cache-rs -- test-upstream --account-email user@example.test
```

Useful commands:

- `create-user` creates an application user.
- `add-account` creates a mail account and stores the upstream IMAP connection details for that account.
- `test-upstream` verifies that a saved account can reach and authenticate with its upstream server.
- `force-sync` triggers a sync for a specific account.
- `list-accounts`, `list-mailboxes`, and `show-sync-status` help with day-to-day operations.

## Architecture

The codebase is split into focused crates so the production boundaries are visible in code:

- `crates/imap-server/` owns the IMAP frontend and HTTP endpoints.
- `crates/upstream/` owns the upstream IMAP client.
- `crates/sync/` owns mailbox synchronization, checkpointing, and mutation replay.
- `crates/db/` owns SQLx repositories and schema migrations.
- `crates/storage/` owns R2/S3, filesystem, and memory object stores.
- `crates/search/` owns Tantivy indexing and search.
- `crates/auth/` owns local authentication and account bootstrap logic.
- `crates/notifications/` owns mailbox events and Redis relay hooks.
- `crates/core/` carries shared domain types, errors, and security helpers.
- `crates/config/` owns configuration loading.
- `crates/test-support/` holds live-test helpers and credentials parsing.

The service flow is:

1. A mail client connects to the IMAP frontend.
2. The frontend authenticates the session and routes commands through repository-backed state.
3. Reads are served from PostgreSQL metadata and cached object storage where possible.
4. Cache misses and sync gaps are filled from the upstream IMAP server configured for that account.
5. Mutations are written locally first, then replayed upstream through the mutation queue.
6. Redis and the in-process event hub fan mailbox changes out to active sessions and background workers.

Storage is split by responsibility:

- PostgreSQL stores canonical metadata, mailbox state, sync checkpoints, pending mutations, quotas, and audit records.
- Object storage stores raw RFC822 blobs, MIME body blobs, attachment data, and other large cached objects.
- Tantivy stores searchable content derived from parsed MIME bodies and headers.
- Redis supports pub/sub fanout, worker coordination, and short-lived shared state.

## Protocol surface

The IMAP frontend advertises and implements the current production command set.

### Advertised capabilities

- `IMAP4rev1`
- `STARTTLS`
- `AUTH=PLAIN`
- `AUTH=XOAUTH2`
- `UIDPLUS`
- `NAMESPACE`
- `SPECIAL-USE`
- `LIST-STATUS`
- `IDLE`
- `CONDSTORE`
- `ENABLE`
- `ID`
- `ESEARCH`
- `MOVE`
- `SORT`
- `THREAD=REFERENCES`
- `THREAD=ORDEREDSUBJECT`
- `UNSELECT`

Capability advertisement is state-dependent. For example, `STARTTLS` is only advertised while the connection is still in cleartext, and the authenticated capability set is larger than the unauthenticated one.

### Implemented commands

- `CAPABILITY`
- `NOOP`
- `LOGOUT`
- `STARTTLS`
- `LOGIN`
- `AUTHENTICATE`
- `SELECT`
- `EXAMINE`
- `CREATE`
- `DELETE`
- `RENAME`
- `SUBSCRIBE`
- `UNSUBSCRIBE`
- `LIST`
- `LSUB`
- `STATUS`
- `APPEND`
- `CHECK`
- `CLOSE`
- `EXPUNGE`
- `SEARCH`
- `FETCH`
- `STORE`
- `COPY`
- `MOVE`
- `UID`
- `IDLE`
- `UNSELECT`
- `NAMESPACE`
- `ENABLE`
- `CONDSTORE`
- `ID`
- `THREAD`
- `LIST-STATUS`

Implementation notes:

- `FETCH` supports raw RFC822 reads, partial fetches, and common metadata items.
- `SEARCH` uses PostgreSQL for structured fields and Tantivy for text-oriented queries.
- The sync engine preserves raw message bytes and uses content-addressed object storage for large payloads.
- Flags are reconciled from upstream state so local mailbox metadata does not drift silently.
- Live upstream tests use real credentials from `.testing-credentials` and are serialized so they do not race each other across test binaries.

## Local development

The repository includes a Docker-based local stack with PostgreSQL, Redis, MinIO-compatible storage, and an optional Dovecot upstream for integration tests.

```bash
make up
```

Run tests:

```bash
make test
```

Bring up the optional test upstream:

```bash
make up-test
```

Shut the stack down:

```bash
make down
```

## Testing

The test suite includes unit, integration, protocol, sync, storage, search, metrics, and live-upstream coverage.

Live upstream tests read credentials from `.testing-credentials` in the repository root and use the listed IMAP SSL/TLS endpoint plus username/password. Those tests are serialized with a shared file lock so they can run safely across multiple cargo test binaries.

Run the full suite with:

```bash
cargo test
```

## Troubleshooting

- If `add-account` says `user not found`, create the application user first with `create-user`, then rerun `add-account` with the same email address.
- If Portainer shows `exec: "run": executable file not found in $PATH`, remove the `command: ["run"]` override from the compose service.
- If downstream clients cannot connect, check the IMAP TLS certificate paths, port mappings, and whether the IMAP ports are exposed on the host.
- If sync or storage fails after restart, confirm that `ENCRYPTION_MASTER_KEY` has not changed and that the PostgreSQL and object storage volumes are still intact.
