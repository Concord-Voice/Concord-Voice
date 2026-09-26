-- Reversal of 000158. Unguarded by design: the column's value is meaningful
-- only for the ~90 s a code stays inside its skew window, so dropping it loses
-- at most one replay of each code accepted in the last 90 s, never durable
-- state. A binary that still writes the column must be rolled back first.
ALTER TABLE user_mfa_totp
    DROP COLUMN IF EXISTS last_used_step;
