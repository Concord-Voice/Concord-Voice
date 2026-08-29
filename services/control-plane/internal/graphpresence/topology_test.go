package graphpresence

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The boundary vocabulary. Every step WithGatedTx crosses records one of these
// into a SHARED trace, so a test asserts on the whole ordered slice rather than
// on call counts: a count cannot tell "gate then BeginTx" from "BeginTx then
// gate", and that order is the entire reason WithGatedTx exists.
const (
	stepGateAcquire = "gate_acquire"
	stepBeginTx     = "begin_tx"
	stepWork        = "work"
	stepCommit      = "commit"
	stepRollback    = "rollback"
	stepGateRelease = "gate_release"

	// stepBeginTopologyBatch is the durable marker write. CaptureInTx's whole
	// shape is where this lands relative to the two savepoints, so it shares the
	// trace with the SQL statements below.
	stepBeginTopologyBatch = "begin_topology_batch"

	// stepCompleteTopologyBatch is the rail's commit-and-deliver. It shares the
	// trace so a test can prove Complete delegated the COMMIT rather than
	// issuing its own alongside it.
	stepCompleteTopologyBatch = "complete_topology_batch"

	// The #2992 revocation bracket. It shares the trace with everything above
	// because its correctness is entirely positional: open after the gate and
	// before BeginTx, close after the transaction resolves and before the gate
	// is released. Counting cannot express any of those.
	stepFenceOpen  = "fence_open"
	stepFenceClose = "fence_close"
)

type callTrace struct {
	mu    sync.Mutex
	steps []string
}

func (t *callTrace) record(step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, step)
}

func (t *callTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.steps...)
}

// railStub stands in for *presencehistory.Service. It brackets work with the
// acquire/release steps exactly as WithSenders does, which is what lets a test
// prove the transaction is opened INSIDE the gate closure rather than beside it.
type railStub struct {
	trace     *callTrace
	gateCalls int
	gatedFor  []uuid.UUID

	// beginErr fails the marker stage. It is a field rather than a second stub
	// so the batch-carrying tests and the fail-closed tests share one rail.
	beginErr   error
	beginCalls int
	beganFor   []uuid.UUID

	// completion is what the rail reports back from the terminal. A nil pointer
	// is the DELIVERED outcome every test written before the terminal existed
	// was built against; the terminal tests point it at the
	// committed-but-undelivered and the unproven outcomes.
	completion    *presencehistory.TopologyCompletion
	completeCalls int
}

func (s *railStub) WithSenders(
	_ context.Context, senderIDs []uuid.UUID, work func() error,
) error {
	s.gateCalls++
	s.gatedFor = append([]uuid.UUID(nil), senderIDs...)
	s.trace.record(stepGateAcquire)
	defer s.trace.record(stepGateRelease)
	return work()
}

func (s *railStub) BeginTopologyBatch(
	_ context.Context, _ *sql.Tx, senderIDs []uuid.UUID,
) (presencehistory.TopologyBatch, error) {
	s.trace.record(stepBeginTopologyBatch)
	s.beginCalls++
	s.beganFor = append([]uuid.UUID(nil), senderIDs...)
	if s.beginErr != nil {
		return presencehistory.TopologyBatch{}, s.beginErr
	}
	return presencehistory.TopologyBatch{}, nil
}

func (s *railStub) CompleteTopologyBatchWithOutcome(
	_ context.Context, _ *sql.Tx, _ presencehistory.TopologyBatch,
) presencehistory.TopologyCompletion {
	s.trace.record(stepCompleteTopologyBatch)
	s.completeCalls++
	if s.completion != nil {
		return *s.completion
	}
	return presencehistory.TopologyCompletion{Committed: true}
}

var _ TopologyRail = (*railStub)(nil)

// The consumer-declared interface must actually be satisfied by the durable
// rail it was extracted from. Without this the three signatures could drift
// apart and nothing would notice until Task 10's construction site.
var _ TopologyRail = (*presencehistory.Service)(nil)

// gatedTxConnector is a database/sql driver that records transaction
// boundaries into the trace, following the sql.OpenDB idiom already used by
// internal/voice/permission_enforcer_shutdown_test.go. No real database is
// needed — or would help — because what is under test is the ORDER of BeginTx
// against the sender gate, not any SQL.
type gatedTxConnector struct {
	trace       *callTrace
	beginErr    error
	rollbackErr error
}

func (c gatedTxConnector) Connect(context.Context) (driver.Conn, error) {
	return gatedTxConn(c), nil
}

func (c gatedTxConnector) Driver() driver.Driver { return c }

func (c gatedTxConnector) Open(string) (driver.Conn, error) {
	return c.Connect(context.Background())
}

type gatedTxConn gatedTxConnector

func (gatedTxConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (gatedTxConn) Close() error { return nil }

func (c gatedTxConn) Begin() (driver.Tx, error) {
	c.trace.record(stepBeginTx)
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return gatedTx(c), nil
}

type gatedTx gatedTxConn

func (t gatedTx) Commit() error {
	t.trace.record(stepCommit)
	return nil
}

func (t gatedTx) Rollback() error {
	t.trace.record(stepRollback)
	return t.rollbackErr
}

func openTracedDB(t *testing.T, connector gatedTxConnector) *sql.DB {
	t.Helper()
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close traced database: %v", err)
		}
	})
	return db
}

func removeSubject(principal, counterpart uuid.UUID) presencecapture.Subject {
	return presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   principal,
		Counterpart: counterpart,
	}
}

// This is the ordering constraint the whole seam exists for. BeginTopologyBatch
// takes the users row lock, and internal/users/presence_settings.go's
// UpdatePresenceSettings takes the same process-local gate BEFORE its BeginTx
// and the same users row inside it — so a gate acquired after BeginTx would
// close a cycle no database deadlock detector can break.
//
// The single slice comparison covers all three duties at once: gate before
// BeginTx (index 0 precedes 1), gate released after work returned (release is
// last), and no COMMIT anywhere (Complete owns it).
func TestWithGatedTxOpensTheTransactionInsideTheSenderGates(t *testing.T) {
	trace := &callTrace{}
	rail := &railStub{trace: trace}
	reconciler := &Reconciler{
		db:   openTracedDB(t, gatedTxConnector{trace: trace}),
		rail: rail,
	}
	principal, counterpart := uuid.New(), uuid.New()

	var received *sql.Tx
	err := reconciler.WithGatedTx(
		context.Background(), removeSubject(principal, counterpart),
		func(tx *sql.Tx) error {
			received = tx
			trace.record(stepWork)
			return nil
		},
	)

	require.NoError(t, err)
	require.NotNil(t, received, "work must receive the transaction WithGatedTx opened")
	assert.Equal(t,
		[]string{stepGateAcquire, stepBeginTx, stepWork, stepRollback, stepGateRelease},
		trace.snapshot(),
		"the gates must bracket BeginTx and work, in that order")
	assert.NotContains(t, trace.snapshot(), stepCommit,
		"WithGatedTx must never commit — Complete owns the COMMIT")
	assert.Equal(t, 1, rail.gateCalls)
	assert.ElementsMatch(t, []uuid.UUID{principal, counterpart}, rail.gatedFor,
		"every focal sender's stripe must be held")
}

// The PRIMARY production path once the handlers move onto WithGatedTx: work's
// Complete COMMITS, so the deferred discard finds a resolved transaction and
// must stay silent. Without the sql.ErrTxDone tolerance in runInTx, every
// successful friend accept / remove / block / FoF toggle would return
// "discard gated graph mutation: sql: transaction has already been committed or
// rolled back" — a 500 for a mutation that committed AND dispatched, which is
// the correctness lie presencecapture.ErrPostCommitDelivery exists to prevent.
//
// Every other test in this file leaves work uncommitted, so the deferred
// rollback reaches the driver and returns nil; the tolerance was pure
// unexercised surface and deleting it kept the suite green. These two cases
// pin it from both terminals.
//
// stepRollback is deliberately absent from both expected traces: database/sql
// resolves Tx.done with a CompareAndSwap in Tx.Commit, so the later
// Tx.Rollback short-circuits to sql.ErrTxDone without ever reaching the driver
// (verified against the toolchain's sql.go: Tx.Rollback -> tx.rollback(false)
// -> `if !tx.done.CompareAndSwap(false, true) { return ErrTxDone }`).
func TestWithGatedTxToleratesTheDiscardOfACommittedTransaction(t *testing.T) {
	t.Run("work committed and returned nil", func(t *testing.T) {
		trace := &callTrace{}
		rail := &railStub{trace: trace}
		reconciler := &Reconciler{
			db:   openTracedDB(t, gatedTxConnector{trace: trace}),
			rail: rail,
		}

		err := reconciler.WithGatedTx(
			context.Background(), removeSubject(uuid.New(), uuid.New()),
			func(tx *sql.Tx) error {
				trace.record(stepWork)
				// Stands in for Complete, which owns the COMMIT on both paths.
				return tx.Commit()
			},
		)

		require.NoError(t, err,
			"a committed mutation must not be reported as a failed discard")
		assert.Equal(t,
			[]string{stepGateAcquire, stepBeginTx, stepWork, stepCommit, stepGateRelease},
			trace.snapshot(),
			"the commit is work's, and the deferred discard adds no driver call")
	})

	t.Run("work committed and then failed delivery", func(t *testing.T) {
		// The post-commit terminal. The deferred discard must not JOIN its
		// ErrTxDone onto this error either: a caller classifying with
		// errors.Is still needs the terminal it was handed, and a joined
		// "discard gated graph mutation" would misdescribe a committed
		// mutation as an unresolved one.
		trace := &callTrace{}
		reconciler := &Reconciler{
			db:   openTracedDB(t, gatedTxConnector{trace: trace}),
			rail: &railStub{trace: trace},
		}
		postCommit := fmt.Errorf("dispatch friendship removal: %w",
			presencecapture.ErrPostCommitDelivery)

		err := reconciler.WithGatedTx(
			context.Background(), removeSubject(uuid.New(), uuid.New()),
			func(tx *sql.Tx) error {
				trace.record(stepWork)
				if commitErr := tx.Commit(); commitErr != nil {
					return commitErr
				}
				return postCommit
			},
		)

		require.Error(t, err)
		assert.Equal(t, postCommit, err,
			"work's terminal must reach the caller unaltered")
		assert.ErrorIs(t, err, presencecapture.ErrPostCommitDelivery)
		assert.NotContains(t, err.Error(), "discard gated graph mutation")
		assert.Equal(t,
			[]string{stepGateAcquire, stepBeginTx, stepWork, stepCommit, stepGateRelease},
			trace.snapshot())
	})
}

