package voicecommand

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

type outboxDeliveryState struct {
	Token       sql.NullString
	LeaseUntil  sql.NullTime
	Attempts    int
	RetryAt     time.Time
	Failure     sql.NullString
	PublishedAt sql.NullTime
	BrokerSeq   sql.NullInt64
}

func outboxDelivery(t *testing.T, db *sql.DB, sequence int64) outboxDeliveryState {
	t.Helper()
	var state outboxDeliveryState
	require.NoError(t, db.QueryRow(`SELECT lease_token, lease_until, attempts, retry_at,
		failure_class, published_at, broker_sequence FROM voice_command_outbox WHERE sequence = $1`,
		sequence).Scan(&state.Token, &state.LeaseUntil, &state.Attempts, &state.RetryAt,
		&state.Failure, &state.PublishedAt, &state.BrokerSeq))
	return state
}

func outboxClaim(t *testing.T, repository *Repository) ClaimedCommand {
	t.Helper()
	token := uuid.New()
	claim, err := repository.Claim(context.Background(), token, 20*time.Second)
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.Equal(t, token, claim.Token)
	return *claim
}

func outboxDue(t *testing.T, db *sql.DB, sequence int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE voice_command_outbox SET retry_at = clock_timestamp() - interval '1 second'
		WHERE sequence = $1`, sequence)
	require.NoError(t, err)
}

func outboxExpireLease(t *testing.T, db *sql.DB, sequence int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Cancellation may retain a finite lease or clear both fields in Retry.
	// Expire only an actual held lease; an already released claim needs no write.
	_, err := db.ExecContext(ctx, `UPDATE voice_command_outbox
		SET lease_until = clock_timestamp() - interval '1 second'
		WHERE sequence = $1 AND lease_token IS NOT NULL`, sequence)
	require.NoError(t, err)
}

func outboxReceipt(sequence uint64) PublishReceipt {
	return PublishReceipt{Stream: outboxTestStream, Sequence: sequence}
}

func TestVoiceOutboxRepository_OnlyHeadMayOwnLeaseOrReceipt(t *testing.T) {
	db, _ := outboxDatabase(t)
	commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	repository := NewRepository(db)
	require.Same(t, db, repository.DB)
	first := outboxClaim(t, repository)
	assert.Equal(t, commands[0], first.Command)
	assert.Equal(t, 1, first.Attempts)
	state := outboxDelivery(t, db, 1)
	assert.Equal(t, first.Token.String(), state.Token.String)
	assert.True(t, state.LeaseUntil.Valid)
	assert.False(t, state.PublishedAt.Valid)
	assert.False(t, state.BrokerSeq.Valid)
	assert.False(t, outboxDelivery(t, db, 2).Token.Valid)
	blocked, err := repository.Claim(context.Background(), uuid.New(), 20*time.Second)
	require.NoError(t, err)
	assert.Nil(t, blocked, "a leased head cannot be skipped")
	_, err = repository.Published(context.Background(), 1)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, repository.MarkPublished(context.Background(), first, outboxReceipt(1)))
	requireOutboxCursor(t, db, 2, 1)
	published, err := repository.Published(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, commands[0], published)
	assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1))
	state = outboxDelivery(t, db, 1)
	assert.True(t, state.PublishedAt.Valid)
	assert.Equal(t, sql.NullInt64{Int64: 1, Valid: true}, state.BrokerSeq)
	assert.False(t, state.Token.Valid)
	assert.False(t, state.LeaseUntil.Valid)
	assert.False(t, state.Failure.Valid)
	second := outboxClaim(t, repository)
	assert.Equal(t, commands[1], second.Command)
	assert.NotEqual(t, first.Token, second.Token)
	receipt := outboxReceipt(2)
	receipt.Duplicate = true
	require.NoError(t, repository.MarkPublished(context.Background(), second, receipt))
	requireOutboxCursor(t, db, 2, 2)
	_, err = repository.Published(context.Background(), 3)
	require.ErrorIs(t, err, sql.ErrNoRows)
	empty, err := repository.Claim(context.Background(), uuid.New(), 20*time.Second)
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestVoiceOutboxRepository_InvalidClaimsLeaveReadyHeadUntouched(t *testing.T) {
	db, _ := outboxDatabase(t)
	command := outboxEnqueue(t, db, outboxSnapshot())[0]
	repository := NewRepository(db)
	before := outboxDelivery(t, db, 1)
	for _, tc := range []struct {
		name  string
		token uuid.UUID
		lease time.Duration
	}{
		{"missing token", uuid.Nil, claimLease},
		{"zero lease", uuid.New(), 0},
		{"negative lease", uuid.New(), -time.Nanosecond},
		{"below SQL resolution", uuid.New(), time.Microsecond - time.Nanosecond},
		{"beyond lease budget", uuid.New(), claimLease + time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim, err := repository.Claim(context.Background(), tc.token, tc.lease)
			require.ErrorContains(t, err, "invalid voice command claim parameters")
			assert.Nil(t, claim)
			assert.Equal(t, before, outboxDelivery(t, db, 1))
			assert.Equal(t, command, outboxStoredCommand(t, db, 1))
			requireOutboxCursor(t, db, 1, 0)
		})
	}
	claim, err := repository.Claim(context.Background(), uuid.New(), time.Microsecond)
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.Equal(t, command, claim.Command)
	state := outboxDelivery(t, db, 1)
	assert.Equal(t, claim.Token.String(), state.Token.String)
	assert.True(t, state.LeaseUntil.Valid)
	assert.Equal(t, 1, state.Attempts)
	assert.False(t, state.PublishedAt.Valid)
	requireOutboxCursor(t, db, 1, 0)
}

func TestVoiceOutboxRepository_RejectsWrongPubAckWithoutAdvancing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		receipt PublishReceipt
	}{
		{"wrong stream", PublishReceipt{Stream: "UNRELATED_STREAM", Sequence: 1}},
		{"zero sequence", outboxReceipt(0)},
		{"future sequence", outboxReceipt(2)},
		{"sequence beyond BIGINT", outboxReceipt(math.MaxUint64)},
		{"duplicate for a different sequence", PublishReceipt{Stream: outboxTestStream, Sequence: 2, Duplicate: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			command := outboxEnqueue(t, db, outboxSnapshot())[0]
			repository := NewRepository(db)
			claim := outboxClaim(t, repository)
			before := outboxDelivery(t, db, 1)
			require.Error(t, repository.MarkPublished(context.Background(), claim, tc.receipt))
			assert.Equal(t, before, outboxDelivery(t, db, 1))
			assert.Equal(t, command, outboxStoredCommand(t, db, 1))
			requireOutboxCursor(t, db, 1, 0)
			_, err := repository.Published(context.Background(), 1)
			require.ErrorIs(t, err, sql.ErrNoRows)
			require.NoError(t, repository.MarkPublished(context.Background(), claim, outboxReceipt(1)))
		})
	}
}

func TestVoiceOutboxRepository_ConcurrentClaimersCannotSkipHead(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	repository := NewRepository(db)
	start := make(chan struct{})
	type result struct {
		claim *ClaimedCommand
		err   error
	}
	finished := make(chan result, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 2 {
		go func() {
			<-start
			claim, err := repository.Claim(ctx, uuid.New(), 20*time.Second)
			finished <- result{claim, err}
		}()
	}
	close(start)
	owners := 0
	for range 2 {
		got := <-finished
		require.NoError(t, got.err)
		if got.claim != nil {
			owners++
			assert.Equal(t, "1", got.claim.Command.Sequence)
			assert.Equal(t, 1, got.claim.Attempts)
		}
	}
	assert.Equal(t, 1, owners)
	assert.Equal(t, 1, outboxDelivery(t, db, 1).Attempts)
	assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
	assert.False(t, outboxDelivery(t, db, 2).Token.Valid)
	requireOutboxCursor(t, db, 2, 0)
}

func TestVoiceOutboxRepository_ExpiredAndReplacedClaimsCannotWrite(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired without replacement", true: "new token owns expired lease"}[replacement], func(t *testing.T) {
			db, _ := outboxDatabase(t)
			commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
			repository := NewRepository(db)
			stale := outboxClaim(t, repository)
			outboxExpireLease(t, db, 1)
			var current ClaimedCommand
			if replacement {
				current = outboxClaim(t, repository)
				assert.NotEqual(t, stale.Token, current.Token)
				assert.Equal(t, 2, current.Attempts)
			}
			before := outboxDelivery(t, db, 1)
			require.ErrorIs(t, repository.MarkPublished(context.Background(), stale, outboxReceipt(1)), ErrClaimLost)
			require.ErrorIs(t, repository.Retry(context.Background(), stale, "broker_continuity", 0), ErrClaimLost)
			assert.Equal(t, before, outboxDelivery(t, db, 1), "a stale token cannot poison its replacement")
			assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1))
			requireOutboxCursor(t, db, 2, 0)
			if !replacement {
				current = outboxClaim(t, repository)
			}
			require.NoError(t, repository.MarkPublished(context.Background(), current, outboxReceipt(1)))
			second := outboxClaim(t, repository)
			require.NoError(t, repository.MarkPublished(context.Background(), second, outboxReceipt(2)))
			settledFirst, settledSecond := outboxDelivery(t, db, 1), outboxDelivery(t, db, 2)
			require.ErrorIs(t, repository.MarkPublished(context.Background(), stale, outboxReceipt(1)), ErrClaimLost)
			require.ErrorIs(t, repository.Retry(context.Background(), stale, "invalid_command", 0), ErrClaimLost)
			assert.Equal(t, settledFirst, outboxDelivery(t, db, 1))
			assert.Equal(t, settledSecond, outboxDelivery(t, db, 2))
			requireOutboxCursor(t, db, 2, 2)
		})
	}
}

func TestVoiceOutboxRepository_DelayedOrPoisonedHeadBlocksLaterRows(t *testing.T) {
	for _, class := range []string{"broker_unavailable", "invalid_command", "broker_continuity"} {
		t.Run(class, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
			repository := NewRepository(db)
			claim := outboxClaim(t, repository)
			require.NoError(t, repository.Retry(context.Background(), claim, class, 30*time.Second))
			state := outboxDelivery(t, db, 1)
			assert.Equal(t, class, state.Failure.String)
			assert.False(t, state.Token.Valid)
			assert.False(t, state.LeaseUntil.Valid)
			assert.False(t, state.PublishedAt.Valid)
			blocked, err := repository.Claim(context.Background(), uuid.New(), 20*time.Second)
			require.NoError(t, err)
			assert.Nil(t, blocked)
			outboxDue(t, db, 1)
			next, err := repository.Claim(context.Background(), uuid.New(), 20*time.Second)
			require.NoError(t, err)
			if class == "broker_unavailable" {
				require.NotNil(t, next)
				assert.Equal(t, commands[0], next.Command)
				assert.Equal(t, 2, next.Attempts)
				assert.False(t, outboxDelivery(t, db, 1).Failure.Valid)
			} else {
				assert.Nil(t, next, "permanent poison is independent of retry time")
				assert.Equal(t, 1, outboxDelivery(t, db, 1).Attempts)
			}
			assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
			assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1))
			assert.Equal(t, commands[1], outboxStoredCommand(t, db, 2))
			requireOutboxCursor(t, db, 2, 0)
		})
	}
}

func TestVoiceOutboxRepository_ClaimIdentityCannotBeSubstituted(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	repository := NewRepository(db)
	claim := outboxClaim(t, repository)
	before := outboxDelivery(t, db, 1)
	for _, change := range []func(*ClaimedCommand){
		func(c *ClaimedCommand) { c.Command.CommandID = uuid.NewString() },
		func(c *ClaimedCommand) { c.Command.UserID = uuid.NewString() },
		func(c *ClaimedCommand) { c.Command.Sequence = "2" },
		func(c *ClaimedCommand) { c.Token = uuid.New() },
	} {
		altered := claim
		change(&altered)
		require.Error(t, repository.MarkPublished(context.Background(), altered, outboxReceipt(1)))
		require.Error(t, repository.Retry(context.Background(), altered, "invalid_command", 0))
		assert.Equal(t, before, outboxDelivery(t, db, 1))
		requireOutboxCursor(t, db, 2, 0)
	}
	require.NoError(t, repository.MarkPublished(context.Background(), claim, outboxReceipt(1)))
}

func TestVoiceOutboxRepository_AttemptsSaturateAndClassesAreClosed(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot())
	_, err := db.Exec(`UPDATE voice_command_outbox SET attempts = $1 WHERE sequence = 1`, math.MaxInt32)
	require.NoError(t, err)
	repository := NewRepository(db)
	claim := outboxClaim(t, repository)
	assert.Equal(t, math.MaxInt32, claim.Attempts)
	assert.Equal(t, math.MaxInt32, outboxDelivery(t, db, 1).Attempts)
	before := outboxDelivery(t, db, 1)
	require.Error(t, repository.Retry(context.Background(), claim, "raw error including identifiers", time.Second))
	assert.Equal(t, before, outboxDelivery(t, db, 1))
	require.NoError(t, repository.Retry(context.Background(), claim, "receipt_unavailable", time.Second))
	outboxDue(t, db, 1)
	assert.Equal(t, math.MaxInt32, outboxClaim(t, repository).Attempts)
}

func TestVoiceOutboxRepository_MalformedImmutableHeadIsRetainedAndNeverSkipped(t *testing.T) {
	db, generation := outboxDatabase(t)
	// PostgreSQL's UUID type accepts this nonzero version-0 value; the frozen
	// wire contract rejects it at the validated Go boundary. This supplies a
	// corrupt persisted head without changing constraints or adding test hooks.
	_, err := db.Exec(`INSERT INTO voice_command_outbox
		(sequence, command_id, stream_generation, scope, kind, action, channel_id, user_id,
		 session_generation, node_boot_id, socket_id, credential_epoch,
		 authorization_revision, server_muted, server_deafened)
		VALUES (1, $1, $2, 'dm', 'server_moderation', 'mute', $3, $4, $5, $6, $7, '', 9, TRUE, FALSE)`,
		uuid.New(), generation, "26230000-0000-4000-8000-000000000013",
		"26230000-0000-0000-8000-000000000099", outboxExactTarget().SessionGeneration,
		outboxExactTarget().NodeBootID, outboxExactTarget().SocketID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE voice_command_sequence SET last_allocated_sequence = 1 WHERE id = TRUE`)
	require.NoError(t, err)
	outboxEnqueue(t, db, outboxSnapshot())
	repository := NewRepository(db)
	claim, err := repository.Claim(context.Background(), uuid.New(), 20*time.Second)
	if err != nil {
		require.ErrorIs(t, err, ErrInvalidCommand)
	}
	assert.Nil(t, claim)
	assert.Equal(t, "invalid_command", outboxDelivery(t, db, 1).Failure.String)
	assert.False(t, outboxDelivery(t, db, 1).PublishedAt.Valid)
	assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
	assert.Equal(t, 2, outboxCount(t, db))
	requireOutboxCursor(t, db, 2, 0)
	outboxDue(t, db, 1)
	claim, err = repository.Claim(context.Background(), uuid.New(), 20*time.Second)
	require.NoError(t, err)
	assert.Nil(t, claim)
	requireOutboxCursor(t, db, 2, 0)
}

