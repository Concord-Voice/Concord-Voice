package presencehistory

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	testhelpers "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetentionWorkerConstants(t *testing.T) {
	assert.Equal(t, 24*time.Hour, retentionInterval)
	assert.Equal(t, 15*time.Minute, retentionRetryInterval)
	assert.Equal(t, 100, retentionOwnerBatch)
	assert.Equal(t, 500, retentionRowBatch)
}

func TestRetentionSweepExpiryCutoffAndRetentionChoices(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	worker := newTestRetentionWorker(db)

	var cutoff time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&cutoff))
	cutoff = cutoff.UTC().Truncate(time.Microsecond)

	for _, days := range []int16{7, 30, 90, 365} {
		ownerID := testhelpers.CreateUser(t, db)
		enableHistory(t, db, ownerID, disclosure, days)
		recordedAt := cutoff.Add(-48 * time.Hour)
		endedAt := recordedAt.Add(time.Hour)
		expiredID := insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
			`{"text":"expired"}`, recordedAt, endedAt, recordedAt, cutoff)
		liveID := insertHistoryRow(t, db, ownerID, CategoryGames, 1,
			`{}`, recordedAt, endedAt, recordedAt, cutoff.Add(time.Hour))

		stats, err := worker.Sweep(ctx)
		require.NoError(t, err)
		assert.False(t, workerHistoryIDExists(t, db, expiredID), "exact-cutoff row must be deleted")
		assert.True(t, workerHistoryIDExists(t, db, liveID), "future row must remain")
		assert.Equal(t, days, historyRetentionDays(t, db, ownerID),
			"cleanup must not change the owner's retention choice")
		assert.GreaterOrEqual(t, stats.RowsDeleted, int64(1))
	}
}

func TestRetentionSweepDeletesExpiredOpenIntervalWithoutChangingSettings(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	ownerID := testhelpers.CreateUser(t, db)
	enableHistory(t, db, ownerID, disclosure, 30)
	recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	expiredID := insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
		`{"text":"old open interval"}`, recordedAt, nil, recordedAt, recordedAt.Add(time.Hour))

	stats, err := newTestRetentionWorker(db).Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.RowsDeleted)
	assert.False(t, workerHistoryIDExists(t, db, expiredID))

	var enabled bool
	var days int16
	require.NoError(t, db.QueryRow(`
		SELECT activity_history_enabled, activity_history_retention_days
		FROM user_presence_settings
		WHERE user_id = $1
	`, ownerID).Scan(&enabled, &days))
	assert.True(t, enabled)
	assert.Equal(t, int16(30), days)
}

