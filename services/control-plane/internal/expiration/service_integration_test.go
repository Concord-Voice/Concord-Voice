//go:build integration

package expiration

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type expirationFixture struct {
	db           *sql.DB
	owner        uuid.UUID
	server       uuid.UUID
	channel      uuid.UUID
	conversation uuid.UUID
}

func newExpirationFixture(t *testing.T) expirationFixture {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	owner := testdb.CreateUser(t, db)
	other := testdb.CreateUser(t, db)
	server, channel, conversation := uuid.New(), uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'expiration test server', $2)`, server, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'expiration test channel', 'text')`, channel, server)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, conversation, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, owner, other)
	require.NoError(t, err)
	return expirationFixture{db: db, owner: owner, server: server, channel: channel, conversation: conversation}
}

func startChannel(t *testing.T, f expirationFixture, request Request) Policy {
	return startChannelDB(t, f.db, f.channel.String(), request)
}

func startChannelDB(t *testing.T, db *sql.DB, channelID string, request Request) Policy {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	policy, err := NewService(db).StartChannel(context.Background(), tx, channelID, request)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return policy
}

func timezoneDB(t *testing.T) *sql.DB {
	t.Helper()
	parsed, err := url.Parse(testdb.DatabaseURL())
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("TimeZone", "America/New_York")
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func startConversation(t *testing.T, f expirationFixture, request Request) Policy {
	t.Helper()
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	policy, err := NewService(f.db).StartConversation(context.Background(), tx, f.conversation.String(), request)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return policy
}

func insertChannelMessage(t *testing.T, f expirationFixture, created time.Time) string {
	t.Helper()
	var id string
	require.NoError(t, f.db.QueryRow(`INSERT INTO messages (channel_id, user_id, content, created_at) VALUES ($1, $2, 'ciphertext', $3) RETURNING id`, f.channel, f.owner, created).Scan(&id))
	return id
}

func insertDMMessage(t *testing.T, f expirationFixture, created time.Time) string {
	t.Helper()
	var id string
	require.NoError(t, f.db.QueryRow(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at) VALUES ($1, $2, 'ciphertext', 'user', $3) RETURNING id`, f.conversation, f.owner, created).Scan(&id))
	return id
}

func expirationAt(t *testing.T, db *sql.DB, table, id string) time.Time {
	t.Helper()
	var query string
	switch table {
	case "messages":
		query = `SELECT expires_at FROM messages WHERE id = $1`
	case "dm_messages":
		query = `SELECT expires_at FROM dm_messages WHERE id = $1`
	default:
		t.Fatalf("unsupported expiration table %q", table)
	}
	var got time.Time
	require.NoError(t, db.QueryRow(query, id).Scan(&got))
	return got.UTC()
}

func expirationAtOrNil(t *testing.T, db *sql.DB, table, id string) *time.Time {
	t.Helper()
	var query string
	switch table {
	case "messages":
		query = `SELECT expires_at FROM messages WHERE id = $1`
	case "dm_messages":
		query = `SELECT expires_at FROM dm_messages WHERE id = $1`
	default:
		t.Fatalf("unsupported expiration table %q", table)
	}
	var got sql.NullTime
	require.NoError(t, db.QueryRow(query, id).Scan(&got))
	if !got.Valid {
		return nil
	}
	value := got.Time.UTC()
	return &value
}

func installBatchFailureAudit(t *testing.T, f expirationFixture) {
	t.Helper()
	_, err := f.db.Exec(`
CREATE TABLE expiration_test_update_audit (
	channel_id UUID NOT NULL,
	message_id UUID NOT NULL,
	txid BIGINT NOT NULL
);
CREATE TABLE expiration_test_update_control (
	channel_id UUID PRIMARY KEY,
	fail_after INTEGER
);
CREATE OR REPLACE FUNCTION expiration_test_audit_update() RETURNS trigger AS $fn$
BEGIN
	IF EXISTS (
		SELECT 1 FROM expiration_test_update_control c
		WHERE c.channel_id = NEW.channel_id
		  AND c.fail_after IS NOT NULL
		  AND (SELECT count(*) FROM expiration_test_update_audit a WHERE a.channel_id = NEW.channel_id) >= c.fail_after
	) THEN
		RAISE EXCEPTION 'expiration test batch failure';
	END IF;
	INSERT INTO expiration_test_update_audit(channel_id, message_id, txid)
	VALUES (NEW.channel_id, NEW.id, txid_current());
	RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
CREATE TRIGGER expiration_test_audit_trigger
AFTER UPDATE OF expires_at ON messages
FOR EACH ROW EXECUTE FUNCTION expiration_test_audit_update();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS expiration_test_audit_trigger ON messages`,
			`DROP FUNCTION IF EXISTS expiration_test_audit_update()`,
			`DROP TABLE IF EXISTS expiration_test_update_control`,
			`DROP TABLE IF EXISTS expiration_test_update_audit`,
		} {
			if _, cleanupErr := f.db.Exec(statement); cleanupErr != nil {
				t.Errorf("expiration test cleanup %q: %v", statement, cleanupErr)
			}
		}
	})
	_, err = f.db.Exec(`INSERT INTO expiration_test_update_control(channel_id, fail_after) VALUES ($1, 5000)`, f.channel)
	require.NoError(t, err)
}

func TestService_StartRollbackLeavesCallerTransactionUnchanged(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	_, err = NewService(f.db).StartChannel(context.Background(), tx, f.channel.String(), Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	var revision int64
	var policy sql.NullInt64
	require.NoError(t, f.db.QueryRow(`SELECT expiration_revision, expiration_window_seconds FROM channels WHERE id = $1`, f.channel).Scan(&revision, &policy))
	assert.Zero(t, revision)
	assert.False(t, policy.Valid)
}

func TestService_StartResumeChannel_UsesOriginalCreatedAtAndUTCConversion(t *testing.T) {
	f := newExpirationFixture(t)
	db := timezoneDB(t)
	created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	var id string
	require.NoError(t, db.QueryRow(`INSERT INTO messages (channel_id, user_id, content, created_at) VALUES ($1, $2, 'ciphertext', $3) RETURNING id`, f.channel, f.owner, created).Scan(&id))
	window := 3600
	policy := startChannelDB(t, db, f.channel.String(), Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	assert.True(t, policy.BackfillPending)
	got, err := NewService(db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.NoError(t, err)
	assert.False(t, got.BackfillPending)
	assert.Equal(t, created.Add(time.Hour), expirationAt(t, db, "messages", id))
}

func TestService_StartResumeConversation_UsesOriginalCreatedAt(t *testing.T) {
	f := newExpirationFixture(t)
	created := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	id := insertDMMessage(t, f, created)
	window := 86400
	policy := startConversation(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	got, err := NewService(f.db).ResumeConversation(context.Background(), f.conversation.String(), policy.Revision)
	require.NoError(t, err)
	assert.False(t, got.BackfillPending)
	assert.Equal(t, created.Add(24*time.Hour), expirationAt(t, f.db, "dm_messages", id))
}

func TestService_ApplyIncludesMessageCreatedExactlyAtCutoff(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	policy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	require.NotNil(t, policy.UpdatedAt)
	id := insertChannelMessage(t, f, *policy.UpdatedAt)
	_, err := NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.NoError(t, err)
	assert.Equal(t, policy.UpdatedAt.Add(time.Hour).UTC(), expirationAt(t, f.db, "messages", id))
}

func TestService_ApplyBackfillBatchesAt5000Rows(t *testing.T) {
	f := newExpirationFixture(t)
	installBatchFailureAudit(t, f)
	created := time.Now().UTC().Add(-48 * time.Hour)
	_, err := f.db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at) SELECT $1, $2, 'ciphertext', $3 FROM generate_series(1, 5001)`, f.channel, f.owner, created)
	require.NoError(t, err)
	window := 3600
	policy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	got, err := NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.Error(t, err)
	assert.True(t, got.BackfillPending)
	var stamped int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND expires_at IS NOT NULL`, f.channel).Scan(&stamped))
	assert.Equal(t, 5000, stamped)
	var committedBatches int
	require.NoError(t, f.db.QueryRow(`SELECT count(DISTINCT txid) FROM expiration_test_update_audit WHERE channel_id = $1`, f.channel).Scan(&committedBatches))
	assert.Equal(t, 1, committedBatches)
	var firstBatchRows int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM expiration_test_update_audit WHERE channel_id = $1`, f.channel).Scan(&firstBatchRows))
	assert.Equal(t, 5000, firstBatchRows)
	_, err = f.db.Exec(`UPDATE expiration_test_update_control SET fail_after = NULL WHERE channel_id = $1`, f.channel)
	require.NoError(t, err)
	got, err = NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.NoError(t, err)
	assert.False(t, got.BackfillPending)
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND expires_at IS NOT NULL`, f.channel).Scan(&stamped))
	assert.Equal(t, 5001, stamped)
	require.NoError(t, f.db.QueryRow(`SELECT count(DISTINCT txid) FROM expiration_test_update_audit WHERE channel_id = $1`, f.channel).Scan(&committedBatches))
	assert.Equal(t, 2, committedBatches)
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM expiration_test_update_audit WHERE channel_id = $1`, f.channel).Scan(&firstBatchRows))
	assert.Equal(t, 5001, firstBatchRows)
}

