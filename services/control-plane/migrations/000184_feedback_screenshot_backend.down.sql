-- Refuse rollback while a vendor-targeted screenshot still needs its placement.
DO $$ BEGIN
    LOCK TABLE feedback_screenshot_uploads IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM feedback_screenshot_uploads
               WHERE storage_backend IS NOT NULL
                 AND (state <> 'deleted' OR put_completed OR put_attempts > 0)) THEN
        RAISE EXCEPTION 'cannot remove screenshot backend while vendor placements exist';
    END IF;
END $$;
ALTER TABLE feedback_screenshot_uploads DROP COLUMN storage_backend;
