-- OpenConvo initial schema.
--
-- Design notes:
--   * Canonical archive tables (communities, channels, actors, messages,
--     blobs, attachments, message_reactions) never use platform-specific
--     column names. Identity on the source platform is always
--     (source, external_id) or (parent, external_id), so future sources
--     (Slack, Discourse, Matrix, ...) fit without redesign.
--   * raw_payload keeps the original source payload so information that is
--     not yet normalized is never lost. It is scrubbed on deletion.
--   * The search vector uses the "simple" text search configuration:
--     communities are frequently not English-speaking, and "simple" is
--     predictable across languages. Language-aware configurations can be
--     added later as a derived, rebuildable index.
--   * All timestamps are timestamptz. updated_at is maintained by the
--     application, not triggers, to keep behavior explicit.
--   * The `derived` schema at the end holds data rebuilt from the canonical
--     tables above. Nothing canonical depends on it, and it may be dropped
--     wholesale and regenerated.


-- ===========================================================================
-- Canonical archive
-- ===========================================================================

-- Communities: a Discord guild today; another community container later.
CREATE TABLE communities (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source        text NOT NULL,
    external_id   text NOT NULL,
    name          text NOT NULL DEFAULT '',
    description   text NOT NULL DEFAULT '',
    icon_url      text NOT NULL DEFAULT '',
    raw_payload   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, external_id)
);

-- Channels: text channels, forum channels, and threads.
-- Threads are channels with a parent_channel_id.
CREATE TABLE channels (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    community_id       uuid NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    external_id        text NOT NULL,
    parent_channel_id  uuid REFERENCES channels(id) ON DELETE CASCADE,
    kind               text NOT NULL DEFAULT 'text',
    name               text NOT NULL DEFAULT '',
    topic              text NOT NULL DEFAULT '',
    position           integer NOT NULL DEFAULT 0,
    -- is_private: not visible to @everyone on the source platform.
    is_private         boolean NOT NULL DEFAULT false,
    -- is_archived: archived/closed state on the source platform (threads).
    is_archived        boolean NOT NULL DEFAULT false,
    -- archive_enabled: whether OpenConvo archives this channel.
    -- Nothing is archived until explicitly selected.
    archive_enabled    boolean NOT NULL DEFAULT false,
    source_created_at  timestamptz,
    raw_payload        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (community_id, external_id)
);

CREATE INDEX channels_parent_idx ON channels (parent_channel_id);

-- Actors: message authors. Deliberately minimal — no emails, no IPs,
-- no profile data OpenConvo does not need.
CREATE TABLE actors (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source        text NOT NULL,
    external_id   text NOT NULL,
    username      text NOT NULL DEFAULT '',
    display_name  text NOT NULL DEFAULT '',
    avatar_url    text NOT NULL DEFAULT '',
    is_bot        boolean NOT NULL DEFAULT false,
    raw_payload   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, external_id)
);

-- Messages: the core table.
CREATE TABLE messages (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id            uuid NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    actor_id              uuid REFERENCES actors(id) ON DELETE SET NULL,
    external_id           text NOT NULL,
    kind                  text NOT NULL DEFAULT 'default',
    -- content is NULL for deleted (tombstoned) messages.
    content               text,
    reply_to_message_id   uuid REFERENCES messages(id) ON DELETE SET NULL,
    -- reply_to_external_id is kept even when the referenced message has
    -- not been imported yet, so reply links can be resolved later.
    reply_to_external_id  text,
    source_created_at     timestamptz NOT NULL,
    source_updated_at     timestamptz,
    deleted_at            timestamptz,
    raw_payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
    search_vector         tsvector GENERATED ALWAYS AS (to_tsvector('simple', coalesce(content, ''))) STORED,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (channel_id, external_id)
);

CREATE INDEX messages_timeline_idx ON messages (channel_id, source_created_at);
CREATE INDEX messages_actor_idx ON messages (actor_id);
CREATE INDEX messages_reply_idx ON messages (reply_to_message_id);
CREATE INDEX messages_search_idx ON messages USING gin (search_vector);

-- Blobs: physical stored files, content-addressed by SHA-256.
CREATE TABLE blobs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sha256        text NOT NULL UNIQUE,
    size          bigint NOT NULL,
    content_type  text NOT NULL DEFAULT '',
    object_key    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Attachments: the relationship between a message and a stored blob.