func TestService_ZeroEffectBatchProbesEligibilityForChannelAndConversation(t *testing.T) {
	f := newExpirationFixture(t)
	created := time.Now().UTC().Truncate(time.Microsecond).Add(-48 * time.Hour)
	channelMessage := insertChannelMessage(t, f, created)
	window := 3600
	channelPolicy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})

	noOpChannelScope := channelScope
	noOpChannelScope.applyBatchSQL = `
UPDATE messages
SET expires_at = expires_at
WHERE channel_id = $1 AND created_at <= $2 AND false
  AND expires_at IS DISTINCT FROM (created_at + make_interval(secs => $3))`
	probeErrorChannelScope := noOpChannelScope
	probeErrorChannelScope.applyEligibleSQL = `SELECT invalid_expiration_probe`
	got, err := NewService(f.db).runBatches(context.Background(), probeErrorChannelScope, f.channel.String(), channelPolicy.Revision)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe channel expiration backfill eligibility")
	assert.True(t, got.BackfillPending)
	assert.Nil(t, expirationAtOrNil(t, f.db, "messages", channelMessage))
	var mode sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT expiration_backfill_mode FROM channels WHERE id = $1`, f.channel).Scan(&mode))
	assert.True(t, mode.Valid)

	got, err = NewService(f.db).runBatches(context.Background(), noOpChannelScope, f.channel.String(), channelPolicy.Revision)
	assert.ErrorIs(t, err, ErrBackfillPending)
	assert.True(t, got.BackfillPending)
	assert.Nil(t, expirationAtOrNil(t, f.db, "messages", channelMessage))
	got, err = NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), channelPolicy.Revision)
	require.NoError(t, err)
	assert.False(t, got.BackfillPending)

	dmMessage := insertDMMessage(t, f, created)
	clearAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	_, err = f.db.Exec(`UPDATE dm_messages SET expires_at = $2 WHERE id = $1`, dmMessage, clearAt)
	require.NoError(t, err)
	conversationPolicy := startConversation(t, f, Request{Mode: "clear", Retroactive: "clear_pending"})
	noOpConversationScope := conversationScope
	noOpConversationScope.clearBatchSQL = `
