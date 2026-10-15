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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

var errOutboxNetwork = errors.New("test-only broker connection unavailable")

// Only the CommandBroker network boundary is substituted. Every claim, lease,
// exact predecessor, retry, receipt and commit-loss oracle uses PostgreSQL.
type outboxBroker struct {
	db                                   *sql.DB
	receipt                              TransportReceipt
	mu                                   sync.Mutex
	latest                               *BrokerCommand
	events                               []string
	verifyFault, lastFault, publishFault error
	verifyHook                           func() error
	appendOnFault                        bool
	ackOverride                          *PublishReceipt
	beforeLast                           func(context.Context) error
	afterAppend                          func(context.Context) error
	activePublications                   int
}

var _ CommandBroker = (*outboxBroker)(nil)

// NOWAIT checks run on an independent connection at each network boundary. A
// successful dispatcher cannot keep its counter, head or receipt locks while
// waiting on Verify, LastVoiceCommand or PublishVoiceCommand.
func (b *outboxBroker) network(ctx context.Context, receipt TransportReceipt, operation string) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) > 10*time.Second {
		return errors.New("dispatcher supplied an unbounded network context")
	}
	if receipt.Generation != b.receipt.Generation || !sameTransportIdentity(receipt.Identity, b.receipt.Identity) {
		return errors.New("network call did not use the committed exact receipt")
	}
	b.mu.Lock()
	b.events = append(b.events, operation)
	b.mu.Unlock()
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, rollbackErr)
		}
	}()
	var allocated int64
	if err := tx.QueryRowContext(ctx, `SELECT last_allocated_sequence FROM voice_command_sequence
		WHERE id = TRUE FOR UPDATE NOWAIT`).Scan(&allocated); err != nil {
		return fmt.Errorf("network observed a locked sequence singleton: %w", err)
	}
	var generation uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM voice_command_transport
		WHERE id = TRUE FOR UPDATE NOWAIT`).Scan(&generation); err != nil {
		return fmt.Errorf("network observed a locked transport receipt: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence FROM voice_command_outbox ORDER BY sequence FOR UPDATE NOWAIT`)
	if err != nil {
		return fmt.Errorf("network observed a locked outbox head: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (b *outboxBroker) Verify(ctx context.Context, receipt TransportReceipt) error {
	if err := b.network(ctx, receipt, "verify"); err != nil {
		return err
	}
	b.mu.Lock()
	hook, fault := b.verifyHook, b.verifyFault
	b.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return fault
}

func (b *outboxBroker) LastVoiceCommand(ctx context.Context, receipt TransportReceipt) (*BrokerCommand, error) {
	if err := b.network(ctx, receipt, "last"); err != nil {
		return nil, err
	}
	b.mu.Lock()
	before, fault := b.beforeLast, b.lastFault
	b.beforeLast = nil
	b.mu.Unlock()
	if before != nil {
		if err := before(ctx); err != nil {
			return nil, err
		}
	}
	if fault != nil {
		return nil, fault
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.latest == nil {
		return nil, nil
	}
	return &BrokerCommand{Sequence: b.latest.Sequence, Data: append([]byte(nil), b.latest.Data...)}, nil
}

func (b *outboxBroker) PublishVoiceCommand(ctx context.Context, receipt TransportReceipt, command VoiceCommand) (PublishReceipt, error) {
	if err := b.network(ctx, receipt, "publish"); err != nil {
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
	b.mu.Lock()
	b.activePublications++
	fault, appendOnFault := b.publishFault, b.appendOnFault
	after, override := b.afterAppend, b.ackOverride
	b.afterAppend = nil
	if fault == nil || appendOnFault {
		b.latest = &BrokerCommand{Sequence: sequence, Data: data}
	}
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.activePublications--; b.mu.Unlock() }()
	if after != nil {
		if err := after(ctx); err != nil {
			return PublishReceipt{}, err
		}
	}
	if fault != nil {
		return PublishReceipt{}, fault
	}
	if override != nil {
		return *override, nil
	}
	return outboxReceipt(sequence), nil
}

func (b *outboxBroker) seed(t *testing.T, command VoiceCommand) {
	t.Helper()
	message := outboxBrokerMessage(t, command)
	b.mu.Lock()
	b.latest = message
	b.mu.Unlock()
}

func outboxBrokerMessage(t *testing.T, command VoiceCommand) *BrokerCommand {
	t.Helper()
	sequence, err := strconv.ParseUint(command.Sequence, 10, 64)
	require.NoError(t, err)
	data, err := json.Marshal(command)
	require.NoError(t, err)
	return &BrokerCommand{Sequence: sequence, Data: data}
}

func (b *outboxBroker) count(operation string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, event := range b.events {
		if event == operation {
			count++
		}
	}
	return count
}

func (b *outboxBroker) eventList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...)
}