// A gate that leaks on the BeginTx error path wedges every later writer for
// that stripe, so the release is asserted on the failure path too.
func TestWithGatedTxReleasesTheGatesWhenBeginTxFails(t *testing.T) {
	trace := &callTrace{}
	rail := &railStub{trace: trace}
	beginErr := errors.New("connection reset")
	reconciler := &Reconciler{
		db:   openTracedDB(t, gatedTxConnector{trace: trace, beginErr: beginErr}),
		rail: rail,
	}

	err := reconciler.WithGatedTx(
		context.Background(), removeSubject(uuid.New(), uuid.New()),
		func(*sql.Tx) error {
			trace.record(stepWork)
			return nil
		},
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, beginErr, "the driver's cause must survive the wrap")
	assert.Contains(t, err.Error(), "begin gated graph mutation",
		"the wrap must name the operation")
	assert.Equal(t,
		[]string{stepGateAcquire, stepBeginTx, stepGateRelease},
		trace.snapshot(),
		"work must not run without a transaction, and the gate must still release")
}

// A rollback that neither succeeds nor reports ErrTxDone leaves the mutation's
// fate unknown. Returning nil there would report success for a write nobody can
// prove landed, so the terminal fails closed.
func TestWithGatedTxFailsClosedWhenTheDiscardingRollbackFails(t *testing.T) {
	trace := &callTrace{}
	rollbackErr := errors.New("connection already closed")
	reconciler := &Reconciler{
		db:   openTracedDB(t, gatedTxConnector{trace: trace, rollbackErr: rollbackErr}),
		rail: &railStub{trace: trace},
	}

	err := reconciler.WithGatedTx(
		context.Background(), removeSubject(uuid.New(), uuid.New()),
		func(*sql.Tx) error { return nil },
	)

	require.Error(t, err, "a failed discard must not resolve as success")
	assert.ErrorIs(t, err, rollbackErr)
}

// An unwired replica keeps the pre-PR-2 behaviour: a plain transaction, no gate.
func TestWithGatedTxRunsWithoutGatesWhenTheRailIsUnwired(t *testing.T) {
	trace := &callTrace{}
	reconciler := &Reconciler{db: openTracedDB(t, gatedTxConnector{trace: trace})}

	err := reconciler.WithGatedTx(
		context.Background(), removeSubject(uuid.New(), uuid.New()),
		func(tx *sql.Tx) error {
			require.NotNil(t, tx)
			trace.record(stepWork)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t,
		[]string{stepBeginTx, stepWork, stepRollback},
		trace.snapshot(),
		"an unwired rail must still open the transaction, with no gate step")
}

// The rail IS wired here, so a zero gate count is a real assertion rather than
// a restatement of the setup: dropping the empty-focal-set branch would call
// WithSenders with an empty slice and turn this red.
func TestWithGatedTxSkipsTheRailWhenTheSubjectHasNoFocalSender(t *testing.T) {
	trace := &callTrace{}
	rail := &railStub{trace: trace}
	reconciler := &Reconciler{
		db:   openTracedDB(t, gatedTxConnector{trace: trace}),
		rail: rail,
	}

	err := reconciler.WithGatedTx(
		context.Background(),
		presencecapture.Subject{Family: presencecapture.FamilyFriendsOfFriendsToggle},
		func(*sql.Tx) error {
			trace.record(stepWork)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, 0, rail.gateCalls, "an empty focal set holds no sender stripe")
	assert.Equal(t, []string{stepBeginTx, stepWork, stepRollback}, trace.snapshot())
}

func TestWithGatedTxRejectsMissingCollaborators(t *testing.T) {
	t.Run("nil work", func(t *testing.T) {
		trace := &callTrace{}
		rail := &railStub{trace: trace}
		reconciler := &Reconciler{
			db:   openTracedDB(t, gatedTxConnector{trace: trace}),
			rail: rail,
		}

		err := reconciler.WithGatedTx(
			context.Background(), removeSubject(uuid.New(), uuid.New()), nil)

		require.Error(t, err)
		assert.Equal(t, 0, rail.gateCalls, "a caller with no work takes no gate")
		assert.Empty(t, trace.snapshot(), "and opens no transaction")
	})

	t.Run("nil reconciler", func(t *testing.T) {
		var reconciler *Reconciler

		err := reconciler.WithGatedTx(
			context.Background(), removeSubject(uuid.New(), uuid.New()),
			func(*sql.Tx) error { return nil })

		require.Error(t, err, "a nil receiver must error rather than panic")
	})

	t.Run("nil database", func(t *testing.T) {
		trace := &callTrace{}
		rail := &railStub{trace: trace}
		reconciler := &Reconciler{rail: rail}

		err := reconciler.WithGatedTx(
			context.Background(), removeSubject(uuid.New(), uuid.New()),
			func(*sql.Tx) error {
				trace.record(stepWork)
				return nil
			})

		require.Error(t, err)
		assert.Equal(t,
			[]string{stepGateAcquire, stepGateRelease},
			trace.snapshot(),
			"work must not run without a transaction, and the gate must still release")
	})
}

func TestTranslateRailErrorMapsThePendingTerminals(t *testing.T) {
	t.Run("held marker keeps its own retry delay", func(t *testing.T) {
		err := translateRailError(&presencehistory.ServiceError{
			Status:     http.StatusServiceUnavailable,
			Code:       "presence_operation_pending",
			RetryAfter: 21 * time.Second,
		})
		assert.True(t, errors.Is(err, presencecapture.ErrCapturePending))

		var pending *presencecapture.PendingError
		require.True(t, errors.As(err, &pending))
		assert.Equal(t, 21*time.Second, pending.After)
	})

	t.Run("the wrapping BeginTopologyBatch applies is transparent", func(t *testing.T) {
		// BeginTopologyBatch wraps every failure with
		// "begin topology audience operation: %w", so classification has to
		// survive it or the pending terminal reaches the handler as a 500.
		err := translateRailError(errors.Join(
			errors.New("begin topology audience operation"),
			&presencehistory.ServiceError{
				Status:     http.StatusServiceUnavailable,
				Code:       "presence_operation_pending",
				RetryAfter: time.Second,
			},
		))
		assert.True(t, errors.Is(err, presencecapture.ErrCapturePending))
	})

	t.Run("eligible marker gets the reconciler tick", func(t *testing.T) {
		err := translateRailError(presencehistory.ErrPendingOperationEligible)
		assert.True(t, errors.Is(err, presencecapture.ErrCapturePending))

		var pending *presencecapture.PendingError
		require.True(t, errors.As(err, &pending))
		assert.Equal(t, reconcileRetryAfter, pending.After)
		assert.Positive(t, pending.After,
			"a zero Retry-After sends the client straight back into the same marker")
	})

	t.Run("another service error code passes through untouched", func(t *testing.T) {
		// The Code check is load-bearing: a non-pending ServiceError must not be
		// reclassified as retryable, or a permanent refusal loops the client.
		cause := &presencehistory.ServiceError{
			Status: http.StatusForbidden,
			Code:   "activity_history_disabled",
		}
		assert.Equal(t, error(cause), translateRailError(cause))
		assert.False(t, errors.Is(translateRailError(cause), presencecapture.ErrCapturePending))
	})

	t.Run("anything else passes through untouched", func(t *testing.T) {
		cause := errors.New("read accepted friendship edge: connection reset")
		assert.Equal(t, cause, translateRailError(cause))
	})

	t.Run("nil stays nil", func(t *testing.T) {
		assert.NoError(t, translateRailError(nil))
	})
}

func TestTopologyRailWiringIsObservable(t *testing.T) {
	reconciler := &Reconciler{}
	assert.False(t, reconciler.HasTopologyRail(), "an unwired reconciler reports false")

	reconciler.SetTopologyRail(&railStub{trace: &callTrace{}})
	assert.True(t, reconciler.HasTopologyRail(), "and a wired one reports true")

	var absent *Reconciler
	assert.False(t, absent.HasTopologyRail(), "a nil receiver must not panic")
	assert.NotPanics(t, func() { absent.SetTopologyRail(&railStub{trace: &callTrace{}}) })
}

// ─── the scripted capture driver ────────────────────────────────────────────
//
// CaptureInTx's contract is an ORDER of statements around one durable marker
// write, and no real database can show that order without also exercising every
// audience query the reads sit on top of. This driver records the statement
// CaptureInTx issues, labelled, into the SAME trace railStub writes into — so
// one slice comparison proves the marker write landed between the two
// savepoints rather than inside either.
//
// An UNSCRIPTED statement is an error, never a silent success: a test that
// forgets to script a read would otherwise pass while proving nothing.

const (
	stmtGateOpen         = "savepoint_gate_open"
	stmtGateRollback     = "savepoint_gate_rollback"
	stmtGateRelease      = "savepoint_gate_release"
	stmtCaptureOpen      = "savepoint_capture_open"
	stmtCaptureRollback  = "savepoint_capture_rollback"
	stmtCaptureRelease   = "savepoint_capture_release"
	stmtAcceptedEdge     = "read_accepted_edge"
	stmtTopologyActivity = "read_topology_activity"
	stmtActiveScopes     = "read_active_scopes"
	stmtPolicySettings   = "read_policy_settings"
	stmtServerMembers    = "read_server_members"

	// The four statements ComputeCustomTextAudience issues for the POST-write
	// After side. They are labelled separately from the capture's reads because
	// topologyAudiences runs after the mutation, on the same transaction.
	stmtCustomTextSettings  = "read_custom_text_settings"
	stmtFriends             = "read_friends"
	stmtFriendsOfFriends    = "read_friends_of_friends"
	stmtFoFFlag             = "read_fof_flag"
	stmtCustomTextOverrides = "read_custom_text_overrides"
)

// statementLabel names the statement by matching the PRODUCTION literals and
// table names, so a renamed savepoint keeps its label here and the assertions
// stay about ordering. The literal text itself is locked separately by
// TestSavepointStatementsAreDistinctFixedLiterals.
func statementLabel(query string) string {
	normalized := strings.Join(strings.Fields(query), " ")
	switch normalized {
	case gateSavepoint.open:
		return stmtGateOpen
	case gateSavepoint.rollback:
		return stmtGateRollback
	case gateSavepoint.release:
		return stmtGateRelease
	case captureSavepoint.open:
		return stmtCaptureOpen
	case captureSavepoint.rollback:
		return stmtCaptureRollback
	case captureSavepoint.release:
		return stmtCaptureRelease
	}
	// THREE different statements read user_presence_settings —
	// readTopologyActivity here, presence.loadPolicySettings under
	// CaptureServerVoiceCandidatesStrict, and presence.ComputeCustomTextAudience
	// on the After side — with different projections and different column
	// counts, so they are separated by the columns each selects. Matching on the
	// table alone would hand one of them another's row shape and fail as a scan
	// error rather than as a missing script. readTopologyActivity is the only
	// one that projects custom_text to a boolean, and it is tested FIRST because
	// it selects custom_text_tier too.
	switch {
	case strings.Contains(normalized, "SELECT EXISTS ( SELECT 1 FROM friendships"):
		return stmtAcceptedEdge
	case strings.Contains(normalized, "FROM user_presence_settings") &&
		strings.Contains(normalized, "COALESCE(custom_text, '') <> ''"):
		return stmtTopologyActivity
	case strings.Contains(normalized, "FROM user_presence_settings") &&
		strings.Contains(normalized, "custom_text_tier"):
		return stmtCustomTextSettings
	case strings.Contains(normalized, "FROM user_presence_settings") &&
		strings.Contains(normalized, "server_voice_tier"):
		return stmtPolicySettings
	case strings.Contains(normalized, "AS fof_id"):
		return stmtFriendsOfFriends
	case strings.Contains(normalized, "FROM friendships") &&
		strings.Contains(normalized, "AS friend_id"):
		return stmtFriends
	case strings.Contains(normalized, "FROM privacy_settings"):
		return stmtFoFFlag
	case strings.Contains(normalized, "FROM user_presence_overrides"):
		return stmtCustomTextOverrides
	case strings.Contains(normalized, "FROM voice_participants"):
		return stmtActiveScopes
	case strings.Contains(normalized, "FROM server_members"):
		return stmtServerMembers
	default:
		return "unscripted: " + normalized
	}
}

type captureAnswer struct {
	columns []string
	rows    [][]driver.Value
	err     error
}

type captureConnector struct {
	trace   *callTrace
	answers map[string]captureAnswer
}

// savepointAnswers scripts the six savepoint statements as successes. Tests
// that need a savepoint to FAIL overwrite the one entry they care about.
func savepointAnswers() map[string]captureAnswer {
	return map[string]captureAnswer{
		stmtGateOpen:        {},
		stmtGateRollback:    {},
		stmtGateRelease:     {},
		stmtCaptureOpen:     {},
		stmtCaptureRollback: {},
		stmtCaptureRelease:  {},
	}
}

// topologyActivityAnswer is the settings row shape readTopologyActivity reads:
// master_enabled, custom_text_tier, and custom_text projected to a BOOLEAN.
func topologyActivityAnswer(masterEnabled bool, tier int64, hasText bool) captureAnswer {
	return captureAnswer{
		columns: []string{"master_enabled", "custom_text_tier", "has_custom_text"},
		rows:    [][]driver.Value{{masterEnabled, tier, hasText}},
	}
}

func acceptedEdgeAnswer(exists bool) captureAnswer {
	return captureAnswer{columns: []string{"exists"}, rows: [][]driver.Value{{exists}}}
}

func noActiveScopes() captureAnswer {
	return captureAnswer{columns: []string{"server_id", "channel_id", "lifecycle_event_at"}}
}

// oneActiveScope is one live Server Voice row for activeVoiceScopes. The UUIDs
// cross the driver boundary as strings, which is what uuid.UUID's sql.Scanner
// accepts; lifecycle_event_at is NULL, and the plan simply carries a zero
// EventAt for it.
func oneActiveScope(serverID, channelID uuid.UUID) captureAnswer {
	return captureAnswer{
		columns: []string{"server_id", "channel_id", "lifecycle_event_at"},
		rows:    [][]driver.Value{{serverID.String(), channelID.String(), nil}},
	}
}

// serverTierPolicySettings is presence.loadPolicySettings' row shape with the
// sender at TierServers, which makes serverVoiceCandidates a straight copy of
// the server members and needs no friendship read at all.
func serverTierPolicySettings() captureAnswer {
	return captureAnswer{
		columns: []string{
			"master_enabled", "server_voice_tier", "server_voice_show_details",
			"private_call_tier", "private_call_show_details",
		},
		rows: [][]driver.Value{{true, int64(presence.TierServers), true, int64(presence.TierOff), false}},
	}
}

// serverMembersAnswer scripts n distinct members for allServerMembers. The rows
// are driver.Values, so n = maxCapturedViewers+1 costs one slice of strings
// rather than a database.
func serverMembersAnswer(n int) captureAnswer {
	rows := make([][]driver.Value, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, []driver.Value{uuid.New().String()})
	}
	return captureAnswer{columns: []string{"user_id"}, rows: rows}
}

// permittedSenderPresence is a DETERMINED permit on both forms of the resolver.
// CaptureServerVoiceCandidatesStrict short-circuits to an empty audience for a
// nil resolver, so the scripted reconciler needs one wired before any leg can
// carry viewers at all.
type permittedSenderPresence struct{}

func (permittedSenderPresence) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return true
}

