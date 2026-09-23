package dmblock

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestCoverageGate_ClaimDueLeasesOnlyDueRowsAndCapsBatch(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	if a.String() > b.String() {
		a, b = b, a
	}
	op := uuid.New()
	c, d := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	if c.String() > d.String() {
		c, d = d, c
	}
	_, err := db.Exec(`INSERT INTO dm_block_reconciliations
		(user_a_id, user_b_id, operation_id, remove_a, reconcile_after)
		VALUES ($1, $2, $3, true, clock_timestamp()), ($4, $5, $6, true, clock_timestamp() + interval '1 hour')`,
		a, b, op, c, d, uuid.New())
	require.NoError(t, err)
	claimed, err := New(db, nil).claimDue(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	var attempts int
	var due bool
	require.NoError(t, db.QueryRow(`SELECT attempts, reconcile_after <= clock_timestamp()
		FROM dm_block_reconciliations WHERE operation_id = $1`, op).Scan(&attempts, &due))
	require.Zero(t, attempts)
	require.False(t, due, "claimed row must be leased into the future")
}

func TestCoverageGate_AcknowledgeOrRescheduleDeletesFinishedMarkerOrRequeuesRemaining(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	if a.String() > b.String() {
		a, b = b, a
	}
	op := uuid.New()
	insertMarker := func() {
		_, err := db.Exec(`INSERT INTO dm_block_reconciliations
			(user_a_id, user_b_id, operation_id, remove_a, reconcile_after)
			VALUES ($1, $2, $3, true, clock_timestamp())`, a, b, op)
		require.NoError(t, err)
	}
	reconciler := New(db, nil)

	t.Run("no shared conversation deletes marker", func(t *testing.T) {
		insertMarker()
		require.NoError(t, reconciler.acknowledgeOrReschedule(context.Background(), claimedObligation{a: a, b: b, removeA: true, operationID: op}))
		var count int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE operation_id = $1`, op).Scan(&count))
		require.Zero(t, count)
	})

	t.Run("shared conversation requeues marker", func(t *testing.T) {
		conversation := uuid.New()
		_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, a, b)
		require.NoError(t, err)
		insertMarker()
		require.NoError(t, reconciler.acknowledgeOrReschedule(context.Background(), claimedObligation{a: a, b: b, removeA: true, operationID: op}))
		var due bool
		require.NoError(t, db.QueryRow(`SELECT reconcile_after <= clock_timestamp() FROM dm_block_reconciliations WHERE operation_id = $1`, op).Scan(&due))
		require.True(t, due)
	})
}

func TestCoverageGate_StaleAcknowledgementDoesNotTouchNewGeneration(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	if a.String() > b.String() {
		a, b = b, a
	}
	oldOp, newOp := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b) VALUES ($1, $2, $3, true, false)`, a, b, newOp)
	require.NoError(t, err)
	require.NoError(t, New(db, nil).acknowledgeOrReschedule(context.Background(), claimedObligation{a: a, b: b, removeA: true, operationID: oldOp}))
	var got uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT operation_id FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, a, b).Scan(&got))
	require.Equal(t, newOp, got)
}

func TestCoverageGate_VoiceEjectionRetryEscalatesAfterTenAttempts(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	conversation, user := uuid.New(), dbtest.CreateUser(t, db)
	for _, attempts := range []int{0, 10} {
		t.Run(fmt.Sprintf("%d threshold", attempts), func(t *testing.T) {
			var generation uuid.UUID
			require.NoError(t, db.QueryRow(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id, attempts, reconcile_after) VALUES ($1, $2, $3, clock_timestamp()) RETURNING generation`, conversation, user, attempts).Scan(&generation))
			require.NoError(t, New(db, nil).retryVoiceEjection(context.Background(), claimedVoiceEjection{conversationID: conversation.String(), userID: user, generation: generation}))
			var gotAttempts int
			var failure string
			var delaySeconds float64
			require.NoError(t, db.QueryRow(`SELECT attempts, failure_class,
				EXTRACT(EPOCH FROM (reconcile_after - clock_timestamp()))
				FROM dm_block_voice_ejections WHERE conversation_id = $1 AND user_id = $2`, conversation, user).Scan(&gotAttempts, &failure, &delaySeconds))
			require.Equal(t, attempts, gotAttempts)
			require.Equal(t, "delivery", failure)
			if attempts == 0 {
				require.InDelta(t, 60, delaySeconds, 5, "ordinary failures retry in about one minute")
			} else {
				require.InDelta(t, 3600, delaySeconds, 5, "repeated failures retry in about one hour")
			}
			_, err := db.Exec(`DELETE FROM dm_block_voice_ejections WHERE conversation_id = $1 AND user_id = $2`, conversation, user)
			require.NoError(t, err)
		})
	}
}

func TestCoverageGate_DrainFailedDeliveryRetainsOutbox(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	conversation, user := uuid.New(), dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO dm_block_voice_ejections (conversation_id, user_id, reconcile_after) VALUES ($1, $2, clock_timestamp())`, conversation, user)
	require.NoError(t, err)
	reconciler := New(db, nil)
	reconciler.SetVoiceEject(func(context.Context, string, uuid.UUID) error { return errors.New("media unavailable") })
	require.NoError(t, reconciler.drainDueVoiceEjections(context.Background(), 1))
	var attempts int
	var failure string
	require.NoError(t, db.QueryRow(`SELECT attempts, failure_class FROM dm_block_voice_ejections WHERE conversation_id = $1 AND user_id = $2`, conversation, user).Scan(&attempts, &failure))
	require.Equal(t, 1, attempts)
	require.Equal(t, "delivery", failure)
}
