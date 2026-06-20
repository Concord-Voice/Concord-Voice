-- Add banner_url column to servers table
ALTER TABLE servers ADD COLUMN IF NOT EXISTS banner_url TEXT;
