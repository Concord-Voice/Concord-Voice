DROP TABLE presence_settings_pending_operations;
DROP TABLE presence_history;

ALTER TABLE user_presence_settings
    DROP CONSTRAINT user_presence_settings_history_consent_check,
    DROP CONSTRAINT user_presence_settings_history_hash_check,
    DROP CONSTRAINT user_presence_settings_history_retention_check,
    DROP CONSTRAINT user_presence_settings_version_nonnegative;

ALTER TABLE user_presence_settings
    DROP COLUMN activity_history_reconsent_required,
    DROP COLUMN activity_history_consented_at,
    DROP COLUMN activity_history_consent_copy_hash,
    DROP COLUMN activity_history_consent_version,
    DROP COLUMN activity_history_retention_days,
    DROP COLUMN activity_history_enabled,
    DROP COLUMN presence_settings_operation_id,
    DROP COLUMN presence_settings_version;
