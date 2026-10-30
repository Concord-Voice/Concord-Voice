package testhelpers

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixtureIdentity(t *testing.T, db *sql.DB, token string, verified bool) loginfamily.AccessIdentity {
	t.Helper()
	claims, err := auth.ValidateAccessToken(token, TestJWTSecret)
	require.NoError(t, err)
	require.NotNil(t, claims.EmailVerified)
	assert.Equal(t, verified, *claims.EmailVerified)
	require.NotNil(t, claims.ExpiresAt)
	id, err := uuid.Parse(claims.UserID)
	require.NoError(t, err)
	sid, err := uuid.Parse(claims.SessionID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, sid)
	identity := loginfamily.AccessIdentity{UserID: id, SID: sid, CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time}
	grant, err := loginfamily.ResolveVerifiedAccess(context.Background(), db, identity)
	require.NoError(t, err, "actual signed JWT must have live committed SQL authority")
	assert.Equal(t, identity.SID, grant.SID)
	assert.Equal(t, identity.UserID, grant.UserID)
	assert.Equal(t, identity.CredentialEpoch, grant.CredentialEpoch)
	var expiry time.Time
	require.NoError(t, db.QueryRow(`SELECT access_expires_at FROM login_family_sids WHERE sid=$1 AND user_id=$2 AND family_id=$3`, sid, id, grant.FamilyID).Scan(&expiry))
	assert.True(t, expiry.Equal(claims.ExpiresAt.Time), "immutable SQL expiry must equal signed NumericDate")
	return identity
}

