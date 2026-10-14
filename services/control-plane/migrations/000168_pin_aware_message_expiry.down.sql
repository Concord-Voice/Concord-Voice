-- 000168 down: drop pin-aware message expiry (#3459), keeping its evidence.
--
-- Dropping the columns alone would lose which contexts keep pins and every held
-- stamp. A later up would re-add expiration_include_pinned as TRUE, relabelling
-- a keep-pins context as one that expires pins, and re-add held_expires_at
-- empty, leaving every formerly kept pin with expires_at NULL forever. So this
-- down copies that state into expiration_keep_pins_evidence and
-- expiration_held_stamps_evidence (created by the up) and the next up restores
-- from them (C-DOWN). The evidence tables are never dropped, and have no
-- foreign key (the 000164 pattern).
--
-- THE DOWN NEVER RE-ARMS A STAMP. A held pin becomes expires_at NULL when the
-- column goes, which under the old binary means "kept". That is the safe
-- direction while rolled back: nothing is deleted that the user asked to keep.
-- The up re-arms a stamp only when its deferral condition ended during the
-- rollback.
--
-- ROLLBACK ORDER. Stop the new control plane first (a running new binary fails
-- every expiry batch against a dropped column, and on restart re-applies the
-- up), run this down with cmd/migrate from the new tag's tree, then start the
-- old image.
--
-- Why this down is not a guard: the 000098, 000130 and 000155 guards protect
-- evidence of an irreversible DELETION. These columns record an attribute of
-- RETENTION, and the evidence tables preserve it. A guard would remove the
-- rollback lever (same reasoning as 000164).
--
-- Held stamps are collected from K contexts only, through the partial pinned
-- indexes (design Q-SHAPE). I-HELD says a held row exists only in a K context,
-- and a sequential scan of messages under ACCESS EXCLUSIVE is never acceptable.
--
-- The schema is restored exactly (pg_constraint and pg_attribute for the
-- surviving columns). Dropped columns leave attisdropped slots in pg_attribute,
-- as for any DROP COLUMN; they do not affect behaviour.

LOCK TABLE channels, dm_conversations, messages, dm_messages IN ACCESS EXCLUSIVE MODE;

-- Defensive: the up creates both tables, but a hand-repaired database that lost
-- them must not fail the rollback half-way.
CREATE TABLE IF NOT EXISTS expiration_keep_pins_evidence (
    context_kind        TEXT   NOT NULL CHECK (context_kind IN ('channel', 'conversation')),
    context_id          UUID   NOT NULL,
    expiration_revision BIGINT NOT NULL,
    PRIMARY KEY (context_kind, context_id)
);

CREATE TABLE IF NOT EXISTS expiration_held_stamps_evidence (
    message_kind    TEXT        NOT NULL CHECK (message_kind IN ('channel', 'dm')),
    message_id      UUID        NOT NULL,
    held_expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (message_kind, message_id)
);

-- The evidence describes the state at THIS down. Anything older is stale.
DELETE FROM expiration_held_stamps_evidence;
DELETE FROM expiration_keep_pins_evidence;

-- K contexts, with the revision they were captured at. The up relabels a
-- context only if its revision is unchanged.
INSERT INTO expiration_keep_pins_evidence (context_kind, context_id, expiration_revision)
SELECT 'channel', c.id, c.expiration_revision
FROM channels c
WHERE c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned;

INSERT INTO expiration_keep_pins_evidence (context_kind, context_id, expiration_revision)
SELECT 'conversation', c.id, c.expiration_revision
FROM dm_conversations c
WHERE c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned;

-- Held stamps, driven from the K parents through idx_messages_pinned and
-- idx_dm_messages_pinned.
INSERT INTO expiration_held_stamps_evidence (message_kind, message_id, held_expires_at)
SELECT 'channel', m.id, m.held_expires_at
FROM channels c
JOIN messages m ON m.channel_id = c.id
WHERE c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned
  AND m.pinned_at IS NOT NULL
  AND m.held_expires_at IS NOT NULL;

INSERT INTO expiration_held_stamps_evidence (message_kind, message_id, held_expires_at)
SELECT 'dm', m.id, m.held_expires_at
FROM dm_conversations c
JOIN dm_messages m ON m.conversation_id = c.id
WHERE c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned
  AND m.pinned_at IS NOT NULL
  AND m.held_expires_at IS NOT NULL;

ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_held_expires_at_shape_check;
ALTER TABLE dm_messages DROP CONSTRAINT IF EXISTS dm_messages_held_expires_at_shape_check;

ALTER TABLE messages DROP COLUMN IF EXISTS held_expires_at;
ALTER TABLE dm_messages DROP COLUMN IF EXISTS held_expires_at;
ALTER TABLE channels DROP COLUMN IF EXISTS expiration_include_pinned;
ALTER TABLE dm_conversations DROP COLUMN IF EXISTS expiration_include_pinned;
