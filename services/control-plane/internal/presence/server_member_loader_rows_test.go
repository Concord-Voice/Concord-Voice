package presence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A second scripted driver, separate from recheckRowsDriver in
// activity_recheck_rows_test.go. That one's default branch fails the members
// query on purpose — every scenario it serves is asserting the audience-read
// failure class — so it cannot serve a SUCCESSFUL counted read, which is the
// whole point here.
//
// The driver records the ARGUMENTS of each server_members read, not just its
// count: a count alone cannot tell a correct read from one issued against the
// wrong server, which is exactly the failure ErrCaptureLoaderServerMismatch
// exists to prevent.

const memberCountDriverName = "presence-server-member-count-test"

var memberCountDriverOnce sync.Once

var errMemberCountQuery = errors.New("forced server_members query failure")

// memberCountState counts server_members reads across every connection the pool
// opens, so the assertion is on reads ISSUED, not on connections used.
type memberCountState struct {
	mu       sync.Mutex
	reads    int
	readArgs []uuid.UUID // the server_id bound to each server_members read
	failRead bool
	masterOn bool
	tier     Tier
	memberID uuid.UUID
	friendID uuid.UUID
	// gate, when non-nil, decides per-call whether the settings row reports
	// presence as enabled. Used by the mixed-capture test.
	gate func(call int) bool
	// settingsCalls counts settings reads so gate can vary by sender.
	settingsCalls int
}

func (s *memberCountState) countRead(serverID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	s.readArgs = append(s.readArgs, serverID)
}

func (s *memberCountState) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *memberCountState) readServerIDs() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.readArgs...)
}

func (s *memberCountState) nextSettingsCall() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsCalls++
	return s.settingsCalls
}

var (
	memberCountRegistryMu sync.Mutex
	memberCountRegistry   = map[string]*memberCountState{}
)

type memberCountDriver struct{}

func (memberCountDriver) Open(scenario string) (driver.Conn, error) {
	memberCountRegistryMu.Lock()
	defer memberCountRegistryMu.Unlock()
	state, ok := memberCountRegistry[scenario]
	if !ok {
		return nil, errors.New("unknown scenario: " + scenario)
	}
	return &memberCountConn{state: state}, nil
}

type memberCountConn struct{ state *memberCountState }

func (*memberCountConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (*memberCountConn) Close() error              { return nil }
func (*memberCountConn) Begin() (driver.Tx, error) { return nil, errors.New("no transactions") }

func (c *memberCountConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "user_presence_settings"):
		return &memberCountSettingsRows{
			state: c.state,
			call:  c.state.nextSettingsCall(),
		}, nil
	case strings.Contains(query, "server_members"):
		c.state.countRead(firstUUIDArg(args))
		if c.state.failRead {
			return nil, errMemberCountQuery
		}
		return &memberIDRows{ids: []uuid.UUID{c.state.memberID}}, nil
	case strings.Contains(query, "privacy_settings"):
		// friendsOfFriendsOf gates on dm_friends_of_friends before querying.
		// Reporting it disabled keeps TierFriends resolving through friendsOf
		// alone, which is all these tests need — the member read is what is
		// under assertion, not the relationship fan-out.
		return &fofFlagRows{}, nil
	case strings.Contains(query, "friendships"):
		// Serves both friendsOf and friendsOfFriendsOf; each returns a single
		// uuid column, so one row shape covers both.
		return &memberIDRows{ids: []uuid.UUID{c.state.friendID}}, nil
	default:
		return nil, errors.New("unexpected query: " + query)
	}
}

type fofFlagRows struct{ delivered bool }

func (*fofFlagRows) Columns() []string { return []string{"dm_friends_of_friends"} }
func (*fofFlagRows) Close() error      { return nil }

func (r *fofFlagRows) Next(values []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	values[0] = false
	return nil
}

// firstUUIDArg extracts the first bound argument as a uuid, so a read can be
// attributed to the server it actually named. Returns uuid.Nil when the driver
// receives a shape it does not recognise.
func firstUUIDArg(args []driver.NamedValue) uuid.UUID {
	if len(args) == 0 {
		return uuid.Nil
	}
	switch v := args[0].Value.(type) {
	case string:
		parsed, err := uuid.Parse(v)
		if err != nil {
			return uuid.Nil
		}
		return parsed
	case []byte:
		parsed, err := uuid.ParseBytes(v)
		if err != nil {
			return uuid.Nil
		}
		return parsed
	default:
		return uuid.Nil
	}
}

