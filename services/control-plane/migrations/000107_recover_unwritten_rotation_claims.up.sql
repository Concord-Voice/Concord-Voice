-- Databases that ran the pre-release 000106 migration lack this default.
-- New installations receive it in 000106; existing NULL claims remain sealed.
ALTER TABLE key_revocations
    ALTER COLUMN rotation_distributor_claimed SET DEFAULT FALSE;
