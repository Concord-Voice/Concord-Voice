-- Remove header image URL field from users table
ALTER TABLE users DROP COLUMN IF EXISTS header_image_url;