func TestRetentionSweepBoundsOwnerAndRowBatches(t *testing.T) {
	t.Run("owner discovery", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx := context.Background()

		_, err := db.ExecContext(ctx, `
			WITH owners AS (
				SELECT md5('retention-owner-' || i::TEXT)::UUID AS id, i
				FROM generate_series(1, 101) AS series(i)
			)
			INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
			SELECT id,
			       'retention-owner-' || i::TEXT || '@test.local',
			       'retention_owner_' || i::TEXT,
			       'x', TRUE, TRUE
			FROM owners
		`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `
			WITH owners AS (
				SELECT md5('retention-owner-' || i::TEXT)::UUID AS id
				FROM generate_series(1, 101) AS series(i)
			)
			INSERT INTO user_presence_settings (user_id)
			SELECT id FROM owners
		`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `
			WITH owners AS (
				SELECT md5('retention-owner-' || i::TEXT)::UUID AS owner_id, i
				FROM generate_series(1, 101) AS series(i)
			)
			INSERT INTO presence_history (
				id, sender_id, category, payload_version, payload,
				started_at, ended_at, recorded_at, expires_at
			)
			SELECT md5('retention-owner-row-' || i::TEXT)::UUID,
			       owner_id, 'custom_text', 1, '{"text":"expired"}'::JSONB,
			       clock_timestamp() - INTERVAL '48 hours',
			       clock_timestamp() - INTERVAL '47 hours',
			       clock_timestamp() - INTERVAL '48 hours',
			       clock_timestamp() - INTERVAL '24 hours'
			FROM owners
		`)
		require.NoError(t, err)

		stats, err := newTestRetentionWorker(db).Sweep(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.OwnerBatchCount,
			"101 owners must be discovered as bounded 100 + 1 pages")
		assert.Equal(t, 101, stats.BatchCount)
		assert.Equal(t, int64(101), stats.RowsDeleted)
		assert.Zero(t, totalHistoryRowCount(t, db))
	})

	t.Run("row deletion", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx := context.Background()
		ownerID := testhelpers.CreateUser(t, db)
		_, err := db.ExecContext(ctx, `INSERT INTO user_presence_settings (user_id) VALUES ($1)`, ownerID)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `
			INSERT INTO presence_history (
				id, sender_id, category, payload_version, payload,
				started_at, ended_at, recorded_at, expires_at
			)
			SELECT md5('retention-row-' || i::TEXT)::UUID,
			       $1, 'custom_text', 1, '{"text":"expired"}'::JSONB,
			       clock_timestamp() - INTERVAL '72 hours' + i * INTERVAL '1 microsecond',
			       clock_timestamp() - INTERVAL '71 hours' + i * INTERVAL '1 microsecond',
			       clock_timestamp() - INTERVAL '72 hours' + i * INTERVAL '1 microsecond',
			       clock_timestamp() - INTERVAL '48 hours' + i * INTERVAL '1 microsecond'
			FROM generate_series(1, 501) AS series(i)
		`, ownerID)
		require.NoError(t, err)

		stats, err := newTestRetentionWorker(db).Sweep(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.OwnerBatchCount)
		assert.Equal(t, 2, stats.BatchCount, "501 rows must commit as bounded 500 + 1 transactions")
		assert.Equal(t, int64(501), stats.RowsDeleted)
		assert.Zero(t, totalHistoryRowCount(t, db))
	})
}

