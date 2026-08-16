package voicepresence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests guard the WIRING, not the loader.
//
// internal/presence covers the loader's own semantics thoroughly, but every
// one of those tests calls the resolver directly. None of them executes the
// loader through PrepareCapture — so without this file the entire #2681
// optimization could be reverted (drop the loader, call the nil-loader
// entry point) with every other test in the repository still green.
//
// executor_rows_test.go deliberately passes senderPresence == nil, which
// short-circuits capture before the member read. This driver supplies a
// permitting resolver and scripted settings rows so phase 1 runs all the way
// to the member read, and counts those reads.

const memberLoaderDriverName = "voicepresence-member-loader-test"

var memberLoaderDriverOnce sync.Once

const (
	memberLoaderChannelA = "77777777-7777-7777-7777-777777777777"
	memberLoaderChannelB = "88888888-8888-8888-8888-888888888888"
)

var memberLoaderEventAt = time.Unix(400, 0).UTC()

// memberLoaderState counts server_members reads across the whole pool and
// records the server each read named.
type memberLoaderState struct {
	mu         sync.Mutex
	reads      int
	readArgs   []uuid.UUID
	sendersPer int
}

func (s *memberLoaderState) countRead(serverID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	s.readArgs = append(s.readArgs, serverID)
}

func (s *memberLoaderState) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *memberLoaderState) readServerIDs() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.readArgs...)
}

var (
	memberLoaderRegistryMu sync.Mutex
	memberLoaderRegistry   = map[string]*memberLoaderState{}
)

type memberLoaderDriver struct{}

func (memberLoaderDriver) Open(scenario string) (driver.Conn, error) {
	memberLoaderRegistryMu.Lock()
	defer memberLoaderRegistryMu.Unlock()
	state, ok := memberLoaderRegistry[scenario]
	if !ok {
		return nil, errors.New("unknown scenario: " + scenario)
	}
	return &memberLoaderConn{state: state}, nil
}

type memberLoaderConn struct{ state *memberLoaderState }

func (*memberLoaderConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (*memberLoaderConn) Close() error              { return nil }
func (*memberLoaderConn) Begin() (driver.Tx, error) { return nil, errors.New("no transactions") }

func (c *memberLoaderConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "SELECT DISTINCT c.id"):
		return &scriptedRows{
			columns: []string{"id"},
			rows: [][]driver.Value{
				{memberLoaderChannelA},
				{memberLoaderChannelB},
			},
		}, nil

	case strings.Contains(query, "user_presence_settings"):
		return &memberLoaderSettingsRows{}, nil

	case strings.Contains(query, "server_members"):
		c.state.countRead(memberLoaderFirstUUID(args))
		return &memberLoaderIDRows{ids: []uuid.UUID{uuid.New()}}, nil

	case strings.Contains(query, "voice_participants"):
		// activeSenders, one call per channel. Fresh sender UUIDs each call so
		// the capture resolves several distinct senders per channel.
		channelID := memberLoaderChannelA
		if len(args) > 0 {
			if s, ok := args[0].Value.(string); ok {
				channelID = s
			}
		}
		rows := make([][]driver.Value, 0, c.state.sendersPer)
		for i := 0; i < c.state.sendersPer; i++ {
			rows = append(rows, []driver.Value{
				uuid.New().String(), channelID, memberLoaderEventAt,
			})
		}
		return &scriptedRows{
			columns: []string{"user_id", "channel_id", "lifecycle_event_at"},
			rows:    rows,
		}, nil

	default:
		return nil, errors.New("unexpected query: " + query)
	}
}

func memberLoaderFirstUUID(args []driver.NamedValue) uuid.UUID {
	if len(args) == 0 {
		return uuid.Nil
	}
	s, ok := args[0].Value.(string)
	if !ok {
		return uuid.Nil
	}
	parsed, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

type memberLoaderSettingsRows struct{ delivered bool }

func (*memberLoaderSettingsRows) Columns() []string {
	return []string{
		"master_enabled", "server_voice_tier", "server_voice_show_details",
		"private_call_tier", "private_call_show_details",
	}
}
func (*memberLoaderSettingsRows) Close() error { return nil }

func (r *memberLoaderSettingsRows) Next(values []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	values[0] = true     // master enabled
	values[1] = int64(2) // TierServers — no relationship reads
	values[2] = false
	values[3] = int64(1)
	values[4] = false
	return nil
}

type memberLoaderIDRows struct {
	ids   []uuid.UUID
	index int
}

func (*memberLoaderIDRows) Columns() []string { return []string{"user_id"} }
func (*memberLoaderIDRows) Close() error      { return nil }

func (r *memberLoaderIDRows) Next(values []driver.Value) error {
	if r.index >= len(r.ids) {
		return io.EOF
	}
	values[0] = r.ids[r.index].String()
	r.index++
	return nil
}

// permitAllSenders is the non-nil resolver that lets phase 1 reach the member
// read at all. executor_rows_test.go passes nil here on purpose; these tests
// need the opposite.
type permitAllSenders struct{}

func (permitAllSenders) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return true
}