type outboxReports struct {
	mu      sync.Mutex
	classes []string
}

func (r *outboxReports) add(class string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.classes = append(r.classes, class)
}

func (r *outboxReports) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.classes...)
}

func outboxDispatcher(t *testing.T, db *sql.DB) (*Dispatcher, *outboxBroker, *outboxReports) {
	t.Helper()
	provisioner := &bootstrapProvisioner{db: db}
	require.NoError(t, BootstrapTransport(context.Background(), db, provisioner))
	receipt, err := LoadReceipt(context.Background(), db)
	require.NoError(t, err)
	broker := &outboxBroker{db: db, receipt: receipt}
	reports := &outboxReports{}
	return &Dispatcher{Repository: NewRepository(db), Broker: broker, Report: reports.add}, broker, reports
}

func requireClosedOutboxReports(t *testing.T, reports *outboxReports) {
	t.Helper()
	for _, class := range reports.values() {
		assert.Contains(t, []string{"transport_unavailable", "broker_unavailable", "broker_continuity",
			"invalid_command", "receipt_unavailable"}, class, "reports carry fixed classes only")
	}
}

func TestVoiceOutboxDispatcher_PublishesHeadOutsideSQLLocks(t *testing.T) {
	db, _ := outboxDatabase(t)
	commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	dispatcher, broker, reports := outboxDispatcher(t, db)
	for sequence := int64(1); sequence <= 2; sequence++ {
		worked, err := dispatcher.Step(context.Background())
		require.NoError(t, err)
		assert.True(t, worked)
		requireOutboxCursor(t, db, 2, sequence)
		published, err := dispatcher.Repository.Published(context.Background(), sequence)
		require.NoError(t, err)
		assert.Equal(t, commands[sequence-1], published)
	}
	assert.Equal(t, 2, broker.count("publish"))
	assert.Equal(t, 2, outboxCount(t, db))
	assert.Empty(t, reports.values())
	events := broker.eventList()
	for index, event := range events {
		if event != "publish" {
			continue
		}
		require.Greater(t, index, 0)
		assert.Equal(t, "verify", events[index-1], "last-message work is reverified before append")
		require.Less(t, index+1, len(events))
		assert.Equal(t, "verify", events[index+1], "resource identity is reverified after PubAck")
	}
	before := broker.eventList()
	worked, err := dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.False(t, worked)
	assert.Equal(t, before, broker.eventList(), "an empty queue does not call the network")
}

func TestVoiceOutboxDispatcher_UnprovisionedTransportRetainsEnqueue(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot())
	reports := &outboxReports{}
	broker := &outboxBroker{db: db}
	dispatcher := &Dispatcher{Repository: NewRepository(db), Broker: broker, Report: reports.add}
	worked, err := dispatcher.Step(context.Background())
	require.Error(t, err)
	assert.True(t, worked)
	assert.Empty(t, broker.eventList())
	assert.Equal(t, 1, outboxCount(t, db))
	requireOutboxCursor(t, db, 1, 0)
	assert.Equal(t, "transport_unavailable", outboxDelivery(t, db, 1).Failure.String)
	assert.Contains(t, reports.values(), "transport_unavailable")
	requireClosedOutboxReports(t, reports)
}

func TestVoiceOutboxDispatcher_OutageRetainsHeadAndUsesBoundedBackoff(t *testing.T) {
	db, _ := outboxDatabase(t)
	commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	dispatcher, broker, reports := outboxDispatcher(t, db)
	broker.publishFault = errOutboxNetwork
	for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		outboxDue(t, db, 1)
		var before, after time.Time
		require.NoError(t, db.QueryRow(`SELECT clock_timestamp()`).Scan(&before))
		worked, err := dispatcher.Step(context.Background())
		require.Error(t, err)
		assert.True(t, worked)
		require.NoError(t, db.QueryRow(`SELECT clock_timestamp()`).Scan(&after))
		state := outboxDelivery(t, db, 1)
		assert.Equal(t, "broker_unavailable", state.Failure.String)
		assert.False(t, state.Token.Valid)
		assert.False(t, state.PublishedAt.Valid)
		assert.False(t, state.RetryAt.Before(before.Add(delay)))
		assert.False(t, state.RetryAt.After(after.Add(delay)))
		assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
		requireOutboxCursor(t, db, 2, 0)
		beforeEvents := broker.eventList()
		blocked, err := dispatcher.Step(context.Background())
		require.NoError(t, err)
		assert.False(t, blocked)
		assert.Equal(t, beforeEvents, broker.eventList())
	}
	assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1))
	assert.Equal(t, commands[1], outboxStoredCommand(t, db, 2))
	assert.Equal(t, 2, outboxCount(t, db))
	broker.publishFault = nil
	outboxDue(t, db, 1)
	worked, err := dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	requireOutboxCursor(t, db, 2, 1)
	assert.False(t, outboxDelivery(t, db, 1).Failure.Valid)
	requireClosedOutboxReports(t, reports)
}

