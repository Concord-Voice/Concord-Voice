ALTER TABLE privacy_settings
    DROP COLUMN IF EXISTS share_personalization_with_gif_provider,
    DROP COLUMN IF EXISTS enable_klipy_proxy,
    DROP COLUMN IF EXISTS load_gifs_automatically;
