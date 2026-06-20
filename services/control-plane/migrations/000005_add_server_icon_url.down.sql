-- Remove icon_url column from servers table
ALTER TABLE servers DROP COLUMN IF EXISTS icon_url;
