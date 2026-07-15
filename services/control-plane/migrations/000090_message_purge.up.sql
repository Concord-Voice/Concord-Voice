-- Message-purge engine (#1352): privacy-safe audit + step-up toggle + receiver-hide ranges.

CREATE TABLE message_purges (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id       UUID REFERENCES users(id) ON DELETE SET NULL,
    context_type   TEXT NOT NULL CHECK (context_type IN ('channel','server','dm','group')),
    context_id     UUID NOT NULL,
    server_id      UUID,
    target_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    range_from     TIMESTAMPTZ,
    range_to       TIMESTAMPTZ,
    deleted_count  INTEGER NOT NULL DEFAULT 0 CHECK (deleted_count >= 0),
    hidden_count   INTEGER NOT NULL DEFAULT 0 CHECK (hidden_count  >= 0),
    reason         TEXT NOT NULL DEFAULT 'manual' CHECK (reason IN ('manual','ban','kick')),
    status         TEXT NOT NULL DEFAULT 'in_progress' CHECK (status IN ('in_progress','completed')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at   TIMESTAMPTZ
);
CREATE INDEX idx_message_purges_context ON message_purges (context_type, context_id);
CREATE INDEX idx_message_purges_created_at ON message_purges (created_at DESC);

ALTER TABLE privacy_settings
    ADD COLUMN IF NOT EXISTS require_auth_before_purge BOOLEAN NOT NULL DEFAULT TRUE;

CREATE TABLE dm_message_hidden_ranges (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    hidden_from     TIMESTAMPTZ NOT NULL,
    hidden_to       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (hidden_from <= hidden_to)
);
CREATE INDEX idx_dm_hidden_ranges_user_conv ON dm_message_hidden_ranges (user_id, conversation_id);

-- Blob-reap marker for the purge reaper's straggler sweep.
--
-- deleted_at alone cannot drive that sweep: it records that the METADATA row was
-- soft-deleted, not that the object was removed from storage. Without a distinct
-- marker the sweep's candidate set never shrinks, so it re-selects the same oldest
-- rows every tick (starvation) while any row aged past a look-back bound is skipped
-- forever (permanent blob leak — a GDPR Art.17 defect on a delete-on-request path).
-- blob_reaped_at is set ONLY after a successful object delete, so the sweep drains
-- monotonically and still retries genuine storage failures.
ALTER TABLE media_files ADD COLUMN blob_reaped_at TIMESTAMPTZ;

-- NO BACKFILL — deliberately. An earlier revision stamped every pre-existing
-- soft-deleted row as reaped ("already reaped by construction, so this just stops
-- the sweep stampeding history"). That premise is FALSE on both legacy writers:
--
--   * media.CleanupObject (media/cleanup.go) warns and soft-deletes ANYWAY when
--     DeleteObject fails, and skips the delete entirely when store == nil.
--   * media.DeleteMedia (media/handlers.go) soft-deletes FIRST, then logs
--     "Failed to delete object from storage (orphaned)" and still returns 200.
--
-- Both legitimately produce `deleted_at IS NOT NULL` rows whose blob is still live.
-- Backfilling them as reaped would permanently exclude exactly those orphans from
-- the sweep — recording a reap that never happened, which is the precise failure
-- reaper.go's success-gated marker exists to prevent, applied to all of history.
-- Leaving them unmarked lets the sweep reap the real orphans (strided, idempotent).

-- Partial index matching the sweep predicate exactly. It stays small by design:
-- rows leave the index the moment they are reaped, so it only ever covers the
-- outstanding backlog, not the soft-delete history.
--
-- NOTE: golang-migrate runs migrations inside a transaction, so CREATE INDEX
-- CONCURRENTLY cannot be used here (it is not allowed inside a transaction block).
-- For large production tables this index should be created manually with
-- CONCURRENTLY before the migration runs, or the migration should be run during a
-- low-traffic window. Same constraint and same operator workaround as the pinning
-- indexes in 000050 / 000057, which take this lock on the much larger messages /
-- dm_messages tables.
CREATE INDEX idx_media_files_unreaped ON media_files (deleted_at)
    WHERE deleted_at IS NOT NULL AND blob_reaped_at IS NULL AND media_tier = 2;
