package rbac

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"testing"

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
