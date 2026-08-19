-- +goose Up

-- Accounts and identity -------------------------------------------------------

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,
    disabled_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Addresses are case-insensitive in practice, so uniqueness must be too;
-- otherwise Alice@example.com and alice@example.com become separate logins.
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

-- How an upstream account authenticates. Stored as text rather than an enum
-- because new SASL mechanisms should not require a migration.
CREATE TABLE mail_accounts (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    display_name  TEXT        NOT NULL DEFAULT '',
    email_address TEXT        NOT NULL,

    upstream_host        TEXT    NOT NULL,
    upstream_port        INTEGER NOT NULL,
    upstream_tls_mode    TEXT    NOT NULL DEFAULT 'tls',
    upstream_auth_method TEXT    NOT NULL DEFAULT 'login',

    -- Sealed with the process encryption master key; never readable from a
    -- database dump alone.
    encrypted_username BYTEA NOT NULL,
    encrypted_secret   BYTEA NOT NULL,

    -- Capabilities observed on the last successful connection, cached so the
    -- UI and the sync planner can reason about the server without dialling it.
    upstream_capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,

    sync_paused BOOLEAN NOT NULL DEFAULT FALSE,
    -- ok | auth_failed | unreachable. auth_failed stops retrying until the
    -- credentials are edited, so a wrong password does not hammer the server
    -- until the provider blocks the address.
    auth_state  TEXT    NOT NULL DEFAULT 'ok',

    last_sync_at    TIMESTAMPTZ,
    last_sync_error TEXT,

    disabled_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mail_accounts_port_valid CHECK (upstream_port BETWEEN 1 AND 65535),
    CONSTRAINT mail_accounts_tls_mode_valid
        CHECK (upstream_tls_mode IN ('plain', 'tls', 'starttls')),
    CONSTRAINT mail_accounts_auth_state_valid
        CHECK (auth_state IN ('ok', 'auth_failed', 'unreachable'))
);

CREATE UNIQUE INDEX mail_accounts_user_address_key
    ON mail_accounts (user_id, lower(email_address));

-- Mailboxes -------------------------------------------------------------------

-- Sync checkpoints live here as typed columns. The Rust version kept them in an
-- untyped JSONB blob, which could not be queried, could not be constrained, and
-- turned every read into a fallible cast.
CREATE TABLE mailboxes (
    id         BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES mail_accounts (id) ON DELETE CASCADE,

    name           TEXT NOT NULL,          -- as the server spells it
    canonical_name TEXT NOT NULL,          -- case-folded, for lookup
    delimiter      TEXT,
    attributes     TEXT[] NOT NULL DEFAULT '{}',
    special_use    TEXT,                   -- \Sent, \Drafts, \Trash, ...
    subscribed     BOOLEAN NOT NULL DEFAULT TRUE,

    -- Upstream sync state.
    uidvalidity   BIGINT,
    uidnext       BIGINT,
    highestmodseq BIGINT,

    -- How far the metadata pass has enumerated. Advanced only after the chunk
    -- containing it has committed, so an interrupted pass resumes correctly.
    metadata_synced_through_uid BIGINT NOT NULL DEFAULT 0,
    last_full_scan_at           TIMESTAMPTZ,
    -- Set when UIDVALIDITY changes and the mailbox needs re-enumerating.
    resync_required             BOOLEAN NOT NULL DEFAULT FALSE,
    sync_generation             BIGINT NOT NULL DEFAULT 0,

    -- Allocator for locally-assigned UIDs, which must never be reused even
    -- after an expunge or a mail client's cache becomes invalid.
    local_uidnext BIGINT NOT NULL DEFAULT 1,

    exists_count BIGINT NOT NULL DEFAULT 0,
    unseen_count BIGINT NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mailboxes_local_uidnext_positive CHECK (local_uidnext >= 1)
);

CREATE UNIQUE INDEX mailboxes_account_canonical_key
    ON mailboxes (account_id, canonical_name);

-- Messages --------------------------------------------------------------------

-- One row per distinct message body per account. The same message appearing in
-- INBOX and All Mail is stored once and referenced twice, which the Rust schema
-- could not express: it had no uniqueness on the content hash and duplicated
-- both the row and the blob.
CREATE TABLE messages (
    id         BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES mail_accounts (id) ON DELETE CASCADE,

    -- Raw sha256 rather than hex text: half the storage and a faster compare.
    rfc822_sha256 BYTEA NOT NULL,
    rfc822_size   BIGINT NOT NULL,
    -- NULL until the body pass stores the blob.
    blob_key      TEXT,

    message_id_hdr TEXT,
    in_reply_to    TEXT,
    refs           TEXT[] NOT NULL DEFAULT '{}',
    subject        TEXT,

    -- {from:[], to:[], cc:[], bcc:[], reply_to:[]} in one column rather than
    -- five, since nothing ever queried them independently.
    addrs         JSONB NOT NULL DEFAULT '{}'::jsonb,
    envelope      JSONB NOT NULL DEFAULT '{}'::jsonb,
    bodystructure JSONB NOT NULL DEFAULT '{}'::jsonb,

    internal_date TIMESTAMPTZ,
    sent_date     TIMESTAMPTZ,

    -- The complete extracted text, not a truncated preview.
    body_text TEXT,
    preview   TEXT,

    -- Set when MIME parsing failed. The raw blob is still stored: a message we
    -- cannot parse must never be a message we lose.
    parse_failed BOOLEAN NOT NULL DEFAULT FALSE,
    parse_error  TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT messages_sha256_length CHECK (octet_length(rfc822_sha256) = 32),
    CONSTRAINT messages_size_nonnegative CHECK (rfc822_size >= 0)
);

