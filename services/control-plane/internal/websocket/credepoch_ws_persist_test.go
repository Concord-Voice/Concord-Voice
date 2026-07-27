package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
)

// #2201 (Codex #2397 review): a WS message frame carrying a stale credential
// epoch — read before a destructive reset's DisconnectUser landed — must be
// fenced by GuardTx inside the persist transaction and write no ciphertext.

const wsPersistArgonHash = "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec // dev-only test hash // pragma: allowlist secret

func TestPersistMessage_StaleEpochRejected(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)
	userID := uuid.New()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, credential_epoch, age_verified, email_verified)
		VALUES ($1, $2, $3, $4, 'newEpoch', true, true)`,
		userID.String(), "wsstalechan@test.concord.chat", "wsstalechan", wsPersistArgonHash)
	require.NoError(t, err)

	_, _, _, errMsg := hub.persistMessage(persistMessageParams{
		channelUUID: uuid.New(), userID: userID, credEpoch: "staleEpoch",
		content: "Y2lwaGVydGV4dA==", keyVersion: 1,
	})
	assert.Equal(t, "Authentication required", errMsg, "stale-epoch WS send must fail closed")

	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE user_id = $1`, userID.String()).Scan(&count))
	assert.Zero(t, count, "no channel message may persist under a stale epoch")
}

// #2201 (Codex #2397 review, F3): a transient GuardTx users-row READ failure is
// not a proven epoch mismatch — the channel path must return the retryable save
// error, not the auth-shaped rejection. A userID with no users row makes
// GuardTx's SELECT fail closed with a wrapped store error (not ErrEpochMismatch).
func TestPersistMessage_GuardReadFailureIsSaveError(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)

	_, _, _, errMsg := hub.persistMessage(persistMessageParams{
		channelUUID: uuid.New(), userID: uuid.New(), credEpoch: "whatever",
		content: "Y2lwaGVydGV4dA==", keyVersion: 1,
	})
	assert.Equal(t, "Failed to save message", errMsg,
		"a store/read guard failure must be the retryable save error, not auth-shaped")
}

func TestPersistMessage_RemovedMemberRejected(t *testing.T) {
	setup := setupMessageTest(t)
	channelID := uuid.MustParse(setup.convID)

	_, err := setup.db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, setup.user2, setup.user1)
	require.NoError(t, err)

	_, _, _, errMsg := setup.hub.persistMessage(persistMessageParams{
		channelUUID: channelID, userID: setup.user1,
		content: "Y2lwaGVydGV4dA==", keyVersion: 1,
	})
	assert.Equal(t, "Not a member of this server", errMsg)

	var count int
	require.NoError(t, setup.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "a removed member's socket must not persist a channel message")
}

func TestPersistMessage_MembershipLockHonorsTimeout(t *testing.T) {
	setup := setupMessageTest(t)
	channelID := uuid.MustParse(setup.convID)

	lockTx, err := setup.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	var membership int
	err = lockTx.QueryRow(
		`SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2 FOR UPDATE`,
		setup.user2, setup.user1,
	).Scan(&membership)
	require.NoError(t, err)

	result := make(chan string, 1)
	go func() {
		_, _, _, errMsg := setup.hub.persistMessage(persistMessageParams{
			channelUUID: channelID, userID: setup.user1,
			content: "Y2lwaGVydGV4dA==", keyVersion: 1,
		})
		result <- errMsg
	}()

	select {
	case errMsg := <-result:
		assert.Equal(t, errMsgFailedSaveMessage, errMsg)
	case <-time.After(channelAuthCtxTimeout + time.Second):
		t.Fatal("membership lock did not honor the WebSocket message timeout")
	}
}

func TestPersistMessage_MembershipLockWriterFirstDefersRemoval(t *testing.T) {
	setup := setupMessageTest(t)
	channelID := uuid.MustParse(setup.convID)

	messageLockTx, err := setup.db.Begin()
	require.NoError(t, err)
	defer func() { _ = messageLockTx.Rollback() }()
	_, err = messageLockTx.Exec(`LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	sendResult := make(chan string, 1)
	go func() {
		_, _, _, errMsg := setup.hub.persistMessage(persistMessageParams{
			channelUUID: channelID, userID: setup.user1,
			content: "Y2lwaGVydGV4dA==", keyVersion: 1,
		})
		sendResult <- errMsg
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := setup.db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%INSERT INTO messages%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, time.Second, 10*time.Millisecond, "socket writer should hold membership while waiting to insert")

	removalResult := make(chan error, 1)
	go func() {
		tx, beginErr := setup.db.Begin()
		if beginErr != nil {
			removalResult <- beginErr
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, deleteErr := tx.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, setup.user2, setup.user1); deleteErr != nil {
			removalResult <- deleteErr
			return
		}
		removalResult <- tx.Commit()
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := setup.db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%DELETE FROM server_members%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, time.Second, 10*time.Millisecond, "removal should wait on the socket writer's membership lock")

	require.NoError(t, messageLockTx.Commit())
	select {
	case errMsg := <-sendResult:
		assert.Empty(t, errMsg)
	case <-time.After(time.Second):
		t.Fatal("socket writer did not commit after releasing the message lock")
	}
	select {
	case err := <-removalResult:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("removal did not finish after the socket writer committed")
	}

	var count int
	require.NoError(t, setup.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&count))
	assert.Equal(t, 1, count, "the admitted socket writer must commit before removal proceeds")
}

func TestPersistDMMessage_StaleEpochRejected(t *testing.T) {
	db := setupHubTestDB(t)
	hub := NewHub(db, nil)
	userID := uuid.New()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, credential_epoch, age_verified, email_verified)
		VALUES ($1, $2, $3, $4, 'newEpoch', true, true)`,
		userID.String(), "wsstaledm@test.concord.chat", "wsstaledm", wsPersistArgonHash)
	require.NoError(t, err)

	_, _, _, gErr := hub.persistDMMessage(uuid.New(), userID, "staleEpoch", &dmMessageInput{
		content: "Y2lwaGVydGV4dA==", keyVersion: 1, msgType: "user",
	})
	assert.ErrorIs(t, gErr, credepoch.ErrEpochMismatch, "stale-epoch WS DM send must fail closed")

	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE user_id = $1`, userID.String()).Scan(&count))
	assert.Zero(t, count, "no DM message may persist under a stale epoch")
}
