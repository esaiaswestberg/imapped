-- +goose Up

-- Full-text search over stored messages.
--
-- The tsvector is a generated column rather than a trigger-maintained one:
-- Postgres keeps it in step with the source columns automatically, so there is
-- no way for an UPDATE path to forget to refresh it.
--
-- Two details matter here:
--
--  * to_tsvector raises an error when its input produces more than about 1MB of
--    lexemes. A single large message would therefore fail to insert at all, so
--    the body is truncated for indexing while the full text stays in body_text.
--
--  * Weights let a subject match outrank a body match. A is subject, B is the
--    correspondents, C is the body.
--
-- The text search configuration is pinned to 'english' rather than taken from a
-- setting, because a generated column must be IMMUTABLE and reading the session
-- default would not be. Changing the language means a migration, which is the
-- honest cost of reindexing anyway.

ALTER TABLE messages
    ADD COLUMN search_tsv tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(subject, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(addrs::text, '')), 'B') ||
        setweight(to_tsvector('english', left(coalesce(body_text, ''), 900000)), 'C')
    ) STORED;

CREATE INDEX messages_search_idx ON messages USING GIN (search_tsv);

-- Substring matching on subjects, for the "find that mail about invoices" case
-- that stemmed full-text search handles poorly.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX messages_subject_trgm_idx ON messages USING GIN (subject gin_trgm_ops);

-- +goose Down
DROP INDEX IF EXISTS messages_subject_trgm_idx;
DROP INDEX IF EXISTS messages_search_idx;
ALTER TABLE messages DROP COLUMN IF EXISTS search_tsv;