func (p permittedSenderPresence) RichPresenceEmissionState(
	ctx context.Context, senderID uuid.UUID,
) (bool, error) {
	return p.RichPresenceEmissionPermitted(ctx, senderID), nil
}

func (c captureConnector) Connect(context.Context) (driver.Conn, error) {
	return captureConn(c), nil
}

func (c captureConnector) Driver() driver.Driver { return c }

func (c captureConnector) Open(string) (driver.Conn, error) {
	return c.Connect(context.Background())
}

type captureConn captureConnector

func (captureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (captureConn) Close() error { return nil }

func (captureConn) Begin() (driver.Tx, error) { return captureTx{}, nil }

func (c captureConn) answer(query string) (driver.Rows, error) {
	label := statementLabel(query)
	c.trace.record(label)
	scripted, ok := c.answers[label]
	if !ok {
		return nil, fmt.Errorf("unscripted statement: %s", label)
	}
	if scripted.err != nil {
		return nil, scripted.err
	}
	return &scriptedRows{columns: scripted.columns, rows: scripted.rows}, nil
}

func (c captureConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	return c.answer(query)
}

func (c captureConn) ExecContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Result, error) {
	rows, err := c.answer(query)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		_ = rows.Close()
	}
	return driver.RowsAffected(0), nil
}

type captureTx struct{}

func (captureTx) Commit() error   { return nil }
func (captureTx) Rollback() error { return nil }

type scriptedRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (r *scriptedRows) Columns() []string { return r.columns }

func (r *scriptedRows) Close() error { return nil }

func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.index])
	r.index++
	return nil
}

// scriptedCapture wires a reconciler, a rail stub and an open transaction over
// the scripted driver, all sharing one trace.
func scriptedCapture(
	t *testing.T, answers map[string]captureAnswer,
) (*Reconciler, *railStub, *sql.Tx, *callTrace) {
	t.Helper()
	trace := &callTrace{}
	scripted := sql.OpenDB(captureConnector{trace: trace, answers: answers})
	t.Cleanup(func() {
		if err := scripted.Close(); err != nil {
			t.Errorf("close scripted database: %v", err)
		}
	})
	tx, err := scripted.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin scripted transaction")
	t.Cleanup(func() { _ = tx.Rollback() })

	rail := &railStub{trace: trace}
	r := &Reconciler{db: scripted}
	r.SetTopologyRail(rail)
	return r, rail, tx, trace
}

func blockSubject(principal, counterpart uuid.UUID) presencecapture.Subject {
	return presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   principal,
		Counterpart: counterpart,
	}
}

// ─── the savepoint statements ───────────────────────────────────────────────

func TestSavepointStatementsAreDistinctFixedLiterals(t *testing.T) {
	assert.Equal(t, "SAVEPOINT concord_graph_presence_gate", gateSavepoint.open)
	assert.Equal(t, "ROLLBACK TO SAVEPOINT concord_graph_presence_gate", gateSavepoint.rollback)
	assert.Equal(t, "RELEASE SAVEPOINT concord_graph_presence_gate", gateSavepoint.release)

	assert.Equal(t, "SAVEPOINT concord_graph_presence_capture", captureSavepoint.open)
	assert.Equal(t, "ROLLBACK TO SAVEPOINT concord_graph_presence_capture", captureSavepoint.rollback)
	assert.Equal(t, "RELEASE SAVEPOINT concord_graph_presence_capture", captureSavepoint.release)

	// Two savepoints, not one. A single savepoint is wrong in BOTH possible
	// placements: opened before the accepted-edge gate it would roll the markers
	// back with the degrade, and opened after it would leave the gate read
	// unprotected and make BlockUser's declared degrade posture inert.
	assert.NotEqual(t, gateSavepoint.open, captureSavepoint.open)

	// The operator-facing labels. beginSavepoint is generic over both statement
	// sets, so these are the only thing that tells a 500 at step 2 (clean
	// transaction, no markers) from one at step 7 (markers already written).
	assert.Equal(t, "gate", gateSavepoint.name)
	assert.Equal(t, "capture", captureSavepoint.name)
	assert.NotEqual(t, gateSavepoint.name, captureSavepoint.name)
}

func TestBeginSavepointSkipsEveryRoundTripForTheFailClosedPosture(t *testing.T) {
	r, _, tx, trace := scriptedCapture(t, savepointAnswers())
	subject := presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		FailPosture: presencecapture.FailClosedBlockWrite,
		Principal:   uuid.New(),
	}

	rollback, release, err := r.beginSavepoint(context.Background(), tx, subject, gateSavepoint)
	require.NoError(t, err)
	assert.NoError(t, rollback(), "the fail-closed rollback must be a no-op")
	assert.NoError(t, release(), "the fail-closed release must be a no-op")
	assert.Empty(t, trace.snapshot(),
		"a fail-closed site blocks the write on a read failure by design, so it "+
			"has nothing to restore and must spend no round trip")
}

