-- #1766: default "Load GIFs from KLIPY automatically" to ON for new users.
-- A column DEFAULT affects only newly-inserted rows; existing privacy_settings
-- rows keep their stored value (NO backfill — the schema cannot distinguish an
-- implicit FALSE from an explicit opt-out, so a backfill would silently reverse
-- a user's privacy choice). See [internal]specs/2026-06-21-1766-gif-autoload-default-design.md
-- (Option B rejection). The KLIPY media proxy is always-on, so default-ON does
-- not expose a user's IP to KLIPY's CDN.
ALTER TABLE privacy_settings
    ALTER COLUMN load_gifs_automatically SET DEFAULT TRUE;