func TestRetentionSweepDiscoversOwnersInUUIDOrder(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	owners := []uuid.UUID{
		uuid.MustParse("f0000000-0000-4000-8000-000000000001"),
		uuid.MustParse("10000000-0000-4000-8000-000000000001"),
		uuid.MustParse("80000000-0000-4000-8000-000000000001"),
	}
	for index, ownerID := range owners {
		insertWorkerOwner(t, db, ownerID, fmt.Sprintf("ordered_%d", index))
		recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
		endedAt := recordedAt.Add(time.Hour)
		insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
			`{"text":"expired"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(24*time.Hour))
	}

	var observed []uuid.UUID
	worker := newTestRetentionWorker(db)
	worker.hooks.afterOwnerLocks = func(ownerID uuid.UUID) {
		observed = append(observed, ownerID)
	}
	_, err := worker.Sweep(ctx)
	require.NoError(t, err)

	want := append([]uuid.UUID(nil), owners...)
	sort.Slice(want, func(i, j int) bool { return strings.Compare(want[i].String(), want[j].String()) < 0 })
	assert.Equal(t, want, observed)
}

func TestRetentionSweepLocksAndDeletesExactRowsInSameTransaction(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ownerID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, ownerID)
	require.NoError(t, err)
	recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	endedAt := recordedAt.Add(time.Hour)
	expiredID := insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
		`{"text":"expired"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(time.Hour))
	liveID := insertHistoryRow(t, db, ownerID, CategoryGames, 1,
		`{}`, recordedAt, endedAt, recordedAt, time.Now().UTC().Add(time.Hour))

	rowsLocked := make(chan struct{})
	releaseDelete := make(chan struct{})
	worker := newTestRetentionWorker(db)
	worker.hooks.afterRowsLocked = func(_ uuid.UUID, ids []uuid.UUID) {
		assert.Equal(t, []uuid.UUID{expiredID}, ids, "only the exact expired ID may be selected")
		close(rowsLocked)
		<-releaseDelete
	}

	result := make(chan error, 1)
	go func() {
		_, sweepErr := worker.Sweep(context.Background())
		result <- sweepErr
	}()
	receiveSignal(t, rowsLocked, "worker did not reach the post-lock seam")

	_, lockErr := db.Exec(`SELECT id FROM presence_history WHERE id = $1 FOR UPDATE NOWAIT`, expiredID)
	var pqErr *pq.Error
	require.ErrorAs(t, lockErr, &pqErr)
	assert.Equal(t, "55P03", string(pqErr.Code),
		"the selected row must remain locked until its exact-ID delete commits")

	close(releaseDelete)
	require.NoError(t, receiveError(t, result, "worker did not finish after delete release"))
	assert.False(t, workerHistoryIDExists(t, db, expiredID))
	assert.True(t, workerHistoryIDExists(t, db, liveID), "same-owner live row must not be deleted")
}

func TestRetentionSweepSkipsLockedRowsAndRecoversOnNextPass(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ownerID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, ownerID)
	require.NoError(t, err)
	recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	endedAt := recordedAt.Add(time.Hour)
	expiredID := insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
		`{"text":"locked"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(time.Hour))

	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `SELECT id FROM presence_history WHERE id = $1 FOR UPDATE`, expiredID)
	require.NoError(t, err)

	stats, err := newTestRetentionWorker(db).Sweep(ctx)
	require.NoError(t, err, "FOR UPDATE SKIP LOCKED must avoid blocking on a claimed row")
	assert.Zero(t, stats.RowsDeleted)
	assert.True(t, stats.ExpiredRowsRemain,
		"a successful-but-contended sweep must request the capped retry cadence")
	assert.True(t, workerHistoryIDExists(t, db, expiredID))
	require.NoError(t, blocker.Rollback())

	stats, err = newTestRetentionWorker(db).Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.RowsDeleted)
	assert.False(t, stats.ExpiredRowsRemain)
	assert.False(t, workerHistoryIDExists(t, db, expiredID))
}

func TestRetentionSweepCancellation(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats, err := newTestRetentionWorker(db).Sweep(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, stats.RowsDeleted)
}

func TestRetentionSweepEmptyAndInvariantFailures(t *testing.T) {
	t.Run("empty database", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()

		stats, err := newTestRetentionWorker(db).Sweep(context.Background())
		require.NoError(t, err)
		assert.Zero(t, stats.RowsDeleted)
		assert.Zero(t, stats.OldestExpiredAge)
	})

	t.Run("missing settings row", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ownerID := testhelpers.CreateUser(t, db)
		recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
		endedAt := recordedAt.Add(time.Hour)
		rowID := insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
			`{"text":"invariant"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(time.Hour))

		_, err := newTestRetentionWorker(db).Sweep(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "lock activity cleanup settings")
		assert.True(t, workerHistoryIDExists(t, db, rowID),
			"an invariant failure must roll back without deleting data")
	})

	t.Run("owner deleted after discovery", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ownerID := testhelpers.CreateUser(t, db)
		_, err := db.Exec(`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, ownerID)
		require.NoError(t, err)
		recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
		endedAt := recordedAt.Add(time.Hour)
		insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
			`{"text":"deleted owner"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(time.Hour))

		ownerTxBegan := make(chan struct{})
		releaseOwnerTx := make(chan struct{})
		var once sync.Once
		worker := newTestRetentionWorker(db)
		worker.hooks.afterOwnerBegin = func(candidateID uuid.UUID) {
			if candidateID == ownerID {
				once.Do(func() {
					close(ownerTxBegan)
					<-releaseOwnerTx
				})
			}
		}
		done := make(chan error, 1)
		go func() {
			_, sweepErr := worker.Sweep(context.Background())
			done <- sweepErr
		}()
		receiveSignal(t, ownerTxBegan, "worker did not begin owner transaction")
		_, err = db.Exec(`DELETE FROM users WHERE id = $1`, ownerID)
		require.NoError(t, err)
		close(releaseOwnerTx)

		require.NoError(t, receiveError(t, done, "worker did not tolerate concurrent account deletion"))
		assert.Zero(t, totalHistoryRowCount(t, db))
	})
}