func TestBeginSavepointIssuesTheStatementsOfTheSavepointItWasHanded(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statements savepointStatements
		wantOpen   string
		wantBack   string
		wantDone   string
	}{
		{"gate", gateSavepoint, stmtGateOpen, stmtGateRollback, stmtGateRelease},
		{"capture", captureSavepoint, stmtCaptureOpen, stmtCaptureRollback, stmtCaptureRelease},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, tx, trace := scriptedCapture(t, savepointAnswers())
			rollback, release, err := r.beginSavepoint(
				context.Background(), tx, blockSubject(uuid.New(), uuid.New()), tc.statements,
			)
			require.NoError(t, err)
			require.NoError(t, rollback())
			require.NoError(t, release())
			assert.Equal(t, []string{tc.wantOpen, tc.wantBack, tc.wantDone}, trace.snapshot())
		})
	}
}

// Each failure names the savepoint that actually failed. The two sites mean
// OPPOSITE things to an operator reading a BlockUser 500: a gate failure means
// no marker was ever written and the transaction is clean, a capture failure
// means BeginTopologyBatch already wrote durable rows for both principals into
// a transaction that is now aborting. Every wrap used to say "capture", so the
// two were indistinguishable in the log.
func TestBeginSavepointFailsClosedWhenAnyOfItsStatementsFails(t *testing.T) {
	boom := errors.New("savepoint refused")

	for _, sp := range []struct {
		statements savepointStatements
		open       string
		rollback   string
		release    string
	}{
		{gateSavepoint, stmtGateOpen, stmtGateRollback, stmtGateRelease},
		{captureSavepoint, stmtCaptureOpen, stmtCaptureRollback, stmtCaptureRelease},
	} {
		t.Run(sp.statements.name+"/open", func(t *testing.T) {
			answers := savepointAnswers()
			answers[sp.open] = captureAnswer{err: boom}
			r, _, tx, _ := scriptedCapture(t, answers)

			rollback, release, err := r.beginSavepoint(
				context.Background(), tx, blockSubject(uuid.New(), uuid.New()), sp.statements,
			)
			require.Error(t, err,
				"without a savepoint the degrade branch cannot restore the transaction")
			assert.ErrorContains(t, err, "open "+sp.statements.name+" savepoint")
			assert.Nil(t, rollback)
			assert.Nil(t, release)
		})

		t.Run(sp.statements.name+"/rollback", func(t *testing.T) {
			answers := savepointAnswers()
			answers[sp.rollback] = captureAnswer{err: boom}
			r, _, tx, _ := scriptedCapture(t, answers)

			rollback, _, err := r.beginSavepoint(
				context.Background(), tx, blockSubject(uuid.New(), uuid.New()), sp.statements,
			)
			require.NoError(t, err)
			assert.ErrorContains(t, rollback(), "restore "+sp.statements.name+" savepoint")
		})

		t.Run(sp.statements.name+"/release", func(t *testing.T) {
			answers := savepointAnswers()
			answers[sp.release] = captureAnswer{err: boom}
			r, _, tx, _ := scriptedCapture(t, answers)

			_, release, err := r.beginSavepoint(
				context.Background(), tx, blockSubject(uuid.New(), uuid.New()), sp.statements,
			)
			require.NoError(t, err)
			assert.ErrorContains(t, release(), "release "+sp.statements.name+" savepoint")
		})
	}
}

// ─── the normative order ────────────────────────────────────────────────────

// This is the task. The durable marker write must land AFTER the gate savepoint
// is released and BEFORE the capture savepoint is opened, so that no degrade
// can roll it back and no gate read runs unprotected. The single slice
// comparison rejects every other placement, including the two single-savepoint
// shapes.
func TestCaptureWritesTheMarkersBetweenTheTwoSavepoints(t *testing.T) {
	answers := savepointAnswers()
	answers[stmtAcceptedEdge] = acceptedEdgeAnswer(true)
	answers[stmtTopologyActivity] = topologyActivityAnswer(false, 0, false)
	answers[stmtActiveScopes] = noActiveScopes()

	r, rail, tx, trace := scriptedCapture(t, answers)
	principal, counterpart := uuid.New(), uuid.New()

	plan, err := r.CaptureInTx(context.Background(), tx, blockSubject(principal, counterpart))
	require.NoError(t, err, "CaptureInTx")
	require.NotNil(t, plan)

	assert.Equal(t, []string{
		stmtGateOpen,
		stmtAcceptedEdge,
		stmtGateRelease,
		stepBeginTopologyBatch,
		stmtTopologyActivity,
		stmtTopologyActivity,
		stmtCaptureOpen,
		stmtActiveScopes,
		stmtActiveScopes,
		stmtCaptureRelease,
	}, trace.snapshot())

	assert.Equal(t, []uuid.UUID{principal, counterpart}, rail.beganFor,
		"the batch is written for the focal set, both principals")
}

// The gate is the #2738 security fix, and now it also decides whether a durable
// row that suppresses a stranger's Custom Status gets written at all. A
// proven-no-change mutation must leave NO markers.
func TestCaptureWritesNoMarkersWhenNoAcceptedEdgeExists(t *testing.T) {
	answers := savepointAnswers()
	answers[stmtAcceptedEdge] = acceptedEdgeAnswer(false)

	r, rail, tx, trace := scriptedCapture(t, answers)

	plan, err := r.CaptureInTx(
		context.Background(), tx, blockSubject(uuid.New(), uuid.New()),
	)
	require.NoError(t, err)
	assert.False(t, plan.HasWork(), "a proven-no-change mutation reconciles nothing")
	assert.False(t, plan.Degraded())
	assert.Zero(t, rail.beginCalls, "no edge destroyed means no marker written")
	assert.Equal(t, []string{stmtGateOpen, stmtAcceptedEdge}, trace.snapshot())
}

// INVARIANT TB-1. Once BeginTopologyBatch has been called the batch is never
// dropped and never conditioned on FailPosture: a C1 degrade yields a degraded
// active leg PLUS a fully valid C2 batch in one Plan. The rollback must reach
// the CAPTURE savepoint and never the gate one, or the markers the returned
// plan references would no longer exist.
func TestCaptureDegradesTheActiveLegAndStillCarriesTheTopologyBatch(t *testing.T) {
	answers := savepointAnswers()
	answers[stmtAcceptedEdge] = acceptedEdgeAnswer(true)
	answers[stmtTopologyActivity] = topologyActivityAnswer(false, 0, false)
	answers[stmtActiveScopes] = captureAnswer{err: errors.New("relation does not exist")}

	r, rail, tx, trace := scriptedCapture(t, answers)
	principal, counterpart := uuid.New(), uuid.New()

	plan, err := r.CaptureInTx(context.Background(), tx, blockSubject(principal, counterpart))
	require.NoError(t, err, "FailConservativeDegrade must not block the block")
	require.True(t, plan.Degraded(), "the C1 leg degraded")

	concrete, ok := plan.(*Plan)
	require.True(t, ok, "the degraded plan is this package's own")
	assert.True(t, concrete.hasTopology, "TB-1: the batch survives a C1 degrade")
	assert.Equal(t, []uuid.UUID{principal, counterpart}, concrete.topologySenders)
	assert.Len(t, concrete.topologyBefore, 2)
	assert.Equal(t, 1, rail.beginCalls)

	steps := trace.snapshot()
	assert.Contains(t, steps, stmtCaptureRollback,
		"the C1 degrade restores the transaction from the capture savepoint")
	assert.NotContains(t, steps, stmtGateRollback,
		"rolling back to the gate savepoint would erase the markers the plan references")
}

// INVARIANT TB-1, at the OTHER exit. CaptureInTx replaces the plan at two
// points after the markers are written — the step-8 C1 degrade above and this
// step-9 bound — and each carries the batch onto its replacement independently.
// The degrade test cannot see this one: deleting step 9's carryTopology leaves
// it green, and production then returns a plan with hasTopology false while the
// durable markers are already written, so nothing completes them and they
// suppress the sender's Custom Status for every reconnecting viewer until the
// grace window expires.
//
// The two trailing trace assertions are what pin this to the BOUND exit rather
// than the degrade exit: the capture savepoint is RELEASED here, never rolled
// back, because no read failed.
func TestCaptureCarriesTheTopologyBatchOntoTheBoundedPlan(t *testing.T) {
	serverID, channelID := uuid.New(), uuid.New()

	answers := savepointAnswers()
	answers[stmtAcceptedEdge] = acceptedEdgeAnswer(true)
	answers[stmtTopologyActivity] = topologyActivityAnswer(false, 0, false)
	answers[stmtActiveScopes] = oneActiveScope(serverID, channelID)
	answers[stmtPolicySettings] = serverTierPolicySettings()
	// One over the bound, so the union countCaptured measures exceeds it on the
	// first focal sender alone and does not depend on how the two principals'
	// audiences overlap.
	answers[stmtServerMembers] = serverMembersAnswer(maxCapturedViewers + 1)

	r, rail, tx, trace := scriptedCapture(t, answers)
	r.senderPresence = permittedSenderPresence{}
	principal, counterpart := uuid.New(), uuid.New()

	plan, err := r.CaptureInTx(context.Background(), tx, blockSubject(principal, counterpart))
	require.NoError(t, err, "the bound is a bound, not a failure")

	concrete, ok := plan.(*Plan)
	require.True(t, ok, "the bounded plan is this package's own")
	require.True(t, plan.Degraded(),
		"precondition: block can revoke, so the bound resolves to the conservative plan")
	assert.True(t, concrete.hasTopology, "TB-1: the batch survives the bound")
	assert.Equal(t, []uuid.UUID{principal, counterpart}, concrete.topologySenders)
	assert.Len(t, concrete.topologyBefore, 2)
	assert.Equal(t, 1, rail.beginCalls)

	steps := trace.snapshot()
	assert.Contains(t, steps, stmtCaptureRelease,
		"the bound is reached AFTER a clean capture, so the savepoint is released")
	assert.NotContains(t, steps, stmtCaptureRollback,
		"this is the bound exit, not the C1 degrade exit")
}

