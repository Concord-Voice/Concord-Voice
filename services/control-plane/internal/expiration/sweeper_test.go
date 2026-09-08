//go:build integration

package expiration

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSweeper_RequiresDatabaseAndPurgeTerminal(t *testing.T) {
	_, err := NewSweeper(SweeperDeps{RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) { return purge.Result{}, nil }})
	require.Error(t, err, "a sweeper cannot discover or globally verify without a database")

	db, _ := testdb.SetupTestDB(t)
	_, err = NewSweeper(SweeperDeps{DB: db})
	require.Error(t, err, "a sweeper cannot turn discovery into deletion without the typed expiry terminal")

	deps := SweeperDeps{DB: db, RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) { return purge.Result{}, nil }}
	_, err = NewSweeper(deps)
	require.Error(t, err, "a sweeper cannot notify DM participants without the DM terminal")
	deps.EmitDMPurged = func(context.Context, string, time.Time) {}
	_, err = NewSweeper(deps)
	require.Error(t, err, "a sweeper cannot notify server subscribers without the server terminal")
}

func TestSweeper_RunPass_UsesFixedCutoffAndBoundsContexts(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner := testdb.CreateUser(t, db)
	server := uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'sweeper server', $2)`, server, owner)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 123456789, time.FixedZone("test", 3600))
	for i := 0; i < MaxExpiryContextsPerTable+1; i++ {
		channel := uuid.New()
		_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`, channel, server, "sweeper-channel-"+channel.String())
		require.NoError(t, err)
		var message string
		require.NoError(t, db.QueryRow(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'ciphertext', $3, $4) RETURNING id`, channel, owner, cutoff.Add(-2*time.Hour), cutoff.Add(-time.Hour)).Scan(&message))
		assert.NotEmpty(t, message)
	}
	var plans []purge.ExpiryPlan
	var notified []string
	s, err := NewSweeper(SweeperDeps{
		DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil },
		RunExpiryBatch: func(_ context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
			plans = append(plans, plan)
			return purge.Result{DeletedCount: 1}, nil
		},
		EmitDMPurged:     func(context.Context, string, time.Time) {},
		EmitServerPurged: func(_ context.Context, id string, _ time.Time) { notified = append(notified, id) },
		Log:              logger.New("test"),
	})
	require.NoError(t, err)
	discovered, err := s.RunPass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, MaxExpiryContextsPerTable, len(plans), "one pass must cap each table at ten contexts")
	assert.GreaterOrEqual(t, discovered, len(plans), "discovery count includes every discovered candidate")
	assert.LessOrEqual(t, discovered, purge.MaxExpiryCandidateIDs, "discovery remains bounded")
	assert.Equal(t, []string{server.String()}, notified, "changed channels in one server coalesce to one notification per pass")
	assert.Equal(t, cutoff.UTC().Truncate(time.Microsecond), plans[0].ExpiresBefore)
	assert.NotEmpty(t, plans[0].CandidateIDs[0], "candidate IDs remain concrete database IDs")
}

func TestSweeper_RunPass_SelectsOnlyTheOldestFiveThousandCandidates(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner, server, channel := testdb.CreateUser(t, db), uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'bounded candidates server', $2)`, server, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'bounded candidates channel', 'text')`, channel, server)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	_, err = db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at)
SELECT $1, $2, 'later expired', $3, $4 FROM generate_series(1, 5000)`, channel, owner, cutoff.Add(-time.Hour), cutoff.Add(-time.Minute))
	require.NoError(t, err)
	var oldest string
	require.NoError(t, db.QueryRow(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'oldest expired', $3, $4) RETURNING id`, channel, owner, cutoff.Add(-2*time.Hour), cutoff.Add(-2*time.Hour)).Scan(&oldest))

	var plan purge.ExpiryPlan
	s, err := NewSweeper(SweeperDeps{
		DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil },
		RunExpiryBatch: func(_ context.Context, got purge.ExpiryPlan) (purge.Result, error) {
			plan = got
			return purge.Result{}, nil
		},
		EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {}, Log: logger.New("test"),
	})
	require.NoError(t, err)
	discovered, err := s.RunPass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, purge.MaxExpiryCandidateIDs, discovered)
	assert.Len(t, plan.CandidateIDs, purge.MaxExpiryCandidateIDs)
	assert.Contains(t, plan.CandidateIDs, oldest, "the oldest candidate must displace a later row inserted first")
}

func TestSweeper_RunPass_FlushesCommittedNotificationsBeforeError(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner, server := testdb.CreateUser(t, db), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'partial server', $2)`, server, owner)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		channel := uuid.New()
		_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`, channel, server, channel.String())
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'ciphertext', $3, $4)`, channel, owner, cutoff.Add(-time.Hour), cutoff.Add(-time.Minute))
		require.NoError(t, err)
	}
	var notified []string
	wantErr := errors.New("second context failed")
	count := 0
	s, err := NewSweeper(SweeperDeps{DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil }, RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
		count++
		if count == 2 {
			return purge.Result{DeletedCount: 1}, wantErr
		}
		return purge.Result{DeletedCount: 1}, nil
	}, EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(_ context.Context, id string, _ time.Time) { notified = append(notified, id) }, Log: logger.New("test")})
	require.NoError(t, err)
	_, err = s.RunPass(context.Background())
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, []string{server.String()}, notified, "one committed server invalidation survives a later error")
}

