-- Reverse #1766: restore the original privacy-first OFF default for new rows.
-- (Existing rows are unaffected, symmetric with the up migration.)
ALTER TABLE privacy_settings
    ALTER COLUMN load_gifs_automatically SET DEFAULT FALSE;
