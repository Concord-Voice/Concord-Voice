-- 000079: Fold usernames to lowercase + enforce case-insensitive uniqueness (#1931).
--
-- Identity is already case-insensitive everywhere (login / existence / search all
-- use LOWER(username)), but the SSO registration path stored raw mixed-case
-- usernames while the password path lowercased them. A mixed-case row broke the
-- profile-edit no-op guard, friend-add resolution, and let a case-variant register
-- as a distinct account (duplicate-by-case / impersonation). This migration makes
-- storage consistent and adds the structural guard.
--
-- TRANSACTIONAL + ATOMIC: the runner (internal/database/postgres.go) executes each
-- migration transactionally, so the collision check, the case-fold, and the index
-- build either all apply or all roll back. A pre-existing duplicate-by-case pair
-- aborts the whole migration (fail-closed — never a silent identity squash).
--
-- PLAIN (non-CONCURRENT) index: CREATE INDEX CONCURRENTLY cannot run inside a
-- transaction block, and `users` is small in the Beta window (matches the plain-
-- index precedent in 000050). Switch to CONCURRENTLY in a separate non-transactional
-- migration only when `users` crosses the large-table trigger in [internal]rules/migrations.md.
--
-- OPERATOR NOTE — if step 1 aborts with a duplicate-by-case collision: this means a
-- pre-existing pair of accounts differs only by username case (e.g. 'JohnDoe' +
-- 'johndoe'), which predates this fix (only the SSO path could create it). Resolve
-- MANUALLY before re-running: pick the surviving account, reassign or remove the
-- loser's owned rows (messages, memberships, friendships, keys) per their FKs, then
-- re-apply. Do NOT auto-merge identities — that is the silent squash this guard exists
-- to prevent. At Beta scale this is expected to be empty (no rows folded).

-- 1. Fail closed on any pre-existing duplicate-by-case rows: folding them would
--    violate uniqueness, and silently merging identities is unacceptable. Surface
--    the colliding lowercased usernames so an operator can resolve them by hand.
DO $$
DECLARE dupes text;
BEGIN
  SELECT string_agg(lu, ', ') INTO dupes FROM (
    SELECT LOWER(username) AS lu
    FROM users
    GROUP BY LOWER(username)
    HAVING COUNT(*) > 1
  ) c;
  IF dupes IS NOT NULL THEN
    RAISE EXCEPTION 'username case-fold collision — resolve these duplicate-by-case usernames manually before migrating (see OPERATOR NOTE in this migration): %', dupes;
  END IF;
END $$;

-- 2. Fold existing mixed-case usernames to lowercase (parity with the password path).
UPDATE users SET username = LOWER(username) WHERE username <> LOWER(username);

-- 3. Structural guard: case-insensitive uniqueness. Also makes the LOWER(username)
--    lookups in profile-update + friend-add index-backed.
CREATE UNIQUE INDEX users_username_lower_key ON users (LOWER(username));
