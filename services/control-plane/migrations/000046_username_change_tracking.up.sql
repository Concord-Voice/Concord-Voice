-- Add username_changed_at to users table for 365-day change cooldown.
-- Default to created_at so existing users are not retroactively penalized.
ALTER TABLE users ADD COLUMN username_changed_at TIMESTAMPTZ;

UPDATE users SET username_changed_at = created_at WHERE username_changed_at IS NULL;

ALTER TABLE users ALTER COLUMN username_changed_at SET NOT NULL;
ALTER TABLE users ALTER COLUMN username_changed_at SET DEFAULT NOW();

-- Audit/abuse tracking table for username changes
CREATE TABLE username_history (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    old_username VARCHAR(50) NOT NULL,
    new_username VARCHAR(50) NOT NULL,
    changed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_username_history_user_id ON username_history(user_id);
CREATE INDEX idx_username_history_old_username ON username_history(old_username);
CREATE INDEX idx_username_history_new_username ON username_history(new_username);