func TestSweeper_RunPreflight_RestoresUntilFreshGlobalCheck(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner := testdb.CreateUser(t, db)
	server, channel := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'preflight server', $2)`, server, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'preflight channel', 'text')`, channel, server)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	var id string
	require.NoError(t, db.QueryRow(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'ciphertext', $3, $4) RETURNING id`, channel, owner, cutoff.Add(-2*time.Hour), cutoff.Add(-time.Hour)).Scan(&id))
	clockCalls, runs := 0, 0
	var inserted string
	s, err := NewSweeper(SweeperDeps{
		DB: db, Clock: func(context.Context) (time.Time, error) {
			clockCalls++
			if clockCalls == 4 {
				err := db.QueryRow(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'eligible after empty discovery', $3, $4) RETURNING id`, channel, owner, cutoff.Add(-time.Hour), cutoff.Add(-time.Minute)).Scan(&inserted)
				require.NoError(t, err)
			}
			return cutoff, nil
		},
		RunExpiryBatch: func(_ context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
			runs++
			if runs == 1 {
				return purge.Result{}, nil
			}
			_, err := db.Exec(`DELETE FROM messages WHERE id = ANY($1)`, pq.Array(plan.CandidateIDs))
			return purge.Result{DeletedCount: 1}, err
		},
		EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {}, Log: logger.New("test"),
	})
	require.NoError(t, err)
	require.NoError(t, s.RunPreflight(context.Background()))
	assert.GreaterOrEqual(t, runs, 3, "a stale zero-result group must not end preflight before a later real deletion")
	assert.GreaterOrEqual(t, clockCalls, 4, "preflight must obtain a fresh verification clock after the empty pass")
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1 AND expires_at < $2`, inserted, cutoff).Scan(&remaining))
	assert.Zero(t, remaining, "fresh global eligibility must resume draining rather than fail startup for an ordinary new expiry")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, id).Scan(&remaining))
	assert.Zero(t, remaining)
}

func TestSweeper_RunPass_StaleZeroDoesNotNotify(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner, server, channel := testdb.CreateUser(t, db), uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'stale server', $2)`, server, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'stale channel', 'text')`, channel, server)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	_, err = db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'stale', $3, $4)`, channel, owner, cutoff.Add(-time.Hour), cutoff.Add(-time.Minute))
	require.NoError(t, err)
	notifications := 0
	s, err := NewSweeper(SweeperDeps{DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil }, RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) { return purge.Result{}, nil }, EmitDMPurged: func(context.Context, string, time.Time) { notifications++ }, EmitServerPurged: func(context.Context, string, time.Time) { notifications++ }, Log: logger.New("test")})
	require.NoError(t, err)
	_, err = s.RunPass(context.Background())
	require.NoError(t, err)
	assert.Zero(t, notifications)
}

func TestSweeper_RunPass_MapsDMAndGroupMetadata(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner, other := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conversation, group := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{conversation, group} {
		_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, $2, $3, $4)`, id, id == group, id != group, owner)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, id, owner, other)
		require.NoError(t, err)
	}
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, id := range []uuid.UUID{conversation, group} {
		_, err := db.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, expires_at) VALUES ($1, $2, 'ciphertext', 'user', $3, $4)`, id, owner, cutoff.Add(-time.Hour), cutoff.Add(-time.Minute))
		require.NoError(t, err)
	}
	var got []purge.ContextType
	s, err := NewSweeper(SweeperDeps{DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil }, RunExpiryBatch: func(_ context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
		got = append(got, plan.ContextType)
		return purge.Result{}, nil
	}, EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {}, Log: logger.New("test")})
	require.NoError(t, err)
	_, err = s.RunPass(context.Background())
	require.NoError(t, err)
	assert.ElementsMatch(t, []purge.ContextType{purge.ContextDM, purge.ContextGroup}, got)
}

