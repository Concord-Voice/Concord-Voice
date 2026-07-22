package presence

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivitySnapshotCandidateLimitCountsRawRowsBeforeDeduplication(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	require.NoError(t, testdb.TruncateAllTables(db))
	ctx := context.Background()
	viewerID := testdb.CreateUser(t, db)
	senderID := testdb.CreateUser(t, db)
	ownerID := testdb.CreateUser(t, db)
	serverID := uuid.New()
	channelID := uuid.New()
	lifecycleAt := time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC)

	_, err := db.ExecContext(ctx,
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'raw-cap-server', $2)`,
		serverID, ownerID,
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO server_members (server_id, user_id)
		VALUES ($1, $2), ($1, $3)
	`, serverID, viewerID, senderID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO channels (id, server_id, name, type)
		VALUES ($1, $2, 'raw-cap-voice', 'voice')
	`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO voice_participants (
			channel_id, user_id, joined_at, lifecycle_event_at
		) VALUES ($1, $2, $3, $3)
	`, channelID, senderID, lifecycleAt)
	require.NoError(t, err)

	// A connection-local temp table safely shadows server_members and multiplies
	// the one legitimate viewer membership without weakening shared constraints.
	// DISTINCT-before-LIMIT collapses these 513 raw candidate rows to one; the
	// required raw-row guard must observe all 513 before any grouping/dedup.
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `DROP TABLE IF EXISTS pg_temp.server_members`)
		_ = conn.Close()
	}()
	_, err = conn.ExecContext(ctx, `
		CREATE TEMP TABLE server_members AS
		SELECT * FROM public.server_members WITH NO DATA
	`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO server_members
		SELECT member.*
		FROM public.server_members AS member
		CROSS JOIN generate_series(1, $3)
		WHERE member.server_id = $1 AND member.user_id = $2
	`, serverID, viewerID, activitySnapshotCandidateLimit+1)
	require.NoError(t, err)

	service := newActivitySnapshotService(conn, nil, nil, nil)
	candidates, err := service.loadCandidates(ctx, viewerID)
	require.NoError(t, err)
	require.Len(t, candidates, activitySnapshotCandidateLimit+1)
	_, err = groupActivitySnapshotCandidates(candidates)
	assert.ErrorIs(t, err, ErrActivitySnapshotCandidateLimit)
}
