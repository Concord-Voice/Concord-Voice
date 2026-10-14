-- 000168: pin-aware message expiry (#3459, epic #1351).
-- Design: [internal]specs/2026-10-09-3459-pin-aware-expiry-design.md §3, §4.
--
-- Adds a per-context "Include pinned messages" switch and the nullable stamp
-- that holds a kept pin's would-be expiry:
--
--   channels.expiration_include_pinned          BOOLEAN NOT NULL DEFAULT TRUE
--   dm_conversations.expiration_include_pinned  BOOLEAN NOT NULL DEFAULT TRUE
--   messages.held_expires_at                    TIMESTAMPTZ NULL
--   dm_messages.held_expires_at                 TIMESTAMPTZ NULL
--
-- DEFAULT TRUE means every timer that exists today keeps expiring pinned
-- messages, exactly as before this migration. FALSE ("keep pins") is stored
-- only by a writer that asks for it. A context is "K" (keeps pins) when its
-- window IS NOT NULL AND NOT expiration_include_pinned.
--
-- Keep is expressed ONLY by the absence of expires_at (design C-SWEEP). The
-- sweeper deletes rows whose expires_at has passed and its discovery query
-- gains no predicate, so a kept pin must have expires_at NULL. Its would-be
-- stamp parks in held_expires_at, which no reader of the expiry index looks at.
-- That is why held_expires_at carries NO index: nothing selects by it. Every
-- statement that does name it is pinned-only and also carries
-- `pinned_at IS NOT NULL`, so it rides idx_messages_pinned or
-- idx_dm_messages_pinned (design Q-SHAPE); the CHECK below makes that
-- predicate complete.
--
-- INVARIANT (I-HELD). held_expires_at IS NOT NULL implies pinned_at IS NOT NULL,
-- expires_at IS NULL, and the parent context is K. The CHECK enforces the first
-- two; the application state machine (internal/expiration) enforces the third.
--
-- THE CHECKS ARE NOT VALID, AND ARE NEVER VALIDATED. messages_held_expires_at_shape_check
-- and dm_messages_held_expires_at_shape_check are added NOT VALID and no later
-- migration runs VALIDATE CONSTRAINT on them. The column is new, so every row
-- that exists when the constraint is added has held_expires_at NULL and
-- satisfies it by construction. NOT VALID already enforces every new and
-- updated row, which is all a CHECK on a new column can ever be asked to do.
-- A VALIDATE would be a sequential scan of messages and dm_messages that proves
-- a tautology. Do not "finish" this by adding a validating migration. The one
-- way a row could violate the CHECK without validation catching it, a row that
-- already existed, cannot occur because the column did not.
--
-- C-DDL. This migration is metadata-only: nullable columns with no default, a
-- constant DEFAULT on the two parents (PostgreSQL 11+ stores it as
-- attmissingval and does not rewrite), NOT VALID CHECKs (no scan), and no
-- index on the held column. A test compares relfilenode before and after.
--
-- ROLLBACK EVIDENCE (C-DOWN). Dropping the columns alone would lose which
-- contexts keep pins and every held stamp; a later up would re-add the switch
-- as TRUE, relabelling a keep-pins context as "expire pins", and re-add the
-- held column empty, leaving every formerly kept pin with expires_at NULL
-- forever. So the down copies that state into two tables this migration
-- creates, and this up restores from them:
--
--   expiration_keep_pins_evidence    one row per K context, with the
--                                    expiration_revision it was captured at
--   expiration_held_stamps_evidence  one row per held message
--
-- Neither table has a foreign key (the 000164 pattern: a message or context may
-- be deleted while the evidence is outstanding, and the restore joins by
-- primary key and ignores ids that no longer exist; an FK from the evidence to
-- messages would also put a cascade trigger on the hottest table). Neither is
-- ever dropped by the down. Both are emptied by the down before it copies, and
-- by this up after it restores, so on a first apply they are created empty and
-- every restore step below matches nothing.
--
-- SUPERSEDES 000164. Migration 000164's header says the expiry writer records
-- include_pinned = TRUE "until #3459 makes expiry pin-aware". After #3459 an
-- expiry purge records TRUE if the context was not K at insert OR any deleted
-- row was pinned, and FALSE otherwise (design §9, Fork 2a). Statements in 000164
-- about the expiry writer recording TRUE are superseded by that rule. The applied
-- 000164 file is intentionally not edited.
--
-- ROLLBACK ORDER (design §12). (1) Stop the new control plane: a running new
-- binary fails every expiry batch against a dropped column, and on restart it
-- re-applies this up. (2) Run the 000168 down with cmd/migrate from the new
-- tag's tree. (3) Start the old image. An image-only rollback is impossible
-- (golang-migrate refuses to boot an image older than the recorded version).
-- Under the old binary a held pin sits at expires_at NULL and is kept, which
-- is the safe direction while rolled back.
--
-- LOCKING. The migration runs at singleton boot with the predecessor stopped.
-- It takes ACCESS EXCLUSIVE on all four tables, parents first, as its first
-- statement, so no writer can interleave between the ADD COLUMNs and the
-- restore. The lock is held for the whole file (one implicit transaction); the
-- statements under it touch only K contexts, found by primary key, and use the
-- partial pinned indexes, never a table scan.

LOCK TABLE channels, dm_conversations, messages, dm_messages IN ACCESS EXCLUSIVE MODE;

ALTER TABLE channels
  ADD COLUMN expiration_include_pinned BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE dm_conversations
  ADD COLUMN expiration_include_pinned BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE messages
  ADD COLUMN held_expires_at TIMESTAMPTZ NULL;

ALTER TABLE dm_messages
  ADD COLUMN held_expires_at TIMESTAMPTZ NULL;

ALTER TABLE messages
  ADD CONSTRAINT messages_held_expires_at_shape_check
  CHECK (held_expires_at IS NULL OR (pinned_at IS NOT NULL AND expires_at IS NULL)) NOT VALID;

ALTER TABLE dm_messages
  ADD CONSTRAINT dm_messages_held_expires_at_shape_check
  CHECK (held_expires_at IS NULL OR (pinned_at IS NOT NULL AND expires_at IS NULL)) NOT VALID;

COMMENT ON COLUMN channels.expiration_include_pinned IS 'TRUE: pinned messages expire with the channel window. FALSE: pins are kept. Read as effective value only while expiration_window_seconds IS NOT NULL.';
COMMENT ON COLUMN dm_conversations.expiration_include_pinned IS 'TRUE: pinned messages expire with the conversation window. FALSE: pins are kept. Read as effective value only while expiration_window_seconds IS NOT NULL.';
COMMENT ON COLUMN messages.held_expires_at IS 'Would-be expiry of a pinned message kept by a keep-pins policy. NULL otherwise. Implies pinned_at IS NOT NULL and expires_at IS NULL. Never indexed.';
COMMENT ON COLUMN dm_messages.held_expires_at IS 'Would-be expiry of a pinned DM message kept by a keep-pins policy. NULL otherwise. Implies pinned_at IS NOT NULL and expires_at IS NULL. Never indexed.';

-- Rollback evidence. IF NOT EXISTS: the tables outlive the down, so a re-apply
-- finds them (see the header). No foreign keys, on purpose.
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

-- Restore (a): a context that kept pins before the rollback keeps them again,
-- but only if its policy has not moved since. If expiration_revision differs,
-- a user acted under the old binary's semantics (which had no switch) and the
-- context is NOT relabelled. A context whose window is now NULL is not K.
-- No-op on first apply: the evidence table is empty.
UPDATE channels c
SET expiration_include_pinned = FALSE
FROM expiration_keep_pins_evidence e
WHERE e.context_kind = 'channel'
  AND e.context_id = c.id
  AND e.expiration_revision = c.expiration_revision
  AND c.expiration_window_seconds IS NOT NULL;

UPDATE dm_conversations c
SET expiration_include_pinned = FALSE
FROM expiration_keep_pins_evidence e
WHERE e.context_kind = 'conversation'
  AND e.context_id = c.id
  AND e.expiration_revision = c.expiration_revision
  AND c.expiration_window_seconds IS NOT NULL;

-- Restore (b): re-place each held stamp the down saved, ONLY where the
-- context's policy has not moved since the down (its expiration_revision still
-- equals the one recorded in expiration_keep_pins_evidence). Only a message that
-- still has expires_at NULL is touched (a stamp written under the old binary
-- wins). Outcomes, decided against the parent AFTER step (a):
--   * policy moved while rolled back (any set or clear advances the revision):
--     nothing. The old binary's action stands. A clear cancelled the schedule
--     and a later new_only set left existing rows unstamped, so re-arming the
--     saved stamp would resurrect a schedule the user cancelled and, if it is
--     already past, delete the message at the next sweep (Codex review of
--     #3459). The message keeps expires_at NULL, the safe direction.
--   * policy unchanged and message still pinned: the context was relabelled K
--     by step (a), so the stamp goes back into held_expires_at.
--   * policy unchanged and message unpinned while rolled back: the stamp goes
--     into expires_at. This re-arms a deferral whose condition (pinned AND
--     kept) ended while rolled back. A stamp already in the past is then
--     deleted by the boot preflight. That is correct: the deferral was
--     conditional, and the retention promise governs once it lapses.
-- Every statement is driven from the evidence by primary key. No-op on first apply.
UPDATE messages m
SET held_expires_at = e.held_expires_at
FROM expiration_held_stamps_evidence e, channels c
WHERE e.message_kind = 'channel'
  AND m.id = e.message_id
  AND c.id = m.channel_id
  AND m.pinned_at IS NOT NULL
  AND m.expires_at IS NULL
  AND c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned;

UPDATE messages m
SET expires_at = e.held_expires_at
FROM expiration_held_stamps_evidence e, channels c
WHERE e.message_kind = 'channel'
  AND m.id = e.message_id
  AND c.id = m.channel_id
  AND m.expires_at IS NULL
  AND m.held_expires_at IS NULL
  AND m.pinned_at IS NULL
  AND c.expiration_window_seconds IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM expiration_keep_pins_evidence k
    WHERE k.context_kind = 'channel'
      AND k.context_id = c.id
      AND k.expiration_revision = c.expiration_revision
  );

