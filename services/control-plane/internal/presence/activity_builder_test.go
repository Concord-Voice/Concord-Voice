package presence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const activityBuilderRowsDriverName = "presence-activity-builder-rows-test"

var (
	activityBuilderRowsDriverOnce sync.Once
	errActivityBuilderQuery       = errors.New("forced activity builder query failure")
	errActivityBuilderIteration   = errors.New("forced activity builder iteration failure")
	errActivityBuilderClose       = errors.New("forced activity builder rows close failure")
)

type activityBuilderRowsDriver struct{}

func (activityBuilderRowsDriver) Open(scenario string) (driver.Conn, error) {
	return &activityBuilderRowsConn{scenario: scenario}, nil
}

type activityBuilderRowsConn struct {
	scenario string
}

func (*activityBuilderRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (*activityBuilderRowsConn) Close() error { return nil }

func (*activityBuilderRowsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *activityBuilderRowsConn) QueryContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Rows, error) {
	if c.scenario == "query" {
		return nil, errActivityBuilderQuery
	}
	return &activityBuilderRows{scenario: c.scenario}, nil
}

type activityBuilderRows struct {
	scenario  string
	delivered bool
}

func (*activityBuilderRows) Columns() []string {
	return []string{
		"channel_id", "server_id", "channel_name", "server_name",
		"is_voice", "is_member", "joined_at", "lifecycle_event_at",
	}
}

func (r *activityBuilderRows) Close() error {
	if r.scenario == "close" {
		return errActivityBuilderClose
	}
	return nil
}

func (r *activityBuilderRows) HasNextResultSet() bool { return r.scenario == "close" }

func (*activityBuilderRows) NextResultSet() error { return io.EOF }

func (r *activityBuilderRows) Next(values []driver.Value) error {
	if !r.delivered {
		r.delivered = true
		values[0] = "22222222-2222-2222-2222-222222222222"
		values[1] = "33333333-3333-3333-3333-333333333333"
		values[2] = "General"
		values[3] = "Concord"
		values[4] = true
		values[5] = true
		values[6] = time.Unix(100, 0).UTC()
		values[7] = time.Unix(200, 0).UTC()
		if r.scenario == "scan" {
			values[0] = "not-a-uuid"
		}
		if r.scenario == "far_future" {
			values[7] = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		return nil
	}
	if r.scenario == "iteration" {
		return errActivityBuilderIteration
	}
	return io.EOF
}

func TestActivityBuilder_ServerRowsErrorsAreNotDiscarded(t *testing.T) {
	activityBuilderRowsDriverOnce.Do(func() {
		sql.Register(activityBuilderRowsDriverName, activityBuilderRowsDriver{})
	})

	senderID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	channelID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	scope := Scope{
		Category: CategoryServerVoice, RoomID: channelID,
		LifecycleID: channelID, EventAt: time.Unix(200, 0).UTC(),
	}

	for _, test := range []struct {
		scenario string
		want     error
		message  string
	}{
		{scenario: "query", want: errActivityBuilderQuery, message: "query server voice activity"},
		{scenario: "scan", message: "scan server voice activity row"},
		{scenario: "iteration", want: errActivityBuilderIteration, message: "iterate rich-presence activity rows"},
		{scenario: "close", want: errActivityBuilderClose, message: "close rich-presence activity rows"},
		{scenario: "far_future", message: "server voice scope"},
	} {
		t.Run(test.scenario, func(t *testing.T) {
			db, err := sql.Open(activityBuilderRowsDriverName, test.scenario)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })

			_, err = NewActivityBuilder(db, nil).Build(context.Background(), senderID, scope)
			require.Error(t, err)
			assert.ErrorContains(t, err, test.message)
			if test.want != nil {
				assert.ErrorIs(t, err, test.want)
			}
		})
	}
}

func TestActivityBuilder_RejectsInvalidTrustedScope(t *testing.T) {
	activityBuilderRowsDriverOnce.Do(func() {
		sql.Register(activityBuilderRowsDriverName, activityBuilderRowsDriver{})
	})
	db, err := sql.Open(activityBuilderRowsDriverName, "valid")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	senderID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	channelID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	valid := Scope{
		Category: CategoryServerVoice, RoomID: channelID,
		LifecycleID: channelID, EventAt: time.Unix(200, 0).UTC(),
	}

	for _, test := range []struct {
		name     string
		senderID uuid.UUID
		scope    Scope
	}{
		{name: "nil sender", scope: valid},
		{name: "unknown category", senderID: senderID, scope: Scope{
			Category: "games", RoomID: channelID, LifecycleID: channelID, EventAt: valid.EventAt,
		}},
		{name: "nil room", senderID: senderID, scope: Scope{
			Category: CategoryServerVoice, LifecycleID: channelID, EventAt: valid.EventAt,
		}},
		{name: "nil lifecycle", senderID: senderID, scope: Scope{
			Category: CategoryServerVoice, RoomID: channelID, EventAt: valid.EventAt,
		}},
		{name: "zero event time", senderID: senderID, scope: Scope{
			Category: CategoryServerVoice, RoomID: channelID, LifecycleID: channelID,
		}},
		{name: "pre-epoch event time", senderID: senderID, scope: Scope{
			Category: CategoryServerVoice, RoomID: channelID, LifecycleID: channelID,
			EventAt: time.Unix(-1, 0).UTC(),
		}},
		{name: "source version above max-safe integer", senderID: senderID, scope: Scope{
			Category: CategoryServerVoice, RoomID: channelID, LifecycleID: channelID,
			EventAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
		{name: "server lifecycle differs from room", senderID: senderID, scope: Scope{
			Category: CategoryServerVoice, RoomID: channelID,
			LifecycleID: uuid.MustParse("44444444-4444-4444-4444-444444444444"),
			EventAt:     valid.EventAt,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewActivityBuilder(db, nil).Build(
				context.Background(), test.senderID, test.scope,
			)
			assert.ErrorIs(t, err, ErrInvalidActivityScope)
		})
	}

	_, err = NewActivityBuilder(nil, nil).Build(context.Background(), senderID, valid)
	assert.ErrorContains(t, err, "activity builder unavailable")

	_, err = NewActivityBuilder(db, nil).Build(context.Background(), senderID, Scope{
		Category: CategoryPrivateCall, RoomID: channelID,
		LifecycleID: uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		EventAt:     valid.EventAt,
	})
	assert.ErrorContains(t, err, "private call lease verifier unavailable")
}
