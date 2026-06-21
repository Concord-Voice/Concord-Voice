-- #89: Add backup email column for recovery when primary email is compromised
ALTER TABLE users ADD COLUMN IF NOT EXISTS backup_email VARCHAR(255);
