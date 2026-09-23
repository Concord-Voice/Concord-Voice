package rbac

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerVisibilityCaptureAdvisoryKey_IsStableAndDomainSeparated(t *testing.T) {
	serverID := "11111111-2222-4333-8444-555555555555"

	first, err := ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	second, err := ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)

	assert.Equal(t, first, second, "the key must be stable across calls")

	other, err := ServerVisibilityCaptureAdvisoryKey("99999999-2222-4333-8444-555555555555")
	require.NoError(t, err)
	assert.NotEqual(t, first, other, "distinct servers take distinct locks")
}

func TestServerVisibilityCaptureAdvisoryKey_DiffersFromOtherDomains(t *testing.T) {
	serverID := "11111111-2222-4333-8444-555555555555"

	key, err := ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)

	// The same UUID under any other domain string must land elsewhere in the
	// advisory key space. Recompute the internal/users domain locally rather
	// than importing it (that package must not become an rbac dependency).
	otherDomain := advisoryKeyForTest("activity_settings_cleanup\x00" + serverID)
	voiceDomain := advisoryKeyForTest("voice_lifecycle\x00" + serverID)

	assert.NotEqual(t, otherDomain, key)
	assert.NotEqual(t, voiceDomain, key)
}

func TestServerVisibilityCaptureAdvisoryKey_InvalidServer_Errors(t *testing.T) {
	t.Run("empty server id", func(t *testing.T) {
		_, err := ServerVisibilityCaptureAdvisoryKey("")
		require.Error(t, err)
	})

	t.Run("nil uuid", func(t *testing.T) {
		_, err := ServerVisibilityCaptureAdvisoryKey(uuid.Nil.String())
		require.Error(t, err)
	})

	t.Run("malformed uuid", func(t *testing.T) {
		_, err := ServerVisibilityCaptureAdvisoryKey("not-a-uuid")
		require.Error(t, err)
	})
}

func TestLockServerVisibilityCapture_InvalidInput_ErrorsBeforeAnyStatement(t *testing.T) {
	t.Run("nil transaction", func(t *testing.T) {
		err := LockServerVisibilityCapture(context.Background(), nil, uuid.New().String())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "transaction unavailable")
	})

	t.Run("invalid server id", func(t *testing.T) {
		// tx is nil, so reaching ExecContext would panic. The server-id
		// validation must run first and return, which it does because the nil
		// check precedes it and the key derivation precedes the statement.
		var tx *sql.Tx
		require.NotPanics(t, func() {
			err := LockServerVisibilityCapture(context.Background(), tx, "not-a-uuid")
			require.Error(t, err)
		})
	})
}

func TestWithAuthorityCapture_LocksUserForeignKeysBeforeServerParent(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "authority_tx.go"))
	require.NoError(t, err)
	body := string(source)
	start := strings.Index(body, "func (h *Handler) withAuthorityCapture(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(body[start:], "// LockAuthorityPrincipalsTx locks")
	require.Greater(t, end, 0)
	body = body[start : start+end]
	principalLock := strings.Index(body, "LockAuthorityPrincipalsTx(ctx, tx, principalIDs)")
	serverLock := strings.Index(body, "SELECT id FROM servers WHERE id = $1 FOR UPDATE")
	require.GreaterOrEqual(t, principalLock, 0)
	require.GreaterOrEqual(t, serverLock, 0)
	assert.Less(t, principalLock, serverLock,
		"user FKs must be locked before the server parent to prevent hidden-FK deadlocks")
	lifecycleLock := strings.Index(body, "lockAuthorityLifecyclePrincipalsTx(ctx, tx, lifecyclePrincipalIDs)")
	visibilityLock := strings.Index(body, "LockServerVisibilityCapture(ctx, tx, serverID)")
	require.GreaterOrEqual(t, lifecycleLock, 0)
	assert.Less(t, visibilityLock, lifecycleLock,
		"visibility advisory lock must precede lifecycle advisory locks")
	assert.Less(t, lifecycleLock, principalLock,
		"lifecycle advisory locks must precede ordinary user locks")
}

