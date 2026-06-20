DROP TABLE IF EXISTS username_history;
ALTER TABLE users DROP COLUMN IF EXISTS username_changed_at;