func TestVoiceOutboxDispatcher_ExactRetainedHeadRecoversWithoutPublish(t *testing.T) {
	db, _ := outboxDatabase(t)
	command := outboxEnqueue(t, db, outboxSnapshot())[0]
	dispatcher, broker, reports := outboxDispatcher(t, db)
	data, err := json.Marshal(command)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	reordered, err := json.MarshalIndent(fields, "", "  ")
	require.NoError(t, err)
	require.NotEqual(t, string(data), string(reordered), "the oracle compares immutable semantics, not JSON field order")
	broker.latest = &BrokerCommand{Sequence: 1, Data: reordered}
	worked, err := dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	assert.Zero(t, broker.count("publish"), "recovery must never depend on a temporary MsgID cache")
	requireOutboxCursor(t, db, 1, 1)
	proof, err := dispatcher.Repository.Published(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, command, proof)
	assert.Empty(t, reports.values())
}

func TestVoiceOutboxDispatcher_LostPubAckReconcilesOnNextPass(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	dispatcher, broker, reports := outboxDispatcher(t, db)
	broker.publishFault, broker.appendOnFault = errOutboxNetwork, true
	worked, err := dispatcher.Step(context.Background())
	require.Error(t, err)
	assert.True(t, worked)
	assert.Equal(t, 1, broker.count("publish"))
	requireOutboxCursor(t, db, 2, 0)
	assert.False(t, outboxDelivery(t, db, 1).PublishedAt.Valid)
	broker.publishFault = nil
	outboxDue(t, db, 1)
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	assert.Equal(t, 1, broker.count("publish"), "retained exact-head evidence confirms the original append")
	requireOutboxCursor(t, db, 2, 1)
	assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
	requireClosedOutboxReports(t, reports)
}

func TestVoiceOutboxDispatcher_ForeignOrGapHeadPermanentlyBlocksProgress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, []VoiceCommand) *BrokerCommand
	}{
		{"different valid command ID", func(t *testing.T, commands []VoiceCommand) *BrokerCommand {
			foreign := commands[0]
			foreign.CommandID = uuid.NewString()
			return outboxBrokerMessage(t, foreign)
		}},
		{"same ID with changed user", func(t *testing.T, commands []VoiceCommand) *BrokerCommand {
			foreign := commands[0]
			foreign.UserID = uuid.NewString()
			return outboxBrokerMessage(t, foreign)
		}},
		{"valid later SQL command with a missing predecessor", func(t *testing.T, commands []VoiceCommand) *BrokerCommand {
			return outboxBrokerMessage(t, commands[1])
		}},
		{"malformed latest data", func(*testing.T, []VoiceCommand) *BrokerCommand {
			return &BrokerCommand{Sequence: 1, Data: []byte("{invalid}")}
		}},
		{"stream and envelope sequence disagree", func(t *testing.T, commands []VoiceCommand) *BrokerCommand {
			message := outboxBrokerMessage(t, commands[0])
			message.Sequence = 2
			return message
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
			dispatcher, broker, reports := outboxDispatcher(t, db)
			broker.latest = tc.change(t, commands)
			worked, err := dispatcher.Step(context.Background())
			require.Error(t, err)
			assert.True(t, worked)
			assert.Zero(t, broker.count("publish"))
			state := outboxDelivery(t, db, 1)
			assert.Contains(t, []string{"broker_continuity", "invalid_command"}, state.Failure.String)
			assert.Contains(t, reports.values(), state.Failure.String)
			assert.False(t, state.PublishedAt.Valid)
			outboxDue(t, db, 1)
			before := broker.eventList()
			blocked, err := dispatcher.Step(context.Background())
			require.NoError(t, err)
			assert.False(t, blocked)
			assert.Equal(t, before, broker.eventList())
			assert.Equal(t, 2, outboxCount(t, db))
			assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
			requireOutboxCursor(t, db, 2, 0)
			requireClosedOutboxReports(t, reports)
		})
	}
}

