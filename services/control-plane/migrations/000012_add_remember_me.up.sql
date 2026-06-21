-- Add remember_me column to track session persistence preference
ALTER TABLE refresh_tokens ADD COLUMN remember_me BOOLEAN NOT NULL DEFAULT TRUE;
