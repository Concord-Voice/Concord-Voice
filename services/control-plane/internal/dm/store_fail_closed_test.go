package dm

import (
	"context"
	"database/sql"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Every fence helper that reads the store must fail closed: a store error is
// returned to the caller, never read as "no rows" and never as permission.
// A finished transaction makes each helper's first read fail.
func TestDMFenceTxHelpers_ReturnStoreErrors(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	ctx := context.Background()
	conv, user := uuid.NewString(), uuid.NewString()
	h := &Handler{log: logger.NewWithWriter(io.Discard)}

	for name, call := range map[string]func() error{
		"readDMVoiceEnforcementSnapshotTx": func() error {
			_, err := readDMVoiceEnforcementSnapshotTx(ctx, tx, conv, user)
			return err
		},
		"dmVoiceEnforcementSnapshotTx": func() error {
			_, err := dmVoiceEnforcementSnapshotTx(ctx, tx, conv, user)
			return err
		},
		"fetchParticipantIDsTx": func() error {
			_, err := fetchParticipantIDsTx(ctx, tx, conv)
			return err
		},
		"lockPreparedDMParticipantsTx": func() error {
			return lockPreparedDMParticipantsTx(ctx, tx, conv, []uuid.UUID{uuid.New()})
		},
		"nextDMVoiceAuthorizationRevision": func() error {
			_, err := nextDMVoiceAuthorizationRevision(ctx, tx)
			return err
		},
		"dmTopologyPermittedTx": func() error {
			permitted, err := dmTopologyPermittedTx(ctx, tx, user, uuid.NewString())
			assert.False(t, permitted, "a store error never permits")
			return err
		},
		"recheckDMTopologyTx": func() error {
			return recheckDMTopologyTx(ctx, tx, uuid.New(), []uuid.UUID{uuid.New(), uuid.New()})
		},
		"removeMemberRowsTx": func() error {
			_, _, _, err := h.removeMemberRowsTx(ctx, tx, conv, uuid.New(), user, memberRemovalPreflight{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { assert.Error(t, call()) })
	}
}

// Writers that open their own transaction fail closed when the store is gone.
func TestDMFenceWriters_ReturnStoreErrors(t *testing.T) {
	closed, err := sql.Open("postgres", "postgres://unused/closed?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	ctx := context.Background()
	conv, user := uuid.NewString(), uuid.NewString()
	h := &Handler{db: closed, log: logger.NewWithWriter(io.Discard)}

	for name, call := range map[string]func() error{
		"ReadVoiceEnforcementSnapshot": func() error {
			_, err := ReadVoiceEnforcementSnapshot(ctx, closed, conv, user)
			return err
		},
		"addMemberTx": func() error {
			_, err := h.addMemberTx(ctx, conv, uuid.NewString(), user, "", []string{user})
			return err
		},
		"removeInactiveMemberTx": func() error {
			_, _, err := h.removeInactiveMemberTx(ctx, conv, uuid.New(), user, "", memberRemovalPreflight{})
			return err
		},
		"updateDMRoleTx": func() error {
			return h.updateDMRoleTx(ctx, dmRoleChange{conversationID: conv, actorID: user, targetID: uuid.NewString(), role: "admin", actor: uuid.MustParse(user), target: uuid.New()}, "")
		},
		"readDMKeyTx": func() error {
			var key dmKeyResponse
			return h.readDMKeyTx(ctx, conv, user, "", uuid.MustParse(user), &key)
		},
	} {
		t.Run(name, func(t *testing.T) { assert.Error(t, call()) })
	}

	_, _, err = h.removeActiveMemberTx(ctx, conv, uuid.New(), user, "", memberRemovalPreflight{})
	assert.ErrorIs(t, err, activepresence.ErrRailNotWired, "an unwired presence rail refuses the removal")
}
