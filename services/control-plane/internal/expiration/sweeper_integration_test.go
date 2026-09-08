//go:build integration

package expiration

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSweeper_RunPreflight_Restore25000(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner := testdb.CreateUser(t, db)
	other := testdb.CreateUser(t, db)
	server, channel, conversation := uuid.New(), uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'preflight 25000 server', $2)`, server, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'preflight 25000 channel', 'text')`, channel, server)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, conversation, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, owner, other)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	_, err = db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at)
SELECT $1, $2, 'expired', $3, $4 FROM generate_series(1, 12500)`, channel, owner, cutoff.Add(-2*time.Hour), cutoff.Add(-time.Hour))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, expires_at)
SELECT $1, $2, 'expired', 'user', $3, $4 FROM generate_series(1, 12500)`, conversation, owner, cutoff.Add(-2*time.Hour), cutoff.Add(-time.Hour))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES
		($1, $2, 'exact cutoff', $3, $4), ($1, $2, 'future', $3, $5), ($1, $2, 'null', $3, NULL)`, channel, owner, cutoff.Add(-time.Hour), cutoff, cutoff.Add(time.Hour))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, expires_at) VALUES
		($1, $2, 'exact cutoff', 'user', $3, $4), ($1, $2, 'future', 'user', $3, $5), ($1, $2, 'null', 'user', $3, NULL)`, conversation, owner, cutoff.Add(-time.Hour), cutoff, cutoff.Add(time.Hour))
	require.NoError(t, err)
	wrappedKey := "wrapped-key-survivor"
	_, err = db.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key) VALUES ($1, $2, $3)`, channel, owner, wrappedKey)
	require.NoError(t, err)
	dmWrappedKey := "dm-wrapped-key-survivor"
	_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key) VALUES ($1, $2, $3)`, conversation, owner, dmWrappedKey)
	require.NoError(t, err)

	engineLog := logger.New("test")
	engine := purge.NewEngine(db, engineLog, purge.NewReaper(db, engineLog, nil), purge.MaxExpiryCandidateIDs)
	var batches int
	s, err := NewSweeper(SweeperDeps{
		DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil },
		RunExpiryBatch: func(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
			batches++
			require.LessOrEqual(t, len(plan.CandidateIDs), purge.MaxExpiryCandidateIDs)
			return engine.RunExpiryBatch(ctx, plan)
		},
		EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {}, Log: logger.New("test"),
	})
	require.NoError(t, err)
	started := time.Now()
	require.NoError(t, s.RunPreflight(context.Background()))
	t.Logf("preflight 25000 expired rows: %s", time.Since(started))
	require.GreaterOrEqual(t, batches, 6, "each table drains its 12,500 candidates through bounded expiry batches")
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT (SELECT count(*) FROM messages WHERE expires_at < $1) + (SELECT count(*) FROM dm_messages WHERE expires_at < $1)`, cutoff).Scan(&remaining))
	require.Zero(t, remaining, "fresh global eligibility check must be empty")
	var exact, future, nullCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FILTER (WHERE expires_at = $2), count(*) FILTER (WHERE expires_at > $2), count(*) FILTER (WHERE expires_at IS NULL) FROM messages WHERE channel_id = $1`, channel, cutoff).Scan(&exact, &future, &nullCount))
	require.Equal(t, 1, exact)
	require.Equal(t, 1, future)
	require.Equal(t, 1, nullCount)
	var dmExact, dmFuture, dmNullCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FILTER (WHERE expires_at = $2), count(*) FILTER (WHERE expires_at > $2), count(*) FILTER (WHERE expires_at IS NULL) FROM dm_messages WHERE conversation_id = $1`, conversation, cutoff).Scan(&dmExact, &dmFuture, &dmNullCount))
	require.Equal(t, 1, dmExact)
	require.Equal(t, 1, dmFuture)
	require.Equal(t, 1, dmNullCount)
	var gotKey string
	require.NoError(t, db.QueryRow(`SELECT wrapped_key FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channel, owner).Scan(&gotKey))
	require.Equal(t, wrappedKey, gotKey)
	var gotDMKey string
	require.NoError(t, db.QueryRow(`SELECT wrapped_key FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2`, conversation, owner).Scan(&gotDMKey))
	require.Equal(t, dmWrappedKey, gotDMKey)
}
