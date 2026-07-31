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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The recheck capture helpers iterate *sql.Rows, so their resolved paths cannot
// be reached through the interface stub in activity_recheck_test.go (which can
// only return a nil *sql.Rows plus an error). This scripted database/sql driver
// yields REAL rows without PostgreSQL, so the row loops, the ambiguity guard,
// the lifecycle-time guard, and the two policy failure classes are all covered
// by a plain unit test. It follows the activityBuilderRowsDriver pattern already
// established in activity_builder_test.go.

const recheckRowsDriverName = "presence-activity-recheck-rows-test"

var recheckRowsDriverOnce sync.Once

var (
	errRecheckScopeQuery     = errors.New("forced recheck scope query failure")
	errRecheckScopeIteration = errors.New("forced recheck scope iteration failure")
	errRecheckSettingsQuery  = errors.New("forced recheck settings query failure")
	errRecheckMembersQuery   = errors.New("forced recheck members query failure")
)

// recheckRowsChannelID is the channel every scripted scope row reports.
const recheckRowsChannelID = "22222222-2222-2222-2222-222222222222"

// recheckRowsEventAt is a lifecycle timestamp IsValidActivitySourceTime accepts.
var recheckRowsEventAt = time.Unix(200, 0).UTC()

type recheckRowsDriver struct{}

func (recheckRowsDriver) Open(scenario string) (driver.Conn, error) {
	return &recheckRowsConn{scenario: scenario}, nil
}

type recheckRowsConn struct{ scenario string }

func (*recheckRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (*recheckRowsConn) Close() error { return nil }

func (*recheckRowsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

// QueryContext routes by statement shape so one connection can serve the
// settings read, the scope read, and the membership read a single capture makes.
// The membership read always fails: every scenario that legitimately reaches it
// is asserting the audience-read failure class, and every scenario that must
// stop short of it proves so by returning no error.
func (c *recheckRowsConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "user_presence_settings"):
		if c.scenario == "settings_query_error" {
			return nil, errRecheckSettingsQuery
		}
		return &recheckSettingsRows{scenario: c.scenario}, nil
	case strings.Contains(query, "voice_participants"):
		if c.scenario == "scope_query_error" {
			return nil, errRecheckScopeQuery
		}
		return newRecheckScopeRows(c.scenario), nil
	default:
		return nil, errRecheckMembersQuery
	}
}

type recheckSettingsRows struct {
	scenario  string
	delivered bool
}

func (*recheckSettingsRows) Columns() []string {
	return []string{
		"master_enabled", "server_voice_tier", "server_voice_show_details",
		"private_call_tier", "private_call_show_details",
	}
}

func (*recheckSettingsRows) Close() error { return nil }

func (r *recheckSettingsRows) Next(values []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	values[0] = r.scenario != "settings_master_off"
	values[1] = int64(TierFriends)
	if r.scenario == "settings_tier_off" {
		values[1] = int64(TierOff)
	}
	values[2] = false
	values[3] = int64(TierFriends)
	values[4] = false
	return nil
}

type recheckScopeRows struct {
	rows        [][]driver.Value
	index       int
	terminalErr error
}

func newRecheckScopeRows(scenario string) *recheckScopeRows {
	switch scenario {
	case "scope_no_rows":
		return &recheckScopeRows{}
	case "scope_ambiguous":
		return &recheckScopeRows{rows: [][]driver.Value{
			{recheckRowsChannelID, recheckRowsEventAt},
			{"33333333-3333-3333-3333-333333333333", recheckRowsEventAt},
		}}
	case "scope_scan_error":
		return &recheckScopeRows{rows: [][]driver.Value{{"not-a-uuid", recheckRowsEventAt}}}
	case "scope_iteration_error":
		return &recheckScopeRows{
			rows:        [][]driver.Value{{recheckRowsChannelID, recheckRowsEventAt}},
			terminalErr: errRecheckScopeIteration,
		}
	case "scope_invalid_time":
		return &recheckScopeRows{rows: [][]driver.Value{{recheckRowsChannelID, time.Time{}}}}
	default:
		return &recheckScopeRows{rows: [][]driver.Value{
			{recheckRowsChannelID, recheckRowsEventAt},
		}}
	}
}

func (*recheckScopeRows) Columns() []string {
	return []string{"channel_id", "lifecycle_event_at"}
}

func (*recheckScopeRows) Close() error { return nil }

func (r *recheckScopeRows) Next(values []driver.Value) error {
	if r.index >= len(r.rows) {
		if r.terminalErr != nil {
			return r.terminalErr
		}
		return io.EOF
	}
	copy(values, r.rows[r.index])
	r.index++
	return nil
}