UPDATE dm_messages
SET expires_at = expires_at
WHERE conversation_id = $1 AND expires_at > $2 AND false`
	got, err = NewService(f.db).runBatches(context.Background(), noOpConversationScope, f.conversation.String(), conversationPolicy.Revision)
	assert.ErrorIs(t, err, ErrBackfillPending)
	assert.True(t, got.BackfillPending)
	assert.Equal(t, clearAt, expirationAt(t, f.db, "dm_messages", dmMessage))
	got, err = NewService(f.db).ResumeConversation(context.Background(), f.conversation.String(), conversationPolicy.Revision)
	require.NoError(t, err)
	assert.False(t, got.BackfillPending)
	assert.Nil(t, expirationAtOrNil(t, f.db, "dm_messages", dmMessage))
}

func TestService_ResumeCapsFiveBatchesAndRetainsPendingMarker(t *testing.T) {
	f := newExpirationFixture(t)
	created := time.Now().UTC().Truncate(time.Microsecond).Add(-48 * time.Hour)
	_, err := f.db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at) SELECT $1, $2, 'ciphertext', $3 FROM generate_series(1, 25001)`, f.channel, f.owner, created)
	require.NoError(t, err)
	dueID := insertChannelMessage(t, f, created)
	dueAt := created.Add(-time.Hour)
	_, err = f.db.Exec(`UPDATE messages SET expires_at = $2 WHERE id = $1`, dueID, dueAt)
	require.NoError(t, err)

	window := 3600
	policy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	got, err := NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.ErrorIs(t, err, ErrBackfillPending)
	assert.True(t, got.BackfillPending)
	assert.Equal(t, policy.Revision, got.Revision)
	var stamped int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND expires_at IS NOT NULL`, f.channel).Scan(&stamped))
	assert.Equal(t, 25001, stamped)
	var applied int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND expires_at = $2`, f.channel, created.Add(time.Hour)).Scan(&applied))
	assert.Equal(t, 25000, applied)
	assert.Equal(t, dueAt, expirationAt(t, f.db, "messages", dueID))
	var mode sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT expiration_backfill_mode FROM channels WHERE id = $1`, f.channel).Scan(&mode))
	assert.True(t, mode.Valid)

	got, err = NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.NoError(t, err)
	assert.False(t, got.BackfillPending)
	assert.Equal(t, policy.Revision, got.Revision)
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND expires_at IS NOT NULL`, f.channel).Scan(&stamped))
	assert.Equal(t, 25002, stamped)
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1 AND expires_at = $2`, f.channel, created.Add(time.Hour)).Scan(&applied))
	assert.Equal(t, 25001, applied)
	assert.Equal(t, dueAt, expirationAt(t, f.db, "messages", dueID))
	require.NoError(t, f.db.QueryRow(`SELECT expiration_backfill_mode FROM channels WHERE id = $1`, f.channel).Scan(&mode))
	assert.False(t, mode.Valid)
}

func TestService_StartChoicesAndPendingGuards(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	for _, tc := range []struct {
		name    string
		request Request
		pending bool
	}{
		{"new only", Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"}, false},
		{"leave pending", Request{Mode: "clear", Retroactive: "leave_pending"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := startChannel(t, f, tc.request)
			assert.Equal(t, tc.pending, policy.BackfillPending)
		})
	}
	first := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"})
	second := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"})
	assert.Equal(t, first.Revision+1, second.Revision)
	require.NotNil(t, first.UpdatedAt)
	require.NotNil(t, second.UpdatedAt)
	assert.False(t, second.UpdatedAt.Before(*first.UpdatedAt))
	policy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	_, err = NewService(f.db).StartChannel(context.Background(), tx, f.channel.String(), Request{Mode: "clear", Retroactive: "clear_pending"})
	assert.ErrorIs(t, err, ErrBackfillPending)
	require.NoError(t, tx.Rollback())

	_, err = NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision+1)
	assert.ErrorIs(t, err, ErrRevisionMismatch)
}

func TestService_StartResumeMatchingPendingRevisionLeavesPolicyUnchanged(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	original := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	before, err := readPolicy(context.Background(), f.db, channelScope, f.channel.String())
	require.NoError(t, err)
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	resumed, err := NewService(f.db).StartChannel(context.Background(), tx, f.channel.String(), Request{Mode: "resume", Revision: &original.Revision})
	require.NoError(t, err)
	assert.Equal(t, original.Revision, resumed.Revision)
	assert.Equal(t, original.WindowSeconds, resumed.WindowSeconds)
	assert.Equal(t, original.UpdatedAt, resumed.UpdatedAt)
	assert.True(t, resumed.BackfillPending)
	require.NoError(t, tx.Commit())
	after, err := readPolicy(context.Background(), f.db, channelScope, f.channel.String())
	require.NoError(t, err)
	assert.Equal(t, before.policy, after.policy)
	assert.Equal(t, before.mode, after.mode)
	assert.Equal(t, before.cutoff, after.cutoff)
}

func TestService_StartResumeRejectsWrongOrCompletedRevisionWithoutMutation(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	original := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"})
	before, err := readPolicy(context.Background(), f.db, channelScope, f.channel.String())
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		revision int64
		wantErr  error
	}{
		{"wrong revision", original.Revision + 1, ErrRevisionMismatch},
		{"no pending marker", original.Revision, ErrBackfillNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.db.Begin()
			require.NoError(t, err)
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					t.Errorf("rollback transaction: %v", rollbackErr)
				}
			}()
			_, err = NewService(f.db).StartChannel(context.Background(), tx, f.channel.String(), Request{Mode: "resume", Revision: &tc.revision})
			assert.ErrorIs(t, err, tc.wantErr)
			require.NoError(t, tx.Commit())
			after, readErr := readPolicy(context.Background(), f.db, channelScope, f.channel.String())
			require.NoError(t, readErr)
			assert.Equal(t, before.policy, after.policy)
			assert.Equal(t, before.mode, after.mode)
			assert.Equal(t, before.cutoff, after.cutoff)
		})
	}
}

func TestService_StartResumeConversationLeavesPolicyUnchanged(t *testing.T) {
	f := newExpirationFixture(t)
	window := 86400
	original := startConversation(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	before, err := readPolicy(context.Background(), f.db, conversationScope, f.conversation.String())
	require.NoError(t, err)
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	resumed, err := NewService(f.db).StartConversation(context.Background(), tx, f.conversation.String(), Request{Mode: "resume", Revision: &original.Revision})
	require.NoError(t, err)
	assert.Equal(t, original.Revision, resumed.Revision)
	assert.Equal(t, original.UpdatedAt, resumed.UpdatedAt)
	require.NoError(t, tx.Commit())
	after, err := readPolicy(context.Background(), f.db, conversationScope, f.conversation.String())
	require.NoError(t, err)
	assert.Equal(t, before.policy, after.policy)
	assert.Equal(t, before.mode, after.mode)
	assert.Equal(t, before.cutoff, after.cutoff)
}

func TestService_StartResumeMissingScopes(t *testing.T) {
	f := newExpirationFixture(t)
	for _, tc := range []struct {
		name  string
		start func(*sql.Tx) error
	}{
		{name: "channel", start: func(tx *sql.Tx) error {
			_, err := NewService(f.db).StartChannel(context.Background(), tx, uuid.NewString(), Request{Mode: "resume", Revision: ptr(int64(1))})
			return err
		}},
		{name: "conversation", start: func(tx *sql.Tx) error {
			_, err := NewService(f.db).StartConversation(context.Background(), tx, uuid.NewString(), Request{Mode: "resume", Revision: ptr(int64(1))})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.db.Begin()
			require.NoError(t, err)
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					t.Errorf("rollback transaction: %v", rollbackErr)
				}
			}()
			err = tc.start(tx)
			assert.ErrorIs(t, err, ErrScopeNotFound)
			require.NoError(t, tx.Commit())
		})
	}
}

func TestService_ClearDoesNotResurrectDueRowsAtCutoff(t *testing.T) {
	f := newExpirationFixture(t)
	created := time.Now().UTC().Add(-48 * time.Hour)
	window := 3600
	id := insertChannelMessage(t, f, created)
	futureID := insertChannelMessage(t, f, time.Now().UTC().Add(time.Hour))
	_, err := f.db.Exec(`UPDATE messages SET expires_at = $2 WHERE id = $1`, futureID, time.Now().UTC().Add(48*time.Hour))
	require.NoError(t, err)
	policy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	_, err = NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.NoError(t, err)
	expired := expirationAt(t, f.db, "messages", id)
	assert.True(t, expired.Before(time.Now().UTC()))

	clearPolicy := startChannel(t, f, Request{Mode: "clear", Retroactive: "clear_pending"})
	_, err = NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), clearPolicy.Revision)
	require.NoError(t, err)
	assert.Equal(t, expired, expirationAt(t, f.db, "messages", id))
	var future sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT expires_at FROM messages WHERE id = $1`, futureID).Scan(&future))
	assert.False(t, future.Valid, "clear_pending clears future expiry timestamps")

	longer := 86400
	longPolicy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &longer, Retroactive: "apply"})
	_, err = NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), longPolicy.Revision)
	require.NoError(t, err)
	assert.Equal(t, expired, expirationAt(t, f.db, "messages", id), "longer apply cannot resurrect a due row")
}

