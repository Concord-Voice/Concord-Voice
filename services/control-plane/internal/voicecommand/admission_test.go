package voicecommand

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

func admissionCount(t *testing.T, db *sql.DB, actor uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_command_admission WHERE actor_id = $1`, actor).Scan(&count))
	return count
}

func admissionSpecs(count int, user string) []CommandSpec {
	specs := make([]CommandSpec, count)
	for index := range specs {
		specs[index] = capacityMarker()
		specs[index].UserID = user
	}
	return specs
}

func admissionEnqueue(t *testing.T, db *sql.DB, specs []CommandSpec, actor ...uuid.UUID) []VoiceCommand {
	t.Helper()
	tx := outboxBegin(t, db)
	commands, err := EnqueueTx(context.Background(), tx, specs, actor...)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return commands
}

func admissionRefuse(t *testing.T, db *sql.DB, specs []CommandSpec, actor ...uuid.UUID) {
	t.Helper()
	allocated, published := outboxCursor(t, db)
	count := outboxCount(t, db)
	tx := outboxBegin(t, db)
	commands, err := EnqueueTx(context.Background(), tx, specs, actor...)
	require.ErrorIs(t, err, ErrQueueFull)
	assert.Empty(t, commands)
	require.NoError(t, tx.Rollback())
	requireOutboxCursor(t, db, allocated, published)
	assert.Equal(t, count, outboxCount(t, db), "a refused batch is all-or-none")
}

func TestVoiceOutboxAdmission_ExplicitActorOwnsWholeBatchAndIsolatedAllowance(t *testing.T) {
	db, _ := outboxDatabase(t)
	actor := uuid.MustParse("26230001-0000-4000-8000-000000000001")
	otherActor := uuid.MustParse("26230001-0000-4000-8000-000000000002")
	target := "26230001-0000-4000-8000-000000000003"
	commands := admissionEnqueue(t, db, admissionSpecs(1000, target), actor)
	require.Len(t, commands, 1000)
	assert.Equal(t, "1000", commands[999].Sequence)
	assert.Equal(t, 1000, admissionCount(t, db, actor))
	assert.Zero(t, admissionCount(t, db, uuid.MustParse(target)), "explicit authenticated actor overrides target fallback")
	admissionRefuse(t, db, admissionSpecs(1, otherActor.String()), actor)
	accepted := admissionEnqueue(t, db, admissionSpecs(1, target), otherActor)
	require.Len(t, accepted, 1)
	assert.Equal(t, "1001", accepted[0].Sequence)
	assert.Equal(t, 1000, admissionCount(t, db, actor))
	assert.Equal(t, 1, admissionCount(t, db, otherActor))
	requireOutboxCursor(t, db, 1001, 0)
}

func TestVoiceOutboxAdmission_TargetFallbackChargesEachTarget(t *testing.T) {
	db, _ := outboxDatabase(t)
	first := uuid.MustParse("26230001-0000-4000-8000-000000000001")
	second := uuid.MustParse("26230001-0000-4000-8000-000000000002")
	admissionEnqueue(t, db, admissionSpecs(1000, first.String()))
	assert.Equal(t, 1000, admissionCount(t, db, first))
	admissionRefuse(t, db, admissionSpecs(1, first.String()))
	admissionEnqueue(t, db, admissionSpecs(2, second.String()))
	assert.Equal(t, 2, admissionCount(t, db, second))
	admissionEnqueue(t, db, admissionSpecs(1, first.String()), second)
	assert.Equal(t, 3, admissionCount(t, db, second))
	assert.Equal(t, 1000, admissionCount(t, db, first))
}

func TestVoiceOutboxAdmission_BatchOverflowLeavesFinalActorSlotAvailable(t *testing.T) {
	db, _ := outboxDatabase(t)
	actor := uuid.MustParse("26230001-0000-4000-8000-000000000001")
	admissionEnqueue(t, db, admissionSpecs(999, actor.String()), actor)
	admissionRefuse(t, db, admissionSpecs(2, actor.String()), actor)
	assert.Equal(t, 999, admissionCount(t, db, actor))
	last := admissionEnqueue(t, db, admissionSpecs(1, actor.String()), actor)
	assert.Equal(t, "1000", last[0].Sequence)
	assert.Equal(t, 1000, admissionCount(t, db, actor))
}

func TestVoiceOutboxAdmission_InvalidActorCannotAllocate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actors []uuid.UUID
	}{
		{"nil authenticated actor", []uuid.UUID{uuid.Nil}},
		{"multiple authenticated actors", []uuid.UUID{uuid.New(), uuid.New()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			tx := outboxBegin(t, db)
			commands, err := EnqueueTx(context.Background(), tx, admissionSpecs(1, capacityMarker().UserID), tc.actors...)
			require.Error(t, err)
			assert.Empty(t, commands)
			require.NoError(t, tx.Rollback())
			requireOutboxCursor(t, db, 0, 0)
			assert.Zero(t, outboxCount(t, db))
		})
	}
}

func TestVoiceOutboxAdmission_GlobalFinalSlotIsAcceptedThenRefused(t *testing.T) {
	db, generation := outboxDatabase(t)
	capacitySeed(t, db, generation, 4999, 0)
	actor := uuid.MustParse("26230001-0000-4000-8000-000000050001")
	accepted := admissionEnqueue(t, db, admissionSpecs(1, actor.String()), actor)
	assert.Equal(t, "5000", accepted[0].Sequence)
	admissionRefuse(t, db, admissionSpecs(1, actor.String()), actor)
	requireOutboxCursor(t, db, 5000, 0)
	assert.Equal(t, 5000, outboxCount(t, db))
}

func TestVoiceOutboxAdmission_ConcurrentActorFinalSlotHasOneWinnerAndNoGap(t *testing.T) {
	db, _ := outboxDatabase(t)
	actor := uuid.MustParse("26230001-0000-4000-8000-000000000001")
	admissionEnqueue(t, db, admissionSpecs(999, actor.String()), actor)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	type result struct {
		commands []VoiceCommand
		err      error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				done <- result{err: err}
				return
			}
			commands, err := EnqueueTx(ctx, tx, admissionSpecs(1, actor.String()), actor)
			if err != nil {
				rollbackErr := tx.Rollback()
				if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					err = errors.Join(err, rollbackErr)
				}
				done <- result{commands: commands, err: err}
				return
			}
			done <- result{commands: commands, err: tx.Commit()}
		}()
	}
	close(start)
	accepted, refused := 0, 0
	for range 2 {
		got := <-done
		if errors.Is(got.err, ErrQueueFull) {
			refused++
			assert.Empty(t, got.commands)
			continue
		}
		require.NoError(t, got.err)
		require.Len(t, got.commands, 1)
		assert.Equal(t, "1000", got.commands[0].Sequence)
		accepted++
	}
	assert.Equal(t, 1, accepted)
	assert.Equal(t, 1, refused)
	assert.Equal(t, 1000, admissionCount(t, db, actor))
	requireOutboxCursor(t, db, 1000, 0)
	assert.Equal(t, 1000, outboxCount(t, db))
}

func TestVoiceOutboxAdmission_PublicationReceiptFreesQuotaButPubAckAloneDoesNot(t *testing.T) {
	db, _ := outboxDatabase(t)
	actor := uuid.MustParse("26230001-0000-4000-8000-000000000001")
	commands := admissionEnqueue(t, db, admissionSpecs(1000, actor.String()), actor)
	dispatcher, broker, _ := outboxDispatcher(t, db)
	claim := outboxClaim(t, dispatcher.Repository)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := broker.PublishVoiceCommand(ctx, broker.receipt, claim.Command)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), receipt.Sequence)
	assert.Equal(t, 1000, admissionCount(t, db, actor))
	admissionRefuse(t, db, admissionSpecs(1, actor.String()), actor)
	require.NoError(t, dispatcher.Repository.MarkPublished(ctx, claim, receipt))
	assert.Equal(t, 999, admissionCount(t, db, actor))
	accepted := admissionEnqueue(t, db, admissionSpecs(1, actor.String()), actor)
	assert.Equal(t, "1001", accepted[0].Sequence)
	assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1), "quota release retains the permanent immutable command")
	requireOutboxCursor(t, db, 1001, 1)
}

func TestVoiceOutboxAdmission_ExpiredClaimCannotFreeReplacementQuota(t *testing.T) {
	db, _ := outboxDatabase(t)
	actor := uuid.MustParse("26230001-0000-4000-8000-000000000001")
	admissionEnqueue(t, db, admissionSpecs(1000, actor.String()), actor)
	repository := NewRepository(db)
	stale := outboxClaim(t, repository)
	outboxExpireLease(t, db, 1)
	current := outboxClaim(t, repository)
	require.ErrorIs(t, repository.MarkPublished(context.Background(), stale, outboxReceipt(1)), ErrClaimLost)
	assert.Equal(t, 1000, admissionCount(t, db, actor))
	admissionRefuse(t, db, admissionSpecs(1, actor.String()), actor)
	require.NoError(t, repository.MarkPublished(context.Background(), current, outboxReceipt(1)))
	assert.Equal(t, 999, admissionCount(t, db, actor))
}

func TestVoiceOutboxAdmission_ReceiptAndQuotaCommitTogetherThroughAmbiguity(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed reply lost", false: "rolled back reply lost"}[committed], func(t *testing.T) {
			db, _ := outboxDatabase(t)
			actor := uuid.MustParse("26230001-0000-4000-8000-000000000001")
			admissionEnqueue(t, db, admissionSpecs(1, actor.String()), actor)
			claim := outboxClaim(t, NewRepository(db))
			hook, wrapped := stmthook.Open(t)
			hook.ArmCommit(committed, stmthook.ErrInjected)
			err := NewRepository(wrapped).MarkPublished(context.Background(), claim, outboxReceipt(1))
			if committed {
				require.NoError(t, err)
				assert.Zero(t, admissionCount(t, db, actor))
				requireOutboxCursor(t, db, 1, 1)
			} else {
				require.Error(t, err)
				assert.Equal(t, 1, admissionCount(t, db, actor))
				requireOutboxCursor(t, db, 1, 0)
				require.NoError(t, NewRepository(db).MarkPublished(context.Background(), claim, outboxReceipt(1)))
				assert.Zero(t, admissionCount(t, db, actor))
			}
			assert.Equal(t, 1, outboxCount(t, db))
		})
	}
}

func TestVoiceOutboxAdmission_CallerRollbackRestoresQuotaAndRows(t *testing.T) {
	db, _ := outboxDatabase(t)
	actor := uuid.MustParse("26230001-0000-4000-8000-000000000001")
	tx := outboxBegin(t, db)
	commands, err := EnqueueTx(context.Background(), tx, admissionSpecs(1000, actor.String()), actor)
	require.NoError(t, err)
	require.Len(t, commands, 1000)
	require.NoError(t, tx.Rollback())
	assert.Zero(t, admissionCount(t, db, actor))
	requireOutboxCursor(t, db, 0, 0)
	assert.Zero(t, outboxCount(t, db))
	admissionEnqueue(t, db, admissionSpecs(1000, actor.String()), actor)
	assert.Equal(t, 1000, admissionCount(t, db, actor))
}
