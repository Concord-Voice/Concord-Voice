package presencehistory

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	repositoryRowsCloseDriverName = "presence-history-rows-close-test"
	repositoryRowsScenarioScan    = "scan"
	repositoryRowsScenarioRead    = "read"
	repositoryRowsScenarioCursor  = "cursor"
	repositoryRowsScenarioClose   = "close"
)

var (
	repositoryRowsCloseDriverOnce sync.Once
	errRepositoryRowsClose        = errors.New("forced activity history rows close failure")
	errRepositoryRowsRead         = errors.New("forced activity history rows read failure")
)

type repositoryRowsCloseDriver struct{}

func (repositoryRowsCloseDriver) Open(scenario string) (driver.Conn, error) {
	return &repositoryRowsCloseConn{scenario: scenario}, nil
}

type repositoryRowsCloseConn struct {
	scenario string
}

func (*repositoryRowsCloseConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*repositoryRowsCloseConn) Close() error { return nil }

func (*repositoryRowsCloseConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *repositoryRowsCloseConn) QueryContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Rows, error) {
	return &repositoryRowsCloseRows{scenario: c.scenario}, nil
}

type repositoryRowsCloseRows struct {
	scenario  string
	delivered int
}

func (*repositoryRowsCloseRows) Columns() []string {
	return []string{
		"id",
		"category",
		"payload_version",
		"payload",
		"started_at",
		"ended_at",
		"recorded_at",
		"expires_at",
	}
}

func (*repositoryRowsCloseRows) Close() error { return errRepositoryRowsClose }

// Report an unread result set so database/sql leaves the final close to List.
func (*repositoryRowsCloseRows) HasNextResultSet() bool { return true }

func (*repositoryRowsCloseRows) NextResultSet() error { return io.EOF }

func (r *repositoryRowsCloseRows) Next(values []driver.Value) error {
	switch r.scenario {
	case repositoryRowsScenarioScan:
		if r.delivered == 0 {
			r.deliver(values, true, false, false)
			return nil
		}
		if r.delivered == 1 {
			r.deliver(values, false, false, true)
			return nil
		}
		return io.EOF
	case repositoryRowsScenarioRead:
		if r.delivered == 0 {
			r.deliver(values, true, false, false)
			return nil
		}
		return errRepositoryRowsRead
	case repositoryRowsScenarioCursor:
		if r.delivered < 2 {
			r.deliver(values, r.delivered == 0, r.delivered == 0, false)
			return nil
		}
		return io.EOF
	case repositoryRowsScenarioClose:
		if r.delivered == 0 {
			r.deliver(values, true, false, false)
			return nil
		}
		return io.EOF
	default:
		return errors.New("unsupported activity history rows test scenario")
	}
}

func (r *repositoryRowsCloseRows) deliver(
	values []driver.Value,
	unsupported bool,
	invalidCursor bool,
	invalidTimestamp bool,
) {
	now := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	if invalidCursor {
		id = uuid.Nil.String()
	}
	version := int64(1)
	payload := []byte(`{"text":"safe"}`)
	if unsupported {
		version = 2
		payload = []byte(`{"text":"future-private-value"}`)
	}
	var startedAt driver.Value = now
	if invalidTimestamp {
		startedAt = "not-a-timestamp"
	}
	values[0] = id
	values[1] = "custom_text"
	values[2] = version
	values[3] = payload
	values[4] = startedAt
	values[5] = nil
	values[6] = now
	values[7] = now.Add(time.Hour)
	r.delivered++
}