CREATE UNIQUE INDEX messages_account_sha256_key ON messages (account_id, rfc822_sha256);
CREATE INDEX messages_account_msgid_idx ON messages (account_id, message_id_hdr)
    WHERE message_id_hdr IS NOT NULL;
CREATE INDEX messages_account_date_idx ON messages (account_id, internal_date DESC);

-- The link between a message and the mailbox it appears in. Flags, UIDs and
-- body-fetch state live here because they are per-appearance, not per-message.
CREATE TABLE mailbox_messages (
    id         BIGSERIAL PRIMARY KEY,
    mailbox_id BIGINT NOT NULL REFERENCES mailboxes (id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,

    local_uid       BIGINT NOT NULL,
    upstream_uid    BIGINT,
    upstream_modseq BIGINT,

    -- System flags and custom keywords in one array. The Rust schema split them
    -- across two columns that nothing ever treated differently, which only
    -- doubled the number of update paths that could disagree.
    flags TEXT[] NOT NULL DEFAULT '{}',

    -- The resumability primitive. A watermark alone cannot express "metadata is
    -- known but the body is not", which is why an interrupted sync used to
    -- restart from the beginning.
    body_state    TEXT NOT NULL DEFAULT 'pending',
    body_attempts INTEGER NOT NULL DEFAULT 0,
    body_error    TEXT,
    claimed_at    TIMESTAMPTZ,

    expunged_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mailbox_messages_body_state_valid CHECK (
        body_state IN ('pending', 'fetching', 'stored', 'skipped_too_large', 'failed')
    ),
    CONSTRAINT mailbox_messages_local_uid_positive CHECK (local_uid >= 1)
);

CREATE UNIQUE INDEX mailbox_messages_mailbox_local_uid_key
    ON mailbox_messages (mailbox_id, local_uid);
CREATE UNIQUE INDEX mailbox_messages_mailbox_upstream_uid_key
    ON mailbox_messages (mailbox_id, upstream_uid)
    WHERE upstream_uid IS NOT NULL;

-- The body pass polls for work with this; a partial index keeps that query cheap
-- even when the mailbox holds hundreds of thousands of already-fetched messages.
CREATE INDEX mailbox_messages_pending_idx
    ON mailbox_messages (mailbox_id, upstream_uid DESC)
    WHERE body_state = 'pending';

-- Reaping messages abandoned by a crashed process.
CREATE INDEX mailbox_messages_claimed_idx ON mailbox_messages (claimed_at)
    WHERE body_state = 'fetching';

CREATE INDEX mailbox_messages_message_idx ON mailbox_messages (message_id);

-- MIME parts ------------------------------------------------------------------

CREATE TABLE mime_parts (
    id         BIGSERIAL PRIMARY KEY,
    message_id BIGINT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,

    part_path         TEXT NOT NULL,   -- RFC 3501 body part number, e.g. "1.2"
    content_type      TEXT NOT NULL,
    charset           TEXT,
    disposition       TEXT,
    filename          TEXT,
    content_id        TEXT,
    transfer_encoding TEXT,
    size_octets       BIGINT NOT NULL DEFAULT 0,
    blob_key          TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT mime_parts_size_nonnegative CHECK (size_octets >= 0)
);

-- Re-ingesting a message must replace its parts, not duplicate them.
CREATE UNIQUE INDEX mime_parts_message_path_key ON mime_parts (message_id, part_path);

-- Blob accounting -------------------------------------------------------------

-- A real enum, so the two-spellings-split-the-refcount bug in the Rust version
-- becomes impossible to represent rather than merely discouraged.
CREATE TYPE cache_object_type AS ENUM ('rfc822', 'mime_part');

CREATE TABLE cache_objects (
    id         BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES mail_accounts (id) ON DELETE CASCADE,

    object_type cache_object_type NOT NULL,
    blob_key    TEXT  NOT NULL,
    sha256      BYTEA NOT NULL,
    size_octets BIGINT NOT NULL DEFAULT 0,

    -- Deduplication means one blob backs many rows, so deletion is refcounted.
    -- The check makes a double-decrement fail loudly instead of silently
    -- leaving a negative count that later reads as "safe to delete".
    ref_count BIGINT NOT NULL DEFAULT 0,

    last_accessed_at TIMESTAMPTZ,
    -- Set when ref_count reaches zero; a sweeper removes the object later, so a
    -- transient drop to zero during re-ingest does not destroy live data.
    unreferenced_at  TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT cache_objects_ref_count_nonnegative CHECK (ref_count >= 0),
    CONSTRAINT cache_objects_sha256_length CHECK (octet_length(sha256) = 32)
);

CREATE UNIQUE INDEX cache_objects_account_key ON cache_objects (account_id, blob_key);
CREATE INDEX cache_objects_sweep_idx ON cache_objects (unreferenced_at)
    WHERE ref_count = 0;

-- Sync runs -------------------------------------------------------------------

-- One row per sync attempt. The Rust version had no equivalent, which is why a
-- wedged sync was invisible: nothing recorded that a run had started, what it
-- was doing, or whether it was still alive.
CREATE TABLE sync_runs (
    id         BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES mail_accounts (id) ON DELETE CASCADE,

    status  TEXT NOT NULL DEFAULT 'running',
    phase   TEXT NOT NULL DEFAULT 'starting',
    trigger TEXT NOT NULL DEFAULT 'scheduled',

    mailboxes_total  INTEGER NOT NULL DEFAULT 0,
    mailboxes_done   INTEGER NOT NULL DEFAULT 0,
    messages_new     BIGINT  NOT NULL DEFAULT 0,
    bodies_fetched   BIGINT  NOT NULL DEFAULT 0,
    bytes_fetched    BIGINT  NOT NULL DEFAULT 0,
    commands_issued  BIGINT  NOT NULL DEFAULT 0,

    error TEXT,

    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Refreshed by the run itself. A stale heartbeat on a 'running' row is how
    -- an orphaned run is detected and reported.
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ,

    CONSTRAINT sync_runs_status_valid
        CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    CONSTRAINT sync_runs_trigger_valid
        CHECK (trigger IN ('scheduled', 'manual', 'startup'))
);

CREATE INDEX sync_runs_account_started_idx ON sync_runs (account_id, started_at DESC);
CREATE INDEX sync_runs_running_idx ON sync_runs (heartbeat_at) WHERE status = 'running';

-- Outbound mutation queue -----------------------------------------------------

-- Client changes are applied locally first, queued here, then replayed upstream.
CREATE TABLE pending_mutations (
    id         BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES mail_accounts (id) ON DELETE CASCADE,
    mailbox_id BIGINT REFERENCES mailboxes (id) ON DELETE CASCADE,
    message_id BIGINT REFERENCES messages (id) ON DELETE SET NULL,

    mutation_type TEXT  NOT NULL,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,

    status          TEXT    NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ,
    last_error      TEXT,

    -- Replay must be safe across a crash: the same logical change enqueued
    -- twice collapses to one row.
    idempotency_key TEXT NOT NULL,

    -- Claimed by a worker via SELECT ... FOR UPDATE SKIP LOCKED, so multiple
    -- replicas can drain the queue without an external lock.
    locked_by TEXT,
    locked_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT pending_mutations_status_valid
        CHECK (status IN ('pending', 'in_flight', 'succeeded', 'failed')),
    CONSTRAINT pending_mutations_type_valid
        CHECK (mutation_type IN ('append', 'store_flags', 'copy', 'move', 'expunge'))
);

CREATE UNIQUE INDEX pending_mutations_idempotency_key ON pending_mutations (idempotency_key);
CREATE INDEX pending_mutations_due_idx
    ON pending_mutations (account_id, next_attempt_at NULLS FIRST)
    WHERE status IN ('pending', 'failed');

-- Sessions and audit ----------------------------------------------------------

CREATE TABLE sessions (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Only a hash is stored, so a database leak does not hand over live sessions.
    token_hash BYTEA  NOT NULL,
    kind       TEXT   NOT NULL DEFAULT 'web',

    user_agent TEXT,
    remote_addr TEXT,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,

    CONSTRAINT sessions_kind_valid CHECK (kind IN ('web', 'imap'))
);

CREATE UNIQUE INDEX sessions_token_hash_key ON sessions (token_hash);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

CREATE TABLE audit_log (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT REFERENCES users (id) ON DELETE SET NULL,
    account_id BIGINT REFERENCES mail_accounts (id) ON DELETE SET NULL,

    action   TEXT  NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    actor    TEXT,
    remote_addr TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_created_idx ON audit_log (created_at DESC);
CREATE INDEX audit_log_account_idx ON audit_log (account_id, created_at DESC);

-- Quotas ----------------------------------------------------------------------

CREATE TABLE quotas (
    id         BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES mail_accounts (id) ON DELETE CASCADE,
    max_bytes  BIGINT NOT NULL,
    used_bytes BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT quotas_used_nonnegative CHECK (used_bytes >= 0)
);

CREATE UNIQUE INDEX quotas_account_key ON quotas (account_id);

-- +goose Down
DROP TABLE IF EXISTS quotas;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS pending_mutations;
DROP TABLE IF EXISTS sync_runs;
DROP TABLE IF EXISTS cache_objects;
DROP TYPE IF EXISTS cache_object_type;
DROP TABLE IF EXISTS mime_parts;
DROP TABLE IF EXISTS mailbox_messages;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS mailboxes;
DROP TABLE IF EXISTS mail_accounts;
DROP TABLE IF EXISTS users;