func TestVoiceOutboxDispatcher_PredecessorMustMatchPermanentSQLCommand(t *testing.T) {
	for _, change := range []string{"none", "user", "target", "snapshot", "missing"} {
		t.Run(change, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
			repository := NewRepository(db)
			first := outboxClaim(t, repository)
			require.NoError(t, repository.MarkPublished(context.Background(), first, outboxReceipt(1)))
			dispatcher, broker, reports := outboxDispatcher(t, db)
			previous := commands[0]
			switch change {
			case "user":
				previous.UserID = uuid.NewString()
			case "target":
				target := *previous.Target
				target.SessionGeneration = uuid.NewString()
				previous.Target = &target
			case "snapshot":
				previous.ServerDeafened = outboxBool(true)
			}
			if change != "missing" {
				broker.seed(t, previous)
			}
			worked, err := dispatcher.Step(context.Background())
			assert.True(t, worked)
			if change == "none" {
				require.NoError(t, err)
				assert.Equal(t, 1, broker.count("publish"))
				requireOutboxCursor(t, db, 2, 2)
			} else {
				require.Error(t, err)
				assert.Zero(t, broker.count("publish"))
				requireOutboxCursor(t, db, 2, 1)
				assert.Equal(t, "broker_continuity", outboxDelivery(t, db, 2).Failure.String)
				assert.False(t, outboxDelivery(t, db, 2).PublishedAt.Valid)
			}
			assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1))
			requireClosedOutboxReports(t, reports)
		})
	}
}

func TestVoiceOutboxDispatcher_InvalidAckAndContinuityFaultNeverRecordReceipt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ack        *PublishReceipt
		continuity bool
	}{
		{"wrong acknowledged stream", &PublishReceipt{Stream: "OTHER_STREAM", Sequence: 1}, false},
		{"wrong acknowledged sequence", &PublishReceipt{Stream: outboxTestStream, Sequence: 2}, false},
		{"negative acknowledged sequence", &PublishReceipt{Stream: outboxTestStream, Sequence: 0}, false},
		{"broker continuity sentinel", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			command := outboxEnqueue(t, db, outboxSnapshot())[0]
			dispatcher, broker, reports := outboxDispatcher(t, db)
			broker.ackOverride = tc.ack
			if tc.continuity {
				broker.verifyFault = ErrBrokerContinuity
			}
			worked, err := dispatcher.Step(context.Background())
			require.Error(t, err)
			assert.True(t, worked)
			assert.Equal(t, command, outboxStoredCommand(t, db, 1))
			assert.False(t, outboxDelivery(t, db, 1).PublishedAt.Valid)
			assert.False(t, outboxDelivery(t, db, 1).BrokerSeq.Valid)
			requireOutboxCursor(t, db, 1, 0)
			if tc.continuity {
				assert.Zero(t, broker.count("publish"))
				assert.Equal(t, "broker_continuity", outboxDelivery(t, db, 1).Failure.String)
			}
			require.NotEmpty(t, reports.values())
			requireClosedOutboxReports(t, reports)
		})
	}
}

func TestVoiceOutboxDispatcher_StaleWorkerCannotPoisonAdvancedReplacement(t *testing.T) {
	db, _ := outboxDatabase(t)
	commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot(), outboxSnapshot())
	dispatcher, broker, reports := outboxDispatcher(t, db)
	entered, resume := make(chan struct{}), make(chan struct{})
	broker.beforeLast = func(context.Context) error { close(entered); <-resume; return nil }
	type result struct {
		worked bool
		err    error
	}
	done := make(chan result, 1)
	go func() { worked, err := dispatcher.Step(context.Background()); done <- result{worked, err} }()
	<-entered
	outboxExpireLease(t, db, 1)
	replacement := &Dispatcher{Repository: NewRepository(db), Broker: broker, Report: reports.add}
	for range 2 {
		worked, err := replacement.Step(context.Background())
		require.NoError(t, err)
		assert.True(t, worked)
	}
	requireOutboxCursor(t, db, 3, 2)
	first, second, third := outboxDelivery(t, db, 1), outboxDelivery(t, db, 2), outboxDelivery(t, db, 3)
	close(resume)
	got := <-done
	require.NoError(t, got.err, "an already published/lost-token claim is a benign stale worker")
	assert.True(t, got.worked)
	assert.Equal(t, first, outboxDelivery(t, db, 1))
	assert.Equal(t, second, outboxDelivery(t, db, 2))
	assert.Equal(t, third, outboxDelivery(t, db, 3))
	assert.Equal(t, commands[2], outboxStoredCommand(t, db, 3))
	assert.False(t, outboxDelivery(t, db, 3).Failure.Valid)
	assert.Equal(t, 2, broker.count("publish"))
	requireOutboxCursor(t, db, 3, 2)
	assert.Empty(t, reports.values(), "a stale worker must not report poison for a legitimate successor")
}

