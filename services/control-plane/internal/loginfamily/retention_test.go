package loginfamily_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Fixtures establish authority through the public mint/capture/revoke adapters.
// SQL only supplies external lifecycle events (delivery completion and erasure).
func retentionCohort(t *testing.T, db *sql.DB, user uuid.UUID, size int, blocked bool) ([]lf.AccessIdentity, uuid.UUID) {
	t.Helper()
	identities := make([]lf.AccessIdentity, size)
	for n := range identities {
		expiry := time.Now().Add(-time.Hour).Truncate(time.Second)
		if blocked && n == size-1 {
			expiry = expires()
		}
		identities[n] = newFamily(t, db, user, expiry)
	}
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.CaptureUserTargetsTx(ctx, tx, u, nil)
	require.NoError(t, err)
	result, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonErasure})
	require.NoError(t, err)
	require.Len(t, result.Owners, 1)
	require.NoError(t, tx.Commit())
	return identities, result.Owners[0].AuthOperationID
}
func retentionComplete(t *testing.T, db *sql.DB, op uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`UPDATE login_family_revocations SET completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1`, op)
	require.NoError(t, err)
}
func retentionSweep(t *testing.T, db *sql.DB, limit int) lf.GCResult {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := lf.SweepRetiredEvidence(bounded, db, limit)
	require.NoError(t, err)
	return result
}
func retentionEvidence(t *testing.T, db *sql.DB, user uuid.UUID, sids, families, targets, operations int64) {
	t.Helper()
	require.Equal(t, sids, count(t, db, `SELECT count(*) FROM login_family_sids WHERE user_id=$1`, user))
	require.Equal(t, families, count(t, db, `SELECT count(*) FROM login_families WHERE user_id=$1`, user))
	require.Equal(t, targets, count(t, db, `SELECT count(*) FROM login_family_revocation_targets WHERE user_id=$1`, user))
	require.Equal(t, operations, count(t, db, `SELECT count(*) FROM login_family_revocations WHERE user_id=$1`, user))
}

func TestRetentionExpiredSIDAndEmptyLiveFamily(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	newFamily(t, db, user, time.Now().Add(-time.Hour).Truncate(time.Second))
	got := retentionSweep(t, db, 1)
	require.EqualValues(t, 1, got.DeletedSIDs)
	require.EqualValues(t, 1, got.DeletedFamilies)
	retentionEvidence(t, db, user, 0, 0, 0, 0)
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM users WHERE id=$1`, user))
	require.Equal(t, lf.GCResult{}, retentionSweep(t, db, 100))
}

func TestRetentionUsableAccessAndAnyRefreshReference(t *testing.T) {
	for _, kind := range []string{"usable access", "expired revoked refresh", "expired revoked predecessor"} {
		t.Run(kind, func(t *testing.T) {
			db := database(t)
			user := testdb.CreateUser(t, db)
			expiry := time.Now().Add(-time.Hour).Truncate(time.Second)
			if kind == "usable access" {
				expiry = expires()
			}
			i := newFamily(t, db, user, expiry)
			if kind != "usable access" {
				id := i.SID
				var predecessor any
				if kind == "expired revoked predecessor" {
					id = uuid.New()
					predecessor = i.SID
				}
				_, err := db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,revoked_at,predecessor_id) VALUES($1,$2,$3,clock_timestamp()-interval '1 hour',clock_timestamp(),$4)`, id, user, id.String(), predecessor)
				require.NoError(t, err)
			}
			got := retentionSweep(t, db, 1)
			require.Zero(t, got.DeletedSIDs)
			retentionEvidence(t, db, user, 1, 1, 0, 0)
			if kind != "usable access" {
				_, err := db.Exec(`DELETE FROM refresh_tokens WHERE user_id=$1`, user)
				require.NoError(t, err)
				got = retentionSweep(t, db, 1)
				require.EqualValues(t, 1, got.DeletedSIDs)
				retentionEvidence(t, db, user, 0, 0, 0, 0)
			}
		})
	}
}