func TestVoiceOutboxRepository_ReceiptCommitAmbiguityUsesPermanentSQLProof(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed with lost acknowledgement", false: "rolled back with lost acknowledgement"}[committed], func(t *testing.T) {
			db, _ := outboxDatabase(t)
			command := outboxEnqueue(t, db, outboxSnapshot())[0]
			claim := outboxClaim(t, NewRepository(db))
			hook, wrapped := stmthook.Open(t)
			hook.ArmCommit(committed, stmthook.ErrInjected)
			err := NewRepository(wrapped).MarkPublished(context.Background(), claim, outboxReceipt(1))
			state := outboxDelivery(t, db, 1)
			assert.Equal(t, committed, state.PublishedAt.Valid)
			assert.Equal(t, committed, state.BrokerSeq.Valid)
			if committed {
				require.NoError(t, err, "a fresh exact permanent receipt confirms the lost commit acknowledgement")
				requireOutboxCursor(t, db, 1, 1)
				proof, proofErr := NewRepository(db).Published(context.Background(), 1)
				require.NoError(t, proofErr)
				assert.Equal(t, command, proof)
			} else {
				require.Error(t, err)
				requireOutboxCursor(t, db, 1, 0)
				_, proofErr := NewRepository(db).Published(context.Background(), 1)
				require.ErrorIs(t, proofErr, sql.ErrNoRows)
				require.NoError(t, NewRepository(db).MarkPublished(context.Background(), claim, outboxReceipt(1)))
			}
		})
	}
}

