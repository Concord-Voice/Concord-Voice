//go:build integration

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var voiceEnforcementIntegrationEpoch = strings.Repeat("0", 32)

func setupVoiceEnforcementDM(t *testing.T, db *sql.DB) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	first, second := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversation := uuid.New()
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, first, second)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, conversation, first)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, first, second)
	require.NoError(t, err)
	return first, second, conversation
}

func integrationVoiceSessionRequest(userID, conversation uuid.UUID) voiceEnforcementSessionRequest {
	return voiceEnforcementSessionRequest{
		SessionGeneration: uuid.NewString(),
		NodeBootID:        uuid.NewString(),
		RoomID:            conversation.String(),
		RoomKind:          "dm",
		UserID:            userID.String(),
		CredentialEpoch:   "",
		SocketID:          "socket-" + uuid.NewString(),
	}
}

func insertVoiceEnforcementSessionTx(ctx context.Context, tx *sql.Tx, request voiceEnforcementSessionRequest) error {
	status, err := guardVoiceEnforcementRegistration(ctx, tx, request)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return errors.New("voice enforcement registration guard did not admit request")
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO voice_enforcement_sessions
			(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		request.SessionGeneration, request.NodeBootID, request.RoomID, request.RoomKind,
		request.UserID, request.CredentialEpoch, request.SocketID)
	return err
}

func openVoiceEnforcementBlockTx(ctx context.Context, db *sql.DB) (*sql.Tx, int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	var txID int64
	if err := tx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&txID); err != nil {
		_ = tx.Rollback()
		return nil, 0, err
	}
	return tx, txID, nil
}

func finishVoiceEnforcementBlockTx(ctx context.Context, tx *sql.Tx, first, second uuid.UUID) error {
	if err := dmblock.RecordBlockTx(ctx, tx, first.String(), second.String(), uuid.NewString()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, first, second)
	return err
}

func beginVoiceEnforcementBlockTx(ctx context.Context, db *sql.DB, first, second uuid.UUID) (*sql.Tx, int64, error) {
	tx, txID, err := openVoiceEnforcementBlockTx(ctx, db)
	if err != nil {
		return nil, 0, err
	}
	if err := finishVoiceEnforcementBlockTx(ctx, tx, first, second); err != nil {
		_ = tx.Rollback()
		return nil, 0, err
	}
	return tx, txID, nil
}

func startVoiceEnforcementBlockTx(t *testing.T, db *sql.DB, first, second uuid.UUID) (*sql.Tx, int64) {
	t.Helper()
	tx, txID, err := beginVoiceEnforcementBlockTx(context.Background(), db, first, second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx, txID
}

// A2 and a block transition use the same users-first lock prefix. Whichever
// transaction wins is allowed to finish, but the loser must not create a
// false zero-row success or an untracked session.
func TestVoiceEnforcementRegistrationRacesDMBlockReconciliation(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("block commits first and denies registration", func(t *testing.T) {
		first, second, conversation := setupVoiceEnforcementDM(t, db)
		blockTx, blockTxID := startVoiceEnforcementBlockTx(t, db, first, second)
		request := integrationVoiceSessionRequest(first, conversation)
		registrationResult := make(chan error, 1)
		go func() {
			registrationTx, err := db.BeginTx(ctx, nil)
			if err != nil {
				registrationResult <- err
				return
			}
			defer func() { _ = registrationTx.Rollback() }()
			registrationResult <- insertVoiceEnforcementSessionTx(ctx, registrationTx, request)
		}()

		dbtest.WaitForRowLockWaiter(t, db, blockTxID)
		require.NoError(t, blockTx.Commit())
		require.ErrorIs(t, <-registrationResult, dmblock.ErrUnavailable)

		var sessions int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation = $1`, request.SessionGeneration).Scan(&sessions))
		assert.Zero(t, sessions, "post-revocation registration must not leave a session row")
	})

	t.Run("registration commits first and leaves pending evidence", func(t *testing.T) {
		first, second, conversation := setupVoiceEnforcementDM(t, db)
		request := integrationVoiceSessionRequest(first, conversation)
		registrationTx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = registrationTx.Rollback() })
		require.NoError(t, insertVoiceEnforcementSessionTx(ctx, registrationTx, request))
		var registrationTxID int64
		require.NoError(t, registrationTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&registrationTxID))

		type blockResult struct {
			tx  *sql.Tx
			err error
		}
		blockStarted := make(chan blockResult, 1)
		blockFinished := make(chan error, 1)
		go func() {
			blockTx, _, blockErr := openVoiceEnforcementBlockTx(ctx, db)
			if blockErr != nil {
				blockStarted <- blockResult{err: blockErr}
				return
			}
			blockStarted <- blockResult{tx: blockTx}
			blockFinished <- finishVoiceEnforcementBlockTx(ctx, blockTx, first, second)
		}()
		var blockTx *sql.Tx
		select {
		case result := <-blockStarted:
			blockTx = result.tx
			require.NoError(t, result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		t.Cleanup(func() { _ = blockTx.Rollback() })
		dbtest.WaitForRowLockWaiter(t, db, registrationTxID)
		require.NoError(t, registrationTx.Commit())
		require.NoError(t, <-blockFinished)
		require.NoError(t, blockTx.Commit())

		var sessions, obligations int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation = $1`, request.SessionGeneration).Scan(&sessions))
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid)`, first, second).Scan(&obligations))
		assert.Equal(t, 1, sessions, "pre-revocation session evidence must remain targetable")
		assert.Equal(t, 1, obligations, "the block obligation must remain pending until media acknowledgement")
	})
}

// The credential fence has the same binary contract independently of the DM
// graph: an old-epoch registration may finish before rotation, or it waits and
// is rejected after the new epoch commits.
func TestVoiceEnforcementRegistrationRacesCredentialEpochRotation(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	conversation := uuid.New()
	_, err := db.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, voiceEnforcementIntegrationEpoch, userID)
	require.NoError(t, err)
	request := integrationVoiceSessionRequest(userID, conversation)
	request.RoomKind = "channel"
	request.CredentialEpoch = voiceEnforcementIntegrationEpoch

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rotationTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rotationTx.Rollback() })
	var locked uuid.UUID
	require.NoError(t, rotationTx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&locked))
	require.NoError(t, rotationTx.QueryRowContext(ctx, `UPDATE users SET credential_epoch = $1 WHERE id = $2 RETURNING id`, strings.Repeat("f", 32), userID).Scan(&locked))
	var rotationTxID int64
	require.NoError(t, rotationTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&rotationTxID))

	registrationResult := make(chan error, 1)
	go func() {
		registrationTx, beginErr := db.BeginTx(ctx, nil)
		if beginErr != nil {
			registrationResult <- beginErr
			return
		}
		defer func() { _ = registrationTx.Rollback() }()
		registrationResult <- insertVoiceEnforcementSessionTx(ctx, registrationTx, request)
	}()
	dbtest.WaitForRowLockWaiter(t, db, rotationTxID)
	require.NoError(t, rotationTx.Commit())
	require.ErrorIs(t, <-registrationResult, credepoch.ErrEpochMismatch)

	var sessions int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation = $1`, request.SessionGeneration).Scan(&sessions))
	assert.Zero(t, sessions, "old-epoch registration must not leave a session row")
}
