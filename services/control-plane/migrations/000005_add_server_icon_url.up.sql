-- Add icon_url column to servers table for server icons (base64 data URLs)
ALTER TABLE servers ADD COLUMN icon_url TEXT;