func TestRetentionAuthCohortPendingThenWholeCompletedUnit(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	_, op := retentionCohort(t, db, user, 3, false)
	got := retentionSweep(t, db, 1)
	require.Zero(t, got.DeletedFamilies)
	retentionEvidence(t, db, user, 3, 3, 3, 1)
	retentionComplete(t, db, op)
	got = retentionSweep(t, db, 1)
	require.EqualValues(t, 3, got.DeletedSIDs)
	require.EqualValues(t, 3, got.DeletedFamilies)
	require.EqualValues(t, 1, got.DeletedOperations)
	retentionEvidence(t, db, user, 0, 0, 0, 0)
}

func TestRetentionOneUsableMemberPreservesCompleteSealedCohort(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	_, op := retentionCohort(t, db, user, 3, true)
	retentionComplete(t, db, op)
	got := retentionSweep(t, db, 1)
	require.Zero(t, got.DeletedSIDs)
	require.Zero(t, got.DeletedFamilies)
	require.Zero(t, got.DeletedOperations)
	retentionEvidence(t, db, user, 3, 3, 3, 1)
}

func TestRetentionActualErasedPrincipalRetainsPendingThenCollects(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	_, op := retentionCohort(t, db, user, 2, false)
	_, err := db.Exec(`DELETE FROM users WHERE id=$1`, user)
	require.NoError(t, err)
	require.Zero(t, count(t, db, `SELECT count(*) FROM users WHERE id=$1`, user))
	retentionSweep(t, db, 1)
	retentionEvidence(t, db, user, 2, 2, 2, 1)
	retentionComplete(t, db, op)
	got := retentionSweep(t, db, 1)
	require.EqualValues(t, 2, got.DeletedFamilies)
	retentionEvidence(t, db, user, 0, 0, 0, 0)
	require.Zero(t, count(t, db, `SELECT count(*) FROM users WHERE id=$1`, user))
}

func TestRetentionCompleteCohortExceedsOneHundredLockPages(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	_, op := retentionCohort(t, db, user, 205, false)
	retentionComplete(t, db, op)
	got := retentionSweep(t, db, 1)
	require.EqualValues(t, 205, got.DeletedSIDs)
	require.EqualValues(t, 205, got.DeletedFamilies)
	require.EqualValues(t, 1, got.DeletedOperations)
	retentionEvidence(t, db, user, 0, 0, 0, 0)
}