func TestRetentionSweepCancellationAfterRowsLockedRollsBack(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ownerID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, ownerID)
	require.NoError(t, err)
	recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	endedAt := recordedAt.Add(time.Hour)
	rowID := insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
		`{"text":"cancel before delete"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	worker := newTestRetentionWorker(db)
	worker.hooks.afterRowsLocked = func(uuid.UUID, []uuid.UUID) { cancel() }
	_, err = worker.Sweep(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, workerHistoryIDExists(t, db, rowID),
		"cancellation must roll back the claimed batch")
}

func TestRetentionWorkerLockOrderWithWriterAndPatch(t *testing.T) {
	t.Run("worker before status writer", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
		ownerID := testhelpers.CreateUser(t, db)
		enableHistory(t, db, ownerID, disclosure, 30)
		recordedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
		endedAt := recordedAt.Add(time.Hour)
		insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
			`{"text":"expired"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(time.Hour))

		workerLocked := make(chan struct{})
		releaseWorker := make(chan struct{})
		var once sync.Once
		worker := newTestRetentionWorker(db)
		worker.hooks.afterOwnerLocks = func(lockedID uuid.UUID) {
			if lockedID != ownerID {
				return
			}
			once.Do(func() {
				close(workerLocked)
				<-releaseWorker
			})
		}

		workerDone := make(chan error, 1)
		go func() {
			_, sweepErr := worker.Sweep(ctx)
			workerDone <- sweepErr
		}()
		receiveSignal(t, workerLocked, "worker did not acquire user and settings locks")

		writerConn, writerTx, writerPID := beginIdentifiedTx(ctx, t, db)
		defer func() { _ = writerConn.Close() }()
		writerDone := make(chan error, 1)
		go func() {
			repository := NewRepository(db, disclosure)
			writeErr := repository.RecordCustomTextTransition(ctx, writerTx, ownerID,
				CustomTextState{}, CustomTextState{Text: "fresh"})
			if writeErr == nil {
				writeErr = writerTx.Commit()
			} else {
				_ = writerTx.Rollback()
			}
			writerDone <- writeErr
		}()
		waitForBackendLock(t, db, writerPID)

		close(releaseWorker)
		require.NoError(t, receiveError(t, workerDone, "worker deadlocked with status writer"))
		require.NoError(t, receiveError(t, writerDone, "status writer deadlocked behind worker"))
		assert.Equal(t, 1, historyRowCount(t, db, ownerID),
			"expired row is deleted before the serialized writer opens the fresh interval")
		assert.Equal(t, 1, openHistoryRowCount(t, db, ownerID))
	})

	t.Run("retention patch before worker", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ownerID := testhelpers.CreateUser(t, db)
		_, err := db.Exec(`
			INSERT INTO user_presence_settings (user_id, activity_history_retention_days)
			VALUES ($1, 7)
		`, ownerID)
		require.NoError(t, err)
		recordedAt := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Microsecond)
		endedAt := recordedAt.Add(time.Hour)
		rowID := insertHistoryRow(t, db, ownerID, CategoryCustomText, 1,
			`{"text":"patch-owned"}`, recordedAt, endedAt, recordedAt, recordedAt.Add(7*24*time.Hour))

		patchTx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = patchTx.Rollback() }()
		var lockedID uuid.UUID
		require.NoError(t, patchTx.QueryRowContext(ctx,
			`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, ownerID).Scan(&lockedID))
		require.NoError(t, patchTx.QueryRowContext(ctx,
			`SELECT user_id FROM user_presence_settings WHERE user_id = $1 FOR UPDATE`, ownerID).Scan(&lockedID))

		workerBegan := make(chan struct{})
		var once sync.Once
		worker := newTestRetentionWorker(db)
		worker.hooks.afterOwnerBegin = func(candidateID uuid.UUID) {
			if candidateID == ownerID {
				once.Do(func() { close(workerBegan) })
			}
		}
		workerDone := make(chan error, 1)
		go func() {
			_, sweepErr := worker.Sweep(ctx)
			workerDone <- sweepErr
		}()
		receiveSignal(t, workerBegan, "worker did not begin the owner transaction")

		_, err = patchTx.ExecContext(ctx, `
			UPDATE user_presence_settings
			SET activity_history_retention_days = 30
			WHERE user_id = $1
		`, ownerID)
		require.NoError(t, err)
		_, err = patchTx.ExecContext(ctx, `
			UPDATE presence_history
			SET expires_at = recorded_at + INTERVAL '30 days'
			WHERE sender_id = $1
		`, ownerID)
		require.NoError(t, err,
			"worker must wait on the earlier user lock rather than locking history first")
		require.NoError(t, patchTx.Commit())

		require.NoError(t, receiveError(t, workerDone, "worker deadlocked with retention patch"))
		assert.True(t, workerHistoryIDExists(t, db, rowID),
			"worker must observe the serialized PATCH's future expiry")
		assert.Equal(t, int16(30), historyRetentionDays(t, db, ownerID))
	})
}

func TestRetentionWorkerRunStartupCadenceRetryAlertRecoveryAndCancellation(t *testing.T) {
	var output bytes.Buffer
	waits := make(chan time.Duration, 5)
	releases := make(chan struct{}, 4)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	worker := &RetentionWorker{
		log: logger.NewWithWriter(&output),
		sweep: func(context.Context) (SweepStats, error) {
			switch calls.Add(1) {
			case 1:
				return SweepStats{Duration: time.Second, BatchCount: 1, RowsDeleted: 2}, nil
			case 2:
				return SweepStats{Duration: 2 * time.Second},
					errors.New("database detail with 00000000-0000-0000-0000-000000000123")
			case 3:
				return SweepStats{
					Duration:          250 * time.Millisecond,
					ExpiredRowsRemain: true,
				}, nil
			default:
				return SweepStats{
					Duration:         3 * time.Second,
					OwnerBatchCount:  1,
					BatchCount:       2,
					RowsDeleted:      3,
					OldestExpiredAge: 25 * time.Hour,
				}, nil
			}
		},
		wait: func(waitCtx context.Context, delay time.Duration) bool {
			select {
			case waits <- delay:
			case <-waitCtx.Done():
				return false
			}
			select {
			case <-releases:
				return true
			case <-waitCtx.Done():
				return false
			}
		},
	}
	go func() {
		worker.Run(ctx)
		close(done)
	}()

	assert.Equal(t, retentionInterval, receiveDuration(t, waits, "startup success did not schedule daily cadence"))
	releases <- struct{}{}
	assert.Equal(t, retentionRetryInterval, receiveDuration(t, waits, "failure did not schedule capped retry"))
	releases <- struct{}{}
	assert.Equal(t, retentionRetryInterval,
		receiveDuration(t, waits, "successful contention did not schedule capped retry"))
	releases <- struct{}{}
	assert.Equal(t, retentionInterval, receiveDuration(t, waits, "recovery did not restore daily cadence"))
	assert.Equal(t, int32(4), calls.Load(), "Run must execute at startup and recover after retries")

	cancel()
	receiveSignal(t, done, "Run did not stop after cancellation")
	logs := output.String()
	assert.Contains(t, logs, "operation=activity_cleanup")
	assert.Contains(t, logs, "error_class=database")
	assert.Contains(t, logs, "oldest_expired_age=25h0m0s")
	assert.Contains(t, logs, "expired_rows_remain=true")
	assert.Contains(t, logs, "level=WARN", "an oldest-expired age over 24 hours must alert")
	assert.NotContains(t, logs, "00000000-0000-0000-0000-000000000123",
		"raw database details and identifiers must not be logged")
}

func TestRetentionWorkerRunPreCanceledContextSkipsStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	worker := &RetentionWorker{
		log: logger.NewWithWriter(&bytes.Buffer{}),
		sweep: func(context.Context) (SweepStats, error) {
			calls.Add(1)
			return SweepStats{}, nil
		},
	}

	worker.Run(ctx)
	assert.Zero(t, calls.Load())
}

func TestRetentionWorkerRunDefaultsAndErrorClasses(t *testing.T) {
	t.Run("default wait observes cancellation after startup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int32
		worker := &RetentionWorker{
			log: logger.NewWithWriter(&bytes.Buffer{}),
			sweep: func(context.Context) (SweepStats, error) {
				calls.Add(1)
				cancel()
				return SweepStats{}, nil
			},
		}
		worker.Run(ctx)
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("default sweep reports a classified database failure", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		cleanup()
		var output bytes.Buffer
		worker := &RetentionWorker{
			db:  db,
			log: logger.NewWithWriter(&output),
			wait: func(_ context.Context, delay time.Duration) bool {
				assert.Equal(t, retentionRetryInterval, delay)
				return false
			},
		}
		worker.Run(context.Background())
		assert.Contains(t, output.String(), "error_class=database")
	})

	assert.Equal(t, "canceled", classifyCleanupError(context.Canceled))
	assert.Equal(t, "deadline", classifyCleanupError(context.DeadlineExceeded))
	assert.Equal(t, "database", classifyCleanupError(errors.New("opaque")))
	assert.True(t, waitForRetention(context.Background(), 0))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, waitForRetention(canceled, time.Hour))
}

func newTestRetentionWorker(db *sql.DB) *RetentionWorker {
	return NewRetentionWorker(db, logger.NewWithWriter(&bytes.Buffer{}))
}

func workerHistoryIDExists(t *testing.T, db *sql.DB, id uuid.UUID) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM presence_history WHERE id = $1)`, id).Scan(&exists))
	return exists
}