// The marker stage fails CLOSED under every posture — including the one posture
// that exists to keep a block from being denied. BeginTopologyBatch returns
// mid-loop with earlier senders' markers already written, and no savepoint is
// open around it, so the only way to undo them is to abort the whole
// transaction. Returning a degraded plan instead would leave durable markers
// behind with nothing that resolves them.
func TestCaptureFailsClosedWhenTheMarkerWriteFails(t *testing.T) {
	t.Run("an ordinary rail failure blocks the write", func(t *testing.T) {
		answers := savepointAnswers()
		answers[stmtAcceptedEdge] = acceptedEdgeAnswer(true)

		r, rail, tx, trace := scriptedCapture(t, answers)
		boom := errors.New("marker write refused")
		rail.beginErr = boom

		plan, err := r.CaptureInTx(
			context.Background(), tx, blockSubject(uuid.New(), uuid.New()),
		)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
		assert.Nil(t, plan)
		assert.NotContains(t, trace.snapshot(), stmtCaptureOpen,
			"the capture stage must not run once the markers are unknown")
	})

	t.Run("a pending terminal is translated for the handler", func(t *testing.T) {
		answers := savepointAnswers()
		answers[stmtAcceptedEdge] = acceptedEdgeAnswer(true)

		r, rail, tx, _ := scriptedCapture(t, answers)
		rail.beginErr = &presencehistory.ServiceError{
			Status:     http.StatusServiceUnavailable,
			Code:       "presence_operation_pending",
			RetryAfter: 7 * time.Second,
		}

		_, err := r.CaptureInTx(
			context.Background(), tx, blockSubject(uuid.New(), uuid.New()),
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, presencecapture.ErrCapturePending),
			"handlers classify on internal/presencecapture, never on presencehistory")
		var pending *presencecapture.PendingError
		require.True(t, errors.As(err, &pending))
		assert.Equal(t, 7*time.Second, pending.After)
	})
}

// The #1234 recipient exceptions are the FINAL filter and an overrides read that
// fails must fail the WRITE, not silently yield an unfiltered audience. C2 has
// no staleness horizon — Custom Status is not republished on a heartbeat and
// carries no TTL — so degrading here would commit a friendship write whose
// Custom Status revocation nothing would ever deliver.
func TestCaptureFailsClosedWhenTheBeforeAudienceReadFails(t *testing.T) {
	answers := savepointAnswers()
	answers[stmtAcceptedEdge] = acceptedEdgeAnswer(true)
	// Active sender: the audience computation runs, and its first query is not
	// scripted, so it fails exactly as a poisoned read would.
	answers[stmtTopologyActivity] = topologyActivityAnswer(true, 1, true)

	r, rail, tx, trace := scriptedCapture(t, answers)

	plan, err := r.CaptureInTx(
		context.Background(), tx, blockSubject(uuid.New(), uuid.New()),
	)
	require.Error(t, err, "the degrade posture must not reach the C2 leg")
	assert.ErrorContains(t, err, "compute topology before audience")
	assert.Nil(t, plan)
	assert.Equal(t, 1, rail.beginCalls, "the markers were already written")
	assert.NotContains(t, trace.snapshot(), stmtCaptureOpen)
}

// An unregistered family has no declared policy, so the capture cannot know
// whether the mutation revokes visibility or carries Custom Status topology.
// It fails CLOSED before any statement, under every posture.
func TestCaptureFailsClosedForAnUnregisteredFamily(t *testing.T) {
	r, rail, tx, trace := scriptedCapture(t, savepointAnswers())
	subject := blockSubject(uuid.New(), uuid.New())
	subject.Family = presencecapture.Family(200)

	plan, err := r.CaptureInTx(context.Background(), tx, subject)
	require.Error(t, err)
	assert.ErrorIs(t, err, presencecapture.ErrFamilyUnregistered)
	assert.Nil(t, plan)
	assert.Zero(t, rail.beginCalls)
	assert.Empty(t, trace.snapshot(), "nothing may run before the family is known")
}

// A Subject naming no principal at all is a caller bug, and the answer must not
// depend on whether the rail happens to be wired. Without this guard the same
// input returns a clean empty plan on an unwired replica and 500s on a wired one
// with presencehistory's "invalid topology sender batch" — an error that names
// neither CaptureInTx nor the empty Subject. Blocking here is not the
// short-circuit-to-empty-plan alternative: that would commit the mutation with
// the markers silently skipped.
func TestCaptureFailsClosedForASubjectWithNoPrincipal(t *testing.T) {
	r, rail, tx, trace := scriptedCapture(t, savepointAnswers())
	subject := blockSubject(uuid.Nil, uuid.Nil)

	plan, err := r.CaptureInTx(context.Background(), tx, subject)
	require.Error(t, err)
	assert.ErrorContains(t, err, "at least one principal")
	assert.Nil(t, plan)
	assert.Zero(t, rail.beginCalls, "no principal means no durable marker")
	assert.Empty(t, trace.snapshot(), "the guard runs before any statement")
}

// ─── the re-derived activity predicate ──────────────────────────────────────

// readTopologyActivity re-derives prepareTopologyPlan's
// `BeforeMasterEnabled && BeforeTier > 0 && Before.Text != ""` from the settings
// row BeginTopologyBatch just locked FOR UPDATE, so the batch stays opaque to
// this package. If these two drift, PrepareTopologyBatch fails closed with
// "inactive topology operation has audience" — that is the escalation
// checkpoint, not a bug to paper over with an accessor.
func TestReadTopologyActivityMirrorsPrepareTopologyPlansPredicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		master     bool
		tier       int64
		hasText    bool
		wantActive bool
		wantTier   int
	}{
		{"master on, tier friends, text present", true, 1, true, true, 1},
		{"master on, tier servers, text present", true, 2, true, true, 2},
		{"master off suppresses everything", false, 2, true, false, 2},
		{"tier off is not an audience", true, 0, true, false, 0},
		{"no text means nothing to revoke", true, 1, false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := savepointAnswers()
			answers[stmtTopologyActivity] = topologyActivityAnswer(tc.master, tc.tier, tc.hasText)
			_, _, tx, _ := scriptedCapture(t, answers)

			active, tier, err := readTopologyActivity(context.Background(), tx, uuid.New())
			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, active)
			assert.Equal(t, tc.wantTier, tier)
		})
	}
}

func TestReadTopologyActivityTreatsAMissingRowAsInactive(t *testing.T) {
	answers := savepointAnswers()
	answers[stmtTopologyActivity] = captureAnswer{
		columns: []string{"master_enabled", "custom_text_tier", "has_custom_text"},
	}
	_, _, tx, _ := scriptedCapture(t, answers)

	active, tier, err := readTopologyActivity(context.Background(), tx, uuid.New())
	require.NoError(t, err, "a missing row is inactive, not an error")
	assert.False(t, active)
	assert.Zero(t, tier)
}

func TestReadTopologyActivityFailsClosedOnAReadError(t *testing.T) {
	answers := savepointAnswers()
	answers[stmtTopologyActivity] = captureAnswer{err: errors.New("poisoned")}
	_, _, tx, _ := scriptedCapture(t, answers)

	active, tier, err := readTopologyActivity(context.Background(), tx, uuid.New())
	require.Error(t, err)
	assert.ErrorContains(t, err, "read topology activity predicate")
	assert.False(t, active)
	assert.Zero(t, tier)
}

func TestCaptureTopologyBeforeLeavesAnInactiveSenderWithNoAudience(t *testing.T) {
	answers := savepointAnswers()
	answers[stmtTopologyActivity] = topologyActivityAnswer(false, 2, true)
	r, _, tx, _ := scriptedCapture(t, answers)
	sender := uuid.New()

	before, err := r.captureTopologyBefore(context.Background(), tx, []uuid.UUID{sender})
	require.NoError(t, err)
	require.Contains(t, before, sender)
	assert.Nil(t, before[sender],
		"prepareTopologyPlan rejects a non-empty audience on an inactive operation")
}

// ─── TB-1, at the helper ────────────────────────────────────────────────────

// TestDegradedPlanCarriesTheTopologyBatch is the TB-1 unit lock on the carry
// itself. TestCaptureDegradesTheActiveLegAndStillCarriesTheTopologyBatch proves
// CaptureInTx actually performs it.
func TestDegradedPlanCarriesTheTopologyBatch(t *testing.T) {
	reconciler := &Reconciler{}
	subject := blockSubject(uuid.New(), uuid.New())
	sender := subject.Principal
	viewer := uuid.New()

	captured := &Plan{
		subject:         subject,
		hasTopology:     true,
		topologySenders: []uuid.UUID{sender},
		topologyBefore:  map[uuid.UUID]map[uuid.UUID]bool{sender: {viewer: true}},
	}

	degraded := reconciler.degradePlan(subject, causeActiveScopeRead)
	carryTopology(captured, degraded)

	assert.True(t, degraded.Degraded())
	assert.True(t, degraded.hasTopology)
	assert.Equal(t, []uuid.UUID{sender}, degraded.topologySenders)
	assert.Equal(t, map[uuid.UUID]bool{viewer: true}, degraded.topologyBefore[sender])
}

func TestCarryTopologyIsANoOpWhenNoBatchWasWritten(t *testing.T) {
	subject := presencecapture.Subject{Family: presencecapture.FamilyBlock, Principal: uuid.New()}
	source := &Plan{subject: subject}
	target := &Plan{subject: subject, degraded: true}

	carryTopology(source, target)

	assert.False(t, target.hasTopology)
	assert.Nil(t, target.topologySenders)
	assert.Nil(t, target.topologyBefore)
}

// ─── the After audience and the C2 terminal ─────────────────────────────────

// customTextSettingsAnswer is ComputeCustomTextAudience's OWN settings read:
// two columns, where readTopologyActivity's is three.
func customTextSettingsAnswer(masterEnabled bool, tier int64) captureAnswer {
	return captureAnswer{
		columns: []string{"master_enabled", "custom_text_tier"},
		rows:    [][]driver.Value{{masterEnabled, tier}},
	}
}

// userIDRowsAnswer is the single-column row shape presence.scanIDs reads. The
// UUIDs cross the driver boundary as strings, which is what uuid.UUID's
// sql.Scanner accepts.
func userIDRowsAnswer(column string, ids ...uuid.UUID) captureAnswer {
	rows := make([][]driver.Value, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, []driver.Value{id.String()})
	}
	return captureAnswer{columns: []string{column}, rows: rows}
}

// fofDisabledAnswer short-circuits friendsOfFriendsOf before its recursive
// query, so the After side needs four scripted statements rather than five.
func fofDisabledAnswer() captureAnswer {
	return captureAnswer{
		columns: []string{"dm_friends_of_friends"},
		rows:    [][]driver.Value{{false}},
	}
}