func TestVoiceOutboxDispatcher_ReceiptCommitLossDoesNotRepublishAcceptedHead(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			db, _ := outboxDatabase(t)
			outboxEnqueue(t, db, outboxSnapshot())
			dispatcher, broker, reports := outboxDispatcher(t, db)
			hook, wrapped := stmthook.Open(t)
			dispatcher.Repository = NewRepository(wrapped)
			broker.afterAppend = func(context.Context) error { hook.ArmCommit(committed, stmthook.ErrInjected); return nil }
			worked, err := dispatcher.Step(context.Background())
			assert.True(t, worked)
			if committed {
				require.NoError(t, err)
				requireOutboxCursor(t, db, 1, 1)
			} else {
				require.Error(t, err)
				requireOutboxCursor(t, db, 1, 0)
				outboxDue(t, db, 1)
				worked, err = dispatcher.Step(context.Background())
				require.NoError(t, err)
				assert.True(t, worked)
				requireOutboxCursor(t, db, 1, 1)
			}
			assert.Equal(t, 1, broker.count("publish"))
			assert.Equal(t, 1, outboxCount(t, db))
			requireClosedOutboxReports(t, reports)
		})
	}
}

func TestVoiceOutboxDispatcher_RunAwaitsInFlightStepAndRetainsPendingWork(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	dispatcher, broker, _ := outboxDispatcher(t, db)
	entered, cancellationObserved, resume := make(chan struct{}), make(chan struct{}), make(chan struct{})
	broker.afterAppend = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(cancellationObserved)
		<-resume
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		dispatcher.Run(ctx)
		broker.mu.Lock()
		returnedWithNetworkInFlight := broker.activePublications != 0
		broker.mu.Unlock()
		done <- returnedWithNetworkInFlight
	}()
	<-entered
	cancel()
	<-cancellationObserved
	close(resume)
	assert.False(t, <-done, "Run must await its network attempt before returning")
	assert.Equal(t, 1, broker.count("publish"))
	assert.Equal(t, 1, outboxDelivery(t, db, 1).Attempts)
	assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
	assert.Equal(t, 2, outboxCount(t, db))
	requireOutboxCursor(t, db, 2, 0)
	outboxExpireLease(t, db, 1)
	outboxDue(t, db, 1)
	worked, err := dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	assert.Equal(t, 1, broker.count("publish"), "a successor recovers the retained append after shutdown")
	requireOutboxCursor(t, db, 2, 1)
}

func TestVoiceOutboxDispatcher_MissingDependenciesAndCanceledRunDoNotClaim(t *testing.T) {
	for _, dispatcher := range []*Dispatcher{{}, {Repository: NewRepository(nil), Broker: &outboxBroker{}}} {
		worked, err := dispatcher.Step(context.Background())
		require.Error(t, err)
		assert.False(t, worked)
	}
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot())
	dispatcher, broker, _ := outboxDispatcher(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dispatcher.Run(ctx)
	assert.Empty(t, broker.eventList())
	assert.Zero(t, outboxDelivery(t, db, 1).Attempts)
	requireOutboxCursor(t, db, 1, 0)
}

func TestVoiceOutboxDispatcher_DependencyFailuresLeavePendingCommandUntouched(t *testing.T) {
	db, _ := outboxDatabase(t)
	command := outboxEnqueue(t, db, outboxSnapshot())[0]
	healthy, broker, _ := outboxDispatcher(t, db)
	before := outboxDelivery(t, db, 1)
	var absent *Dispatcher
	worked, err := absent.Step(context.Background())
	require.Error(t, err)
	assert.False(t, worked)
	for _, tc := range []struct {
		name, class string
		repository  *Repository
		broker      CommandBroker
	}{
		{"repository missing", "receipt_unavailable", nil, broker},
		{"database missing", "receipt_unavailable", NewRepository(nil), broker},
		{"broker missing", "broker_unavailable", NewRepository(db), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reports := &outboxReports{}
			dispatcher := &Dispatcher{Repository: tc.repository, Broker: tc.broker, Report: reports.add}
			worked, err := dispatcher.Step(context.Background())
			require.Error(t, err)
			assert.False(t, worked)
			assert.Equal(t, []string{tc.class}, reports.values())
			assert.Equal(t, before, outboxDelivery(t, db, 1))
			assert.Equal(t, command, outboxStoredCommand(t, db, 1))
			assert.Empty(t, broker.eventList())
			requireOutboxCursor(t, db, 1, 0)
		})
	}
	worked, err = healthy.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	requireOutboxCursor(t, db, 1, 1)
}

