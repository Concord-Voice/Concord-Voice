package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration000145_ParentLockSerializesRevocationInsert(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	conversationID := testhelpers.CreateUser(t, db)
	ownerID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, ownerID)
	require.NoError(t, err)
	down := migrationReadFile(t, "../../migrations/000145_serialize_dm_revocation_parent_lock.down.sql")
	up := migrationReadFile(t, "../../migrations/000145_serialize_dm_revocation_parent_lock.up.sql")
	_, err = db.Exec(down)
	require.NoError(t, err)
	migrationDown := true
	t.Cleanup(func() {
		if migrationDown {
			_, restoreErr := db.Exec(up)
			require.NoError(t, restoreErr, "restore migration 000145 after test")
		}
	})
	parentTx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = parentTx.Rollback() }()
	_, err = parentTx.Exec(`SELECT id FROM dm_conversations WHERE id = $1 FOR SHARE`, conversationID)
	require.NoError(t, err)
	oldConn, err := db.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, oldConn.Close()) })
	insertTx, err := oldConn.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = insertTx.Rollback() }()
	_, err = insertTx.Exec(`SET LOCAL lock_timeout = '100ms'`)
	require.NoError(t, err)
	oldDone := make(chan error, 1)
	go func() {
		_, insertErr := insertTx.Exec(`INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'member_removed', $2)`, conversationID, ownerID)
		oldDone <- insertErr
	}()
	select {
	case err = <-oldDone:
		require.NoError(t, err, "old FOR SHARE trigger is compatible with a parent FOR SHARE lock")
	case <-time.After(time.Second):
		t.Fatal("old FOR SHARE trigger unexpectedly waited on a parent FOR SHARE lock")
	}
	require.NoError(t, insertTx.Rollback())
	require.NoError(t, parentTx.Rollback())
	_, err = db.Exec(up)
	require.NoError(t, err)

	forwardTx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = forwardTx.Rollback() }()
	_, err = forwardTx.Exec(`SELECT id FROM dm_conversations WHERE id = $1 FOR SHARE`, conversationID)
	require.NoError(t, err)
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	var pid int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid))
	done := make(chan error, 1)
	go func() {
		_, insertErr := conn.ExecContext(context.Background(), `INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'member_removed', $2)`, conversationID, ownerID)
		done <- insertErr
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, pid).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, forwardTx.Commit())
	require.NoError(t, <-done)
	_, err = db.Exec(`DELETE FROM dm_key_revocations WHERE conversation_id = $1`, conversationID)
	require.NoError(t, err)
	_, err = db.Exec(up)
	require.NoError(t, err, "up migration must be replayable")
	migrationDown = false
}