func openRecheckRowsDB(t *testing.T, scenario string) *sql.DB {
	t.Helper()
	recheckRowsDriverOnce.Do(func() {
		sql.Register(recheckRowsDriverName, recheckRowsDriver{})
	})
	db, err := sql.Open(recheckRowsDriverName, scenario)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestCurrentServerVoiceScope_ResolvesTheCommittedLifecycleGeneration(t *testing.T) {
	db := openRecheckRowsDB(t, "scope_resolved")

	scope, found, err := CurrentServerVoiceScope(context.Background(), db, uuid.New())

	require.NoError(t, err)
	require.True(t, found)
	channelID := uuid.MustParse(recheckRowsChannelID)
	assert.Equal(t, CategoryServerVoice, scope.Category)
	assert.Equal(t, channelID, scope.RoomID)
	assert.Equal(t, channelID, scope.LifecycleID,
		"the voice channel is both the room and the lifecycle identity")
	assert.True(t, scope.EventAt.Equal(recheckRowsEventAt),
		"EventAt MUST be the committed lifecycle_event_at, never a fabricated clock read")
}

func TestCurrentServerVoiceScope_UnresolvedShapes_AreNotFoundWithoutError(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario string
		reason   string
	}{
		{
			name:     "no voice row",
			scenario: "scope_no_rows",
			reason:   "a sender who is not in voice simply has no generation",
		},
		{
			name:     "ambiguous multi-row sender",
			scenario: "scope_ambiguous",
			reason:   "an ambiguous sender asserts nothing and is skipped by the caller",
		},
		{
			name:     "lifecycle timestamp outside the source-version domain",
			scenario: "scope_invalid_time",
			reason:   "an unusable timestamp cannot derive a SourceVersion",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openRecheckRowsDB(t, test.scenario)

			scope, found, err := CurrentServerVoiceScope(context.Background(), db, uuid.New())

			require.NoError(t, err, test.reason)
			assert.False(t, found, test.reason)
			assert.Equal(t, Scope{}, scope, "an unresolved read yields the zero scope")
		})
	}
}

func TestCurrentServerVoiceScope_RowFailures_AreWrappedAndNotFound(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario string
		message  string
		wrapped  error
	}{
		{
			name:     "scan failure",
			scenario: "scope_scan_error",
			message:  "scan current server voice scope",
		},
		{
			name:     "iteration failure",
			scenario: "scope_iteration_error",
			message:  "iterate current server voice scope",
			wrapped:  errRecheckScopeIteration,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openRecheckRowsDB(t, test.scenario)

			_, found, err := CurrentServerVoiceScope(context.Background(), db, uuid.New())

			require.Error(t, err)
			assert.False(t, found, "a row failure is never reported as a resolved scope")
			assert.ErrorContains(t, err, test.message)
			if test.wrapped != nil {
				assert.ErrorIs(t, err, test.wrapped)
			}
		})
	}
}

func TestCaptureServerVoiceCandidates_SettingsReadFailure_IsAFailureNotAnEmptySet(t *testing.T) {
	db := openRecheckRowsDB(t, "settings_query_error")

	candidates, err := CaptureServerVoiceCandidates(
		context.Background(), db, &recheckPresenceStub{permitted: true},
		uuid.New(), uuid.New(),
	)

	require.Error(t, err,
		"an unreadable settings row is uncertainty and must block the write, not resolve to empty")
	assert.ErrorIs(t, err, errRecheckSettingsQuery)
	assert.Nil(t, candidates)
}

func TestCaptureServerVoiceCandidates_PresenceDisabled_StopsBeforeTheAudienceRead(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario string
		reason   string
	}{
		{
			name:     "master switch off",
			scenario: "settings_master_off",
			reason:   "master off means no Server Voice audience exists to capture",
		},
		{
			name:     "server voice tier off",
			scenario: "settings_tier_off",
			reason:   "TierOff means no Server Voice audience exists to capture",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The scripted membership read always fails, so NoError is itself the
			// proof that the capture short-circuited before reaching it.
			db := openRecheckRowsDB(t, test.scenario)

			candidates, err := CaptureServerVoiceCandidates(
				context.Background(), db, &recheckPresenceStub{permitted: true},
				uuid.New(), uuid.New(),
			)

			require.NoError(t, err, test.reason)
			assert.Empty(t, candidates, test.reason)
		})
	}
}

func TestCaptureServerVoiceCandidates_AudienceReadFailure_IsReturned(t *testing.T) {
	db := openRecheckRowsDB(t, "members_read_error")

	candidates, err := CaptureServerVoiceCandidates(
		context.Background(), db, &recheckPresenceStub{permitted: true},
		uuid.New(), uuid.New(),
	)

	require.Error(t, err,
		"an unreadable membership set is uncertainty and must block the write")
	assert.ErrorIs(t, err, errRecheckMembersQuery)
	assert.Nil(t, candidates)
}