UPDATE dm_messages m
SET held_expires_at = e.held_expires_at
FROM expiration_held_stamps_evidence e, dm_conversations c
WHERE e.message_kind = 'dm'
  AND m.id = e.message_id
  AND c.id = m.conversation_id
  AND m.pinned_at IS NOT NULL
  AND m.expires_at IS NULL
  AND c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned;

UPDATE dm_messages m
SET expires_at = e.held_expires_at
FROM expiration_held_stamps_evidence e, dm_conversations c
WHERE e.message_kind = 'dm'
  AND m.id = e.message_id
  AND c.id = m.conversation_id
  AND m.expires_at IS NULL
  AND m.held_expires_at IS NULL
  AND m.pinned_at IS NULL
  AND c.expiration_window_seconds IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM expiration_keep_pins_evidence k
    WHERE k.context_kind = 'conversation'
      AND k.context_id = c.id
      AND k.expiration_revision = c.expiration_revision
  );

-- Restore (c): normalise pins made while rolled back. The old binary stamped a
-- pin like any other message, so a pinned row in a K context can carry a future
-- expires_at. Move it into held_expires_at (I-HELD). Only a stamp that has not
-- passed moves: a stamp already due is never moved or cleared (C-NORES), so the
-- next sweep deletes it. statement_timestamp() is read once per statement.
-- Pinned-only, hence `pinned_at IS NOT NULL` (Q-SHAPE). Driven from the K
-- parents, which are few. No-op on first apply: no context is K yet, unless
-- step (a) just restored one, in which case this is the intended repair.
UPDATE messages m
SET held_expires_at = m.expires_at,
    expires_at = NULL
FROM channels c
WHERE c.id = m.channel_id
  AND c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned
  AND m.pinned_at IS NOT NULL
  AND m.expires_at > statement_timestamp();

UPDATE dm_messages m
SET held_expires_at = m.expires_at,
    expires_at = NULL
FROM dm_conversations c
WHERE c.id = m.conversation_id
  AND c.expiration_window_seconds IS NOT NULL
  AND NOT c.expiration_include_pinned
  AND m.pinned_at IS NOT NULL
  AND m.expires_at > statement_timestamp();

-- Restore (d): the evidence is consumed. Delete every row, including rows whose
-- context was not relabelled (revision moved) or whose message is gone: a
-- stale row left behind could be applied by a later up against state it never
-- described. The tables themselves stay.
DELETE FROM expiration_held_stamps_evidence;
DELETE FROM expiration_keep_pins_evidence;
