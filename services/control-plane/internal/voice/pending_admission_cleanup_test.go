package voice_test

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileExpiredVoicePendingAdmissions_BoundedOldestFirst(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "pending-cleanup-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "pending-cleanup-server")
	channel := ts.CreateVoiceChannel(t, serverID, "pending-cleanup-channel")
	users := []string{
		ts.CreateTestUser(t, "pending-cleanup-expired-a").ID,
		ts.CreateTestUser(t, "pending-cleanup-expired-b").ID,
		ts.CreateTestUser(t, "pending-cleanup-fresh").ID,
	}
	for _, user := range users {
		ts.AddMemberToServer(t, serverID, user, "member")
	}
	_, err := ts.DB.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, 'socket-a', clock_timestamp() - INTERVAL '3 minutes'),
		       ($1, $4, $5, 'socket-b', clock_timestamp() - INTERVAL '1 minute'),
		       ($1, $6, $7, 'socket-fresh', clock_timestamp() + INTERVAL '30 seconds')
	`, channel, users[0], uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		users[1], uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		users[2], uuid.MustParse("33333333-3333-4333-8333-333333333333"))
	require.NoError(t, err)

	removed, err := voice.ReconcileExpiredVoicePendingAdmissions(context.Background(), ts.DB, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	var remaining int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions`).Scan(&remaining))
	assert.Equal(t, 2, remaining)
	var fresh bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM voice_pending_admissions WHERE user_id = $1)`, users[2]).Scan(&fresh))
	assert.True(t, fresh)
	var oldest, newer bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM voice_pending_admissions WHERE user_id = $1)`, users[0]).Scan(&oldest))
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM voice_pending_admissions WHERE user_id = $1)`, users[1]).Scan(&newer))
	assert.False(t, oldest)
	assert.True(t, newer)
	removed, err = voice.ReconcileExpiredVoicePendingAdmissions(context.Background(), ts.DB, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the next bounded pass should make progress")
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions`).Scan(&remaining))
	assert.Equal(t, 1, remaining, "fresh reservations survive subsequent cleanup passes")
}

func TestReconcileExpiredVoicePendingAdmissions_CanceledRetainsRows(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "pending-cleanup-error-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "pending-cleanup-error-server")
	channel := ts.CreateVoiceChannel(t, serverID, "pending-cleanup-error-channel")
	member := ts.CreateTestUser(t, "pending-cleanup-error-member")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	_, err := ts.DB.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, 'socket', clock_timestamp() - INTERVAL '1 minute')
	`, channel, member.ID, uuid.MustParse("44444444-4444-4444-8444-444444444444"))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = voice.ReconcileExpiredVoicePendingAdmissions(ctx, ts.DB, 1)
	assert.Error(t, err)
	var remaining int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions`).Scan(&remaining))
	assert.Equal(t, 1, remaining)
}

func TestReconcileExpiredVoicePendingAdmissions_SkipsLockedOldest(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "pending-cleanup-lock-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "pending-cleanup-lock-server")
	channel := ts.CreateVoiceChannel(t, serverID, "pending-cleanup-lock-channel")
	first := ts.CreateTestUser(t, "pending-cleanup-lock-first")
	second := ts.CreateTestUser(t, "pending-cleanup-lock-second")
	ts.AddMemberToServer(t, serverID, first.ID, "member")
	ts.AddMemberToServer(t, serverID, second.ID, "member")
	_, err := ts.DB.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, 'socket-first', clock_timestamp() - INTERVAL '2 minutes'),
		       ($1, $4, $5, 'socket-second', clock_timestamp() - INTERVAL '1 minute')
	`, channel, first.ID, uuid.MustParse("55555555-5555-4555-8555-555555555555"), second.ID,
		uuid.MustParse("66666666-6666-4666-8666-666666666666"))
	require.NoError(t, err)
	lockTx, err := ts.DB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	var locked string
	require.NoError(t, lockTx.QueryRowContext(t.Context(), `
		SELECT user_id FROM voice_pending_admissions WHERE user_id = $1 FOR UPDATE
	`, first.ID).Scan(&locked))
	assert.Equal(t, first.ID, locked)

	done := make(chan struct{})
	var removed int
	var cleanupErr error
	go func() {
		removed, cleanupErr = voice.ReconcileExpiredVoicePendingAdmissions(context.Background(), ts.DB, 1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup blocked on a locked row")
	}
	require.NoError(t, cleanupErr)
	assert.Equal(t, 1, removed)
	require.NoError(t, lockTx.Rollback())
	removed, err = voice.ReconcileExpiredVoicePendingAdmissions(context.Background(), ts.DB, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
}