func TestService_LeavePendingLeavesExistingRowsUnchanged(t *testing.T) {
	f := newExpirationFixture(t)
	id := insertChannelMessage(t, f, time.Now().UTC().Add(-time.Hour))
	want := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	_, err := f.db.Exec(`UPDATE messages SET expires_at = $2 WHERE id = $1`, id, want)
	require.NoError(t, err)
	policy := startChannel(t, f, Request{Mode: "clear", Retroactive: "leave_pending"})
	assert.False(t, policy.BackfillPending)
	assert.Equal(t, want, expirationAt(t, f.db, "messages", id))
}

func TestService_CanceledResumeLeavesPendingMarker(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	policy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := NewService(f.db).ResumeChannel(ctx, f.channel.String(), policy.Revision)
	require.Error(t, err)
	assert.True(t, got.BackfillPending)
	var mode sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT expiration_backfill_mode FROM channels WHERE id = $1`, f.channel).Scan(&mode))
	assert.True(t, mode.Valid)
	retried, retryErr := NewService(f.db).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.NoError(t, retryErr)
	assert.False(t, retried.BackfillPending)
}

func TestService_ResumeDatabaseFailureRetainsPendingFallback(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	policy := startChannel(t, f, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"})
	broken, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, broken.Close())

	got, err := NewService(broken).ResumeChannel(context.Background(), f.channel.String(), policy.Revision)
	require.Error(t, err)
	assert.True(t, got.BackfillPending)
}

func TestService_NotFoundAndUnreadySentinels(t *testing.T) {
	f := newExpirationFixture(t)
	tx, err := f.db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	_, err = NewService(f.db).StartChannel(context.Background(), tx, uuid.NewString(), Request{Mode: "clear", Retroactive: "leave_pending"})
	assert.ErrorIs(t, err, ErrScopeNotFound)
	require.NoError(t, tx.Rollback())
	_, err = (*Service)(nil).ResumeChannel(context.Background(), f.channel.String(), 0)
	assert.ErrorIs(t, err, ErrServiceUnready)
	assert.False(t, errors.Is(err, ErrBackfillPending))
}
