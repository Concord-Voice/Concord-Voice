DO $$
BEGIN
    -- Keep the emptiness check and DROP under one table lock so a concurrent
    -- reservation cannot commit between them and lose lifecycle evidence.
    LOCK TABLE feedback_screenshot_uploads IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM feedback_screenshot_uploads) THEN
        RAISE EXCEPTION 'refusing to drop feedback screenshot lifecycle evidence';
    END IF;
    DROP TABLE feedback_screenshot_uploads;
END $$;
