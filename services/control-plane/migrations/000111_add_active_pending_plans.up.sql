-- Durable reconciliation evidence for the ACTIVE Rich Presence categories
-- (Server Voice, Private Call). Custom Status already has such a rail
-- (presence_settings_pending_operations, 000087); the active categories were
-- in-memory only, so a process death between commit and delivery left no
-- record that a clear was owed.
--
-- This is a SIBLING table, deliberately not a widening of 000087's table:
-- that one is PRIMARY KEY (user_id), which structurally forbids a second
-- concurrent pending operation per user, and a row in it suppresses the
-- sender's whole Custom Status reconnect snapshot for the row's lifetime.
CREATE TABLE presence_active_pending_plans (
    user_id            UUID        NOT NULL,
    category           TEXT        NOT NULL,
    operation_id       UUID        NOT NULL,
    resolution         TEXT        NOT NULL,
    scope_lifecycle_id UUID,
    scope_event_at     TIMESTAMPTZ NOT NULL,
    attempts           INTEGER     NOT NULL DEFAULT 0,
    failure_class      TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    reconcile_after    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    CONSTRAINT presence_active_pending_plans_pkey
        PRIMARY KEY (user_id, category),

    -- RESTRICT, not CASCADE. 000087's rail satisfies custody structurally --
    -- its CASCADE makes it incapable of serving an erased principal. This rail
    -- cannot: a user in a Private Call inside a deleted group IS the subject,
    -- and can then be erased. RESTRICT is what compels the erasure path to
    -- drain the plan first. Do not soften this to CASCADE; doing so silently
    -- discards unresolved privacy-repair evidence.
    CONSTRAINT presence_active_pending_plans_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT,

    CONSTRAINT presence_active_pending_plans_operation_id_key
        UNIQUE (operation_id),

    CONSTRAINT presence_active_pending_plans_category_check
        CHECK (category IN ('server_voice', 'private_call')),

    CONSTRAINT presence_active_pending_plans_resolution_check
        CHECK (resolution IN ('exact', 'conservative')),

    -- An exact plan MUST carry the generation it is aimed at. Without one the
    -- resolver cannot tell "this stale call" from "a successor call", and a
    -- clear aimed at a successor kills a live call's presence. The database
    -- enforces the resolver's fail-closed rule rather than trusting Go to.
    CONSTRAINT presence_active_pending_plans_exact_evidence_check
        CHECK (resolution = 'conservative' OR scope_lifecycle_id IS NOT NULL),

    CONSTRAINT presence_active_pending_plans_attempts_check
        CHECK (attempts >= 0),

    -- Closed vocabulary, mirrored from a Go enum. A free-text column here
    -- would eventually hold a wrapped database error, and the log line that
    -- prints it would become a data leak.
    CONSTRAINT presence_active_pending_plans_failure_class_check
        CHECK (failure_class IS NULL OR failure_class IN (
            'state_read', 'state_unexpiring', 'state_malformed',
            'generation_delete', 'delivery', 'plan_invalid')),

    CONSTRAINT presence_active_pending_plans_reconcile_after_check
        CHECK (reconcile_after >= created_at)
);

-- The reconciler scans by due time; user_id and category complete the key so
-- the claim can be taken from the index entry.
CREATE INDEX idx_presence_active_pending_plans_due
    ON presence_active_pending_plans (reconcile_after, user_id, category);

COMMENT ON TABLE presence_active_pending_plans IS
    'Durable per-subject reconciliation obligations for the active Rich Presence categories. Stores subjects and generations only - never an audience, a conversation, or any social-graph edge. One row names exactly one user: itself.';
COMMENT ON COLUMN presence_active_pending_plans.user_id IS
    'The subject sender whose own active-category state is owed a retraction. Never a viewer.';
COMMENT ON COLUMN presence_active_pending_plans.category IS
    'server_voice or private_call. Part of the primary key so one user may owe one obligation per category concurrently.';
COMMENT ON COLUMN presence_active_pending_plans.operation_id IS
    'Exact-successor identity. Acknowledgement deletes on the full key WHERE user_id, category AND operation_id all match, so a stale in-flight resolver acknowledges zero rows rather than discharging a fresher obligation.';
COMMENT ON COLUMN presence_active_pending_plans.resolution IS
    'exact or conservative. Monotone: a CAPTURE collision ratchets exact -> conservative and never back, because the conservative action may already have run. The retry ceiling does NOT ratchet it -- it retains the row and moves reconcile_after to the quarantine interval.';
COMMENT ON COLUMN presence_active_pending_plans.scope_lifecycle_id IS
    'The generation this plan is aimed at, required for an exact resolution. NULL is legal only for a conservative plan. Not a room identifier and not dereferenceable after the mutation.';
COMMENT ON COLUMN presence_active_pending_plans.scope_event_at IS
    'The captured lifecycle watermark. The resolver refuses an exact clear when live state is newer than this.';
COMMENT ON COLUMN presence_active_pending_plans.attempts IS
    'Bounded retry counter. Past the ceiling the row is QUARANTINED -- retained, with reconcile_after moved to the quarantine interval -- never deleted, because the row is the evidence that an obligation is undischarged. The resolution is not changed by the ceiling.';
COMMENT ON COLUMN presence_active_pending_plans.failure_class IS
    'Closed diagnostic vocabulary, never a wrapped error string.';
COMMENT ON COLUMN presence_active_pending_plans.reconcile_after IS
    'Claim lease. Advancing it under the row lock IS the claim; there is no separate claimed_by column.';