func TestVoiceOutboxRepository_ClaimReportsQueryAndRollbackFailures(t *testing.T) {
	for _, rollbackFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "checked rollback succeeds", true: "checked rollback fails"}[rollbackFails], func(t *testing.T) {
			db, _ := outboxDatabase(t)
			command := outboxEnqueue(t, db, outboxSnapshot())[0]
			before := outboxDelivery(t, db, 1)
			hook, wrapped := stmthook.Open(t)
			ended := 0
			var between func() error
			if rollbackFails {
				// End only the isolated transaction holding the cursor lock.
				between = stmthook.TerminateIdleBackend(db, "FROM voice_command_sequence", &ended)
			}
			hook.Arm([]string{"FROM voice_command_outbox WHERE sequence"}, between, stmthook.ErrInjected)
			claim, err := NewRepository(wrapped).Claim(context.Background(), uuid.New(), claimLease)
			require.Nil(t, claim)
			require.ErrorIs(t, err, stmthook.ErrInjected, "the original query fault remains observable")
			seen, betweenErr := hook.Report()
			require.Equal(t, 1, seen)
			require.NoError(t, betweenErr)
			if rollbackFails {
				require.Equal(t, 1, ended, "the rollback must run on the one terminated transaction")
				require.ErrorContains(t, err, "rollback outbox transaction")
			} else {
				assert.Zero(t, ended)
				assert.NotContains(t, err.Error(), "rollback outbox transaction")
			}
			assert.Equal(t, before, outboxDelivery(t, db, 1))
			assert.Equal(t, command, outboxStoredCommand(t, db, 1))
			requireOutboxCursor(t, db, 1, 0)
			assert.Equal(t, command, outboxClaim(t, NewRepository(db)).Command, "rollback releases the cursor and head for a successor")
		})
	}
}

func TestVoiceOutboxRepository_CancellationAndUnavailableDBDoNotAcceptWork(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot())
	repository := NewRepository(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	claim, err := repository.Claim(ctx, uuid.New(), 20*time.Second)
	require.Error(t, err)
	assert.Nil(t, claim)
	assert.Zero(t, outboxDelivery(t, db, 1).Attempts)
	_, err = repository.Claim(context.Background(), uuid.Nil, 20*time.Second)
	require.Error(t, err)
	_, err = repository.Claim(context.Background(), uuid.New(), 0)
	require.Error(t, err)
	unavailable := NewRepository(nil)
	_, err = unavailable.Claim(context.Background(), uuid.New(), 20*time.Second)
	require.Error(t, err)
	require.Error(t, unavailable.MarkPublished(context.Background(), ClaimedCommand{}, outboxReceipt(1)))
	require.Error(t, unavailable.Retry(context.Background(), ClaimedCommand{}, "broker_unavailable", time.Second))
	_, err = unavailable.Published(context.Background(), 1)
	require.Error(t, err)
	requireOutboxCursor(t, db, 1, 0)
	assert.False(t, outboxDelivery(t, db, 1).Token.Valid)
}
