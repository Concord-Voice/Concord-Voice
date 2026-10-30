//go:build integration

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupVoiceEnforcementDM(t *testing.T, db *sql.DB, first uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	second := dbtest.CreateUser(t, db)
	conversation := uuid.New()
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, first, second)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, conversation, first)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, first, second)
	require.NoError(t, err)
	return second, conversation
}

func integrationVoiceSessionRequest(r *voiceAuthRig, roomID uuid.UUID, kind string) protocol.RoomRegister {
	return protocol.RoomRegister{Schema: "concord.auth-room-binding", Version: 1, Mode: "attach", ConnectionGeneration: r.target.ConnectionGeneration, NodeBootID: r.target.NodeBootID, SocketID: r.target.SocketID, CredentialEpoch: r.epoch, SessionGeneration: uuid.NewString(), RoomID: roomID.String(), RoomKind: kind}
}

func requireVoiceEnforcementRows(t *testing.T, r *voiceAuthRig, request protocol.RoomRegister, expected int) {
	t.Helper()
	var sessions, bindings int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation=$1`, request.SessionGeneration).Scan(&sessions))
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_room_bindings WHERE session_generation=$1 AND connection_generation=$2`, request.SessionGeneration, request.ConnectionGeneration).Scan(&bindings))
	assert.Equal(t, expected, sessions)
	assert.Equal(t, expected, bindings, "room evidence must remain associated with its exact connection")
}

func rollbackVoiceEnforcementTx(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		require.NoError(t, err)
	}
}

func setupVoiceEnforcementChannel(t *testing.T, db *sql.DB, userID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ownerID := dbtest.CreateUser(t, db)
	serverID := uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'voice enforcement', $2)`, serverID, ownerID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, userID)
	require.NoError(t, err)
	roleID := uuid.New()
	_, err = db.Exec(`
		INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		VALUES ($1, $2, '@all', 0, $3, true, true)`,
		roleID, serverID, int64(rbac.BasePermissions))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, userID, roleID)
	require.NoError(t, err)
	channelID := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'voice', 'voice')`, channelID, serverID)
	require.NoError(t, err)
	return serverID, channelID
}

func openVoiceEnforcementBlockTx(ctx context.Context, db *sql.DB) (*sql.Tx, int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	var txID int64
	if err := tx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&txID); err != nil {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
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
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
		return nil, 0, err
	}
	return tx, txID, nil
}

func startVoiceEnforcementBlockTx(t *testing.T, db *sql.DB, first, second uuid.UUID) (*sql.Tx, int64) {
	t.Helper()
	tx, txID, err := beginVoiceEnforcementBlockTx(context.Background(), db, first, second)
	require.NoError(t, err)
	t.Cleanup(func() { rollbackVoiceEnforcementTx(t, tx) })
	return tx, txID
}