// afterAudienceAnswers scripts one ACTIVE sender at TierFriends whose friend
// set is `friends` and whose materialized #1234 exclusions are `excluded`.
func afterAudienceAnswers(friends, excluded []uuid.UUID) map[string]captureAnswer {
	return map[string]captureAnswer{
		stmtCustomTextSettings:  customTextSettingsAnswer(true, int64(presence.TierFriends)),
		stmtFriends:             userIDRowsAnswer("friend_id", friends...),
		stmtFoFFlag:             fofDisabledAnswer(),
		stmtCustomTextOverrides: userIDRowsAnswer("target_user_id", excluded...),
	}
}

// recordingSink is a SYNCHRONOUS dispatchSink. The production sink hands the
// plan to a background worker, which would make every terminal assertion below
// a race against that goroutine rather than a statement about the terminal.
type recordingSink struct {
	plans []*Plan
}

func (s *recordingSink) Enqueue(plan *Plan) { s.plans = append(s.plans, plan) }

func (s *recordingSink) Close() {}

var _ dispatchSink = (*recordingSink)(nil)

// terminalReconciler wires the four collaborators a C2 terminal touches: the
// rail it delegates the commit to, the sink the C1 leg is enqueued on, the
// disconnector the fail-closed terminal clears through, and the log.
func terminalReconciler(
	t *testing.T,
	completion *presencehistory.TopologyCompletion,
	logBuf *bytes.Buffer,
) (*Reconciler, *railStub, *recordingSink, *fakeDisconnector) {
	t.Helper()
	rail := &railStub{trace: &callTrace{}, completion: completion}
	sink := &recordingSink{}
	disconnector := &fakeDisconnector{}
	r := &Reconciler{sink: sink, disconnector: disconnector}
	if logBuf != nil {
		r.log = logger.NewWithWriter(logBuf)
	}
	r.SetTopologyRail(rail)
	return r, rail, sink, disconnector
}

// topologyPlan is a plan carrying a batch for one sender plus one peripheral
// viewer, so HasWork() is true and the C1 enqueue is observable.
func topologyPlan(sender, viewer uuid.UUID, before map[uuid.UUID]bool) *Plan {
	return &Plan{
		hasTopology:     true,
		topologySenders: []uuid.UUID{sender},
		topologyBefore:  map[uuid.UUID]map[uuid.UUID]bool{sender: before},
		viewers:         map[uuid.UUID]bool{viewer: true},
	}
}

// The pairing is by KEY. BeginTopologyBatch sorts its senders by uuid.String()
// while topologySenders keeps the bridge's derivation order, so a positional
// zip against anything the batch yields would hand two senders each other's
// audience — and each sender's Custom Status to the wrong people.
func TestBuildTopologyAudiencesPairsEachSenderWithItsOwnSets(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	firstBefore, firstAfter := uuid.New(), uuid.New()
	secondBefore, secondAfter := uuid.New(), uuid.New()

	audiences := buildTopologyAudiences(
		[]uuid.UUID{first, second},
		map[uuid.UUID]map[uuid.UUID]bool{
			first:  {firstBefore: true},
			second: {secondBefore: true},
		},
		map[uuid.UUID]map[uuid.UUID]bool{
			first:  {firstAfter: true},
			second: {secondAfter: true},
		},
	)

	require.Len(t, audiences, 2, "one audience per operation is the coverage PrepareTopologyBatch requires")
	bySender := make(map[uuid.UUID]presencehistory.TopologyAudience, len(audiences))
	for _, audience := range audiences {
		bySender[audience.SenderID] = audience
	}
	assert.Equal(t, map[uuid.UUID]bool{firstBefore: true}, bySender[first].Before)
	assert.Equal(t, map[uuid.UUID]bool{firstAfter: true}, bySender[first].After)
	assert.Equal(t, map[uuid.UUID]bool{secondBefore: true}, bySender[second].Before)
	assert.Equal(t, map[uuid.UUID]bool{secondAfter: true}, bySender[second].After)
}

// nil is the only value prepareTopologyPlan accepts for an inactive operation,
// so it must survive the pairing rather than becoming an empty map.
func TestBuildTopologyAudiencesLeavesAnInactiveSenderNil(t *testing.T) {
	sender := uuid.New()

	audiences := buildTopologyAudiences(
		[]uuid.UUID{sender},
		map[uuid.UUID]map[uuid.UUID]bool{sender: nil},
		map[uuid.UUID]map[uuid.UUID]bool{sender: nil},
	)

	require.Len(t, audiences, 1)
	assert.Nil(t, audiences[0].Before)
	assert.Nil(t, audiences[0].After)
}

// The After side reads the CALLER's transaction and applies the #1234
// exclusions LAST. Both are asserted at once because they are the same
// property: the exported entry point is what runs the exclusion filter, and the
// unexported base audience — which does not — is the value a switch to
// computeCustomTextBaseAudienceForTier would produce here.
func TestTopologyAudiencesComputesTheAfterSideOnTheCallerTransaction(t *testing.T) {
	sender, kept, excluded := uuid.New(), uuid.New(), uuid.New()
	r, _, tx, trace := scriptedCapture(t,
		afterAudienceAnswers([]uuid.UUID{kept, excluded, sender}, []uuid.UUID{excluded}))
	// r.db must NOT be the handle the After side reads: it is another
	// connection and therefore another snapshot. This one cannot answer a query
	// at all, so a switch from tx to r.db fails the test rather than passing on
	// a read of committed state.
	r.db = openTracedDB(t, gatedTxConnector{trace: trace})

	plan := topologyPlan(sender, uuid.New(), map[uuid.UUID]bool{kept: true, excluded: true})
	audiences, err := r.topologyAudiences(context.Background(), tx, plan)

	require.NoError(t, err)
	require.Len(t, audiences, 1)
	assert.Equal(t, sender, audiences[0].SenderID)
	assert.Equal(t, map[uuid.UUID]bool{kept: true}, audiences[0].After,
		"the exclusion is the final filter and the sender is never in its own audience")
	assert.Equal(t, map[uuid.UUID]bool{kept: true, excluded: true}, audiences[0].Before,
		"the pre-mutation side is carried through untouched")
}

// An inactive operation rejects ANY audience, so the After side must stay nil —
// and no audience query may run for it at all.
func TestTopologyAudiencesSkipsTheDatabaseForAnInactiveSender(t *testing.T) {
	sender := uuid.New()
	r, _, tx, trace := scriptedCapture(t, map[string]captureAnswer{})

	plan := topologyPlan(sender, uuid.New(), nil)
	audiences, err := r.topologyAudiences(context.Background(), tx, plan)

	require.NoError(t, err)
	require.Len(t, audiences, 1)
	assert.Nil(t, audiences[0].After, "a friendship write cannot make an inactive Custom Status deliverable")
	assert.Empty(t, trace.snapshot(), "an inactive sender costs no round trip")
}

// C2 has no staleness horizon, so an overrides- or audience-read failure blocks
// the write instead of falling back to an unfiltered set.
//
// It drives the TERMINAL rather than topologyAudiences directly. Calling the
// inner function proved the error propagates but skipped completeTopology's own
// logTopologyFailure call, so topologyAudienceRead was the one class of the four
// that no test ever emitted — and its String() arm went with it.
func TestCompleteTopologyFailsClosedWhenTheAfterAudienceReadFails(t *testing.T) {
	sender, friend := uuid.New(), uuid.New()
	answers := afterAudienceAnswers([]uuid.UUID{friend}, nil)
	answers[stmtCustomTextOverrides] = captureAnswer{err: errors.New("overrides unavailable")}
	r, rail, tx, _ := scriptedCapture(t, answers)
	var logBuf bytes.Buffer
	r.log = logger.NewWithWriter(&logBuf)

	plan := topologyPlan(sender, uuid.New(), map[uuid.UUID]bool{friend: true})
	err := r.completeTopology(context.Background(), tx, plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "compute topology after audience")
	assert.Zero(t, rail.completeCalls,
		"a partial audience must never reach PrepareTopologyBatch, let alone the rail")

	logged := logBuf.String()
	assert.Contains(t, logged, "topology_audience_read", "the fixed failure_class enum is logged")
	assert.NotContains(t, logged, "overrides unavailable",
		"the driver error may not reach the log sink ([internal]rules/observability.md)")
	assert.NotContains(t, logged, sender.String(), "and no user ID may either")
	assert.NotContains(t, logged, friend.String())
}

func TestPostCommitDeliveryFailureIsAPostCommitSentinel(t *testing.T) {
	err := classifyTopologyCompletion(presencehistory.TopologyCompletion{
		Committed: true,
		Err:       errors.New("deliver custom text: hub unreachable"),
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, presencecapture.ErrPostCommitDelivery),
		"a durable mutation whose delivery failed is a 503, not a 500")
	assert.False(t, errors.Is(err, presencecapture.ErrCapturePending))
	assert.Contains(t, err.Error(), "hub unreachable", "the cause is preserved for the server-side log")
}

func TestUnprovenCommitIsNotAPostCommitSentinel(t *testing.T) {
	err := classifyTopologyCompletion(presencehistory.TopologyCompletion{
		Committed: false,
		Err:       errors.New("commit topology audience batch: connection reset"),
	})

	require.Error(t, err)
	assert.False(t, errors.Is(err, presencecapture.ErrPostCommitDelivery),
		"an unproven commit must not be reported as a durable mutation")
}

func TestSuccessfulCompletionIsNil(t *testing.T) {
	assert.NoError(t, classifyTopologyCompletion(
		presencehistory.TopologyCompletion{Committed: true}))
}

// {Committed: false, Err: nil} is unreachable through *presencehistory.Service,
// which is exactly why the arm is worth pinning: if that ever became reachable,
// classifying it as success would report a durable mutation for a commit nobody
// proved — the one direction TopologyCompletion's own contract forbids.
func TestACompletionThatProvesNothingIsStillAnError(t *testing.T) {
	err := classifyTopologyCompletion(presencehistory.TopologyCompletion{})

	require.Error(t, err)
	assert.False(t, errors.Is(err, presencecapture.ErrPostCommitDelivery))
}

func TestCompleteTopologyBatchEnqueuesTheC1LegAfterASuccessfulCompletion(t *testing.T) {
	r, rail, sink, disconnector := terminalReconciler(t, nil, nil)
	sender, viewer := uuid.New(), uuid.New()
	plan := topologyPlan(sender, viewer, nil)

	err := r.completeTopologyBatch(
		context.Background(), nil, plan, presencehistory.TopologyBatch{})

	require.NoError(t, err)
	assert.Equal(t, 1, rail.completeCalls, "the rail owns the commit")
	require.Len(t, sink.plans, 1, "a topology batch says nothing about Server Voice, so C1 still runs")
	assert.Same(t, plan, sink.plans[0])
	assert.Empty(t, disconnector.recipients, "a delivered terminal disconnects nobody")
}