type memberCountSettingsRows struct {
	state     *memberCountState
	call      int
	delivered bool
}

func (*memberCountSettingsRows) Columns() []string {
	return []string{
		"master_enabled", "server_voice_tier", "server_voice_show_details",
		"private_call_tier", "private_call_show_details",
	}
}

func (*memberCountSettingsRows) Close() error { return nil }

func (r *memberCountSettingsRows) Next(values []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	enabled := r.state.masterOn
	if r.state.gate != nil {
		enabled = r.state.gate(r.call)
	}
	values[0] = enabled
	values[1] = int64(r.state.tier)
	values[2] = false
	values[3] = int64(TierFriends)
	values[4] = false
	return nil
}

type memberIDRows struct {
	ids   []uuid.UUID
	index int
}

func (*memberIDRows) Columns() []string { return []string{"user_id"} }
func (*memberIDRows) Close() error      { return nil }

func (r *memberIDRows) Next(values []driver.Value) error {
	if r.index >= len(r.ids) {
		return io.EOF
	}
	values[0] = r.ids[r.index].String()
	r.index++
	return nil
}

// openMemberCountDB registers a scenario keyed on the test's own name and
// removes it at cleanup, so a copy-pasted scenario literal in a future test
// cannot silently share counter state with this one.
func openMemberCountDB(t *testing.T) (*sql.DB, *memberCountState) {
	t.Helper()
	memberCountDriverOnce.Do(func() {
		sql.Register(memberCountDriverName, memberCountDriver{})
	})
	scenario := t.Name()
	state := &memberCountState{
		masterOn: true,
		tier:     TierServers,
		memberID: uuid.New(),
		friendID: uuid.New(),
	}

	memberCountRegistryMu.Lock()
	memberCountRegistry[scenario] = state
	memberCountRegistryMu.Unlock()
	t.Cleanup(func() {
		memberCountRegistryMu.Lock()
		delete(memberCountRegistry, scenario)
		memberCountRegistryMu.Unlock()
	})

	db, err := sql.Open(memberCountDriverName, scenario)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, state
}

// The headline assertion: N senders, ONE read. Before #2681 this was N reads,
// each re-materialising the same server's entire membership.
//
// It asserts the resolved SET, not merely a non-nil map: a loader that read
// once and replayed an empty set would satisfy a NotNil check while defeating
// the whole point.
func TestServerMemberLoader_FiveSenders_IssueExactlyOneMemberRead(t *testing.T) {
	db, state := openMemberCountDB(t)
	serverID := uuid.New()
	loader := NewServerMemberLoader(db, serverID)

	for i := 0; i < 5; i++ {
		candidates, err := CaptureServerVoiceCandidatesWithMembers(
			context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
		)
		require.NoError(t, err)
		assert.Equal(t, map[uuid.UUID]bool{state.memberID: true}, candidates,
			"every sender resolves the full membership, not an empty replay")
	}

	assert.Equal(t, 1, state.readCount(),
		"five senders must share ONE server_members read")
	assert.Equal(t, []uuid.UUID{serverID}, state.readServerIDs(),
		"the read must name the capture's server")

	// The ownership path uses the strict entry point, but it must retain the
	// same per-capture member-read sharing guarantee.
	for i := 0; i < 5; i++ {
		candidates, err := CaptureServerVoiceCandidatesWithMembersStrict(
			context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
		)
		require.NoError(t, err)
		assert.Equal(t, map[uuid.UUID]bool{state.memberID: true}, candidates,
			"strict ownership capture resolves the full membership")
	}
	assert.Equal(t, 1, state.readCount(),
		"strict captures must reuse the same server_members read")
}

