package dm

import (
	"context"
	"database/sql"
	"testing"
	"time"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsertCallEvent_UsesDatabaseClockAndSharedExpiry(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, caller, _ := seedHiddenConv(t, db)
	h := &Handler{db: db}
	startedAt := time.Now().UTC().Add(-time.Minute)

	for _, tc := range []struct {
		name   string
		window any
		want   bool
	}{
		{name: "disabled policy", window: nil, want: false},
		{name: "one hour policy", window: 3600, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(`UPDATE dm_conversations SET expiration_window_seconds = $1 WHERE id = $2`, tc.window, convID)
			require.NoError(t, err)
			err = h.insertCallEvent(context.Background(), uuid.MustParse(convID), CallEventPayload{
				RingID: uuid.New(), CallerUserID: uuid.MustParse(caller), ParticipantUserIDs: []uuid.UUID{uuid.MustParse(caller)},
				StartedAt: startedAt, EndedAt: startedAt.Add(time.Second), Status: CallEventCanceled,
			})
			require.NoError(t, err)
			var created time.Time
			var expires sql.NullTime
			require.NoError(t, db.QueryRow(`SELECT created_at, expires_at FROM dm_messages WHERE conversation_id = $1 ORDER BY created_at DESC LIMIT 1`, convID).Scan(&created, &expires))
			assert.WithinDuration(t, time.Now().UTC(), created, 5*time.Second)
			if tc.want {
				require.NotNil(t, expires)
				assert.WithinDuration(t, created.Add(time.Hour), expires.Time, time.Microsecond)
			} else {
				assert.False(t, expires.Valid)
			}
			_, err = db.Exec(`DELETE FROM dm_messages WHERE conversation_id = $1`, convID)
			require.NoError(t, err)
		})
	}
}

func TestCallEventWriters_RollBackWhenCallerOrConversationIsMissing(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, caller, _ := seedHiddenConv(t, db)
	h := &Handler{db: db}
	callerID := uuid.MustParse(caller)
	now := time.Now().UTC()
	payload := func(userID uuid.UUID) CallEventPayload {
		return CallEventPayload{RingID: uuid.New(), CallerUserID: userID, ParticipantUserIDs: []uuid.UUID{userID}, StartedAt: now, EndedAt: now.Add(time.Second), Status: CallEventCanceled}
	}

	cases := []struct {
		name   string
		conv   uuid.UUID
		caller uuid.UUID
	}{
		{name: "missing caller", conv: uuid.MustParse(convID), caller: uuid.New()},
		{name: "missing conversation", conv: uuid.New(), caller: callerID},
	}
	for _, tc := range cases {
		t.Run(tc.name+" ordinary", func(t *testing.T) {
			inUse := db.Stats().InUse
			err := h.insertCallEvent(context.Background(), tc.conv, payload(tc.caller))
			require.Error(t, err)
			assert.ErrorIs(t, err, sql.ErrNoRows)
			assert.Equal(t, inUse, db.Stats().InUse, "failed ordinary writer must release its transaction")
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, convID).Scan(&count))
			assert.Zero(t, count)
		})
	}

	for _, tc := range cases {
		t.Run(tc.name+" completed", func(t *testing.T) {
			callID := uuid.New()
			inUse := db.Stats().InUse
			err := InsertCompletedCallEvent(context.Background(), db, tc.conv, CompletedCallSummary{
				CallID: callID, CallerUserID: tc.caller, ParticipantUserIDs: []uuid.UUID{tc.caller}, StartedAt: now, EndedAt: now.Add(time.Second),
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, sql.ErrNoRows)
			assert.Equal(t, inUse, db.Stats().InUse, "failed completed writer must release its transaction")
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, callID).Scan(&count))
			assert.Zero(t, count)
		})
	}

	// A subsequent valid write still succeeds after all failed paths.
	validID := uuid.New()
	require.NoError(t, InsertCompletedCallEvent(context.Background(), db, uuid.MustParse(convID), CompletedCallSummary{
		CallID: validID, CallerUserID: callerID, ParticipantUserIDs: []uuid.UUID{callerID}, StartedAt: now, EndedAt: now.Add(time.Second),
	}))
}