// The mutation IS durable here. Skipping the C1 leg would leave viewers who
// just lost Server Voice authorization holding it until the presence TTL, and
// disconnecting them would tear down sessions for a write that succeeded.
func TestCompleteTopologyBatchStillRunsTheC1LegWhenDeliveryFailsPostCommit(t *testing.T) {
	var logBuf bytes.Buffer
	r, _, sink, disconnector := terminalReconciler(t, &presencehistory.TopologyCompletion{
		Committed: true,
		Err:       errors.New("deliver custom text: hub unreachable"),
	}, &logBuf)
	sender, viewer := uuid.New(), uuid.New()
	plan := topologyPlan(sender, viewer, nil)

	err := r.completeTopologyBatch(
		context.Background(), nil, plan, presencehistory.TopologyBatch{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, presencecapture.ErrPostCommitDelivery))
	require.Len(t, sink.plans, 1, "the committed mutation's Server Voice leg must still be delivered")
	assert.Empty(t, disconnector.recipients, "a proven commit must not take the fail-closed teardown")

	logged := logBuf.String()
	assert.Contains(t, logged, "topology_post_commit_delivery", "the fixed failure_class enum is logged")
	assert.NotContains(t, logged, "hub unreachable",
		"the completion error may not reach the log sink ([internal]rules/observability.md)")
	assert.NotContains(t, logged, sender.String(), "and no user ID may either")
	assert.NotContains(t, logged, viewer.String())
}

// Committed == false covers a proven rollback AND an unresolved commit, so it
// is not proof that nothing landed: it fails closed over the captured audience.
func TestCompleteTopologyBatchFailsClosedWhenTheCommitIsNotProven(t *testing.T) {
	var logBuf bytes.Buffer
	r, _, sink, disconnector := terminalReconciler(t, &presencehistory.TopologyCompletion{
		Committed: false,
		Err:       errors.New("commit topology audience batch: connection reset"),
	}, &logBuf)
	sender, viewer := uuid.New(), uuid.New()
	plan := topologyPlan(sender, viewer, nil)

	err := r.completeTopologyBatch(
		context.Background(), nil, plan, presencehistory.TopologyBatch{})

	require.Error(t, err)
	assert.False(t, errors.Is(err, presencecapture.ErrPostCommitDelivery))
	assert.Empty(t, sink.plans, "an unproven commit must not dispatch as if it had landed")
	assert.Equal(t, map[uuid.UUID]bool{viewer: true}, disconnector.recipients,
		"unknown state disconnects the captured audience")

	logged := logBuf.String()
	assert.Contains(t, logged, "topology_commit_unproven")
	assert.NotContains(t, logged, "connection reset", "the driver error may not reach the log sink")
}