func TestRepositoryPaginationAndSafeDecoding(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	ctx := context.Background()
	repository := NewRepository(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}))
	user := ts.CreateTestUser(t, "history_pagination")
	userID := uuid.MustParse(user.ID)
	other := ts.CreateTestUser(t, "history_pagination_other")
	otherID := uuid.MustParse(other.ID)
	recordedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	expiresAt := recordedAt.Add(24 * time.Hour)
	endedAt := recordedAt.Add(time.Second)

	ids := []uuid.UUID{
		uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		uuid.MustParse("33333333-3333-4333-8333-333333333333"),
	}
	for index, id := range ids {
		_, err := ts.DB.ExecContext(ctx, `
			INSERT INTO presence_history (
				id, sender_id, category, payload_version, payload,
				started_at, ended_at, recorded_at, expires_at
			) VALUES ($1, $2, 'custom_text', 1, $3::JSONB, $4, $5, $4, $6)
		`, id, userID, `{"text":"item `+string(rune('1'+index))+`"}`,
			recordedAt, endedAt, expiresAt)
		require.NoError(t, err)
	}
	insertHistoryRow(t, ts.DB, otherID, CategoryCustomText, 1,
		`{"text":"not yours"}`, recordedAt, endedAt, recordedAt, expiresAt)

	first, err := repository.List(ctx, userID, ListOptions{Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Items, 2)
	assert.Equal(t, ids[2], first.Items[0].ID)
	assert.Equal(t, ids[1], first.Items[1].ID)
	require.NotNil(t, first.NextCursor)
	decodedCursor, err := DecodeCursor(*first.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, first.Items[1].ID, decodedCursor.ID)
	assert.Equal(t, first.Items[1].RecordedAt, decodedCursor.RecordedAt)

	second, err := repository.List(ctx, userID, ListOptions{Limit: 2, Before: &decodedCursor})
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	assert.Equal(t, ids[0], second.Items[0].ID)
	assert.Nil(t, second.NextCursor)
	for _, item := range append(first.Items, second.Items...) {
		assert.Equal(t, ItemStatusSupported, item.Status)
		require.NotNil(t, item.Payload)
		assert.Equal(t, CategoryCustomText, item.Category)
	}
}

func TestRepositoryFullTerminalPageHasNoCursor(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	repository := NewRepository(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}))
	user := ts.CreateTestUser(t, "history_terminal")
	userID := uuid.MustParse(user.ID)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	for _, text := range []string{"one", "two"} {
		insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
			`{"text":"`+text+`"}`, now, now, now, now.Add(time.Hour))
	}

	page, err := repository.List(context.Background(), userID, ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, page.Items, 2)
	assert.Nil(t, page.NextCursor, "limit+1, not len==limit, determines more pages")
}

func TestRepositoryExpirySelfScopeAndUnsupportedPayloads(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	var logOutput bytes.Buffer
	repository := newRepositoryWithLogger(
		ts.DB,
		BuildDisclosure(DisclosureOptions{InstanceType: "saas"}),
		logger.NewWithWriter(&logOutput),
	)
	user := ts.CreateTestUser(t, "history_decode")
	userID := uuid.MustParse(user.ID)
	other := ts.CreateTestUser(t, "history_decode_other")
	otherID := uuid.MustParse(other.ID)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)

	supportedID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"supported"}`, now, now, now, now.Add(time.Hour))
	malformedID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":""}`, now.Add(time.Microsecond), now.Add(time.Second),
		now.Add(time.Microsecond), now.Add(time.Hour))
	unknownVersionID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 2,
		`{"text":"future-private-value"}`, now.Add(2*time.Microsecond), now.Add(time.Second),
		now.Add(2*time.Microsecond), now.Add(time.Hour))
	unsupportedCategoryID := insertHistoryRow(t, ts.DB, userID, CategoryServerVoice, 1,
		`{"channel":"confidential-channel"}`, now.Add(3*time.Microsecond), now.Add(time.Second),
		now.Add(3*time.Microsecond), now.Add(time.Hour))
	insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"expired"}`, now.Add(4*time.Microsecond), now.Add(time.Second),
		now.Add(4*time.Microsecond), time.Now().UTC().Add(-time.Second))
	insertHistoryRow(t, ts.DB, otherID, CategoryCustomText, 1,
		`{"text":"other-account-secret"}`, now.Add(5*time.Microsecond), now.Add(time.Second),
		now.Add(5*time.Microsecond), now.Add(time.Hour))

	page, err := repository.List(context.Background(), userID, ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 4)
	byID := make(map[uuid.UUID]HistoryItem, len(page.Items))
	for _, item := range page.Items {
		byID[item.ID] = item
	}
	assert.Equal(t, ItemStatusSupported, byID[supportedID].Status)
	require.NotNil(t, byID[supportedID].Payload)
	for _, id := range []uuid.UUID{malformedID, unknownVersionID, unsupportedCategoryID} {
		assert.Equal(t, ItemStatusUnsupported, byID[id].Status)
		assert.Nil(t, byID[id].Payload, "raw malformed or unsupported JSON must not cross the API boundary")
	}

	logs := logOutput.String()
	assert.Contains(t, logs, "stored activity payload is unsupported")
	assert.Equal(t, 1, strings.Count(logs, "stored activity payload is unsupported"))
	assert.Contains(t, logs, "error_class=unsupported_stored_payload")
	assert.Contains(t, logs, "unsupported_count=3")
	for _, forbidden := range []string{
		userID.String(),
		otherID.String(),
		supportedID.String(),
		malformedID.String(),
		unknownVersionID.String(),
		unsupportedCategoryID.String(),
		"future-private-value",
		"confidential-channel",
		"other-account-secret",
		"custom_text",
		"server_voice",
		"payload_version",
		recordedAtLogValue(now),
	} {
		assert.NotContains(t, logs, forbidden)
	}
}

func recordedAtLogValue(value time.Time) string {
	return strings.TrimSuffix(value.Format(time.RFC3339Nano), "Z")
}

func TestNewRepositoryWithLoggerDefaultsNilLogger(t *testing.T) {
	repository := newRepositoryWithLogger(nil, DisclosureState{}, nil)
	require.NotNil(t, repository.log)
}

func TestRepositoryAccountDeletionCascadesHistory(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	user := ts.CreateTestUser(t, "history_repo_cascade")
	userID := uuid.MustParse(user.ID)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"delete me"}`, now, nil, now, now.Add(time.Hour))
	_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err)
	assert.Zero(t, historyRowCount(t, ts.DB, userID))
}

