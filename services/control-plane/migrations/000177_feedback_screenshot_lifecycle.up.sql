-- Durable lifecycle ledger for user-submitted feedback screenshots.
-- Pending bytes live in PostgreSQL until publication, so abandoned uploads
-- can be reclaimed without an uncertain object-store delete.
CREATE TABLE feedback_screenshot_uploads (
    object_key UUID PRIMARY KEY,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    state TEXT NOT NULL CHECK (state IN ('pending', 'pinned', 'deleted')),
    put_completed BOOLEAN NOT NULL DEFAULT FALSE,
    put_attempts INTEGER NOT NULL DEFAULT 0 CHECK (put_attempts >= 0 AND put_attempts <= 3),
    staged_data BYTEA,
    content_type VARCHAR(100),
    pin_attempts BIGINT NOT NULL DEFAULT 0 CHECK (pin_attempts >= 0),
    pin_tokens UUID[] NOT NULL DEFAULT '{}',
    published BOOLEAN NOT NULL DEFAULT FALSE,
    retained BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at TIMESTAMPTZ NOT NULL,
    retained_at TIMESTAMPTZ,
    retry_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT feedback_screenshot_staging_consistent CHECK
        (staged_data IS NULL OR (content_type IS NOT NULL AND octet_length(staged_data) = size_bytes)),
    CONSTRAINT feedback_screenshot_pending_staged CHECK
        (state <> 'pending' OR staged_data IS NOT NULL),
    CONSTRAINT feedback_screenshot_pinned_readable CHECK
        (state <> 'pinned' OR staged_data IS NOT NULL OR put_completed),
    CONSTRAINT feedback_screenshot_deleted_unstaged CHECK
        (state <> 'deleted' OR staged_data IS NULL),
    CONSTRAINT feedback_screenshot_offload_confirmed CHECK
        (NOT put_completed OR staged_data IS NULL),
    CONSTRAINT feedback_screenshot_retained_pinned CHECK
        (NOT retained OR state = 'pinned'),
    CONSTRAINT feedback_screenshot_provisional_deadline CHECK
        (state <> 'pinned' OR published OR retained OR retained_at IS NOT NULL),
    CONSTRAINT feedback_screenshot_pending_deadline CHECK
        (state <> 'pending' OR retained_at IS NULL),
    CONSTRAINT feedback_screenshot_pin_receipts CHECK
        (cardinality(pin_tokens) = pin_attempts)
);

CREATE INDEX feedback_screenshot_uploads_expiry_idx
    ON feedback_screenshot_uploads (expires_at, retry_at)
    WHERE state = 'pending';
-- Admission scans only live charges; deleted tombstones remain for proxy denial
-- but no longer slow every reservation as their number grows.
CREATE INDEX feedback_screenshot_uploads_charged_idx
    ON feedback_screenshot_uploads (user_id, state) INCLUDE (size_bytes)
    WHERE state <> 'deleted';
CREATE INDEX feedback_screenshot_uploads_offload_idx
    ON feedback_screenshot_uploads (retry_at, object_key)
    WHERE state = 'pinned' AND (published OR retained) AND staged_data IS NOT NULL AND put_attempts < 3;
CREATE INDEX feedback_screenshot_uploads_retention_idx
    ON feedback_screenshot_uploads (retained_at, object_key)
    WHERE state = 'pinned' AND NOT published AND NOT retained;
-- ON DELETE SET NULL must locate retained owners without a table scan.
-- Confirmed deletion clears user_id, keeping this index bounded by live rows.
CREATE INDEX feedback_screenshot_uploads_owner_fk_idx
    ON feedback_screenshot_uploads (user_id) WHERE user_id IS NOT NULL;

-- Existing screenshots have no trustworthy submission timestamps. Preserve them
-- indefinitely, including uploads no longer referenced by a feedback issue.
INSERT INTO feedback_screenshot_uploads
    (object_key, user_id, size_bytes, state, put_completed, published, expires_at)
SELECT substring(storage_key FROM '^feedback-screenshots/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$')::uuid,
       uploader_id, GREATEST(file_size, 1), 'pinned', TRUE, TRUE, NOW()
FROM media_files
WHERE media_tier = 1
  AND storage_key ~ '^feedback-screenshots/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
ON CONFLICT (object_key) DO NOTHING;
