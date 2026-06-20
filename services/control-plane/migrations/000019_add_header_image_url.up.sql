-- Add header image URL field to users table (banner/header image, stored as base64 data URL)
ALTER TABLE users ADD COLUMN header_image_url TEXT;