func TestRetentionEpochParentMustSettleWithoutBeingRecreated(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	newFamily(t, db, user, time.Now().Add(-time.Hour).Truncate(time.Second))
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.CaptureEpochTargetsTx(ctx, tx, u, "")
	require.NoError(t, err)
	replacement := "11111111111111111111111111111111"
	generation := uuid.New()
	_, err = tx.Exec(`INSERT INTO credential_epoch_voice_ejections(user_id,credential_epoch,superseded_credential_epoch,generation) VALUES($1,$2,'',$3)`, user, replacement, generation)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, user, replacement)
	require.NoError(t, err)
	_, err = lf.RevokeFamiliesForEpochTx(ctx, tx, targets, lf.EpochOwner{Generation: generation, UserID: user, CredentialEpoch: replacement})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	retentionSweep(t, db, 1)
	retentionEvidence(t, db, user, 1, 1, 0, 0)
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM credential_epoch_voice_ejections WHERE generation=$1`, generation))
	_, err = db.Exec(`DELETE FROM credential_epoch_voice_ejections WHERE generation=$1`, generation)
	require.NoError(t, err)
	got := retentionSweep(t, db, 1)
	require.EqualValues(t, 1, got.DeletedFamilies)
	retentionEvidence(t, db, user, 0, 0, 0, 0)
	require.Zero(t, count(t, db, `SELECT count(*) FROM credential_epoch_voice_ejections WHERE generation=$1`, generation))
}

func TestRetentionRejectsInvalidLimitAndUnavailableDependencies(t *testing.T) {
	db := database(t)
	for _, limit := range []int{0, -1} {
		_, err := lf.SweepRetiredEvidence(ctx, db, limit)
		reason(t, err, lf.ErrContract, lf.ReasonArgumentInvalid)
	}
	_, err := lf.SweepRetiredEvidence(ctx, nil, 1)
	require.ErrorIs(t, err, lf.ErrContract)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = lf.SweepRetiredEvidence(canceled, db, 1)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	closed, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	_, err = lf.SweepRetiredEvidence(ctx, closed, 1)
	require.ErrorIs(t, err, lf.ErrUnavailable)
}

func retentionTransportCleanup(t *testing.T, db *sql.DB) {
	t.Helper()
	t.Cleanup(func() {
		_, err := testdb.ExecReplica(db, `TRUNCATE voice_auth_room_bindings,voice_auth_connections,voice_auth_room_closures,voice_auth_connection_closures,voice_auth_boot_fences CASCADE`)
		require.NoError(t, err)
	})
}

func TestRetentionKeepsNativeConnectionAndItsFamilyEvidence(t *testing.T) {
	db := database(t)
	retentionTransportCleanup(t, db)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, time.Now().Add(-time.Hour).Truncate(time.Second))
	boot, conn := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_auth_connections(connection_generation,family_id,user_id,credential_epoch,node_boot_id,socket_id,registration_token_sha256,registration_body_sha256)
 SELECT $1,family_id,user_id,'',$2,'retention-native-socket',repeat('1',64),repeat('2',64) FROM login_family_sids WHERE sid=$3`, conn, boot, i.SID)
	require.NoError(t, err)
	got := retentionSweep(t, db, 1)
	require.Zero(t, got.DeletedFamilies)
	retentionEvidence(t, db, user, 1, 1, 0, 0)
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_connections WHERE connection_generation=$1`, conn))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_boot_fences WHERE node_boot_id=$1 AND retired_at IS NULL`, boot))
}

func TestRetentionOldClosureNeedsPermanentBootRetirement(t *testing.T) {
	for _, state := range []string{"unknown", "active"} {
		t.Run(state, func(t *testing.T) {
			db := database(t)
			retentionTransportCleanup(t, db)
			boot, conn := uuid.New(), uuid.New()
			if state == "active" {
				_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
				require.NoError(t, err)
			}
			_, err := db.Exec(`INSERT INTO voice_auth_connection_closures(connection_generation,node_boot_id,socket_id,credential_epoch,variant,registration_token_sha256,registration_body_sha256,closed_at)
 VALUES($1,$2,'retention-old-socket','','unregistered',repeat('1',64),repeat('2',64),clock_timestamp()-interval '100 years')`, conn, boot)
			require.NoError(t, err)
			got := retentionSweep(t, db, 1)
			require.Zero(t, got.DeletedConnectionClosures)
			require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn))
		})
	}
}

// The fixture's retired fence represents the verified operator retirement gate.
// HTTP/native gate enforcement is a later integration prerequisite, not proven
// by inserting this SQL fixture.
func TestRetentionRetiredZeroLiveBootCollectsEligibleClosure(t *testing.T) {
	db := database(t)
	retentionTransportCleanup(t, db)
	boot, conn := retentionUnregisteredClosure(t, db)
	got := retentionSweep(t, db, 1)
	require.EqualValues(t, 1, got.DeletedConnectionClosures)
	require.Zero(t, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_boot_fences WHERE node_boot_id=$1 AND retired_at IS NOT NULL`, boot))
}

func retentionUnregisteredClosure(t *testing.T, db *sql.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	boot, conn := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO voice_auth_connection_closures(connection_generation,node_boot_id,socket_id,credential_epoch,variant,registration_token_sha256,registration_body_sha256,closed_at)
 VALUES($1,$2,'retention-retired-socket','','unregistered',repeat('1',64),repeat('2',64),clock_timestamp()-interval '100 years')`, conn, boot)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, boot)
	require.NoError(t, err)
	return boot, conn
}