func TestVoiceOutboxDispatcher_ClaimSQLFailureAndPreCanceledStepRemainRetryable(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(strconv.FormatBool(canceled), func(t *testing.T) {
			db, _ := outboxDatabase(t)
			command := outboxEnqueue(t, db, outboxSnapshot())[0]
			dispatcher, broker, reports := outboxDispatcher(t, db)
			before := outboxDelivery(t, db, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var hook *stmthook.Hook
			if canceled {
				cancel()
			} else {
				var wrapped *sql.DB
				hook, wrapped = stmthook.Open(t)
				hook.Arm([]string{"voice_command_sequence"}, nil, stmthook.ErrInjected)
				dispatcher.Repository = NewRepository(wrapped)
			}
			worked, err := dispatcher.Step(ctx)
			require.Error(t, err)
			assert.False(t, worked)
			if hook != nil {
				seen, betweenErr := hook.Report()
				require.NoError(t, betweenErr)
				require.Equal(t, 1, seen)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			assert.Equal(t, before, outboxDelivery(t, db, 1))
			assert.Equal(t, command, outboxStoredCommand(t, db, 1))
			assert.Empty(t, broker.eventList())
			assert.Equal(t, []string{"receipt_unavailable"}, reports.values())
			worked, err = dispatcher.Step(context.Background())
			require.NoError(t, err)
			assert.True(t, worked)
			requireOutboxCursor(t, db, 1, 1)
		})
	}
}

func TestVoiceOutboxDispatcher_CorruptOrMissingSQLHeadIsVisibleAndNotSkipped(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(strconv.FormatBool(missing), func(t *testing.T) {
			db, generation := outboxDatabase(t)
			class := "invalid_command"
			if missing {
				outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
				_, err := db.Exec(`DELETE FROM voice_command_admission WHERE sequence = 1`)
				require.NoError(t, err)
				_, err = db.Exec(`DELETE FROM voice_command_outbox WHERE sequence = 1`)
				require.NoError(t, err)
				class = "broker_continuity"
			} else {
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
			}
			dispatcher, broker, reports := outboxDispatcher(t, db)
			worked, err := dispatcher.Step(context.Background())
			require.Error(t, err)
			assert.False(t, worked)
			assert.Equal(t, []string{class}, reports.values())
			assert.Empty(t, broker.eventList())
			assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
			assert.False(t, outboxDelivery(t, db, 2).PublishedAt.Valid)
			if !missing {
				assert.Equal(t, class, outboxDelivery(t, db, 1).Failure.String)
				assert.False(t, outboxDelivery(t, db, 1).PublishedAt.Valid)
				assert.Equal(t, 2, outboxCount(t, db))
			} else {
				assert.Equal(t, 1, outboxCount(t, db))
			}
			requireOutboxCursor(t, db, 2, 0)
		})
	}
}

func TestVoiceOutboxDispatcher_VerificationLossRetainsProofAndPublicationOrder(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		continuity  bool
	}{
		{"initial transport outage", "before read", false},
		{"continuity lost after retained read", "after read", true},
		{"transport outage after append", "after append", false},
		{"continuity lost after append", "after append", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := outboxDatabase(t)
			command := outboxEnqueue(t, db, outboxSnapshot())[0]
			dispatcher, broker, reports := outboxDispatcher(t, db)
			broker.verifyHook = func() error {
				reads, publishes := broker.count("last"), broker.count("publish")
				matches := tc.phase == "before read" ||
					(tc.phase == "after read" && reads > 0) ||
					(tc.phase == "after append" && publishes > 0)
				if !matches {
					return nil
				}
				if tc.continuity {
					return ErrBrokerContinuity
				}
				return errOutboxNetwork
			}
			worked, err := dispatcher.Step(context.Background())
			require.Error(t, err)
			assert.True(t, worked)
			class := "transport_unavailable"
			if tc.continuity {
				class = "broker_continuity"
			}
			assert.Equal(t, class, outboxDelivery(t, db, 1).Failure.String)
			assert.Equal(t, []string{class}, reports.values())
			assert.False(t, outboxDelivery(t, db, 1).PublishedAt.Valid)
			assert.Equal(t, command, outboxStoredCommand(t, db, 1))
			requireOutboxCursor(t, db, 1, 0)
			wantPublishes := 0
			if tc.phase == "after append" {
				wantPublishes = 1
			}
			assert.Equal(t, wantPublishes, broker.count("publish"))
			broker.verifyHook = nil
			outboxDue(t, db, 1)
			worked, err = dispatcher.Step(context.Background())
			require.NoError(t, err)
			if tc.continuity {
				assert.False(t, worked)
				assert.Equal(t, wantPublishes, broker.count("publish"))
				requireOutboxCursor(t, db, 1, 0)
			} else {
				assert.True(t, worked)
				assert.Equal(t, 1, broker.count("publish"), "an accepted append is recovered without another publish")
				requireOutboxCursor(t, db, 1, 1)
			}
		})
	}
}

