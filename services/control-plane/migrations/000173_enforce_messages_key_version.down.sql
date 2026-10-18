ALTER TABLE messages
    ALTER COLUMN key_version DROP NOT NULL,
    ALTER COLUMN key_version SET DEFAULT 1;