-- blob_id is NULL until the download job has stored the file, so the
-- attachment metadata survives even if the download is still pending
-- or has failed. One blob may back many attachments (deduplication).
CREATE TABLE attachments (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id       uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    blob_id          uuid REFERENCES blobs(id) ON DELETE RESTRICT,
    external_id      text NOT NULL DEFAULT '',
    filename         text NOT NULL DEFAULT '',
    description      text NOT NULL DEFAULT '',
    content_type     text NOT NULL DEFAULT '',
    size             bigint NOT NULL DEFAULT 0,
    source_url       text NOT NULL DEFAULT '',
    -- pending | stored | failed
    download_status  text NOT NULL DEFAULT 'pending',
    -- Why a download failed, for the operator's benefit; NULL while an
    -- attachment is pending or stored. An oversize or gone-at-source file
    -- is "failed" carrying a reason rather than a status of its own, so no
    -- later query has to learn a wider status set.
    download_error   text,
    raw_payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, external_id)
);

CREATE INDEX attachments_blob_idx ON attachments (blob_id);
CREATE INDEX attachments_status_idx ON attachments (download_status) WHERE download_status <> 'stored';

-- Reaction aggregates. Individual reactor identity is deliberately not
-- recorded; counts preserve the conversational signal with less data.
CREATE TABLE message_reactions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id   uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    -- emoji_key uniquely identifies the emoji within the source platform:
    -- the literal character for unicode emoji, "custom:<id>:<name>" for
    -- custom emoji.
    emoji_key    text NOT NULL,
    emoji_name   text NOT NULL DEFAULT '',
    count        integer NOT NULL DEFAULT 0,
    raw_payload  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, emoji_key)
);


-- ===========================================================================
-- Curation
-- ===========================================================================

-- Bookmarks: manual curation of important messages.
--
-- A collection is deliberately bookmark metadata rather than a separate
-- hierarchy: it keeps exports boring, permits an operator to rename a group
-- by editing its members, and creates no empty/load-bearing derived records.
CREATE TABLE bookmarks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id   uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    title        text NOT NULL DEFAULT '',
    description  text NOT NULL DEFAULT '',
    collection   text NOT NULL DEFAULT '',
    tags         text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX bookmarks_message_idx ON bookmarks (message_id);
CREATE INDEX bookmarks_collection_idx ON bookmarks (collection, created_at DESC);
CREATE INDEX bookmarks_tags_idx ON bookmarks USING gin (tags);


-- ===========================================================================
-- Operations
-- ===========================================================================