func retentionBoundClosures(t *testing.T, db *sql.DB, user uuid.UUID, room bool) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	i := newFamily(t, db, user, expires())
	boot, conn, session := uuid.New(), uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
	require.NoError(t, err)
	tx := transaction(t, db)
	_, err = tx.Exec(`INSERT INTO voice_auth_connections(connection_generation,family_id,user_id,credential_epoch,node_boot_id,socket_id,registration_token_sha256,registration_body_sha256)
 SELECT $1,family_id,user_id,'',$2,'retention-bound',repeat('1',64),repeat('2',64) FROM login_family_sids WHERE sid=$3`, conn, boot, i.SID)
	require.NoError(t, err)
	if room {
		_, err = tx.Exec(`INSERT INTO voice_auth_room_closures(session_generation,connection_generation,node_boot_id,socket_id,credential_epoch,user_id,family_id,room_id,room_kind,closed_at)
 SELECT $1,connection_generation,node_boot_id,socket_id,credential_epoch,user_id,family_id,$2,'dm',clock_timestamp() FROM voice_auth_connections WHERE connection_generation=$3`, session, uuid.New(), conn)
		require.NoError(t, err)
	}
	_, err = tx.Exec(`INSERT INTO voice_auth_connection_closures(connection_generation,node_boot_id,socket_id,credential_epoch,variant,user_id,family_id,registration_token_sha256,registration_body_sha256,closed_at)
 SELECT connection_generation,node_boot_id,socket_id,credential_epoch,'bound',user_id,family_id,registration_token_sha256,registration_body_sha256,clock_timestamp() FROM voice_auth_connections WHERE connection_generation=$1`, conn)
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM voice_auth_connections WHERE connection_generation=$1`, conn)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = db.Exec(`UPDATE voice_auth_boot_fences SET retired_at=clock_timestamp(),updated_at=clock_timestamp() WHERE node_boot_id=$1`, boot)
	require.NoError(t, err)
	return boot, conn, session
}

func TestRetentionRetiredRoomClosureCollectedBeforeConnection(t *testing.T) {
	db := database(t)
	retentionTransportCleanup(t, db)
	user := testdb.CreateUser(t, db)
	boot, conn, session := retentionBoundClosures(t, db, user, true)
	got := retentionSweep(t, db, 1)
	require.EqualValues(t, 1, got.DeletedRoomClosures)
	require.EqualValues(t, 1, got.DeletedConnectionClosures)
	require.Zero(t, count(t, db, `SELECT count(*) FROM voice_auth_room_closures WHERE session_generation=$1`, session))
	require.Zero(t, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot))
}

func TestRetentionRetiredClosurePendingDeliveryRefusesThenProgresses(t *testing.T) {
	for _, owner := range []string{"auth", "dm"} {
		t.Run(owner, func(t *testing.T) {
			db := database(t)
			retentionTransportCleanup(t, db)
			user := testdb.CreateUser(t, db)
			_, conn, _ := retentionBoundClosures(t, db, user, false)
			var op uuid.UUID
			conversation := uuid.New()
			if owner == "auth" {
				tx := transaction(t, db)
				u := authority(t, tx, user, lf.UserNoKeyUpdate)
				targets, err := lf.CaptureUserTargetsTx(ctx, tx, u, nil)
				require.NoError(t, err)
				r, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonErasure})
				require.NoError(t, err)
				op = r.Owners[0].AuthOperationID
				require.NoError(t, tx.Commit())
			} else {
				_, err := db.Exec(`INSERT INTO dm_block_voice_ejections(conversation_id,user_id) VALUES($1,$2)`, conversation, user)
				require.NoError(t, err)
			}
			got := retentionSweep(t, db, 1)
			require.Zero(t, got.DeletedConnectionClosures)
			require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1`, conn))
			if owner == "auth" {
				retentionComplete(t, db, op)
			} else {
				_, err := db.Exec(`DELETE FROM dm_block_voice_ejections WHERE conversation_id=$1 AND user_id=$2`, conversation, user)
				require.NoError(t, err)
			}
			got = retentionSweep(t, db, 1)
			require.EqualValues(t, 1, got.DeletedConnectionClosures)
		})
	}
}