func TestRepositoryListRejectsInvalidLimits(t *testing.T) {
	repository := NewRepository(&sql.DB{}, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}))
	for _, limit := range []int{-1, 101} {
		_, err := repository.List(context.Background(), uuid.New(), ListOptions{Limit: limit})
		require.Error(t, err)
	}
}

func TestRepositoryListDoesNotWarnBeforeSuccessfulCompletion(t *testing.T) {
	repositoryRowsCloseDriverOnce.Do(func() {
		sql.Register(repositoryRowsCloseDriverName, repositoryRowsCloseDriver{})
	})
	tests := []struct {
		name           string
		scenario       string
		limit          int
		wantError      error
		wantErrorText  string
		wantCloseError bool
	}{
		{
			name:           "scan failure",
			scenario:       repositoryRowsScenarioScan,
			limit:          10,
			wantErrorText:  "scan activity history metadata",
			wantCloseError: true,
		},
		{
			name:          "rows failure",
			scenario:      repositoryRowsScenarioRead,
			limit:         10,
			wantError:     errRepositoryRowsRead,
			wantErrorText: "list activity history rows",
		},
		{
			name:           "cursor failure",
			scenario:       repositoryRowsScenarioCursor,
			limit:          1,
			wantError:      ErrInvalidCursor,
			wantErrorText:  "encode activity history page boundary",
			wantCloseError: true,
		},
		{
			name:          "close failure",
			scenario:      repositoryRowsScenarioClose,
			limit:         10,
			wantError:     errRepositoryRowsClose,
			wantErrorText: "close activity history rows",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, err := sql.Open(repositoryRowsCloseDriverName, test.scenario)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			var logOutput bytes.Buffer
			repository := newRepositoryWithLogger(
				db,
				BuildDisclosure(DisclosureOptions{InstanceType: "saas"}),
				logger.NewWithWriter(&logOutput),
			)

			_, err = repository.List(context.Background(), uuid.New(), ListOptions{Limit: test.limit})
			require.Error(t, err)
			if test.wantError != nil {
				assert.ErrorIs(t, err, test.wantError)
			}
			if test.wantCloseError {
				assert.ErrorIs(t, err, errRepositoryRowsClose)
			}
			assert.Contains(t, err.Error(), test.wantErrorText)
			assert.NotContains(t, logOutput.String(), "stored activity payload is unsupported")
		})
	}
}