func TestCallEventWriter_RespawnsParticipantsAfterPersistedMessage(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, caller, _ := seedHiddenConv(t, db)
	hiddenAt := time.Now().UTC().Add(-time.Minute)
	_, err := db.Exec(`UPDATE dm_participants SET hidden_at = $1 WHERE conversation_id = $2`, hiddenAt, convID)
	require.NoError(t, err)

	h := &Handler{db: db}
	now := time.Now().UTC()
	require.NoError(t, h.insertCallEvent(context.Background(), uuid.MustParse(convID), CallEventPayload{
		RingID: uuid.New(), CallerUserID: uuid.MustParse(caller), ParticipantUserIDs: []uuid.UUID{uuid.MustParse(caller)},
		StartedAt: now.Add(-time.Minute), EndedAt: now, Status: CallEventCanceled,
	}))

	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND hidden_at IS NOT NULL`, convID).Scan(&remaining))
	assert.Zero(t, remaining, "a persisted call event respawns hidden participants")
}

func TestCompletedCallEvent_NoOpDoesNotRespawn(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, caller, _ := seedHiddenConv(t, db)
	convUUID := uuid.MustParse(convID)
	callerUUID := uuid.MustParse(caller)
	hiddenAt := time.Now().UTC().Add(-time.Minute)
	_, err := db.Exec(`UPDATE dm_participants SET hidden_at = $1 WHERE conversation_id = $2`, hiddenAt, convID)
	require.NoError(t, err)

	payload := CallEventPayload{
		RingID: uuid.New(), CallerUserID: callerUUID, ParticipantUserIDs: []uuid.UUID{callerUUID},
		StartedAt: hiddenAt, EndedAt: hiddenAt.Add(time.Second), Status: CallEventCompleted,
	}
	callID := uuid.New()
	require.NoError(t, insertCompletedCallEvent(context.Background(), db, convUUID, callID, payload, false))
	_, err = db.Exec(`UPDATE dm_participants SET hidden_at = $1 WHERE conversation_id = $2`, hiddenAt, convID)
	require.NoError(t, err)
	require.NoError(t, insertCompletedCallEvent(context.Background(), db, convUUID, callID, payload, false))

	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND hidden_at IS NOT NULL`, convID).Scan(&remaining))
	assert.Equal(t, 2, remaining, "DO NOTHING conflict must not respawn hidden participants")
}

func TestCompletedCallEvent_EndedAtBeforeHideKeepsParticipantsHidden(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, caller, _ := seedHiddenConv(t, db)
	convUUID := uuid.MustParse(convID)
	callerUUID := uuid.MustParse(caller)
	now := time.Now().UTC()
	endedAt := now.Add(-2 * time.Second)
	hiddenAt := now.Add(-time.Second)
	_, err := db.Exec(`UPDATE dm_participants SET hidden_at = $1 WHERE conversation_id = $2`, hiddenAt, convID)
	require.NoError(t, err)

	first := CallEventPayload{
		RingID: uuid.New(), CallerUserID: callerUUID, ParticipantUserIDs: []uuid.UUID{callerUUID},
		StartedAt: endedAt.Add(-time.Minute), EndedAt: endedAt, Status: CallEventCompleted,
	}
	require.NoError(t, insertCompletedCallEvent(context.Background(), db, convUUID, uuid.New(), first, false))
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND hidden_at IS NOT NULL`, convID).Scan(&remaining))
	assert.Equal(t, 2, remaining, "an event timestamped before the hide must not reveal participants")

	later := first
	later.EndedAt = now.Add(time.Second)
	require.NoError(t, insertCompletedCallEvent(context.Background(), db, convUUID, uuid.New(), later, false))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND hidden_at IS NOT NULL`, convID).Scan(&remaining))
	assert.Zero(t, remaining, "a truly later event may reveal participants")
}