-- Per-channel synchronization state and checkpoints.
CREATE TABLE sync_states (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id          uuid NOT NULL UNIQUE REFERENCES channels(id) ON DELETE CASCADE,
    -- pending | importing | synced | error
    status              text NOT NULL DEFAULT 'pending',
    oldest_external_id  text,
    newest_external_id  text,
    backfill_complete   boolean NOT NULL DEFAULT false,
    started_at          timestamptz,
    completed_at        timestamptz,
    last_synced_at      timestamptz,
    last_error          text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Deletion ledger: records deletions so that restoring an old database
-- backup can never silently resurrect content that was deleted on the
-- source platform or by an operator. Replayed after restores.
CREATE TABLE deletion_ledger (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source        text NOT NULL,
    -- message | actor | channel | community | attachment
    object_type   text NOT NULL,
    external_id   text NOT NULL,
    deleted_at    timestamptz NOT NULL DEFAULT now(),
    processed_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX deletion_ledger_object_idx ON deletion_ledger (source, object_type, external_id);

-- Background jobs: PostgreSQL-backed queue, no Redis required.
-- Workers claim jobs with FOR UPDATE SKIP LOCKED.
CREATE TABLE jobs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind          text NOT NULL,
    payload       jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- pending | running | succeeded | failed
    status        text NOT NULL DEFAULT 'pending',
    attempts      integer NOT NULL DEFAULT 0,
    max_attempts  integer NOT NULL DEFAULT 10,
    -- dedupe_key prevents enqueueing duplicate live work
    -- (e.g. two backfills of the same channel).
    dedupe_key    text,
    available_at  timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    completed_at  timestamptz,
    last_error    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX jobs_claim_idx ON jobs (status, available_at);
CREATE UNIQUE INDEX jobs_dedupe_idx ON jobs (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND status IN ('pending', 'running');

-- Settings: small key-value store for operator-editable subsystem
-- configuration that belongs in the database rather than the environment,
-- because the admin UI writes it. Two keys exist: 'database_backup' and
-- 'embeddings'. Credentials never live here — the Discord token, S3 keys
-- and the admin password come from the environment, and the admin
-- password is only ever hashed in memory, never stored.
CREATE TABLE settings (
    key         text PRIMARY KEY,
    value       jsonb NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Logical PostgreSQL backup history. Destination details are snapshotted on
-- each run so a later settings change does not make the record ambiguous.
-- Credentials remain environment configuration and are never stored here.
CREATE TABLE database_backups (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    trigger           text NOT NULL CHECK (trigger IN ('manual', 'scheduled')),
    status            text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'expired')),
    provider          text NOT NULL,
    endpoint          text NOT NULL DEFAULT '',
    region            text NOT NULL,
    bucket            text NOT NULL,
    prefix            text NOT NULL,
    force_path_style  boolean NOT NULL DEFAULT false,
    retention_count   integer NOT NULL CHECK (retention_count BETWEEN 1 AND 1000),
    object_key        text NOT NULL UNIQUE,
    size              bigint NOT NULL DEFAULT 0,
    sha256            text NOT NULL DEFAULT '',
    error             text NOT NULL DEFAULT '',
    started_at        timestamptz,
    completed_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX database_backups_recent_idx ON database_backups (created_at DESC);

-- One dump at a time. This is also the race-safe dedupe boundary between the
-- scheduler and a simultaneous "Back up now" click.
CREATE UNIQUE INDEX database_backups_one_active_idx ON database_backups ((true))
    WHERE status IN ('pending', 'running');


-- ===========================================================================
-- Derived
-- ===========================================================================

-- Optional semantic-search embeddings. These rows are derived entirely from
-- canonical messages and may be discarded and rebuilt at any time — but
-- discard the whole schema, with DROP SCHEMA derived CASCADE. Dropping
-- derived.message_embeddings on its own leaves the invalidation trigger
-- below attached to messages, and every later UPDATE of a message's
-- content or deleted_at — every edit and every deletion — then fails.
--
-- pgvector keeps the default deployment at two containers while providing a
-- purpose-built vector type and index inside PostgreSQL.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE SCHEMA derived;

CREATE TABLE derived.embedding_generations (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider       text NOT NULL,
    model          text NOT NULL,
    dimensions     integer NOT NULL CHECK (dimensions > 0),
    input_version  text NOT NULL,
    status         text NOT NULL DEFAULT 'building'
                   CHECK (status IN ('building', 'active', 'retired')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    activated_at   timestamptz,
    UNIQUE (provider, model, dimensions, input_version)
);

CREATE TABLE derived.message_embeddings (
    generation_id  uuid NOT NULL REFERENCES derived.embedding_generations(id) ON DELETE CASCADE,
    message_id     uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    content_hash   text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    -- Fixed at 256 even though embedding_generations.dimensions is per-row:
    -- a generation at any other width inserts cleanly and then fails every
    -- embedding write. Widening means a new migration for this column and
    -- the index below, not just a new generations row.
    embedding      vector(256) NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (generation_id, message_id)
);

CREATE INDEX message_embeddings_message_idx
    ON derived.message_embeddings (message_id);
CREATE INDEX message_embeddings_cosine_idx
    ON derived.message_embeddings USING hnsw (embedding vector_cosine_ops);

-- Canonical writes must never wait for an embedding provider, but stale or
-- deleted derived data must disappear transactionally. The trigger belongs to
-- the derived subsystem and never changes canonical values.
CREATE FUNCTION derived.invalidate_message_embeddings() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM derived.message_embeddings WHERE message_id = NEW.id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER invalidate_message_embeddings
AFTER UPDATE OF content, deleted_at ON messages
FOR EACH ROW
WHEN (OLD.content IS DISTINCT FROM NEW.content OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at)
EXECUTE FUNCTION derived.invalidate_message_embeddings();
