-- #89: Add recovery-only and hardened mode columns (split from 029 edit)

-- Methods restricted to account recovery only (can't be used for login or sensitive ops)
ALTER TABLE users ADD COLUMN IF NOT EXISTS recovery_only_methods TEXT[] NOT NULL DEFAULT '{}';
-- When true, Email+SMS recovery requires BOTH codes (dual-channel verification)
-- Default TRUE: hardened mode is on by default (safer default — user can opt out)
ALTER TABLE users ADD COLUMN IF NOT EXISTS recovery_hardened BOOLEAN NOT NULL DEFAULT TRUE;
