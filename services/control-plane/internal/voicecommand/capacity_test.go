package voicecommand

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const capacityMessageLimit uint64 = 50000

var errCapacityMessageLimit = errors.New("test broker reached the frozen 50000-message capacity")

// The fake substitutes only broker I/O. Allocation, publication receipts,
// retirement ownership and quota decisions remain real PostgreSQL operations.
// The additional methods deliberately use existing types so these regressions
// compile against the publisher before retirement is implemented.
type capacityBroker struct {
	db           *sql.DB
	receipt      TransportReceipt
	mu           sync.Mutex
	first        uint64
	latest       *BrokerCommand
	ack          uint64
	born         time.Time
	retired      []uint64
	appends      int
	ackFault     error
	beforeRetire func(context.Context) error
	afterRetire  func(context.Context) error
}

var _ CommandBroker = (*capacityBroker)(nil)

func (b *capacityBroker) network(ctx context.Context, receipt TransportReceipt) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) > 10*time.Second {
		return errors.New("capacity broker received an unbounded network context")
	}
	if receipt.Generation != b.receipt.Generation || !sameTransportIdentity(receipt.Identity, b.receipt.Identity) {
		return ErrBrokerContinuity
	}
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	var allocated, published int64
	if err := tx.QueryRowContext(ctx, `SELECT last_allocated_sequence, last_published_sequence
		FROM voice_command_sequence WHERE id = TRUE FOR UPDATE NOWAIT`).Scan(&allocated, &published); err != nil {
		return fmt.Errorf("network observed a locked command cursor: %w", err)
	}
	var generation uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM voice_command_transport
		WHERE id = TRUE FOR UPDATE NOWAIT`).Scan(&generation); err != nil {
		return fmt.Errorf("network observed a locked transport receipt: %w", err)
	}
	// First, retained predecessor and pending head are sufficient to catch a
	// transaction spanning network I/O without locking 50,000 history rows.
	rows, err := tx.QueryContext(ctx, `SELECT sequence FROM voice_command_outbox
		WHERE sequence IN (1, $1, $2, $3) ORDER BY sequence FOR UPDATE NOWAIT`,
		published, published+1, allocated)
	if err != nil {
		return fmt.Errorf("network observed a locked outbox command: %w", err)
	}
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	// The checkpoint/claim lock is not enough: retirement proof and temporary
	// admission accounting must also be released before broker I/O begins.
	for _, check := range []struct {
		query string
		args  []any
	}{
		{query: `SELECT intent_id FROM voice_command_retirement ORDER BY from_sequence FOR UPDATE NOWAIT`},
		{query: `SELECT sequence FROM voice_command_admission WHERE sequence IN (1, $1, $2) ORDER BY sequence FOR UPDATE NOWAIT`,
			args: []any{published + 1, allocated}},
	} {
		locked, err := tx.QueryContext(ctx, check.query, check.args...)
		if err != nil {
			return fmt.Errorf("network observed locked retirement or admission evidence: %w", err)
		}
		for locked.Next() {
			var value any
			if err := locked.Scan(&value); err != nil {
				return errors.Join(err, locked.Close())
			}
		}
		if err := errors.Join(locked.Err(), locked.Close()); err != nil {
			return err
		}
	}
	return nil
}

func (b *capacityBroker) Verify(ctx context.Context, receipt TransportReceipt) error {
	return b.network(ctx, receipt)
}

func (b *capacityBroker) LastVoiceCommand(ctx context.Context, receipt TransportReceipt) (*BrokerCommand, error) {
	return b.LastVoiceCommandAfter(ctx, receipt, 0)
}

func (b *capacityBroker) LastVoiceCommandAfter(ctx context.Context, receipt TransportReceipt, floor uint64) (*BrokerCommand, error) {
	if err := b.network(ctx, receipt); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.latest == nil || b.first != floor+1 || b.latest.Sequence < b.first {
		return nil, ErrBrokerContinuity
	}
	return &BrokerCommand{Sequence: b.latest.Sequence, Data: append([]byte(nil), b.latest.Data...)}, nil
}

func (b *capacityBroker) PublishVoiceCommand(ctx context.Context, receipt TransportReceipt, command VoiceCommand) (PublishReceipt, error) {
	return b.PublishVoiceCommandAfter(ctx, receipt, command, 0)
}

func (b *capacityBroker) PublishVoiceCommandAfter(ctx context.Context, receipt TransportReceipt, command VoiceCommand, floor uint64) (PublishReceipt, error) {
	if err := b.network(ctx, receipt); err != nil {
		return PublishReceipt{}, err
	}
	sequence, err := strconv.ParseUint(command.Sequence, 10, 64)
	if err != nil {
		return PublishReceipt{}, err
	}
	data, err := json.Marshal(command)
	if err != nil {
		return PublishReceipt{}, err
	}
	decoded, err := DecodeVoiceCommand(data, receipt.Generation)
	if err != nil || !sameVoiceCommand(decoded, command) {
		return PublishReceipt{}, errors.Join(ErrBrokerContinuity, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.latest == nil || b.first != floor+1 || sequence != b.latest.Sequence+1 {
		return PublishReceipt{}, ErrBrokerContinuity
	}
	if b.latest.Sequence-b.first+1 >= capacityMessageLimit {
		return PublishReceipt{}, errCapacityMessageLimit
	}
	b.latest = &BrokerCommand{Sequence: sequence, Data: data}
	b.appends++
	return PublishReceipt{Stream: StreamName, Sequence: sequence}, nil
}

func (b *capacityBroker) VoiceCommandAckFloor(ctx context.Context, receipt TransportReceipt) (time.Time, uint64, error) {
	if err := b.network(ctx, receipt); err != nil {
		return time.Time{}, 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.born, b.ack, b.ackFault
}

func (b *capacityBroker) RetireVoiceCommandPrefix(ctx context.Context, receipt TransportReceipt, from, through uint64) error {
	if err := b.network(ctx, receipt); err != nil {
		return err
	}
	// The destructive boundary must already exist in durable SQL. A consumer
	// recreated after preparation may have ack zero; its new cursor cannot erase
	// the historical proof that authorized this exact pending interval.
	var preparedFrom, preparedThrough, preparedAck uint64
	if err := b.db.QueryRowContext(ctx, `SELECT r.from_sequence, r.through_sequence, r.acknowledged_sequence
		FROM voice_command_retirement r JOIN voice_command_sequence s ON s.pending_retirement_id = r.intent_id
		WHERE s.id = TRUE`).Scan(&preparedFrom, &preparedThrough, &preparedAck); err != nil {
		return fmt.Errorf("prefix removal has no durable prepared proof: %w", err)
	}
	if from != preparedFrom || through != preparedThrough || through > preparedAck {
		return ErrBrokerContinuity
	}
	b.mu.Lock()
	before, after := b.beforeRetire, b.afterRetire
	b.beforeRetire, b.afterRetire = nil, nil
	b.mu.Unlock()
	if before != nil {
		if err := before(ctx); err != nil {
			return err
		}
	}
	b.mu.Lock()
	if b.latest == nil || through <= from || through >= b.latest.Sequence {
		b.mu.Unlock()
		return ErrBrokerContinuity
	}
	if b.first == through+1 {
		b.mu.Unlock()
		return nil // A lost broker reply may resume the exact prepared interval.
	}
	if b.first != from+1 {
		b.mu.Unlock()
		return ErrBrokerContinuity
	}
	b.first = through + 1
	b.retired = append(b.retired, through)
	b.mu.Unlock()
	if after != nil {
		return after(ctx)
	}
	return nil
}

func capacityMarker() CommandSpec {
	return CommandSpec{
		Scope: "dm", Kind: "disconnect", Action: "disconnect", Reason: "membership_removed",
		ChannelID: outboxString("26230000-0000-4000-8000-000000000013"),
		UserID:    "26230000-0000-4000-8000-000000050001",
	}
}

// Seed a bounded history in one statement, with a distinct deterministic target
// per row so global admission cases do not also exhaust one actor's allowance.
func capacitySeed(t *testing.T, db *sql.DB, generation uuid.UUID, allocated, published int64) {
	t.Helper()
	tx := outboxBegin(t, db)
	_, err := tx.Exec(`INSERT INTO voice_command_outbox
		(sequence, command_id, stream_generation, envelope_version, scope, kind,
		action, channel_id, user_id, reason, published_at, broker_sequence)
		SELECT n, ('26230000-0000-4000-8000-' || lpad(n::text, 12, '0'))::uuid,
		$1, 1, 'dm', 'disconnect', 'disconnect',
		'26230000-0000-4000-8000-000000000013'::uuid,
		('26230001-0000-4000-8000-' || lpad(n::text, 12, '0'))::uuid,
		'membership_removed', CASE WHEN n <= $3 THEN clock_timestamp() ELSE NULL END,
		CASE WHEN n <= $3 THEN n ELSE NULL END
		FROM generate_series(1::bigint, $2::bigint) AS n`, generation, allocated, published)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO voice_command_admission (sequence, actor_id)
		SELECT sequence, user_id FROM voice_command_outbox WHERE published_at IS NULL`)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE voice_command_sequence SET last_allocated_sequence = $1,
		last_published_sequence = $2 WHERE id = TRUE`, allocated, published)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	requireOutboxCursor(t, db, allocated, published)
	require.Equal(t, int(allocated), outboxCount(t, db))
}

func capacityDispatcher(t *testing.T, db *sql.DB, acknowledged uint64) (*Dispatcher, *capacityBroker) {
	t.Helper()
	dispatcher, original, _ := outboxDispatcher(t, db)
	broker := &capacityBroker{
		db: db, receipt: original.receipt, first: 1, ack: acknowledged,
		born:   time.Date(2026, 10, 14, 0, 0, 0, 123456789, time.UTC),
		latest: outboxBrokerMessage(t, outboxStoredCommand(t, db, int64(capacityMessageLimit))),
	}
	dispatcher.Broker = broker
	return dispatcher, broker
}

func TestVoiceOutboxCapacity_AcknowledgedPrefixFreesCapacity(t *testing.T) {
	db, generation := outboxDatabase(t)
	capacitySeed(t, db, generation, 50000, 50000)
	predecessor := outboxStoredCommand(t, db, 50000)
	commands := outboxEnqueue(t, db, capacityMarker())
	require.Equal(t, "50001", commands[0].Sequence)
	dispatcher, broker := capacityDispatcher(t, db, capacityMessageLimit)
	beforeReceipt := broker.receipt
	for attempts := 0; attempts < 3; attempts++ {
		_, err := dispatcher.Step(context.Background())
		require.NoError(t, err, "acknowledged broker history must not permanently block the next SQL head")
		_, published := outboxCursor(t, db)
		if published == 50001 {
			break
		}
	}
	requireOutboxCursor(t, db, 50001, 50001)
	assert.Equal(t, 50001, outboxCount(t, db), "retirement keeps all permanent typed SQL evidence")
	assert.Equal(t, predecessor, outboxStoredCommand(t, db, 50000), "the next append retains its exact SQL predecessor")
	published, err := dispatcher.Repository.Published(context.Background(), 50001)
	require.NoError(t, err)
	assert.Equal(t, commands[0], published)
	afterReceipt, err := LoadReceipt(context.Background(), db)
	require.NoError(t, err)
	assert.Equal(t, beforeReceipt, afterReceipt, "retirement does not replace the generation or resource births")
	broker.mu.Lock()
	defer broker.mu.Unlock()
	assert.Equal(t, []uint64{49999}, broker.retired, "the published predecessor cannot be retired")
	assert.Equal(t, uint64(50000), broker.first)
	require.NotNil(t, broker.latest)
	assert.Equal(t, uint64(50001), broker.latest.Sequence)
	assert.Equal(t, 1, broker.appends)
}

func TestVoiceOutboxCapacity_GlobalPendingBoundRejectsNextEnqueue(t *testing.T) {
	db, generation := outboxDatabase(t)
	capacitySeed(t, db, generation, 5000, 0)
	tx := outboxBegin(t, db)
	commands, err := EnqueueTx(context.Background(), tx, []CommandSpec{capacityMarker()})
	assert.Error(t, err, "a full unpublished queue must refuse admission before its caller commits")
	assert.Empty(t, commands, "refusal must not return a partially admitted command")
	require.NoError(t, tx.Rollback())
	requireOutboxCursor(t, db, 5000, 0)
	assert.Equal(t, 5000, outboxCount(t, db), "a refused request consumes no durable row or sequence")
}

func TestVoiceOutboxCapacity_UnacknowledgedHistoryCannotBeRetired(t *testing.T) {
	db, generation := outboxDatabase(t)
	capacitySeed(t, db, generation, 50000, 50000)
	outboxEnqueue(t, db, capacityMarker())
	dispatcher, broker := capacityDispatcher(t, db, 0)
	_, err := dispatcher.Step(context.Background())
	require.ErrorIs(t, err, errCapacityMessageLimit)
	requireOutboxCursor(t, db, 50001, 50000)
	assert.Equal(t, 50001, outboxCount(t, db))
	broker.mu.Lock()
	defer broker.mu.Unlock()
	assert.Empty(t, broker.retired)
	assert.Equal(t, uint64(1), broker.first)
	assert.Equal(t, 0, broker.appends)
}

func TestVoiceOutboxCapacity_UnrecordedPrefixLossCannotBecomeRetirementProof(t *testing.T) {
	db, generation := outboxDatabase(t)
	capacitySeed(t, db, generation, 50000, 50000)
	outboxEnqueue(t, db, capacityMarker())
	dispatcher, broker := capacityDispatcher(t, db, capacityMessageLimit)
	broker.first = 2 // No SQL intent or checkpoint authorizes this deletion.
	_, err := dispatcher.Step(context.Background())
	require.ErrorIs(t, err, ErrBrokerContinuity)
	requireOutboxCursor(t, db, 50001, 50000)
	assert.Equal(t, 50001, outboxCount(t, db))
	broker.mu.Lock()
	defer broker.mu.Unlock()
	assert.Empty(t, broker.retired, "a fresh acknowledgement cannot bless an unexplained prefix loss")
	assert.Equal(t, uint64(2), broker.first)
	assert.Equal(t, 0, broker.appends)
}
