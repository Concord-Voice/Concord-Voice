-- Remove profile fields from users table
ALTER TABLE users DROP COLUMN IF EXISTS links;
ALTER TABLE users DROP COLUMN IF EXISTS avatar_url;
ALTER TABLE users DROP COLUMN IF EXISTS bio;
ALTER TABLE users DROP COLUMN IF EXISTS display_name;