func TestAccessOnlyFamilyFixtures_ActualJWTAndExactAuthority(t *testing.T) {
	for _, verified := range []bool{true, false} {
		name := "unverified"
		if verified {
			name = "verified"
		}
		t.Run(name, func(t *testing.T) {
			require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
			db, cleanup := SetupTestDB(t)
			defer cleanup()
			ts := &TestServer{DB: db}
			var user TestUser
			if verified {
				user = ts.CreateTestUser(t, "familyverified")
			} else {
				user = ts.CreateTestUserUnverified(t, "familyunverified")
			}
			identity := fixtureIdentity(t, db, user.AccessToken, verified)
			for _, mutation := range []string{"SID", "expiry", "epoch"} {
				t.Run("reject mismatched "+mutation, func(t *testing.T) {
					wrong := identity
					switch mutation {
					case "SID":
						wrong.SID = uuid.New()
					case "expiry":
						wrong.AccessExpiresAt = wrong.AccessExpiresAt.Add(time.Second)
					case "epoch":
						wrong.CredentialEpoch = "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
					}
					_, err := loginfamily.ResolveVerifiedAccess(context.Background(), db, wrong)
					require.ErrorIs(t, err, loginfamily.ErrDenied)
				})
			}
			var refreshCount int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1`, user.ID).Scan(&refreshCount))
			assert.Zero(t, refreshCount, "access-only fixtures must preserve session counts")
		})
	}
}

func TestAccessOnlyFamilyFixtures_VerificationUsesFreshSIDAndCurrentEpoch(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	db, cleanup := SetupTestDB(t)
	defer cleanup()
	ts := &TestServer{DB: db}
	user := ts.CreateTestUserUnverified(t, "familyverify")
	before := fixtureIdentity(t, db, user.AccessToken, false)
	const epoch = "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	_, err := db.Exec(`UPDATE users SET credential_epoch=$1 WHERE id=$2`, epoch, user.ID)
	require.NoError(t, err)
	after := fixtureIdentity(t, db, ts.VerifyUserEmail(t, user.ID), true)
	assert.Equal(t, epoch, after.CredentialEpoch)
	assert.NotEqual(t, before.SID, after.SID)
	var verified bool
	require.NoError(t, db.QueryRow(`SELECT email_verified FROM users WHERE id=$1`, user.ID).Scan(&verified))
	assert.True(t, verified)
	var oldExpiry time.Time
	require.NoError(t, db.QueryRow(`SELECT access_expires_at FROM login_family_sids WHERE sid=$1`, before.SID).Scan(&oldExpiry))
	assert.True(t, before.AccessExpiresAt.Equal(oldExpiry), "verification must not rewrite an existing SID")
	_, err = loginfamily.ResolveVerifiedAccess(context.Background(), db, before)
	require.ErrorIs(t, err, loginfamily.ErrDenied)
	var families, refreshes int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM login_families WHERE user_id=$1`, user.ID).Scan(&families))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1`, user.ID).Scan(&refreshes))
	assert.Equal(t, 2, families, "verification chooses a fresh access-only family")
	assert.Zero(t, refreshes)
}

func TestAccessOnlyFamilyFixtures_DisposableCleanupRestoresIsolation(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	db, cleanup := SetupTestDB(t)
	defer cleanup()
	ts := &TestServer{DB: db}
	ctx := context.Background()
	// Two independently populated runs catch retained evidence surviving reset.
	for run := 0; run < 2; run++ {
		user := ts.CreateTestUser(t, "familycleanup")
		identity := fixtureIdentity(t, db, user.AccessToken, true)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Errorf("rollback cleanup fixture: %v", err)
			}
		}()
		barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
		require.NoError(t, err)
		users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{identity.UserID}, loginfamily.UserNoKeyUpdate)
		require.NoError(t, err)
		u, err := users.ForUser(identity.UserID)
		require.NoError(t, err)
		targets, err := loginfamily.CaptureUserTargetsTx(ctx, tx, u, nil)
		require.NoError(t, err)
		_, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonErasure})
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		boot := uuid.New()
		_, err = db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO voice_auth_connection_closures(connection_generation,node_boot_id,socket_id,credential_epoch,variant,registration_token_sha256,registration_body_sha256,closed_at) VALUES($1,$2,'fixture-unregistered','','unregistered',repeat('a',64),repeat('b',64),clock_timestamp())`, uuid.New(), boot)
		require.NoError(t, err)
		// Choose active initial state in this disposable fixture, without invoking
		// production activation or mutating an existing active floor to prepared.
		_, err = testdb.ExecReplica(db, `TRUNCATE login_family_rollout`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO login_family_rollout(id,activated_at,receipt) VALUES(TRUE,clock_timestamp(),'{}')`)
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE login_family_rollout SET activated_at=NULL,receipt=NULL WHERE id=TRUE`)
		require.Error(t, err, "one-way activation guard remains enabled")
		_, err = db.Exec(`DELETE FROM users WHERE id=$1`, user.ID)
		require.NoError(t, err)
		var retained int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM login_family_sids WHERE user_id=$1`, user.ID).Scan(&retained))
		require.Equal(t, 1, retained, "user deletion deliberately retains family evidence")
		require.NoError(t, testdb.TruncateAllTables(db))
		rows, err := db.Query(`
			SELECT 'login_family_sids', count(*) FROM login_family_sids
			UNION ALL SELECT 'login_families', count(*) FROM login_families
			UNION ALL SELECT 'login_family_revocations', count(*) FROM login_family_revocations
			UNION ALL SELECT 'login_family_revocation_targets', count(*) FROM login_family_revocation_targets
			UNION ALL SELECT 'voice_auth_connections', count(*) FROM voice_auth_connections
			UNION ALL SELECT 'voice_auth_room_bindings', count(*) FROM voice_auth_room_bindings
			UNION ALL SELECT 'voice_auth_boot_fences', count(*) FROM voice_auth_boot_fences
			UNION ALL SELECT 'voice_auth_connection_closures', count(*) FROM voice_auth_connection_closures
			UNION ALL SELECT 'voice_auth_room_closures', count(*) FROM voice_auth_room_closures`)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		for rows.Next() {
			var table string
			var count int
			require.NoError(t, rows.Scan(&table, &count))
			assert.Zero(t, count, table)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		var prepared bool
		require.NoError(t, db.QueryRow(`SELECT activated_at IS NULL AND receipt IS NULL FROM login_family_rollout WHERE id=TRUE`).Scan(&prepared))
		assert.True(t, prepared)
		var sequences, transports int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_command_sequence WHERE id=TRUE`).Scan(&sequences))
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_command_transport WHERE id=TRUE`).Scan(&transports))
		assert.Equal(t, 1, sequences)
		assert.Equal(t, 1, transports)
	}
}