func historyRetentionDays(t *testing.T, db *sql.DB, ownerID uuid.UUID) int16 {
	t.Helper()
	var days int16
	require.NoError(t, db.QueryRow(`
		SELECT activity_history_retention_days
		FROM user_presence_settings
		WHERE user_id = $1
	`, ownerID).Scan(&days))
	return days
}

func totalHistoryRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM presence_history`).Scan(&count))
	return count
}

func insertWorkerOwner(t *testing.T, db *sql.DB, ownerID uuid.UUID, name string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, 'x', TRUE, TRUE)
	`, ownerID, name+"@test.local", name)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, ownerID)
	require.NoError(t, err)
}

func beginIdentifiedTx(ctx context.Context, t *testing.T, db *sql.DB) (*sql.Conn, *sql.Tx, int) {
	t.Helper()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	var pid int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return conn, tx, pid
}

func waitForBackendLock(t *testing.T, db *sql.DB, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`
			SELECT COALESCE(wait_event_type = 'Lock', FALSE)
			FROM pg_stat_activity
			WHERE pid = $1
		`, pid).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 10*time.Millisecond, "database backend did not wait on the canonical lock")
}

func receiveSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func receiveError(t *testing.T, result <-chan error, message string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal(message)
		return nil
	}
}

func receiveDuration(t *testing.T, waits <-chan time.Duration, message string) time.Duration {
	t.Helper()
	select {
	case delay := <-waits:
		return delay
	case <-time.After(3 * time.Second):
		t.Fatal(message)
		return 0
	}
}