// The signed HTTP owner and block writer serialize on complete user authority.
func TestVoiceEnforcementRegistrationRacesDMBlockReconciliation(t *testing.T) {
	t.Run("block commits first and denies registration", func(t *testing.T) {
		r := newVoiceAuthRig(t)
		r.register(t)
		second, conversation := setupVoiceEnforcementDM(t, r.db, r.userID)
		request := integrationVoiceSessionRequest(r, conversation, "dm")
		blockTx, blockTxID := startVoiceEnforcementBlockTx(t, r.db, r.userID, second)
		wire, _ := r.signed(t, protocol.EndpointRoomRegister, request, r.token)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- r.do(wire) }()
		dbtest.WaitForRowLockWaiter(t, r.db, blockTxID)
		require.NoError(t, blockTx.Commit())
		response := <-done
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		requireVoiceEnforcementRows(t, r, request, 0)
	})
	t.Run("registration commits first and leaves pending evidence", func(t *testing.T) {
		r := newVoiceAuthRig(t)
		binding := r.register(t)
		second, conversation := setupVoiceEnforcementDM(t, r.db, r.userID)
		request := integrationVoiceSessionRequest(r, conversation, "dm")
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		// Pause the handler at its connection row, after its users/family prefix.
		gate, err := r.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { rollbackVoiceEnforcementTx(t, gate) })
		var generation string
		require.NoError(t, gate.QueryRowContext(ctx, `SELECT connection_generation FROM voice_auth_connections WHERE connection_generation=$1 FOR UPDATE`, r.target.ConnectionGeneration).Scan(&generation))
		var gateTxID int64
		var gatePID int
		require.NoError(t, gate.QueryRowContext(ctx, `SELECT txid_current(),pg_backend_pid()`).Scan(&gateTxID, &gatePID))
		wire, signedRequest := r.signed(t, protocol.EndpointRoomRegister, request, r.token)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- r.do(wire) }()
		dbtest.WaitForRowLockWaiter(t, r.db, gateTxID)
		var registrationTxID int64
		require.NoError(t, r.db.QueryRowContext(ctx, `SELECT l.transactionid::text::bigint FROM pg_locks l WHERE l.locktype='transactionid' AND l.granted AND $1=ANY(pg_blocking_pids(l.pid))`, gatePID).Scan(&registrationTxID))
		blockTx, _, err := openVoiceEnforcementBlockTx(ctx, r.db)
		require.NoError(t, err)
		t.Cleanup(func() { rollbackVoiceEnforcementTx(t, blockTx) })
		blockFinished := make(chan error, 1)
		go func() { blockFinished <- finishVoiceEnforcementBlockTx(ctx, blockTx, r.userID, second) }()
		dbtest.WaitForRowLockWaiter(t, r.db, registrationTxID)
		require.NoError(t, gate.Commit())
		response := <-done
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		r.verify(t, protocol.EndpointRoomRegister, response, signedRequest, protocol.ExpectedResponse{Binding: &binding})
		require.NoError(t, <-blockFinished)
		require.NoError(t, blockTx.Commit())
		requireVoiceEnforcementRows(t, r, request, 1)
		var obligations int
		require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE user_a_id=LEAST($1::uuid,$2::uuid) AND user_b_id=GREATEST($1::uuid,$2::uuid)`, r.userID, second).Scan(&obligations))
		assert.Equal(t, 1, obligations, "block obligation remains pending until media acknowledgement")
	})
}

func TestVoiceEnforcementRegistrationRacesCredentialEpochRotation(t *testing.T) {
	r := newVoiceAuthRig(t)
	r.register(t)
	_, channelID := setupVoiceEnforcementChannel(t, r.db, r.userID)
	request := integrationVoiceSessionRequest(r, channelID, "channel")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	rotationTx, err := r.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { rollbackVoiceEnforcementTx(t, rotationTx) })
	_, err = rotationTx.ExecContext(ctx, `UPDATE users SET credential_epoch=$1 WHERE id=$2`, strings.Repeat("f", 32), r.userID)
	require.NoError(t, err)
	var rotationTxID int64
	require.NoError(t, rotationTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&rotationTxID))
	wire, _ := r.signed(t, protocol.EndpointRoomRegister, request, r.token)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.do(wire) }()
	dbtest.WaitForRowLockWaiter(t, r.db, rotationTxID)
	require.NoError(t, rotationTx.Commit())
	response := <-done
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	requireVoiceEnforcementRows(t, r, request, 0)
}

func TestVoiceEnforcementRegistrationRechecksChannelAuthority(t *testing.T) {
	tests := []struct {
		name   string
		revoke func(t *testing.T, db *sql.DB, serverID, channelID, userID uuid.UUID)
	}{
		{
			name: "server membership removed",
			revoke: func(t *testing.T, db *sql.DB, serverID, _ uuid.UUID, userID uuid.UUID) {
				t.Helper()
				_, err := db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, userID)
				require.NoError(t, err)
			},
		},
		{
			name: "view voice permission removed",
			revoke: func(t *testing.T, db *sql.DB, _ uuid.UUID, channelID, userID uuid.UUID) {
				t.Helper()
				_, err := db.Exec(`
					INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
					VALUES ($1, $2, 'user', $3, 0, $4)`,
					uuid.New(), channelID, userID, int64(rbac.PermViewVoiceChannels))
				require.NoError(t, err)
			},
		},
		{
			name: "join voice permission removed",
			revoke: func(t *testing.T, db *sql.DB, _ uuid.UUID, channelID, userID uuid.UUID) {
				t.Helper()
				_, err := db.Exec(`
					INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
					VALUES ($1, $2, 'user', $3, 0, $4)`,
					uuid.New(), channelID, userID, int64(rbac.PermJoinVoice))
				require.NoError(t, err)
			},
		},
		{
			name: "voice timeout made active",
			revoke: func(t *testing.T, db *sql.DB, serverID, _ uuid.UUID, userID uuid.UUID) {
				t.Helper()
				_, err := db.Exec(`UPDATE server_members SET timed_out_until = NOW() + INTERVAL '1 hour' WHERE server_id = $1 AND user_id = $2`, serverID, userID)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			binding := r.register(t)
			serverID, channelID := setupVoiceEnforcementChannel(t, r.db, r.userID)
			request := integrationVoiceSessionRequest(r, channelID, "channel")
			response, signedRequest := r.request(t, protocol.EndpointRoomRegister, request, r.token)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			r.verify(t, protocol.EndpointRoomRegister, response, signedRequest, protocol.ExpectedResponse{Binding: &binding})
			requireVoiceEnforcementRows(t, r, request, 1)
			request.Mode = "renew"
			unchanged, _ := r.request(t, protocol.EndpointRoomRegister, request, r.token)
			require.Equal(t, http.StatusOK, unchanged.Code, unchanged.Body.String())
			tc.revoke(t, r.db, serverID, channelID, r.userID)
			revoked, _ := r.request(t, protocol.EndpointRoomRegister, request, r.token)
			assert.Equal(t, http.StatusForbidden, revoked.Code, revoked.Body.String())
			requireVoiceEnforcementRows(t, r, request, 1)
		})
	}
}