func TestVoiceOutboxDispatcher_LastReadFailureDistinguishesOutageFromContinuity(t *testing.T) {
	for _, continuity := range []bool{false, true} {
		t.Run(strconv.FormatBool(continuity), func(t *testing.T) {
			db, _ := outboxDatabase(t)
			outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
			dispatcher, broker, reports := outboxDispatcher(t, db)
			broker.lastFault = errOutboxNetwork
			class := "broker_unavailable"
			if continuity {
				broker.lastFault, class = ErrBrokerContinuity, "broker_continuity"
			}
			worked, err := dispatcher.Step(context.Background())
			require.Error(t, err)
			assert.True(t, worked)
			assert.Equal(t, class, outboxDelivery(t, db, 1).Failure.String)
			assert.Equal(t, []string{class}, reports.values())
			assert.Zero(t, broker.count("publish"))
			assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
			requireOutboxCursor(t, db, 2, 0)
			broker.lastFault = nil
			outboxDue(t, db, 1)
			worked, err = dispatcher.Step(context.Background())
			require.NoError(t, err)
			assert.Equal(t, !continuity, worked)
			if continuity {
				requireOutboxCursor(t, db, 2, 0)
			} else {
				requireOutboxCursor(t, db, 2, 1)
			}
		})
	}
}

func TestVoiceOutboxDispatcher_PredecessorSQLProofCannotBeReplacedByBrokerData(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(strconv.FormatBool(missing), func(t *testing.T) {
			db, _ := outboxDatabase(t)
			commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
			repository := NewRepository(db)
			require.NoError(t, repository.MarkPublished(context.Background(), outboxClaim(t, repository), outboxReceipt(1)))
			dispatcher, broker, reports := outboxDispatcher(t, db)
			broker.seed(t, commands[0])
			var hook *stmthook.Hook
			class := "broker_continuity"
			if missing {
				_, err := db.Exec(`DELETE FROM voice_command_outbox WHERE sequence = 1`)
				require.NoError(t, err)
			} else {
				var wrapped *sql.DB
				hook, wrapped = stmthook.Open(t)
				hook.Arm([]string{"voice_command_transport", "voice_command_outbox"}, nil, stmthook.ErrInjected)
				dispatcher.Repository = NewRepository(wrapped)
				class = "receipt_unavailable"
			}
			worked, err := dispatcher.Step(context.Background())
			require.Error(t, err)
			assert.True(t, worked)
			assert.Zero(t, broker.count("publish"))
			assert.Equal(t, class, outboxDelivery(t, db, 2).Failure.String)
			assert.Equal(t, []string{class}, reports.values())
			assert.False(t, outboxDelivery(t, db, 2).PublishedAt.Valid)
			requireOutboxCursor(t, db, 2, 1)
			if hook != nil {
				seen, betweenErr := hook.Report()
				require.NoError(t, betweenErr)
				require.Equal(t, 2, seen, "the outage must occur at the fresh predecessor proof read")
				outboxDue(t, db, 2)
				worked, err = dispatcher.Step(context.Background())
				require.NoError(t, err)
				assert.True(t, worked)
				requireOutboxCursor(t, db, 2, 2)
			} else {
				outboxDue(t, db, 2)
				worked, err = dispatcher.Step(context.Background())
				require.NoError(t, err)
				assert.False(t, worked)
			}
		})
	}
}

func TestVoiceOutboxDispatcher_RetrySQLFailureRetainsLeaseAndHeadForSuccessor(t *testing.T) {
	db, _ := outboxDatabase(t)
	commands := outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot())
	dispatcher, broker, reports := outboxDispatcher(t, db)
	hook, wrapped := stmthook.Open(t)
	dispatcher.Repository = NewRepository(wrapped)
	broker.publishFault = errOutboxNetwork
	broker.afterAppend = func(context.Context) error {
		hook.Arm([]string{"UPDATE voice_command_outbox"}, nil, stmthook.ErrInjected)
		return nil
	}
	worked, err := dispatcher.Step(context.Background())
	require.ErrorIs(t, err, errOutboxNetwork)
	require.ErrorIs(t, err, stmthook.ErrInjected)
	assert.True(t, worked)
	seen, betweenErr := hook.Report()
	require.NoError(t, betweenErr)
	require.Equal(t, 1, seen)
	assert.Equal(t, []string{"broker_unavailable", "receipt_unavailable"}, reports.values())
	state := outboxDelivery(t, db, 1)
	assert.True(t, state.Token.Valid, "failed cleanup retains the finite claim for expiry")
	assert.True(t, state.LeaseUntil.Valid)
	assert.False(t, state.PublishedAt.Valid)
	assert.Equal(t, commands[0], outboxStoredCommand(t, db, 1))
	assert.Zero(t, outboxDelivery(t, db, 2).Attempts)
	requireOutboxCursor(t, db, 2, 0)
	broker.publishFault = nil
	outboxExpireLease(t, db, 1)
	worked, err = dispatcher.Step(context.Background())
	require.NoError(t, err)
	assert.True(t, worked)
	requireOutboxCursor(t, db, 2, 1)
}