func TestCaptureChannelKeyCandidatesTx_OversizedLegacyHistoryIsBounded(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	owner := dbtest.CreateUser(t, db)
	serverID, channelID := uuid.NewString(), uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'candidate-cap-server', $2)`, serverID, owner)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID) })
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'candidate-cap-channel', 'text')`, channelID, serverID)
	require.NoError(t, err)
	for i := 0; i < 501; i++ {
		candidate := dbtest.CreateUser(t, db)
		_, err = db.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'a2V5', 1)`, channelID, candidate)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	candidates, err := CaptureChannelKeyCandidatesTx(ctx, tx, []string{channelID}, 500)
	require.NoError(t, err, "legacy oversized history must not abort the authority mutation")
	assert.Len(t, candidates[channelID], 500, "cleanup candidates remain bounded")
}

func TestCaptureAuthorityCandidates_TargetedUserIsLimitedToDurableChannels(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	owner := dbtest.CreateUser(t, db)
	target := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'targeted-candidate-server', $2)`, serverID, owner)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID) })

	keyChannelID, pendingChannelID, hiddenChannelID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, channelID := range []string{keyChannelID, pendingChannelID, hiddenChannelID} {
		_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`, channelID, serverID, channelID)
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'a2V5', 1)`, keyChannelID, target)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO pending_key_requests (channel_id, user_id) VALUES ($1, $2)`, pendingChannelID, target)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	targetID := target.String()
	candidates, err := captureAuthorityCandidates(ctx, tx, []string{keyChannelID, pendingChannelID, hiddenChannelID}, &targetID)
	require.NoError(t, err)
	assert.Equal(t, []string{targetID}, candidates[keyChannelID])
	assert.Equal(t, []string{targetID}, candidates[pendingChannelID])
	assert.Empty(t, candidates[hiddenChannelID], "a user with no durable key state must not receive a channel revocation")
}

func TestRecordDeniedChannelKeyCandidates_OmitsEmptyCandidateSets(t *testing.T) {
	deniedByChannel := make(map[string][]string)
	recordDeniedChannelKeyCandidates(deniedByChannel, "visible-channel", nil)
	assert.NotContains(t, deniedByChannel, "visible-channel")

	deniedUserIDs := []string{"denied-user"}
	recordDeniedChannelKeyCandidates(deniedByChannel, "denied-channel", deniedUserIDs)
	assert.Equal(t, deniedUserIDs, deniedByChannel["denied-channel"])
}

func TestLockAuthorityPrincipalsTx_ValidatesAndRequiresEveryUser(t *testing.T) {
	ctx := context.Background()
	t.Run("invalid id is rejected before the transaction is used", func(t *testing.T) {
		assert.Error(t, LockAuthorityPrincipalsTx(ctx, nil, []string{"not-a-uuid"}))
	})

	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db).String()

	t.Run("existing user is locked", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		require.NoError(t, LockAuthorityPrincipalsTx(ctx, tx, []string{userID}))

		probe, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = probe.Rollback() })
		var lockedID string
		err = probe.QueryRowContext(ctx,
			`SELECT id FROM users WHERE id = $1 FOR UPDATE NOWAIT`, userID,
		).Scan(&lockedID)
		require.Error(t, err, "a second transaction must not acquire the principal row lock")
	})
	t.Run("missing user fails closed", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		err = LockAuthorityPrincipalsTx(ctx, tx, []string{uuid.NewString()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no longer exists")
	})
}

func TestWithAuthorityCapture_WriteError_RollsBack(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	owner := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'rollback-server', $2)`, serverID, owner)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID) })

	h := &Handler{db: db}
	writeErr := errors.New("authority write failed")
	_, err = h.withAuthorityCapture(ctx, serverID, nil, nil,
		func(context.Context, *sql.Tx) error { return writeErr }, owner.String())
	require.Error(t, err)
	assert.ErrorIs(t, err, writeErr)

	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM servers WHERE id = $1`, serverID).Scan(&name))
	assert.Equal(t, "rollback-server", name)
}

func TestWithAuthorityCapture_CommitError_AbandonsPlanAfterCommit(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	owner := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'before-commit', $2)`, serverID, owner)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID) })

	stub := &presenceRecheckStub{plan: &presenceRecheckPlanStub{work: true}}
	h := &Handler{db: db}
	h.SetPresenceRecheck(stub)
	commitErr := errors.New("commit acknowledgement lost")
	h.authorityCommit = func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		return commitErr
	}

	plan, err := h.withAuthorityCapture(ctx, serverID, nil, nil,
		func(ctx context.Context, tx *sql.Tx) error {
			_, updateErr := tx.ExecContext(ctx, `UPDATE servers SET name = 'committed' WHERE id = $1`, serverID)
			return updateErr
		}, owner.String())
	require.Error(t, err)
	assert.True(t, IsAmbiguousAuthorityCommit(err))
	assert.Nil(t, plan)
	assert.Equal(t, []string{"PrepareCapture", "CaptureVisibility", "Abandon"}, stub.sequence)
	assert.Equal(t, []string{"ambiguous_commit"}, stub.abandons)

	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM servers WHERE id = $1`, serverID).Scan(&name))
	assert.Equal(t, "committed", name)
}

// ORDERING REGRESSION LOCK. Phase 1 must run BEFORE BeginTx, outside the
// advisory lock. Collapsing the two phases back into one tx-bound call would
// make advisory-lock hold time O(#senders) instead of O(#affected channels) —
// on a 1000-participant channel that is ~1000 sequential round trips holding a
// lock that serializes every RBAC mutation on the server. h.db is deliberately
// nil: if BeginTx were reached before PrepareCapture returned its error, this
// would panic on a nil *sql.DB.
func TestWithAuthorityCapture_PrepareCaptureError_NeverOpensTheTransaction(t *testing.T) {
	stub := &presenceRecheckStub{prepareErr: errors.New("candidate read failed")}
	h := &Handler{}
	h.SetPresenceRecheck(stub)
	wrote := false

	var (
		plan PresenceRecheckPlan
		err  error
	)
	require.NotPanics(t, func() {
		plan, err = h.withAuthorityCapture(
			context.Background(), uuid.New().String(), []string{"channel"}, nil,
			func(context.Context, *sql.Tx) error { wrote = true; return nil },
		)
	}, "BeginTx must never be reached when PrepareCapture fails")

	require.Error(t, err)
	assert.Nil(t, plan)
	assert.False(t, wrote, "the permission write never happens (spec section 8, class 1)")
	assert.Equal(t, []string{"PrepareCapture"}, stub.sequence,
		"no CaptureVisibility, no Execute, no Abandon")
}

// A nil recheck is the pre-#2445 default. Phase 1 must still be a no-op that
// returns a nil plan, so the handler falls through to the transaction with no
// capture at all rather than failing the write.
func TestWithAuthorityCapture_NilRecheck_SkipsCaptureEntirely(t *testing.T) {
	h := &Handler{}

	plan, err := h.preparePresenceCapture(
		context.Background(), uuid.New().String(), nil, nil,
	)

	require.NoError(t, err)
	assert.Nil(t, plan, "a nil recheck produces no plan and therefore no dispatch")
}

func advisoryKeyForTest(domain string) int64 {
	digest := sha256.Sum256([]byte(domain))
	return int64(binary.BigEndian.Uint64(digest[:8])) //nolint:gosec // test-local mirror of the production derivation
}
