package auth

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/google/uuid"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginFamilyNormalAndGraceRefreshRetainIdentity(t *testing.T) {
	rig := newFamilyTestRig(t)
	mintContext, _ := familyTestContext()
	access, refresh, _, err := rig.h.IssueTokenPair(mintContext, rig.user, "")
	require.NoError(t, err)
	family, originalSID := assertFamilyAccess(t, rig, access)
	var originalExpiry string
	require.NoError(t, rig.db.QueryRow(`SELECT access_expires_at::text FROM login_family_sids WHERE sid = $1`, originalSID).Scan(&originalExpiry))
	for _, mode := range []string{"normal", "grace"} {
		t.Run(mode, func(t *testing.T) {
			c, w := familyTestContext()
			// Replay the original secret after its first successful rotation. Grace
			// must select its exact successor and preserve the logical family.
			c.Request.Header.Set("X-Refresh-Token", refresh)
			rig.h.Refresh(c)
			pair := familyResponse(t, w)
			nextFamily, nextSID := assertFamilyAccess(t, rig, pair.Access)
			assert.Equal(t, family, nextFamily)
			assert.NotEqual(t, originalSID, nextSID)
			assert.NotEqual(t, refresh, pair.Refresh)
			var retainedExpiry string
			require.NoError(t, rig.db.QueryRow(`SELECT access_expires_at::text FROM login_family_sids WHERE sid = $1`, originalSID).Scan(&retainedExpiry))
			assert.Equal(t, originalExpiry, retainedExpiry)
			oldFamily, oldSID := assertFamilyAccess(t, rig, access)
			assert.Equal(t, family, oldFamily)
			assert.Equal(t, originalSID, oldSID)
		})
	}
}

func TestLoginFamilyLegacyRefreshRequiresReauthentication(t *testing.T) {
	rig := newFamilyTestRig(t)
	// This is deliberately the unmapped historical fixture, never used as a
	// helper for family-aware successful authentication.
	_, err := rig.db.Exec(`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, remember_me) VALUES ($1, $2, NOW()+INTERVAL '1 day', true)`, rig.user, HashRefreshToken("legacy-fixture-secret"))
	require.NoError(t, err)
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", "legacy-fixture-secret")
	rig.h.Refresh(c)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "access_token")
	var families int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_families WHERE user_id = $1`, rig.user).Scan(&families))
	assert.Zero(t, families, "unknown predecessor lineage must never be guessed")
}

// A real users-row wait orders terminal commit before the final rotation guard.
// The request has already read a valid source; middleware snapshots cannot win.
func TestLoginFamilyRevocationWinsAgainstFetchedRefresh(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, secret, sid, err := rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
	require.NoError(t, err)
	source, err := rig.h.fetchActiveRefreshToken(HashRefreshToken(secret))
	require.NoError(t, err)
	ctx := context.Background()
	tx, err := rig.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := tx.Rollback()
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	user, err := lockMintUserTx(ctx, tx, rig.user)
	require.NoError(t, err)
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, tx, user, uuid.MustParse(sid))
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonExplicitRevoke})
	require.NoError(t, err)
	var blocker int
	require.NoError(t, tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker))
	c, w := familyTestContext()
	done := make(chan struct{})
	go func() { defer close(done); rig.h.rotateAndRespond(c, source, "") }()
	require.Eventually(t, func() bool {
		var n int
		err := rig.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, blocker).Scan(&n)
		return err == nil && n > 0
	}, 5*time.Second, 10*time.Millisecond, "refresh must actually reach and wait at the SQL authority barrier")
	select {
	case <-done:
		t.Fatal("refresh responded before terminal transaction committed")
	default:
	}
	require.NoError(t, tx.Commit())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish after terminal commit")
	}
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "access_token")
	assert.Empty(t, w.Header().Values("Set-Cookie"))
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, rig.user).Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_sids WHERE user_id = $1`, rig.user).Scan(&n))
	assert.Equal(t, 1, n, "a losing refresh cannot bind a new SID")
}

func TestLoginFamilyRefreshPreservesExpiredRefreshFence(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, secret, sid, err := rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
	require.NoError(t, err)
	_, err = rig.db.Exec(`UPDATE refresh_tokens SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, sid)
	require.NoError(t, err)
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	rig.h.Refresh(c)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "expired")
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_sids WHERE user_id = $1`, rig.user).Scan(&n))
	assert.Equal(t, 1, n)
}

