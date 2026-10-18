//go:build integration

package database_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageKeyVersionMigrationsUpDownUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	type phase struct{ up, down string }
	phases := []phase{
		{"000171_guard_messages_key_version.up.sql", "000171_guard_messages_key_version.down.sql"},
		{"000172_validate_messages_key_version.up.sql", "000172_validate_messages_key_version.down.sql"},
		{"000173_enforce_messages_key_version.up.sql", "000173_enforce_messages_key_version.down.sql"},
		{"000174_guard_dm_messages_key_version.up.sql", "000174_guard_dm_messages_key_version.down.sql"},
		{"000175_validate_dm_messages_key_version.up.sql", "000175_validate_dm_messages_key_version.down.sql"},
		{"000176_enforce_dm_messages_key_version.up.sql", "000176_enforce_dm_messages_key_version.down.sql"},
	}
	ups, downs := make([]string, len(phases)), make([]string, len(phases))
	for i, p := range phases {
		ups[i] = migrationReadFile(t, "../../migrations/"+p.up)
		downs[i] = migrationReadFile(t, "../../migrations/"+p.down)
	}
	var phaseApplied [6]bool
	for i := range phaseApplied {
		phaseApplied[i] = true
	}
	testUserID := ""
	t.Cleanup(func() {
		if testUserID != "" {
			for _, table := range []string{"messages", "dm_messages"} {
				if _, err := ts.DB.ExecContext(context.Background(),
					fmt.Sprintf(`DELETE FROM %s WHERE user_id = $1 AND key_version IS NULL`, table), testUserID,
				); !assert.NoError(t, err, "remove legacy NULL fixture before restoring constraints") {
					return
				}
			}
		}
		for i, up := range ups {
			if phaseApplied[i] {
				continue
			}
			if _, err := ts.DB.ExecContext(context.Background(), up); !assert.NoError(
				t, err, "restore migration phase %d before test database cleanup", i+1,
			) {
				return
			}
			phaseApplied[i] = true
		}
	})
	for i := len(downs) - 1; i >= 0; i-- {
		_, err := ts.DB.ExecContext(ctx, downs[i])
		require.NoErrorf(t, err, "roll back migration phase %d", i+1)
		phaseApplied[i] = false
	}

	owner := ts.CreateTestUser(t, "message_key_version_phase")
	testUserID = owner.ID
	serverID := ts.CreateTestServer(t, owner.ID, "Message key version migration")
	channelID := ts.CreateTestChannel(t, serverID, "message-key-version-phase")
	conversationID := uuid.NewString()
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, owner.ID)
	require.NoError(t, err)

	tables := []messageKeyVersionTable{
		{name: "messages", parentColumn: "channel_id", parentID: channelID, userID: owner.ID},
		{name: "dm_messages", parentColumn: "conversation_id", parentID: conversationID, userID: owner.ID},
	}
	for _, table := range tables {
		_, err = insertMessageKeyVersionRowVersion(ctx, ts.DB, table, uuid.NewString(), "user", nil, true)
		require.NoError(t, err)
	}
	metadataIDs := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	metadataVersion := 1
	insertMessageKeyVersionRow(t, ctx, ts.DB, tables[0], metadataIDs[0], "expiration_event", &metadataVersion)
	insertMessageKeyVersionRow(t, ctx, ts.DB, tables[1], metadataIDs[1], "call_event", &metadataVersion)
	insertMessageKeyVersionRow(t, ctx, ts.DB, tables[1], metadataIDs[2], "expiration_event", &metadataVersion)

	for _, i := range []int{0, 3} {
		_, err = ts.DB.ExecContext(ctx, ups[i])
		require.NoErrorf(t, err, "apply guard phase %d", i+1)
		phaseApplied[i] = true
	}
	for _, table := range tables {
		var count int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE key_version IS NULL AND type = 'user'`, table.name),
		).Scan(&count))
		assert.Equal(t, 1, count, "the NOT VALID guard must preserve existing NULL rows")
		_, insertErr := insertMessageKeyVersionRowVersion(
			ctx, ts.DB, table, uuid.NewString(), "user", nil, true,
		)
		requireMessageKeyVersionSQLState(t, insertErr, pq.ErrorCode("23514"))
	}
	for _, i := range []int{1, 4} {
		_, err = ts.DB.ExecContext(ctx, ups[i])
		requireMessageKeyVersionSQLState(t, err, pq.ErrorCode("23514"))
	}
	for _, table := range tables {
		_, err = ts.DB.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE user_id = $1 AND type = 'user' AND key_version IS NULL`, table.name),
			owner.ID)
		require.NoError(t, err)
	}
	for _, i := range []int{1, 4} {
		_, err = ts.DB.ExecContext(ctx, ups[i])
		require.NoErrorf(t, err, "validate phase %d after removing legacy NULL fixture", i+1)
		phaseApplied[i] = true
	}
	for _, table := range tables {
		var validated bool
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT convalidated FROM pg_constraint
			WHERE conrelid = $1::regclass AND conname = $2
		`, table.name, table.name+"_key_version_positive").Scan(&validated))
		assert.True(t, validated)
	}
	for _, i := range []int{2, 5} {
		_, err = ts.DB.ExecContext(ctx, ups[i])
		require.NoErrorf(t, err, "enforce phase %d", i+1)
		phaseApplied[i] = true
	}
	for _, table := range tables {
		var nullable string
		var defaultValue sql.NullString
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT is_nullable, column_default FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'key_version'
		`, table.name).Scan(&nullable, &defaultValue))
		assert.Equal(t, "NO", nullable)
		assert.False(t, defaultValue.Valid)
	}

	positiveIDs := make([][]string, len(tables))
	for i, table := range tables {
		for _, tc := range []struct {
			name    string
			version any
			state   pq.ErrorCode
		}{
			{name: "omitted", state: pq.ErrorCode("23502")},
			{name: "explicit null", version: nil, state: pq.ErrorCode("23502")},
			{name: "zero", version: 0, state: pq.ErrorCode("23514")},
			{name: "negative", version: -1, state: pq.ErrorCode("23514")},
		} {
			_, insertErr := insertMessageKeyVersionRowVersion(ctx, ts.DB, table, uuid.NewString(), "user", tc.version, tc.name != "omitted")
			requireMessageKeyVersionSQLState(t, insertErr, tc.state)
		}
		positiveIDs[i] = []string{uuid.NewString(), uuid.NewString()}
		for j, version := range []int{1, 2} {
			insertMessageKeyVersionRow(t, ctx, ts.DB, table, positiveIDs[i][j], "user", &version)
		}
	}

	for i := len(downs) - 1; i >= 0; i-- {
		_, err = ts.DB.ExecContext(ctx, downs[i])
		require.NoErrorf(t, err, "roll back migration phase %d", i+1)
		phaseApplied[i] = false
	}
	for i, table := range tables {
		var nullable, defaultValue string
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT is_nullable, column_default FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'key_version'
		`, table.name).Scan(&nullable, &defaultValue))
		assert.Equal(t, "YES", nullable)
		assert.Equal(t, "1", defaultValue)
		defaultID := uuid.NewString()
		insertMessageKeyVersionRow(t, ctx, ts.DB, table, defaultID, "user", nil)
		var defaultVersion int
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT key_version FROM `+table.name+` WHERE id = $1
		`, defaultID).Scan(&defaultVersion))
		assert.Equal(t, 1, defaultVersion)
		var nullID = uuid.NewString()
		_, err = insertMessageKeyVersionRowVersion(ctx, ts.DB, table, nullID, "user", nil, true)
		require.NoError(t, err)
		_, err = ts.DB.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table.name), nullID)
		require.NoError(t, err)
		for j, id := range positiveIDs[i] {
			var version int
			require.NoError(t, ts.DB.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT key_version FROM %s WHERE id = $1`, table.name), id,
			).Scan(&version))
			assert.Equal(t, j+1, version)
		}
	}
	for i, up := range ups {
		_, err = ts.DB.ExecContext(ctx, up)
		require.NoError(t, err, "all phases must reapply after full rollback")
		phaseApplied[i] = true
	}
	for i, table := range tables {
		for j, id := range positiveIDs[i] {
			var version int
			require.NoError(t, ts.DB.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT key_version FROM %s WHERE id = $1`, table.name), id,
			).Scan(&version))
			assert.Equal(t, j+1, version)
		}
	}
	for i, id := range metadataIDs {
		table := tables[0]
		if i > 0 {
			table = tables[1]
		}
		var version int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT key_version FROM %s WHERE id = $1`, table.name), id,
		).Scan(&version))
		assert.Equal(t, 1, version)
	}
}

type messageKeyVersionTable struct {
	name, parentColumn, parentID, userID string
}

func insertMessageKeyVersionRow(
	t *testing.T, ctx context.Context, db *sql.DB, table messageKeyVersionTable,
	id, kind string, version *int,
) {
	t.Helper()
	_, err := insertMessageKeyVersionRowVersion(ctx, db, table, id, kind, version, version != nil)
	require.NoError(t, err)
}

func insertMessageKeyVersionRowVersion(
	ctx context.Context, db *sql.DB, table messageKeyVersionTable,
	id, kind string, version any, includeVersion bool,
) (sql.Result, error) {
	payload := any(nil)
	if kind != "user" {
		payload = []byte(`{}`)
	}
	payloadColumn := "expiration_event_payload"
	if table.name == "dm_messages" && kind == "call_event" {
		payloadColumn = "call_event_payload"
	}
	content := ""
	if kind == "user" {
		content = "ciphertext"
	}
	columns := fmt.Sprintf("id, %s, user_id, content, type, %s", table.parentColumn, payloadColumn)
	values := "$1, $2, $3, $4, $5, $6"
	args := []any{id, table.parentID, table.userID, content, kind, payload}
	if includeVersion {
		columns += ", key_version"
		values += ", $7"
		args = append(args, version)
	}
	return db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table.name, columns, values), args...)
}

func requireMessageKeyVersionSQLState(t *testing.T, err error, want pq.ErrorCode) {
	t.Helper()
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, want, pgErr.Code)
}