func TestCaptureServerVoiceCandidatesWithMembers_UndeterminedPresence_LenientVsStrict(t *testing.T) {
	db, state := openMemberCountDB(t)
	serverID := uuid.New()
	senderID := uuid.New()
	loader := NewServerMemberLoader(db, serverID)

	candidates, err := CaptureServerVoiceCandidatesWithMembers(
		context.Background(), db, undeterminedPresenceStub{}, senderID, serverID, loader,
	)
	require.NoError(t, err)
	assert.Empty(t, candidates, "ordinary RBAC captures absorb transient presence errors")
	assert.Zero(t, state.readCount(), "the lenient error short-circuits before membership")

	candidates, err = CaptureServerVoiceCandidatesWithMembersStrict(
		context.Background(), db, undeterminedPresenceStub{}, senderID, serverID, loader,
	)
	require.Error(t, err, "ownership captures must not turn an unknown sender state into empty")
	assert.Nil(t, candidates)
	assert.Zero(t, state.readCount(), "strict presence failure precedes membership reads")
}

// TierFriends is the tier that also issues per-sender relationship reads, so
// the memoization has to hold on the branch where it is least obvious.
func TestServerMemberLoader_TierFriends_StillIssuesOneMemberRead(t *testing.T) {
	db, state := openMemberCountDB(t)
	state.tier = TierFriends
	serverID := uuid.New()
	// The scripted friendship rows and the scripted member row must agree, or
	// the intersect is empty and the assertion below proves nothing.
	state.friendID = state.memberID
	loader := NewServerMemberLoader(db, serverID)

	for i := 0; i < 4; i++ {
		candidates, err := CaptureServerVoiceCandidatesWithMembers(
			context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
		)
		require.NoError(t, err)
		assert.Equal(t, map[uuid.UUID]bool{state.memberID: true}, candidates,
			"TierFriends intersects relationships with the shared member set")
	}

	assert.Equal(t, 1, state.readCount(),
		"the member read is shared on TierFriends exactly as on TierServers")
}

// The short-circuit must survive the memoization: an eager fetch in the
// executor regresses a fully-disabled capture from zero reads to one.
func TestServerMemberLoader_ShortCircuitedSenders_IssueZeroMemberReads(t *testing.T) {
	for _, test := range []struct {
		name  string
		apply func(*memberCountState)
	}{
		{"master off", func(s *memberCountState) { s.masterOn = false }},
		{"tier off", func(s *memberCountState) { s.tier = TierOff }},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, state := openMemberCountDB(t)
			test.apply(state)
			serverID := uuid.New()
			loader := NewServerMemberLoader(db, serverID)

			for i := 0; i < 3; i++ {
				candidates, err := CaptureServerVoiceCandidatesWithMembers(
					context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
				)
				require.NoError(t, err)
				assert.Empty(t, candidates)
			}

			assert.Zero(t, state.readCount(),
				"the loader must not be consulted above the short-circuits")
		})
	}
}

// Production captures are MIXED — some senders short-circuit, some do not.
// This exercises laziness and correctness together: the read must be deferred
// past the disabled senders and still serve the enabled one correctly.
func TestServerMemberLoader_MixedCapture_DefersReadUntilFirstEnabledSender(t *testing.T) {
	db, state := openMemberCountDB(t)
	// Senders 1 and 2 report master-off; sender 3 is enabled.
	state.gate = func(call int) bool { return call >= 3 }
	serverID := uuid.New()
	loader := NewServerMemberLoader(db, serverID)

	for i := 0; i < 2; i++ {
		candidates, err := CaptureServerVoiceCandidatesWithMembers(
			context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
		)
		require.NoError(t, err)
		require.Empty(t, candidates)
	}
	assert.Zero(t, state.readCount(), "two disabled senders read nothing")

	candidates, err := CaptureServerVoiceCandidatesWithMembers(
		context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
	)
	require.NoError(t, err)
	assert.Equal(t, map[uuid.UUID]bool{state.memberID: true}, candidates,
		"the first enabled sender still resolves the full membership")
	assert.Equal(t, 1, state.readCount(), "and issues exactly one read")
}

// No cross-capture caching. Membership between two mutations is exactly the
// state a capture must re-read; caching it across captures would reconstruct an
// audience from pre-mutation membership (the #2445 error).
//
// NOTE: this constructs both loaders itself, so it guards the loader's own
// lifetime semantics only. The guard against a loader being hoisted onto the
// Executor and shared across captures lives in the voicepresence executor test,
// which drives the real PrepareCapture wiring.
func TestServerMemberLoader_TwoCaptures_IssueTwoMemberReads(t *testing.T) {
	db, state := openMemberCountDB(t)
	serverID := uuid.New()

	for capture := 0; capture < 2; capture++ {
		loader := NewServerMemberLoader(db, serverID)
		_, err := CaptureServerVoiceCandidatesWithMembers(
			context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
		)
		require.NoError(t, err)
	}

	assert.Equal(t, 2, state.readCount(),
		"each capture re-reads membership; a loader never outlives its capture")
}

