package age

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A reviewed authority API creates real capability-bound membership; no guessed SID mapping.
func ageFamily(t *testing.T, e *ageITEnv) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tx, err := e.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := tx.Rollback()
		if err != nil && err != sql.ErrTxDone {
			t.Error(err)
		}
	}()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{uuid.MustParse(e.userID)}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(uuid.MustParse(e.userID))
	require.NoError(t, err)
	var epoch string
	require.NoError(t, tx.QueryRow(`SELECT COALESCE(credential_epoch,'') FROM users WHERE id=$1`, e.userID).Scan(&epoch))
	family, err := loginfamily.CreateFamilyTx(ctx, tx, user, epoch)
	require.NoError(t, err)
	sid := uuid.New()
	require.NoError(t, loginfamily.BindSIDTx(ctx, tx, family, sid, time.Now().Add(time.Minute).Truncate(time.Second)))
	_, err = tx.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,NOW()+INTERVAL '1 hour')`, sid, e.userID, "age-family-fixture-"+sid.String())
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return family.Binding().FamilyID
}
func TestLoginFamilyTerminalAgeDisableRevokesAndValidClaimPreserves(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "disable"}[valid], func(t *testing.T) {
			e, cleanup := setupAgeIT(t, true)
			defer cleanup()
			family := ageFamily(t, e)
			claim := e.freshClaim()
			claim.ValidAge = valid
			w := e.submit(t, e.signBody(t, claim, nil))
			require.Equal(t, 200, w.Code, w.Body.String())
			var terminal bool
			require.NoError(t, e.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, family).Scan(&terminal))
			assert.Equal(t, !valid, terminal)
			assert.Equal(t, !valid, e.userDisabled(t))
			var count int
			require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL`, e.userID).Scan(&count))
			if valid {
				assert.Equal(t, 1, count)
			} else {
				assert.Zero(t, count)
			}
		})
	}
}
func TestLoginFamilyTerminalAgeFailureRollsBackDisableAndClaim(t *testing.T) {
	e, cleanup := setupAgeIT(t, true)
	defer cleanup()
	family := ageFamily(t, e)
	_, err := e.db.Exec(`ALTER TABLE login_family_revocations ADD CONSTRAINT terminal_age_fixture_refuse CHECK(false) NOT VALID`)
	require.NoError(t, err)
	defer func() {
		_, err := e.db.Exec(`ALTER TABLE login_family_revocations DROP CONSTRAINT terminal_age_fixture_refuse`)
		require.NoError(t, err)
	}()
	claim := e.freshClaim()
	claim.ValidAge = false
	_, err = e.h.persist(e.actor, claim, "fixture-signature")
	require.Error(t, err)
	assert.False(t, e.userDisabled(t))
	var terminal bool
	require.NoError(t, e.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, family).Scan(&terminal))
	assert.False(t, terminal)
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM age_verification_records WHERE user_id=$1`, e.userID).Scan(&n))
	assert.Zero(t, n)
}

// Both claim branches must wait on users before touching the age child. The
// NOWAIT row lock below distinguishes users-first from merely eventually waiting.
func TestLoginFamilyTerminalAgeLocksUserBeforeAgeRecord(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "disable"}[valid], func(t *testing.T) {
			e, cleanup := setupAgeIT(t, true)
			defer cleanup()
			ageFamily(t, e)
			claim := e.freshClaim()
			_, err := e.h.persist(e.actor, claim, "initial-fixture-signature")
			require.NoError(t, err)
			blocker, err := e.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			defer func() {
				err := blocker.Rollback()
				if err != nil && err != sql.ErrTxDone {
					t.Error(err)
				}
			}()
			var pid int
			require.NoError(t, blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
			var user string
			require.NoError(t, blocker.QueryRow(`SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, e.userID).Scan(&user))
			claim.ValidAge = valid
			done := make(chan error, 1)
			go func() {
				_, err := e.h.persist(e.actor, claim, "replacement-fixture-signature")
				done <- err
			}()
			defer func() {
				err := blocker.Rollback()
				if err != nil && err != sql.ErrTxDone {
					t.Error(err)
				}
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(10 * time.Second):
					t.Error("claim did not finish after user unlock")
				}
			}()
			require.Eventually(t, func() bool {
				var n int
				err := e.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&n)
				return err == nil && n > 0
			}, 5*time.Second, 10*time.Millisecond, "claim must reach the user authority lock")
			probe, err := e.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			defer func() {
				err := probe.Rollback()
				if err != nil && err != sql.ErrTxDone {
					t.Error(err)
				}
			}()
			require.NoError(t, probe.QueryRow(`SELECT user_id FROM age_verification_records WHERE user_id=$1 FOR UPDATE NOWAIT`, e.userID).Scan(&user), "waiting claim must not already own its age record")
			require.NoError(t, probe.Rollback())
		})
	}
}

// A login revoked while its claim waited must not disable the account and
// revoke the surviving logins: persist revalidates the requesting family
// under the user lock.
func TestLoginFamilyRevokedRequesterCannotDisable(t *testing.T) {
	e, cleanup := setupAgeIT(t, true)
	defer cleanup()
	survivor := ageFamily(t, e)
	identity, ok := loginfamily.AccessIdentityFromContext(e.actor)
	require.True(t, ok)

	ctx := context.Background()
	revoke, err := e.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, revoke)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(ctx, revoke, barrier, []uuid.UUID{identity.UserID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(identity.UserID)
	require.NoError(t, err)
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, revoke, user, identity.SID)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(ctx, revoke, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonLogout})
	require.NoError(t, err)
	require.NoError(t, revoke.Commit())

	claim := e.freshClaim()
	claim.ValidAge = false
	w := e.submit(t, e.signBody(t, claim, nil))

	require.Equal(t, 401, w.Code, w.Body.String())
	assert.False(t, e.userDisabled(t))
	var terminal bool
	require.NoError(t, e.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, survivor).Scan(&terminal))
	assert.False(t, terminal, "the surviving login stays live")
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM age_verification_records WHERE user_id=$1`, e.userID).Scan(&n))
	assert.Zero(t, n)
}
