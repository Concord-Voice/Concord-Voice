-- 000164: record whether a purge included pinned messages (#3458).
--
-- include_pinned = TRUE means pinned messages in the purge's scope were
-- eligible for deletion. That is true of every row written before this
-- migration (the DM clear reap's included), and of every writer that omits
-- the column:
--   * historical purges (every purge deleted pins until #3458),
--   * the expiry writer (until #3459 makes expiry pin-aware),
--   * an older binary still running during a rolling deploy.
-- The user-requested purges (channel, server, DM, group, kick, ban) write the
-- request's value; FALSE means pinned messages were kept. The DM clear reap
-- writes the column too (#3458 §18.2): FALSE for a batch bounded by the Clear
-- watermark, which keeps every pin, TRUE for the zero-participant batch, which
-- deletes them.
--
-- A constant default is metadata-only on PostgreSQL 16 (attmissingval): a
-- brief ACCESS EXCLUSIVE lock and no table rewrite.
ALTER TABLE message_purges ADD COLUMN include_pinned BOOLEAN NOT NULL DEFAULT TRUE;

-- Rollback evidence (see the down). Restore what an earlier 000164 down kept;
-- on a first apply the table is created empty, nothing matches and nothing is
-- rewritten. The table stays: the trigger below records every purge that keeps
-- pins as it is written, so a later down has nothing to copy (#3552 review).
CREATE TABLE IF NOT EXISTS message_purges_kept_pins (purge_id UUID PRIMARY KEY);
UPDATE message_purges SET include_pinned = FALSE
WHERE id IN (SELECT purge_id FROM message_purges_kept_pins);

CREATE OR REPLACE FUNCTION record_message_purge_kept_pins()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO message_purges_kept_pins (purge_id) VALUES (NEW.id)
    ON CONFLICT DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- ponytail: INSERT only. Every writer sets include_pinned once, at insert; a
-- writer that later changed it would need UPDATE OF include_pinned here.
CREATE TRIGGER message_purges_record_kept_pins
AFTER INSERT ON message_purges
FOR EACH ROW WHEN (NOT NEW.include_pinned)
EXECUTE FUNCTION record_message_purge_kept_pins();
