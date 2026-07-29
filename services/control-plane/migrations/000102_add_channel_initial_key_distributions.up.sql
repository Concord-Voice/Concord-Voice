-- Restrict only an incomplete channel-key distribution to its creator. The
-- marker advances with any mandatory rotation and is deleted once every current
-- server member has that marker epoch's key.
CREATE TABLE channel_initial_key_distributions (
    channel_id UUID PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    creator_id UUID REFERENCES users(id) ON DELETE SET NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- An incomplete channel has exactly one distributor. Do not leave a marker
-- pointing at a deleted account, because no remaining client can complete it.
CREATE FUNCTION delete_incomplete_channels_for_erased_creator()
RETURNS TRIGGER AS $$
BEGIN
    DELETE FROM channels
    WHERE id IN (
        SELECT channel_id
        FROM channel_initial_key_distributions
        WHERE creator_id = OLD.id
    );
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER delete_incomplete_channels_for_erased_creator
BEFORE DELETE ON users
FOR EACH ROW EXECUTE FUNCTION delete_incomplete_channels_for_erased_creator();
