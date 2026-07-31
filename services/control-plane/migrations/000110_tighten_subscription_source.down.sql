ALTER TABLE subscriptions
    DROP CONSTRAINT subscriptions_source_check;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_source_check
    CHECK (source IN ('kickstarter', 'stripe', 'code'));
