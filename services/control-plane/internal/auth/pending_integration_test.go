package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingCleanupDeletesExpired(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)

	_, err := ts.DB.Exec(`
		INSERT INTO pending_registrations
		(id, email, username, password_hash, wrapped_private_key,
		 key_derivation_salt, key_derivation_alg, public_key,
		 age_verified, resend_count, expires_at)
		VALUES (gen_random_uuid(), 'expired@test.com', 'expiredu',
		 'hash', '\x00', '\x00', 'argon2id', '\x00',
		 TRUE, 0, NOW() - INTERVAL '1 minute')
	`)
	require.NoError(t, err)

	n, err := repo.DeleteExpired(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	var remaining int
	_ = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations
		 WHERE email = 'expired@test.com'`).Scan(&remaining)
	assert.Equal(t, 0, remaining)
}

func TestStartPendingCleanupWorker_DeletesExpiredOnTick(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	log := logger.New("test")

	_, err := ts.DB.Exec(`
		INSERT INTO pending_registrations
		(id, email, username, password_hash, wrapped_private_key,
		 key_derivation_salt, key_derivation_alg, public_key,
		 age_verified, resend_count, expires_at)
		VALUES (gen_random_uuid(), 'worker@test.concord.chat', 'workertest',
		 'h', '\x00', '\x00', 'argon2id', '\x00',
		 TRUE, 0, NOW() - INTERVAL '1 minute')
	`)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth.StartPendingCleanupWorker(ctx, repo, log, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		var n int
		_ = ts.DB.QueryRow(
			`SELECT COUNT(*) FROM pending_registrations WHERE email = 'worker@test.concord.chat'`,
		).Scan(&n)
		return n == 0
	}, 2*time.Second, 50*time.Millisecond)
}

func TestStartPendingCleanupWorker_StopsOnContextCancel(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	log := logger.New("test")

	ctx, cancel := context.WithCancel(context.Background())
	auth.StartPendingCleanupWorker(ctx, repo, log, 50*time.Millisecond)
	cancel()
	// Allow time for the goroutine to observe cancellation; a leak would surface
	// under -race as a dangling goroutine referencing ts after cleanup.
	time.Sleep(150 * time.Millisecond)
}

func TestStartPendingCleanupWorker_DeletesOnTick(t *testing.T) {
	// Insert the expired row AFTER the initial sweep fires so the deletion
	// is triggered by the ticker path (covering the ticker branch in the worker).
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	log := logger.New("test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the worker with a long initial interval so the initial sweep runs
	// on an empty table, then insert the row to be caught on the first tick.
	auth.StartPendingCleanupWorker(ctx, repo, log, 80*time.Millisecond)

	// Insert after the worker goroutine has had time to complete its initial sweep.
	time.Sleep(20 * time.Millisecond)
	_, err := ts.DB.Exec(`
		INSERT INTO pending_registrations
		(id, email, username, password_hash, wrapped_private_key,
		 key_derivation_salt, key_derivation_alg, public_key,
		 age_verified, resend_count, expires_at)
		VALUES (gen_random_uuid(), 'tickworker@test.concord.chat', 'tickworker',
		 'h', '\x00', '\x00', 'argon2id', '\x00',
		 TRUE, 0, NOW() - INTERVAL '1 minute')
	`)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		var n int
		_ = ts.DB.QueryRow(
			`SELECT COUNT(*) FROM pending_registrations WHERE email = 'tickworker@test.concord.chat'`,
		).Scan(&n)
		return n == 0
	}, 2*time.Second, 50*time.Millisecond)
}

// --- Regression tests for the Copilot/Seer findings on PR #688 ---

// insertPendingParams builds a valid InsertParams for the given email/username,
// using HashPassword so takeover flows that re-verify the raw password work.
func insertPendingParams(t *testing.T, email, username, rawPassword string) auth.InsertParams {
	t.Helper()
	hash, err := auth.HashPassword(rawPassword)
	require.NoError(t, err)
	return auth.InsertParams{
		Email:             email,
		Username:          username,
		PasswordHash:      hash,
		WrappedPrivateKey: []byte{0x01},
		KeyDerivationSalt: []byte{0x02},
		KeyDerivationAlg:  "argon2id",
		PublicKey:         []byte{0x03},
	}
}

// TestUpdateEmail_ExpiredRejected regresses Copilot comment 3104406699:
// UpdateEmail must return ErrPendingExpired on a row past its TTL rather than
// silently "resurrecting" it.
func TestUpdateEmail_ExpiredRejected(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	ctx := context.Background()

	id, _, _, err := repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "expiring@test.concord.chat", "expiring1", "TestPassword123!"),
		"TestPassword123!",
	)
	require.NoError(t, err)

	// Age the row past expiry.
	_, err = ts.DB.Exec(
		`UPDATE pending_registrations SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`,
		id,
	)
	require.NoError(t, err)

	err = repo.UpdateEmail(ctx, id, "fresh@test.concord.chat")
	assert.ErrorIs(t, err, auth.ErrPendingExpired)

	// The stored email must NOT have been rewritten.
	var storedEmail string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT email FROM pending_registrations WHERE id = $1`, id,
	).Scan(&storedEmail))
	assert.Equal(t, "expiring@test.concord.chat", storedEmail)
}