func TestSweeper_RunPass_PropagatesClockAndQueryErrors(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	clockErr := errors.New("clock unavailable")
	s, err := NewSweeper(SweeperDeps{DB: db, Clock: func(context.Context) (time.Time, error) { return time.Time{}, clockErr }, RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) { return purge.Result{}, nil }, EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {}, Log: logger.New("test")})
	require.NoError(t, err)
	_, err = s.RunPass(context.Background())
	assert.ErrorIs(t, err, clockErr)
	closedDB, openErr := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, openErr)
	require.NoError(t, closedDB.Close())
	s, err = NewSweeper(SweeperDeps{DB: closedDB, Clock: func(context.Context) (time.Time, error) { return time.Now(), nil }, RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) { return purge.Result{}, nil }, EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {}, Log: logger.New("test")})
	require.NoError(t, err)
	_, err = s.RunPass(context.Background())
	assert.Error(t, err)
}

func TestSweeper_RunPass_ReturnsCancellationAfterFinalNotification(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner, server, channel := testdb.CreateUser(t, db), uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'canceled notification server', $2)`, server, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'canceled notification channel', 'text')`, channel, server)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	_, err = db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'expired', $3, $4)`, channel, owner, cutoff.Add(-time.Hour), cutoff.Add(-time.Minute))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notified := false
	s, err := NewSweeper(SweeperDeps{
		DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil },
		RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
			return purge.Result{DeletedCount: 1}, nil
		},
		EmitDMPurged: func(context.Context, string, time.Time) {},
		EmitServerPurged: func(context.Context, string, time.Time) {
			notified = true
			cancel()
		},
		Log: logger.New("test"),
	})
	require.NoError(t, err)
	_, err = s.RunPass(ctx)
	assert.True(t, notified, "committed deletion notification is attempted before cancellation is returned")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSweeper_RunWorker_WaitsForIntervalAndStopsOnCancellation(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	clockCalls := make(chan struct{}, 1)
	s, err := NewSweeper(SweeperDeps{DB: db, Clock: func(context.Context) (time.Time, error) {
		clockCalls <- struct{}{}
		return time.Now(), nil
	}, RunExpiryBatch: func(context.Context, purge.ExpiryPlan) (purge.Result, error) {
		return purge.Result{}, nil
	}, EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {}, Log: logger.New("test")})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunWorker(ctx, time.Hour); close(done) }()
	select {
	case <-clockCalls:
		t.Fatal("worker performed an immediate startup pass")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestSweeper_RunWorker_RunsScheduledPass(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	owner := testdb.CreateUser(t, db)
	server, channel := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'scheduled sweeper server', $2)`, server, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'scheduled sweeper channel', 'text')`, channel, server)
	require.NoError(t, err)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	var messageID string
	require.NoError(t, db.QueryRow(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at) VALUES ($1, $2, 'scheduled', $3, $4) RETURNING id`, channel, owner, cutoff.Add(-time.Hour), cutoff.Add(-time.Minute)).Scan(&messageID))

	plans := make(chan purge.ExpiryPlan, 1)
	s, err := NewSweeper(SweeperDeps{
		DB: db, Clock: func(context.Context) (time.Time, error) { return cutoff, nil },
		RunExpiryBatch: func(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
			select {
			case plans <- plan:
			case <-ctx.Done():
				return purge.Result{}, ctx.Err()
			}
			return purge.Result{DeletedCount: 1}, nil
		},
		EmitDMPurged:     func(context.Context, string, time.Time) {},
		EmitServerPurged: func(context.Context, string, time.Time) {},
		Log:              logger.New("test"),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct{})
	go func() {
		s.RunWorker(ctx, 10*time.Millisecond)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker did not stop after cancellation")
		}
	})

	var plan purge.ExpiryPlan
	select {
	case plan = <-plans:
	case <-time.After(time.Second):
		t.Fatal("worker did not run a scheduled expiry pass")
	}
	cancel()
	assert.Equal(t, purge.ContextChannel, plan.ContextType)
	assert.Equal(t, channel.String(), plan.ContextID)
	assert.Equal(t, cutoff, plan.ExpiresBefore)
	assert.Equal(t, []string{messageID}, plan.CandidateIDs)
}