func (d permitAllSenders) RichPresenceEmissionState(
	ctx context.Context, senderID uuid.UUID,
) (bool, error) {
	// Always DETERMINED — this double exercises the suppression path.
	return d.RichPresenceEmissionPermitted(ctx, senderID), nil
}

func newMemberLoaderExecutor(t *testing.T, sendersPerChannel int) (*Executor, *memberLoaderState) {
	t.Helper()
	memberLoaderDriverOnce.Do(func() {
		sql.Register(memberLoaderDriverName, memberLoaderDriver{})
	})
	scenario := t.Name()
	state := &memberLoaderState{sendersPer: sendersPerChannel}

	memberLoaderRegistryMu.Lock()
	memberLoaderRegistry[scenario] = state
	memberLoaderRegistryMu.Unlock()
	t.Cleanup(func() {
		memberLoaderRegistryMu.Lock()
		delete(memberLoaderRegistry, scenario)
		memberLoaderRegistryMu.Unlock()
	})

	db, err := sql.Open(memberLoaderDriverName, scenario)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	executor := NewExecutor(
		db, &refresherStub{}, &visibilityStub{},
		permitAllSenders{}, &disconnectorStub{}, testLogger(),
	)
	t.Cleanup(executor.Close)
	return executor, state
}

// THE guard. Two channels, three senders each — six senders through ONE
// PrepareCapture — must issue exactly ONE server_members read.
//
// Reverting #2681 (drop the memberLoader local, call the nil-loader
// CaptureServerVoiceCandidates) makes this six reads. No other test in the
// repository changes.
func TestPrepareCapture_SharesOneMemberReadAcrossEverySender(t *testing.T) {
	executor, state := newMemberLoaderExecutor(t, 3)
	serverID := uuid.New()

	plan, err := executor.PrepareCapture(
		context.Background(), serverID.String(), nil, nil,
	)
	require.NoError(t, err)

	typed, ok := plan.(*Plan)
	require.True(t, ok)
	require.Len(t, typed.Senders, 6,
		"precondition: 2 channels x 3 senders — if this is not 6 the read count proves nothing")

	assert.Equal(t, 1, state.readCount(),
		"six senders in one capture share ONE server_members read")
	assert.Equal(t, []uuid.UUID{serverID}, state.readServerIDs(),
		"and it names the capture's own server")
}

// The loader must not outlive its capture. This drives the REAL wiring, so it
// fails if a future change hoists the loader onto the Executor — which the
// internal/presence two-captures test cannot catch, because that one
// constructs both loaders itself.
func TestPrepareCapture_TwoCaptures_DoNotShareAMemberRead(t *testing.T) {
	executor, state := newMemberLoaderExecutor(t, 2)
	serverID := uuid.New()

	for capture := 0; capture < 2; capture++ {
		_, err := executor.PrepareCapture(
			context.Background(), serverID.String(), nil, nil,
		)
		require.NoError(t, err)
	}

	assert.Equal(t, 2, state.readCount(),
		"a loader is per-capture; membership between two mutations must be re-read")
}

// Distinct servers must not share a member read either — the loader is built
// from the capture's own serverID, and a hoisted or cached loader would show up
// here as a single read naming only the first server.
func TestPrepareCapture_DistinctServers_EachIssueTheirOwnMemberRead(t *testing.T) {
	executor, state := newMemberLoaderExecutor(t, 2)
	serverA := uuid.New()
	serverB := uuid.New()

	for _, serverID := range []uuid.UUID{serverA, serverB} {
		_, err := executor.PrepareCapture(
			context.Background(), serverID.String(), nil, nil,
		)
		require.NoError(t, err)
	}

	assert.Equal(t, []uuid.UUID{serverA, serverB}, state.readServerIDs(),
		"each capture reads its own server's membership, in order")
}