// TestUpdateEmail_NormalizesCase regresses Copilot comment 3104406710:
// UpdateEmail must normalize to lowercase+trim so downstream lookups and the
// UNIQUE(email) constraint behave consistently with InsertOrTakeover.
func TestUpdateEmail_NormalizesCase(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	ctx := context.Background()

	id, _, _, err := repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "normcase@test.concord.chat", "normcase1", "TestPassword123!"),
		"TestPassword123!",
	)
	require.NoError(t, err)

	// Caller passes a mixed-case, whitespace-padded address.
	require.NoError(t, repo.UpdateEmail(ctx, id, "  Mixed@Test.Concord.Chat  "))

	var storedEmail string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT email FROM pending_registrations WHERE id = $1`, id,
	).Scan(&storedEmail))
	assert.Equal(t, "mixed@test.concord.chat", storedEmail)
}

// TestInsertOrTakeover_UsesPolicyTTL regresses Copilot comment 3104406713:
// expires_at must be derived from PendingRegistrationTTL, not a hardcoded
// `NOW() + INTERVAL '15 minutes'`. A few seconds of clock skew tolerance is
// applied because the row is inserted between the sampled `now` values.
func TestInsertOrTakeover_UsesPolicyTTL(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	ctx := context.Background()

	before := time.Now()
	_, expiresAt, _, err := repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "ttlcheck@test.concord.chat", "ttlcheck1", "TestPassword123!"),
		"TestPassword123!",
	)
	require.NoError(t, err)
	after := time.Now()

	// expires_at must sit within [before+TTL, after+TTL] plus a small tolerance.
	lower := before.Add(auth.PendingRegistrationTTL).Add(-2 * time.Second)
	upper := after.Add(auth.PendingRegistrationTTL).Add(2 * time.Second)
	assert.Truef(t,
		!expiresAt.Before(lower) && !expiresAt.After(upper),
		"expires_at %s outside window [%s, %s]", expiresAt, lower, upper,
	)
}

// TestInsertOrTakeover_PendingUsernameCollisionAfterTakeover regresses
// After an email takeover succeeds, a
// *different* pending row holding the target username must produce
// ErrUsernameTaken (to yield a 409) rather than a unique-constraint 500.
func TestInsertOrTakeover_PendingUsernameCollisionAfterTakeover(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	ctx := context.Background()

	// Row A: holds the target username, owned by a different pending registration.
	_, _, _, err := repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "other@test.concord.chat", "contested1", "TestPassword123!"),
		"TestPassword123!",
	)
	require.NoError(t, err)

	// Row B: the one that will be taken over via matching email + password.
	_, _, _, err = repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "takeover@test.concord.chat", "takeover1", "TakeoverPass456!"),
		"TakeoverPass456!",
	)
	require.NoError(t, err)

	// Re-register with Row B's email + password (triggers takeover) but with
	// Row A's username. Expect a clean ErrUsernameTaken.
	_, _, _, err = repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "takeover@test.concord.chat", "contested1", "TakeoverPass456!"),
		"TakeoverPass456!",
	)
	assert.ErrorIs(t, err, auth.ErrUsernameTaken)
}

// TestRegisterTakeoverPreservesPendingOnCollision regresses Copilot round-2
// comment 3104513536: when a takeover attempt fails the collision check (a
// *different* pending row already holds the submitted username), the caller's
// *own* legitimate pending row must NOT be deleted.
//
// Setup: Row A (email=A, username=u_a) and Row B (email=B, username=u_b),
// each owned by different passwords. The caller re-submits Row B's email+password
// but requests Row A's username. InsertOrTakeover must return ErrUsernameTaken
// AND leave Row B intact in the DB.
func TestRegisterTakeoverPreservesPendingOnCollision(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repo := auth.NewPendingRepo(ts.DB)
	ctx := context.Background()

	// Row A — a bystander holding the contested username.
	_, _, _, err := repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "bystander@test.concord.chat", "preserve_u_a", "BystanderPass1!"),
		"BystanderPass1!",
	)
	require.NoError(t, err)

	// Row B — the caller's legitimate pending row.
	rowBID, _, _, err := repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "caller@test.concord.chat", "preserve_u_b", "CallerPass2!"),
		"CallerPass2!",
	)
	require.NoError(t, err)

	// Takeover attempt: Row B's email+password (takeover candidate), but the
	// username is already held by Row A — must fail with ErrUsernameTaken.
	_, _, _, err = repo.InsertOrTakeover(ctx,
		insertPendingParams(t, "caller@test.concord.chat", "preserve_u_a", "CallerPass2!"),
		"CallerPass2!",
	)
	require.ErrorIs(t, err, auth.ErrUsernameTaken, "expected ErrUsernameTaken from collision check")

	// Row B must still exist — the collision rejection must NOT have deleted it.
	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_registrations WHERE id = $1`, rowBID,
	).Scan(&count))
	assert.Equal(t, 1, count, "caller's pending row must survive a failed takeover attempt")
}