// A failed read is REPLAYED, not retried once per sender — and the replayed
// error must carry the same sentinel and the same failure CLASS as the
// original, because PolicyErrorClass feeds the stable failure taxonomy.
func TestServerMemberLoader_FailedRead_IsReplayedNotRetried(t *testing.T) {
	db, state := openMemberCountDB(t)
	state.failRead = true
	serverID := uuid.New()
	loader := NewServerMemberLoader(db, serverID)

	for i := 0; i < 4; i++ {
		_, err := CaptureServerVoiceCandidatesWithMembers(
			context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
		)
		require.Error(t, err, "every sender sees the failure")
		assert.ErrorIs(t, err, errMemberCountQuery,
			"the replayed error keeps the original cause, including for senders 2..N")
		assert.Equal(t, FailureAudienceRead, PolicyErrorClass(err),
			"and keeps its failure class — a reclassified replay would corrupt the taxonomy")
	}

	assert.Equal(t, 1, state.readCount(),
		"four senders must not produce four failing round trips")
}

// A loader built for one server must never serve a capture that names another.
// membersFor resolves from the loader's OWN binding, so an unchecked mismatch
// returns a foreign server's members as this sender's audience, silently.
func TestCaptureServerVoiceCandidatesWithMembers_ServerMismatch_FailsClosed(t *testing.T) {
	db, state := openMemberCountDB(t)
	serverA := uuid.New()
	serverB := uuid.New()
	loader := NewServerMemberLoader(db, serverA)

	candidates, err := CaptureServerVoiceCandidatesWithMembers(
		context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverB, loader,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCaptureLoaderServerMismatch)
	assert.Nil(t, candidates, "a mismatch yields no audience, never a foreign one")
	assert.Zero(t, state.readCount(),
		"and issues no read at all — the mismatch is caught before any query")

	// A correctly-bound loader on the same db still resolves normally.
	ok, err := CaptureServerVoiceCandidatesWithMembers(
		context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverA, loader,
	)
	require.NoError(t, err)
	assert.Equal(t, map[uuid.UUID]bool{state.memberID: true}, ok)
}

// A nil loader preserves the pre-#2681 per-sender read. internal/graphpresence
// stays on this path: its loop varies serverID, not senderID, so a loader keyed
// to one server would gain it nothing.
func TestCaptureServerVoiceCandidatesWithMembers_NilLoader_ReadsPerSender(t *testing.T) {
	db, state := openMemberCountDB(t)
	serverID := uuid.New()

	for i := 0; i < 3; i++ {
		_, err := CaptureServerVoiceCandidatesWithMembers(
			context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, nil,
		)
		require.NoError(t, err)
	}

	assert.Equal(t, 3, state.readCount(),
		"a nil loader means 'read it yourself'")
}

// The exclusion is per-sender even though the read is shared: sender A must not
// vanish from sender B's audience.
func TestServerMemberLoader_ExclusionIsPerSenderNotShared(t *testing.T) {
	db, state := openMemberCountDB(t)
	serverID := uuid.New()
	senderA := uuid.New()
	state.memberID = senderA // the one member the scripted read returns IS sender A
	loader := NewServerMemberLoader(db, serverID)

	forA, err := CaptureServerVoiceCandidatesWithMembers(
		context.Background(), db, alwaysPermitPresence{}, senderA, serverID, loader,
	)
	require.NoError(t, err)
	forB, err := CaptureServerVoiceCandidatesWithMembers(
		context.Background(), db, alwaysPermitPresence{}, uuid.New(), serverID, loader,
	)
	require.NoError(t, err)

	assert.Empty(t, forA, "sender A is excluded from their own audience")
	assert.True(t, forB[senderA],
		"sender A's exclusion must not follow the shared map into sender B's audience")
	assert.Equal(t, 1, state.readCount())
}