func TestVoiceOutboxDispatcher_StalePubAckCannotRegressReplacementReceipt(t *testing.T) {
	db, _ := outboxDatabase(t)
	outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot(), outboxSnapshot())
	dispatcher, broker, reports := outboxDispatcher(t, db)
	entered, resume := make(chan struct{}), make(chan struct{})
	broker.afterAppend = func(context.Context) error { close(entered); <-resume; return nil }
	done := make(chan error, 1)
	go func() { _, err := dispatcher.Step(context.Background()); done <- err }()
	<-entered
	outboxExpireLease(t, db, 1)
	for range 2 {
		worked, err := dispatcher.Step(context.Background())
		require.NoError(t, err)
		assert.True(t, worked)
	}
	first, second, third := outboxDelivery(t, db, 1), outboxDelivery(t, db, 2), outboxDelivery(t, db, 3)
	close(resume)
	require.NoError(t, <-done)
	assert.Equal(t, first, outboxDelivery(t, db, 1))
	assert.Equal(t, second, outboxDelivery(t, db, 2))
	assert.Equal(t, third, outboxDelivery(t, db, 3))
	assert.Empty(t, reports.values())
	assert.Equal(t, 2, broker.count("publish"))
	requireOutboxCursor(t, db, 3, 2)
}

func TestVoiceOutboxDispatcher_RunWakesFromIdleAndRetriesSQLFailure(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(strconv.FormatBool(outage), func(t *testing.T) {
			db, _ := outboxDatabase(t)
			dispatcher, broker, reports := outboxDispatcher(t, db)
			hook, wrapped := stmthook.Open(t)
			dispatcher.Repository = NewRepository(wrapped)
			type enqueued struct {
				commands []VoiceCommand
				err      error
			}
			queued := make(chan enqueued, 1)
			if outage {
				queued <- enqueued{commands: outboxEnqueue(t, db, outboxSnapshot(), outboxSnapshot(), outboxSnapshot())}
				hook.Arm([]string{"voice_command_sequence"}, nil, stmthook.ErrInjected)
			} else {
				// The first pass reads an empty queue's cursor. The next cursor read
				// follows the worker's idle wait; the callback commits new work
				// before that second claim acquires the sequence singleton.
				hook.Arm([]string{"voice_command_sequence", "voice_command_sequence"}, func() error {
					tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
					if err != nil {
						queued <- enqueued{err: err}
						return err
					}
					commands, err := EnqueueTx(context.Background(), tx, []CommandSpec{outboxSnapshot(), outboxSnapshot(), outboxSnapshot()})
					if err != nil {
						err = errors.Join(err, tx.Rollback())
						queued <- enqueued{err: err}
						return err
					}
					err = tx.Commit()
					queued <- enqueued{commands: commands, err: err}
					return err
				}, nil)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			thirdClaimed, done := make(chan struct{}), make(chan struct{})
			lastCalls := 0
			var observeLast func(context.Context) error
			observeLast = func(ctx context.Context) error {
				lastCalls++
				if lastCalls < 3 {
					broker.mu.Lock()
					broker.beforeLast = observeLast
					broker.mu.Unlock()
					return nil
				}
				close(thirdClaimed)
				<-ctx.Done()
				return ctx.Err()
			}
			broker.beforeLast = observeLast
			go func() { dispatcher.Run(ctx); close(done) }()
			select {
			case <-thirdClaimed:
			case <-done:
				t.Fatal("Run stopped before draining committed work")
			case <-ctx.Done():
				t.Fatal("Run never reached the pending successor")
			}
			inserted := <-queued
			require.NoError(t, inserted.err)
			require.Len(t, inserted.commands, 3)
			seen, betweenErr := hook.Report()
			require.NoError(t, betweenErr)
			wantSeen := 2
			if outage {
				wantSeen = 1
			}
			require.Equal(t, wantSeen, seen)
			assert.Equal(t, 2, broker.count("publish"))
			requireOutboxCursor(t, db, 3, 2)
			for sequence := int64(1); sequence <= 2; sequence++ {
				proof, err := NewRepository(db).Published(context.Background(), sequence)
				require.NoError(t, err)
				assert.Equal(t, inserted.commands[sequence-1], proof)
			}
			if outage {
				assert.Equal(t, []string{"receipt_unavailable"}, reports.values())
			} else {
				assert.Empty(t, reports.values())
			}
			cancel()
			<-done
			assert.False(t, outboxDelivery(t, db, 3).PublishedAt.Valid)
			assert.Equal(t, inserted.commands[2], outboxStoredCommand(t, db, 3))
			assert.Equal(t, 3, outboxCount(t, db))
			requireOutboxCursor(t, db, 3, 2)
			requireClosedOutboxReports(t, reports)
		})
	}
}
