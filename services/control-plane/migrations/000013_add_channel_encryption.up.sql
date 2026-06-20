-- Add encryption support to channels and servers
ALTER TABLE channels ADD COLUMN is_encrypted BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE servers ADD COLUMN e2ee_default BOOLEAN NOT NULL DEFAULT TRUE;
