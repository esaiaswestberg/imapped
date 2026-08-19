# imapped

An IMAP caching proxy. It mirrors your mail accounts into local storage, serves
them to mail clients over IMAP, and gives you a web interface to manage and
search it all.

Mail stays readable when the upstream server is slow, rate-limited or down, and
search works over the full text of every message rather than whatever the
provider chose to index.

## What it does

- **Mirrors IMAP accounts** into Postgres (metadata) and disk (message bodies).
- **Serves mail clients** over IMAP on ports 143/993, so Thunderbird and friends
  talk to imapped instead of to the provider.
- **Full-text search** over subjects, correspondents and complete message text.
- **A web interface** for adding accounts, watching syncs, browsing and
  searching, with live progress as mail arrives.
- **Pushes changes back**: marking a message read locally reaches the upstream
  server.

## Getting started

```bash
cp .env.example .env          # fill in the two generated secrets
docker compose -f compose.prod.yaml up -d
```

Then open the web interface, sign in with the bootstrap account, and add a mail
account. Syncing starts immediately.

For local development:

```bash
docker compose up --build
```

which brings up the app on <http://localhost:8080> with an `admin@example.com`
account, password `development-password`.

### Without Docker

```bash
go build -o imapped ./cmd/imapped
export DATABASE_URL="postgres://user:pass@localhost:5432/imapped"
export ENCRYPTION_MASTER_KEY="$(openssl rand -hex 32)"

./imapped migrate
./imapped user create --email you@example.com
./imapped run
```

## Configuration

Configuration comes from a TOML file and the environment, with the environment
taking precedence. Every setting is documented in
[`imapped.example.toml`](imapped.example.toml).

```bash
./imapped config check          # validate and exit non-zero if unusable
./imapped config show --all     # every value, and where it came from
```

`config show` reports the source of each value — a built-in default, the file,
or a specific environment variable — which turns "why did my setting not take
effect" into a glance. The same table is on the web interface's settings page.
Unknown keys in the file are a startup error rather than a silent no-op.

Two settings need attention before production:

- `ENCRYPTION_MASTER_KEY` protects upstream credentials at rest. Generate one
  with `openssl rand -hex 32`. **Changing it makes every stored credential
  unreadable**, and accounts will need their passwords re-entered.
- `DATABASE_URL` is required and has no default.

## Commands

Almost everything is done through the web interface. The CLI covers what
genuinely needs a shell:

| Command | Purpose |
|---|---|
| `imapped run` | Run the server |
| `imapped migrate` | Apply database migrations |
| `imapped migrate status` | Show which migrations have been applied |
| `imapped user create --email …` | Create a user (needed once, to sign in) |
| `imapped user set-password --email …` | Change a password |
| `imapped config check` | Validate configuration |
| `imapped config show` | Show effective configuration and its sources |
| `imapped version` | Print version information |

No command performs sync work in its own process; anything that would trigger
work goes through the running server.

## Connecting a mail client

Point the client at the host running imapped, port 143 (STARTTLS) or 993
(TLS), and sign in with **your imapped credentials** — not the upstream mail
account's. The upstream password stays encrypted in the database and is never
handed out.

Mirrored mailboxes are read-only apart from flags: marking messages read or
flagged works and is pushed upstream. Creating mailboxes, moving and deleting
messages are refused rather than silently accepted, so a client never believes
a change succeeded when it did not.

`SORT` and `THREAD` are not advertised, so clients sort and thread locally as
they do against most IMAP servers.

## How syncing works

Each mailbox syncs in two passes.

The **metadata pass** enumerates messages in one command per chunk rather than
one per message. Where the server supports `CONDSTORE`, flag changes come back
through `CHANGEDSINCE` and a mailbox with nothing new is settled by `SELECT`
alone — a single command.

The **body pass** then downloads whatever the first pass recorded as missing,
batched by byte budget across several connections, newest first so recent mail
appears while an old archive is still transferring. Each message carries its own
download state, so an interrupted sync resumes without re-downloading anything.

Bodies are content-addressed, so a message appearing in several mailboxes is
stored once.

**Cost, for a mailbox of 8,300 messages:**

| | commands |
|---|---|
| First sync | ~1 for metadata, plus one per body batch |
| Nothing changed | 1 |
| 20 new messages | 3 |

Every network operation is bounded by a deadline: dial, TLS handshake, greeting,
each command, and an inactivity window on the connection itself. `TCP_USER_TIMEOUT`
makes the kernel surface a black-holed peer in seconds rather than the ~11
minutes Linux defaults to. A sync that stops making progress fails and is
reported; it cannot hang indefinitely.

Only one sync per account runs at a time, held by a Postgres advisory lock on a
dedicated session. If the process dies the database releases the lock
immediately — there is no lease to expire and nothing to clear by hand.

## Operating it

- `GET /healthz` — liveness. Deliberately independent of the database, so an
  outage removes the instance from rotation rather than getting it restarted.
- `GET /readyz` — readiness, naming any failing subsystem.
- `GET /metrics` — Prometheus metrics. Set `HTTP_METRICS_BIND` to serve these on
  a separate, internal listener.

The **History** page lists every sync attempt. A run whose heartbeat stopped
while still marked running belonged to a process that died, and is shown as
stalled.

## Development

```bash
make test              # unit tests, hermetic, no Docker needed
make test-integration  # integration tests against a throwaway Postgres
make lint
```

Integration tests are behind the `integration` build tag so the default suite
stays fast. They use a throwaway Postgres, cloning a migrated template database
per test; set `IMAPPED_TEST_PG_URL` to reuse a server you already have running.

`internal/testutil/fakeimap` runs a real IMAP server for tests, with hooks to
make it misbehave in ways a correct server cannot — hanging mid-command,
dropping the connection, trickling bytes. Those are the regression tests for the
timeout handling, and its command recorder is what keeps the sync from silently
regressing to one request per message.

### Layout

```
cmd/imapped        entry point
internal/
  app              wiring
  blob             content-addressed body storage
  cli              command tree
  config           settings, layering, validation
  crypto           password hashing, credential sealing
  db               connection pool, migrations
  imapsrv          IMAP server for mail clients
  mailstore        MIME parsing and ingest
  search           full-text search
  store            database queries
  syncer           the mirroring engine
  upstream         IMAP client for the mirrored server
  web              browser interface
```

## Licence

See [LICENSE](LICENSE).
