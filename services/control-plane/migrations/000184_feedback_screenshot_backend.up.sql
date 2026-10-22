-- Preserve each staged screenshot's selected object backend across selector changes.
ALTER TABLE feedback_screenshot_uploads ADD COLUMN storage_backend TEXT;
