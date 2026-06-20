-- KLIPY-related privacy settings.
--
-- - load_gifs_automatically: when FALSE, GIFs in messages show a "Click to
--   load" placeholder until the user explicitly taps them. Default OFF
--   (privacy-first).
-- - enable_klipy_proxy: when TRUE, all KLIPY traffic (search, trending,
--   items, recent, categories, media) routes through the Concord control-plane
--   proxy so KLIPY never sees the user's IP address or search terms. Default
--   OFF (lets users opt-in to the slightly higher latency).
-- - share_personalization_with_gif_provider: when TRUE, Concord sends an
--   opaque per-user customer_id to KLIPY for personalized recent + search
--   results. Default ON because turning it OFF degrades search quality with
--   no privacy benefit when the proxy is also ON. Users can flip it OFF if
--   they prefer fully unpersonalized results.
ALTER TABLE privacy_settings
    ADD COLUMN IF NOT EXISTS load_gifs_automatically                 BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS enable_klipy_proxy                      BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS share_personalization_with_gif_provider BOOLEAN NOT NULL DEFAULT TRUE;
