-- 000164 down: drop message_purges.include_pinned, keeping its evidence.
--
-- Dropping the column alone would lose which purges kept pinned messages, and
-- a later up would re-add it with DEFAULT TRUE, recording every such purge as
-- one that made pins eligible for deletion: a false audit statement, not a
-- gap. So the up keeps that evidence in message_purges_kept_pins as each purge
-- is written, and the next up restores FALSE from it. Purges written by an
-- older binary in between delete pins, so TRUE is correct for them.
--
-- Why this down is not a guard: the 000098, 000130 and 000155 guards protect
-- evidence of an irreversible DELETION. This column records an attribute of
-- RETENTION, and a guard would refuse after the first default purge and remove
-- the image-rollback lever for good. The evidence table keeps both.
--
-- The down copies nothing (#3552 review). The trigger writes each purge's
-- evidence in that purge's own transaction, so the lock below waits for
-- in-flight purges, whose evidence commits with them, and holds later ones
-- until the column is gone. The lock lasts milliseconds whatever the size of
-- the append-only audit table; copying it here would hold every reader and
-- writer for a full scan. The evidence table outlives the down and has no
-- foreign key: an earlier down (000090) drops message_purges without CASCADE,
-- and the up ignores ids that no longer exist.
LOCK TABLE message_purges IN ACCESS EXCLUSIVE MODE;
DROP TRIGGER message_purges_record_kept_pins ON message_purges;
DROP FUNCTION record_message_purge_kept_pins();
ALTER TABLE message_purges DROP COLUMN include_pinned;