// The public preparation clock is the historical-mint fixture seam. Construct
// real authority and signed lifetime evidence, then spend it at actual Refresh.
func TestLoginFamilyExpiredAccessDoesNotExpireRefreshAuthority(t *testing.T) {
	rig := newFamilyTestRig(t)
	ctx := context.Background()
	tx, err := rig.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := tx.Rollback()
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	user, err := lockMintUserTx(ctx, tx, rig.user)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(ctx, tx, user, "")
	require.NoError(t, err)
	sid := uuid.New()
	prepared, err := PrepareAccessToken(AccessTokenInput{UserID: uuid.MustParse(rig.user), SID: sid, EmailVerified: true}, time.Now().Add(-16*time.Minute))
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(ctx, tx, family, sid, prepared.ExpiresAt()))
	secret, err := GenerateRefreshToken()
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, remember_me) VALUES ($1,$2,$3,NOW()+INTERVAL '1 day',true)`, sid, rig.user, HashRefreshToken(secret))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	access, err := prepared.Sign(familyTestSecret)
	require.NoError(t, err)
	_, err = ValidateAccessToken(access, familyTestSecret)
	require.ErrorIs(t, err, ErrExpiredToken, "premise: access has actually expired")
	var exact bool
	require.NoError(t, rig.db.QueryRow(`SELECT access_expires_at = $2 FROM login_family_sids WHERE sid = $1`, sid, prepared.ExpiresAt()).Scan(&exact))
	require.True(t, exact)
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	rig.h.Refresh(c)
	next := familyResponse(t, w)
	nextFamily, nextSID := assertFamilyAccess(t, rig, next.Access)
	assert.Equal(t, family.Binding().FamilyID.String(), nextFamily)
	assert.NotEqual(t, sid.String(), nextSID)
}

func TestLoginFamilyGraceRefreshPreservesSuccessorMachineFence(t *testing.T) {
	rig := newFamilyTestRig(t)
	mintContext, _ := familyTestContext()
	_, secret, _, err := rig.h.IssueTokenPair(mintContext, rig.user, "")
	require.NoError(t, err)
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	c.Request.Header.Set("X-Machine-Id", "11111111-1111-4111-8111-111111111111")
	rig.h.Refresh(c)
	pair := familyResponse(t, w)
	_, sid := assertFamilyAccess(t, rig, pair.Access)
	var machine string
	require.NoError(t, rig.db.QueryRow(`SELECT machine_id FROM refresh_tokens WHERE id=$1 AND revoked_at IS NULL`, sid).Scan(&machine))
	require.Equal(t, "11111111-1111-4111-8111-111111111111", machine)
	c, w = familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	c.Request.Header.Set("X-Machine-Id", "22222222-2222-4222-8222-222222222222")
	rig.h.Refresh(c)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "access_token")
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL AND id=$2`, rig.user, sid).Scan(&n))
	assert.Equal(t, 1, n, "wrong-machine replay must not consume the legitimate successor")
}

func TestLoginFamilyGraceRefreshCannotReviveExpiredSuccessor(t *testing.T) {
	rig := newFamilyTestRig(t)
	mintContext, _ := familyTestContext()
	_, secret, _, err := rig.h.IssueTokenPair(mintContext, rig.user, "")
	require.NoError(t, err)
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	rig.h.Refresh(c)
	pair := familyResponse(t, w)
	_, sid := assertFamilyAccess(t, rig, pair.Access)
	_, err = rig.db.Exec(`UPDATE refresh_tokens SET expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, sid)
	require.NoError(t, err)
	c, w = familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	rig.h.Refresh(c)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "access_token")
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_sids WHERE user_id=$1`, rig.user).Scan(&n))
	assert.Equal(t, 2, n, "failed grace replay cannot create a third membership")
}

func TestLoginFamilyGraceRequiresCorroboratingSourceSignal(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, secret, _, err := rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
	require.NoError(t, err)
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	rig.h.Refresh(c)
	pair := familyResponse(t, w)
	_, successor := assertFamilyAccess(t, rig, pair.Access)
	c, w = familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", secret)
	rig.h.Refresh(c)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var active int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE id=$1 AND revoked_at IS NULL`, successor).Scan(&active))
	assert.Equal(t, 1, active, "family membership alone cannot authorize source replay without a recorded signal")
}