// A batch rejected by PrepareTopologyBatch is PRE-commit: the rail was never
// called, so the handler's deferred rollback discards the write and the markers
// with it. Abandoning here would convert one failed request into a teardown of
// the whole captured audience (#2738). The §3.5 escalation — "inactive topology
// operation has audience" — takes this same path.
func TestCompleteTopologyAbandonsNobodyWhenTheBatchIsRejected(t *testing.T) {
	var logBuf bytes.Buffer
	r, rail, sink, disconnector := terminalReconciler(t, nil, &logBuf)
	sender, viewer := uuid.New(), uuid.New()
	plan := topologyPlan(sender, viewer, nil)

	err := r.completeTopology(context.Background(), nil, plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prepare topology audience batch")
	assert.Zero(t, rail.completeCalls, "a rejected batch must never reach the rail")
	assert.Empty(t, sink.plans)
	assert.Empty(t, disconnector.recipients,
		"nothing committed, so no viewer's authorization changed and nobody is stale")
	assert.Contains(t, logBuf.String(), "topology_audience_coverage")
}

// Complete must route a topology-carrying plan to the rail INSTEAD of committing
// the transaction itself. Delete the branch and this test goes red twice: the
// bare tx.Commit lands in the trace, and the rejected batch's error disappears.
func TestCompleteDelegatesTheCommitOfATopologyPlanToTheRail(t *testing.T) {
	trace := &callTrace{}
	rail := &railStub{trace: trace}
	sink := &recordingSink{}
	db := openTracedDB(t, gatedTxConnector{trace: trace})
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin traced transaction")
	t.Cleanup(func() { _ = tx.Rollback() })
	r := &Reconciler{db: db, sink: sink, disconnector: &fakeDisconnector{}}
	r.SetTopologyRail(rail)

	plan := topologyPlan(uuid.New(), uuid.New(), nil)
	err = r.Complete(context.Background(), tx, plan)

	require.Error(t, err, "the stub rail's empty batch cannot be prepared")
	assert.NotContains(t, trace.snapshot(), stepCommit,
		"Complete must not commit alongside the rail that owns the commit")
	assert.Empty(t, sink.plans, "and must not dispatch a plan whose write it never resolved")
}

// A plan carrying a batch with NO rail to resolve it must not fall through to
// the bare commit. That path lands the markers and never calls
// CompleteTopologyBatchWithOutcome — invariant TB-1's failure mode, conditioned
// on a field instead of on FailPosture — so the sender's Custom Status stays
// suppressed for every reconnecting viewer until the pending-operation grace
// expires. Unreachable today; the guard is what keeps it that way.
func TestCompleteRefusesATopologyPlanWithNoRailToResolveIt(t *testing.T) {
	trace := &callTrace{}
	sink := &recordingSink{}
	db := openTracedDB(t, gatedTxConnector{trace: trace})
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin traced transaction")
	t.Cleanup(func() { _ = tx.Rollback() })
	r := &Reconciler{db: db, sink: sink, disconnector: &fakeDisconnector{}}

	plan := topologyPlan(uuid.New(), uuid.New(), nil)
	err = r.Complete(context.Background(), tx, plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "without a rail")
	assert.NotContains(t, trace.snapshot(), stepCommit,
		"committing here would strand the markers the batch was opened for")
	assert.Empty(t, sink.plans, "and dispatching would report a mutation nothing resolved")
}

// The pre-PR-2 path is unchanged for a plan with no batch: Complete commits it
// directly and the rail is never consulted.
func TestCompleteWithoutATopologyBatchCommitsDirectly(t *testing.T) {
	trace := &callTrace{}
	rail := &railStub{trace: trace}
	sink := &recordingSink{}
	db := openTracedDB(t, gatedTxConnector{trace: trace})
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin traced transaction")
	r := &Reconciler{db: db, sink: sink, disconnector: &fakeDisconnector{}}
	r.SetTopologyRail(rail)

	plan := &Plan{viewers: map[uuid.UUID]bool{uuid.New(): true}}
	require.NoError(t, r.Complete(context.Background(), tx, plan))

	assert.Contains(t, trace.snapshot(), stepCommit, "an unbatched plan still takes the bare commit")
	assert.Zero(t, rail.completeCalls, "and never reaches the durable rail")
	require.Len(t, sink.plans, 1, "a committed plan with work is dispatched")
}

// The nil-tx guard runs FIRST, so a foreign plan handed in with no transaction
// reports the transaction. Both terminals refuse; this pins the order so a
// future edit cannot make Complete dereference a plan it has not yet decided it
// can act on. The foreign-plan refusal itself is asserted against a REAL
// transaction by TestCompleteRejectsAForeignPlanWithoutCommitting, because what
// matters there is that the caller's write is not committed.
func TestCompleteRequiresATransactionBeforeItInspectsThePlan(t *testing.T) {
	r := &Reconciler{}

	err := r.Complete(context.Background(), nil, foreignPlan{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a transaction")
}

// Complete(nil) is reachable in production, not a defensive hypothetical:
// internal/users leaves plan nil on a privacy PATCH that supplies
// dm_friends_of_friends without changing it, and hands that nil straight to
// Complete. It must commit and enqueue nothing.
//
// CodeRabbit read this as a nil dereference in enqueue and asked for a guard
// before HasWork. There is no dereference -- HasWork guards its own receiver,
// which is idiomatic Go -- so no guard was added. The coverage was worth taking
// anyway: the path was reachable and nothing asserted it.
func TestCompleteWithANilPlanCommitsAndEnqueuesNothing(t *testing.T) {
	trace := &callTrace{}
	sink := &recordingSink{}
	db := openTracedDB(t, gatedTxConnector{trace: trace})
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin traced transaction")
	r := &Reconciler{db: db, sink: sink, disconnector: &fakeDisconnector{}}

	require.NotPanics(t, func() {
		require.NoError(t, r.Complete(context.Background(), tx, nil),
			"a nil plan is the benign terminal: it must still commit")
	})

	assert.Contains(t, trace.snapshot(), stepCommit, "the caller's transaction must be committed")
	assert.Empty(t, sink.plans, "a nil plan carries no C1 work to enqueue")
}

// ---------------------------------------------------------------------------
// #2992 — the revocation bracket.
// ---------------------------------------------------------------------------

// fenceStub stands in for the Hub's authorization fence. It records into the
// SHARED trace rather than counting, because what is under test is ORDER: the
// bracket must open after the gate and before BeginTx, and close after the
// transaction has resolved but before the gate is released. A counter cannot
// tell any of those apart.
type fenceStub struct {
	trace *callTrace
	opens int
	// depth is sampled by the work closure to prove the bracket is HELD across
	// the transaction rather than merely opened and dropped before it.
	depth        int
	depthAtWork  int
	disconnected int
}

func (f *fenceStub) BeginAudienceRevocation() func() {
	f.opens++
	f.depth++
	f.trace.record(stepFenceOpen)
	var once sync.Once
	return func() {
		once.Do(func() {
			f.depth--
			f.trace.record(stepFenceClose)
		})
	}
}

func (f *fenceStub) DisconnectRichPresenceClients(
	_ context.Context, _ map[uuid.UUID]bool,
) error {
	f.disconnected++
	return nil
}

func (f *fenceStub) DisconnectAllRichPresenceClients(_ context.Context) error {
	f.disconnected++
	return nil
}

var _ Disconnector = (*fenceStub)(nil)

func fencedReconciler(t *testing.T, trace *callTrace, rail TopologyRail) (*Reconciler, *fenceStub) {
	t.Helper()
	fence := &fenceStub{trace: trace}
	return &Reconciler{
		db:           openTracedDB(t, gatedTxConnector{trace: trace}),
		rail:         rail,
		disconnector: fence,
	}, fence
}

func subjectFor(family presencecapture.Family) presencecapture.Subject {
	return presencecapture.Subject{
		Family:      family,
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
	}
}

// The load-bearing ordering test. One slice comparison carries four separate
// duties, each of which is a real defect if inverted:
//
//   - fence_open AFTER gate_acquire — a bracket hoisted above rail.WithSenders
//     would be held across the Go-channel gate wait, blacking out presence for
//     the duration of gate contention rather than for the transaction.
//   - fence_open BEFORE begin_tx — the whole point. A bracket taken after the
//     transaction opens cannot order a query that started in between.
//   - fence_close AFTER the transaction resolved — the closer's defer must be
//     registered BEFORE the rollback defer so LIFO runs it LAST. Register it
//     after and the fence drops while the transaction is still live.
//   - fence_close BEFORE gate_release — it lives inside runInTx, not outside.
func TestWithGatedTxBracketsARevokingFamilyAcrossTheTransaction(t *testing.T) {
	trace := &callTrace{}
	rail := &railStub{trace: trace}
	reconciler, fence := fencedReconciler(t, trace, rail)

	err := reconciler.WithGatedTx(
		context.Background(), subjectFor(presencecapture.FamilyFriendshipRemove),
		func(_ *sql.Tx) error {
			fence.depthAtWork = fence.depth
			trace.record(stepWork)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t,
		[]string{
			stepGateAcquire, stepFenceOpen, stepBeginTx, stepWork,
			stepRollback, stepFenceClose, stepGateRelease,
		},
		trace.snapshot(),
		"the fence must open inside the gate and before BeginTx, and close after the "+
			"transaction resolves but before the gate is released")
	assert.Equal(t, 1, fence.depthAtWork,
		"the bracket must be HELD for the duration of the transaction, not opened and dropped before it")
	assert.Zero(t, fence.disconnected,
		"taking the bracket must not disconnect anyone — that is Abandon's job, not the fence's")
}

// Family scoping, driven off the boot-guarded registry rather than a hand list.
// Bracketing an additive family would suppress presence for the highest-volume
// graph traffic in exchange for nothing: an accept, an add and a join cannot
// remove a viewer from any audience.
func TestWithGatedTxSkipsTheBracketForAdditiveFamilies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family presencecapture.Family
	}{
		{"friendship accept", presencecapture.FamilyFriendshipAccept},
		{"member add", presencecapture.FamilyMemberAdd},
		{"member join", presencecapture.FamilyMemberJoin},
	} {
		family := tc.family
		t.Run(tc.name, func(t *testing.T) {
			policy, err := presencecapture.PolicyFor(family)
			require.NoError(t, err)
			require.False(t, policy.CanRevokeVisibility,
				"registry precondition: this test is only meaningful for a non-revoking family")

			trace := &callTrace{}
			reconciler, fence := fencedReconciler(t, trace, &railStub{trace: trace})

			require.NoError(t, reconciler.WithGatedTx(
				context.Background(), subjectFor(family),
				func(_ *sql.Tx) error { trace.record(stepWork); return nil },
			))

			assert.Zero(t, fence.opens, "an additive family must not take the bracket")
			assert.NotContains(t, trace.snapshot(), stepFenceOpen)
		})
	}
}

func TestWithGatedTxBracketsEveryRevokingFamily(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family presencecapture.Family
	}{
		{"friendship remove", presencecapture.FamilyFriendshipRemove},
		{"block", presencecapture.FamilyBlock},
		{"friends-of-friends toggle", presencecapture.FamilyFriendsOfFriendsToggle},
		{"member remove", presencecapture.FamilyMemberRemove},
		{"member ban", presencecapture.FamilyMemberBan},
	} {
		family := tc.family
		t.Run(tc.name, func(t *testing.T) {
			policy, err := presencecapture.PolicyFor(family)
			require.NoError(t, err)
			require.True(t, policy.CanRevokeVisibility,
				"registry precondition: this test is only meaningful for a revoking family")

			trace := &callTrace{}
			reconciler, fence := fencedReconciler(t, trace, &railStub{trace: trace})

			require.NoError(t, reconciler.WithGatedTx(
				context.Background(), subjectFor(family),
				func(_ *sql.Tx) error { trace.record(stepWork); return nil },
			))

			assert.Equal(t, 1, fence.opens, "every CanRevokeVisibility family must bracket")
			assert.Zero(t, fence.depth, "and must release it")
		})
	}
}

// The fail-closed arm, and it is the ONLY branch in this change whose inversion
// converts the control from fail-closed to fail-open. WithGatedTx brackets when
// the policy lookup ERRORS or the family can revoke:
//
//	if policy, policyErr := presencecapture.PolicyFor(subject.Family); policyErr != nil ||
//		policy.CanRevokeVisibility {
//
// Every other test in this file drives a REGISTERED family, so deleting
// `policyErr != nil ||` leaves all of them green -- an unregistered family is
// the only input that reaches this branch at all. An unknown family is exactly
// the case where we do NOT know the write is additive, so it must bracket.
// (@security-reviewer, PR #3010.)
func TestWithGatedTxBracketsAnUnregisteredFamilyFailClosed(t *testing.T) {
	const unregistered = presencecapture.Family(200)

	_, err := presencecapture.PolicyFor(unregistered)
	require.Error(t, err,
		"registry precondition: this family must be UNKNOWN to PolicyFor, or the "+
			"test exercises the CanRevokeVisibility arm instead and proves nothing")

	trace := &callTrace{}
	reconciler, fence := fencedReconciler(t, trace, &railStub{trace: trace})

	require.NoError(t, reconciler.WithGatedTx(
		context.Background(), subjectFor(unregistered),
		func(_ *sql.Tx) error { trace.record(stepWork); return nil },
	))

	assert.Equal(t, 1, fence.opens,
		"a family whose policy cannot be resolved must be bracketed -- failing OPEN "+
			"here would silently disable the fence for any future unregistered family")
	assert.Zero(t, fence.depth, "and the bracket must still be released")
}

// The catastrophic mode is a LEAKED bracket: a permanently non-zero open count
// suppresses base presence hub-wide, forever, and self-heals never. Cover every
// terminal, including the two that do not go through work at all.
func TestWithGatedTxReleasesTheBracketOnEveryTerminal(t *testing.T) {
	subject := subjectFor(presencecapture.FamilyFriendshipRemove)

	t.Run("work returns an error", func(t *testing.T) {
		trace := &callTrace{}
		reconciler, fence := fencedReconciler(t, trace, &railStub{trace: trace})
		err := reconciler.WithGatedTx(context.Background(), subject,
			func(_ *sql.Tx) error { return errors.New("boom") })
		require.Error(t, err)
		assert.Zero(t, fence.depth, "an error terminal must still release the bracket")
	})

	t.Run("work commits", func(t *testing.T) {
		trace := &callTrace{}
		reconciler, fence := fencedReconciler(t, trace, &railStub{trace: trace})
		require.NoError(t, reconciler.WithGatedTx(context.Background(), subject,
			func(tx *sql.Tx) error { return tx.Commit() }))
		assert.Zero(t, fence.depth, "the normal ErrTxDone path must still release")
	})

	t.Run("BeginTx fails", func(t *testing.T) {
		trace := &callTrace{}
		fence := &fenceStub{trace: trace}
		reconciler := &Reconciler{
			db: openTracedDB(t, gatedTxConnector{
				trace: trace, beginErr: errors.New("begin refused"),
			}),
			rail:         &railStub{trace: trace},
			disconnector: fence,
		}
		err := reconciler.WithGatedTx(context.Background(), subject,
			func(_ *sql.Tx) error { return nil })
		require.Error(t, err)
		assert.Zero(t, fence.depth,
			"a transaction that never opened must still release the bracket taken before it")
	})

	t.Run("work panics", func(t *testing.T) {
		trace := &callTrace{}
		reconciler, fence := fencedReconciler(t, trace, &railStub{trace: trace})
		assert.Panics(t, func() {
			_ = reconciler.WithGatedTx(context.Background(), subject,
				func(_ *sql.Tx) error { panic("boom") })
		})
		assert.Zero(t, fence.depth,
			"a panic must still release the bracket — defers run, and a leaked count "+
				"is a permanent hub-wide presence blackout")
	})
}

// An unwired disconnector is a replica without the hub. It must still run the
// transaction: degrading to the pre-#2992 behaviour is correct, panicking is not.
func TestWithGatedTxToleratesAnUnwiredFence(t *testing.T) {
	trace := &callTrace{}
	reconciler := &Reconciler{
		db:   openTracedDB(t, gatedTxConnector{trace: trace}),
		rail: &railStub{trace: trace},
	}

	require.NoError(t, reconciler.WithGatedTx(
		context.Background(), subjectFor(presencecapture.FamilyFriendshipRemove),
		func(_ *sql.Tx) error { trace.record(stepWork); return nil },
	))
	assert.NotContains(t, trace.snapshot(), stepFenceOpen)
}

// The three reaction rails own no audience-relation write, so there is no
// transaction to bracket and they correctly keep only the post-hoc epoch bump
// via DisconnectAllRichPresenceClients. "Deliberately not wired" and "forgotten"
// look identical in a diff; this asserts the former.
//
// This is a source inventory lock, NOT a logic test — the repo distrusts
// grep-shaped assertions as behavioural proof, and it is used here only to make
// the enumeration in the design's section 4.4 self-defending. Its failure
// message says what to do rather than what to assert.
func TestReactionRailsDoNotTakeTheAudienceBracket(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"voice lifecycle", "../voice/nats.go"},
		{"active category reconciler", "../activepresence/reconciler.go"},
		{"voice presence executor", "../voicepresence/executor.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile(tc.path)
			require.NoError(t, err, "the inventory must name a file that exists")
			assert.NotContains(t, string(src), "BeginAuthzRevocation",
				tc.path+" is a reaction to an anomaly, not a revoking write. If it has "+
					"gained a bracket it now owns a transaction, and this case is the "+
					"wrong answer — bracket it deliberately and delete this entry.")
			assert.NotContains(t, string(src), "BeginAudienceRevocation",
				tc.path+" must not take the fence directly either")
		})
	}
}
