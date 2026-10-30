package auth

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actorContext carries access's verified identity the way AuthRequired does,
// so the revoke paths can revalidate the requesting family.
func actorContext(t *testing.T, access string) context.Context {
	t.Helper()
	claims, err := ValidateAccessToken(access, familyTestSecret)
	require.NoError(t, err)
	return loginfamily.WithAccessIdentity(context.Background(), loginfamily.AccessIdentity{
		UserID: uuid.MustParse(claims.UserID), SID: uuid.MustParse(claims.SessionID),
		CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time,
	})
}

func terminalMint(t *testing.T, rig familyTestRig) (familyPair, string, string) {
	t.Helper()
	access, refresh, _, err := rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
	require.NoError(t, err)
	family, sid := assertFamilyAccess(t, rig, access)
	return familyPair{Access: access, Refresh: refresh}, family, sid
}

// familyFenceRecorder records exact post-commit family fences and whether the
// response had been written when each was reached.
type familyFenceRecorder struct {
	users    []uuid.UUID
	families []uuid.UUID
	written  []bool
	response *httptest.ResponseRecorder
}

func (r *familyFenceRecorder) DisconnectUser(user uuid.UUID) { r.users = append(r.users, user) }
func (r *familyFenceRecorder) DisconnectFamilyContext(_ context.Context, _, family uuid.UUID) error {
	r.families = append(r.families, family)
	r.written = append(r.written, r.response != nil && r.response.Body.Len() > 0)
	return nil
}

func terminalState(t *testing.T, rig familyTestRig, family string) (sql.NullTime, sql.NullString) {
	t.Helper()
	var at sql.NullTime
	var owner sql.NullString
	require.NoError(t, rig.db.QueryRow(`SELECT terminal_at,auth_operation_id FROM login_families WHERE family_id=$1`, family).Scan(&at, &owner))
	return at, owner
}

// Removing family preservation or restoring id != preservedSID makes the rotated predecessor terminal.
func TestLoginFamilyTerminalBulkPreservesWholeRotatedFamily(t *testing.T) {
	rig := newFamilyTestRig(t)
	pair, family, sid := terminalMint(t, rig)
	claims, err := ValidateAccessToken(pair.Access, familyTestSecret)
	require.NoError(t, err)
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", pair.Refresh)
	rig.h.Refresh(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	rotated := familyResponse(t, w)
	rotatedFamily, _ := assertFamilyAccess(t, rig, rotated.Access)
	require.Equal(t, family, rotatedFamily)
	_, other, _ := terminalMint(t, rig)
	result, err := RevokeAllRefreshTokens(actorContext(t, pair.Access), rig.db, rig.user, RevokeAllRefreshTokensOptions{ExcludeSessionID: sid, ExcludeAccessExpiresAt: claims.ExpiresAt.Time, ExpectedCredentialEpoch: claims.CredentialEpoch, EnforceCredentialEpoch: true})
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.NewlyTerminalFamilies)
	at, _ := terminalState(t, rig, family)
	assert.False(t, at.Valid)
	at, _ = terminalState(t, rig, other)
	assert.True(t, at.Valid)
}

func TestLoginFamilyTerminalBulkCountsExpiredAndKeepsOriginalOwner(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, family, sid := terminalMint(t, rig)
	_, err := rig.db.Exec(`UPDATE refresh_tokens SET expires_at=NOW()-INTERVAL '1 day' WHERE id=$1`, sid)
	require.NoError(t, err)
	result, err := RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, RevokeAllRefreshTokensOptions{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.RevokedRefreshRows)
	firstAt, firstOwner := terminalState(t, rig, family)
	require.True(t, firstAt.Valid)
	require.True(t, firstOwner.Valid)
	// A fresh request may capture old terminal families, but cannot move their age/owner.
	result, err = RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, RevokeAllRefreshTokensOptions{})
	require.NoError(t, err)
	assert.Zero(t, result.RevokedRefreshRows)
	assert.Zero(t, result.NewlyTerminalFamilies)
	at, owner := terminalState(t, rig, family)
	assert.Equal(t, firstAt, at)
	assert.Equal(t, firstOwner, owner)
}

func TestLoginFamilyTerminalUnknownPreserveHasNoEffects(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, family, _ := terminalMint(t, rig)
	_, err := RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, RevokeAllRefreshTokensOptions{ExcludeSessionID: uuid.NewString(), ExcludeAccessExpiresAt: time.Now().Add(time.Minute).Truncate(time.Second)})
	require.Error(t, err)
	at, _ := terminalState(t, rig, family)
	assert.False(t, at.Valid)
}

func TestLoginFamilyTerminalExplicitLogoutNeverFallsBack(t *testing.T) {
	for _, header := range []string{"X-Refresh-Token", "X-Session-ID"} {
		t.Run(header, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			pair, family, sid := terminalMint(t, rig)
			c, w := familyTestContext()
			c.Request.Header.Set("Authorization", "Bearer "+pair.Access)
			c.Request.AddCookie(&http.Cookie{Name: "refresh_token", Value: pair.Refresh, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			c.Request.Header.Set(header, "invalid-explicit-credential")
			rig.h.Logout(c)
			assert.Less(t, w.Code, 500, w.Body.String())
			at, _ := terminalState(t, rig, family)
			assert.False(t, at.Valid)
			var revoked sql.NullTime
			require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, sid).Scan(&revoked))
			assert.False(t, revoked.Valid)
			claims, err := ValidateAccessToken(pair.Access, familyTestSecret)
			require.NoError(t, err)
			exists, err := rig.h.redis.Exists(context.Background(), "blacklist:"+claims.ID).Result()
			require.NoError(t, err)
			assert.Zero(t, exists)
			assert.Empty(t, w.Header().Values("Set-Cookie"))
		})
	}
}

func TestLoginFamilyTerminalLogoutOtherFamilyLeavesBearerLive(t *testing.T) {
	rig := newFamilyTestRig(t)
	bearer, first, _ := terminalMint(t, rig)
	target, second, _ := terminalMint(t, rig)
	c, w := familyTestContext()
	fences := &familyFenceRecorder{response: w}
	rig.h.hub = fences
	c.Request.Header.Set("Authorization", "Bearer "+bearer.Access)
	c.Request.Header.Set("X-Refresh-Token", target.Refresh)
	rig.h.Logout(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []uuid.UUID{uuid.MustParse(second)}, fences.families, "logout fences exactly the logged-out family")
	assert.Equal(t, []bool{false}, fences.written, "the fence precedes the logout response")
	assert.Empty(t, fences.users, "logout never kills the bearer's sibling family")
	at, _ := terminalState(t, rig, first)
	assert.False(t, at.Valid)
	at, _ = terminalState(t, rig, second)
	assert.True(t, at.Valid)
	claims, err := ValidateAccessToken(bearer.Access, familyTestSecret)
	require.NoError(t, err)
	exists, err := rig.h.redis.Exists(context.Background(), "blacklist:"+claims.ID).Result()
	require.NoError(t, err)
	assert.Zero(t, exists)
}

func TestLoginFamilyTerminalTheftPrerequisiteFailureRollsBack(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, family, _ := terminalMint(t, rig)
	require.NoError(t, EnqueueTokenTheftRevocation(context.Background(), rig.db, rig.user))
	require.Error(t, ProcessTokenTheftRevocation(context.Background(), rig.db, nil, rig.user))
	at, _ := terminalState(t, rig, family)
	assert.False(t, at.Valid)
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL`, rig.user).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, ProcessTokenTheftRevocation(context.Background(), rig.db, rig.h.redis, rig.user))
	at, _ = terminalState(t, rig, family)
	assert.True(t, at.Valid)
}

func TestLoginFamilyTerminalLogoutCommitFailureNeverBlacklists(t *testing.T) {
	rig := newFamilyTestRig(t)
	pair, family, _ := terminalMint(t, rig)
	_, err := rig.db.Exec(`CREATE FUNCTION terminal_fixture_reject_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'terminal fixture rejects commit'; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := rig.db.Exec(`DROP FUNCTION terminal_fixture_reject_commit() CASCADE`)
		require.NoError(t, err)
	})
	_, err = rig.db.Exec(`CREATE CONSTRAINT TRIGGER terminal_fixture_reject_commit AFTER UPDATE ON login_families DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION terminal_fixture_reject_commit()`)
	require.NoError(t, err)
	c, w := familyTestContext()
	fences := &familyFenceRecorder{response: w}
	rig.h.hub = fences
	c.Request.Header.Set("Authorization", "Bearer "+pair.Access)
	c.Request.Header.Set("X-Refresh-Token", pair.Refresh)
	rig.h.Logout(c)
	assert.GreaterOrEqual(t, w.Code, 500, w.Body.String())
	assert.Empty(t, fences.families, "a failed logout commit fences nothing")
	assert.NotContains(t, w.Body.String(), "successfully")
	at, _ := terminalState(t, rig, family)
	assert.False(t, at.Valid)
	claims, err := ValidateAccessToken(pair.Access, familyTestSecret)
	require.NoError(t, err)
	exists, err := rig.h.redis.Exists(context.Background(), "blacklist:"+claims.ID).Result()
	require.NoError(t, err)
	assert.Zero(t, exists)
}

func TestLoginFamilyTerminalDisplacementKeepsExactMatchingPredicates(t *testing.T) {
	for _, mode := range []string{"machine", "ip-and-agent", "metadata-less-oauth"} {
		t.Run(mode, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			_, matched, matchSID := terminalMint(t, rig)
			_, unmatched, otherSID := terminalMint(t, rig)
			_, err := rig.db.Exec(`UPDATE refresh_tokens SET machine_id='terminal-match',ip_address='192.0.2.10',user_agent='logical-family-fixture' WHERE id=$1`, matchSID)
			require.NoError(t, err)
			_, err = rig.db.Exec(`UPDATE refresh_tokens SET machine_id='terminal-other',ip_address='192.0.2.11',user_agent='other-agent' WHERE id=$1`, otherSID)
			require.NoError(t, err)
			var access string
			if mode == "metadata-less-oauth" {
				access, _, _, err = rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
				require.NoError(t, err)
			} else {
				c, w := familyTestContext()
				if mode == "machine" {
					c.Request.Header.Set("X-Machine-Id", "terminal-match")
					c.Request.RemoteAddr = "192.0.2.99:1234"
				}
				require.True(t, rig.h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword))
				access = familyResponse(t, w).Access
			}
			fresh, _ := assertFamilyAccess(t, rig, access)
			assert.NotEqual(t, matched, fresh)
			assert.NotEqual(t, unmatched, fresh)
			at, _ := terminalState(t, rig, matched)
			assert.Equal(t, mode != "metadata-less-oauth", at.Valid)
			at, _ = terminalState(t, rig, unmatched)
			assert.False(t, at.Valid)
			at, _ = terminalState(t, rig, fresh)
			assert.False(t, at.Valid)
		})
	}
}

func TestLoginFamilyTerminalCapturedReplayCannotSweepLaterLogin(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, oldFamily, sid := terminalMint(t, rig)
	ctx := context.Background()
	tx, err := rig.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := tx.Rollback()
		if err != nil && err != sql.ErrTxDone {
			t.Error(err)
		}
	}()
	user, err := lockMintUserTx(ctx, tx, rig.user)
	require.NoError(t, err)
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, tx, user, uuid.MustParse(sid))
	require.NoError(t, err)
	result, err := loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.Len(t, result.Owners, 1)
	require.NoError(t, tx.Commit())
	before, owner := terminalState(t, rig, oldFamily)
	_, fresh, _ := terminalMint(t, rig)
	retry, err := rig.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := retry.Rollback()
		if err != nil && err != sql.ErrTxDone {
			t.Error(err)
		}
	}()
	_, err = loginfamily.RevokeFamiliesTx(ctx, retry, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonExplicitRevoke})
	require.ErrorIs(t, err, loginfamily.ErrContract)
	require.NoError(t, retry.Rollback())
	after, afterOwner := terminalState(t, rig, oldFamily)
	assert.Equal(t, before, after)
	assert.Equal(t, owner, afterOwner)
	at, _ := terminalState(t, rig, fresh)
	assert.False(t, at.Valid)
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_revocation_targets WHERE operation_id=$1 AND family_id=$2`, owner.String, fresh).Scan(&n))
	assert.Zero(t, n, "sealed captured operation excludes a later login")
}

func TestLoginFamilyTerminalTheftSQLFailureRetainsPendingAndLiveFamily(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, family, _ := terminalMint(t, rig)
	require.NoError(t, EnqueueTokenTheftRevocation(context.Background(), rig.db, rig.user))
	_, err := rig.db.Exec(`ALTER TABLE login_family_revocations ADD CONSTRAINT terminal_theft_fixture_refuse CHECK(false) NOT VALID`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := rig.db.Exec(`ALTER TABLE login_family_revocations DROP CONSTRAINT terminal_theft_fixture_refuse`)
		require.NoError(t, err)
	})
	require.Error(t, ProcessTokenTheftRevocation(context.Background(), rig.db, rig.h.redis, rig.user))
	at, _ := terminalState(t, rig, family)
	assert.False(t, at.Valid)
	var pending bool
	require.NoError(t, rig.db.QueryRow(`SELECT completed_at IS NULL FROM token_theft_pending_revocations WHERE user_id=$1`, rig.user).Scan(&pending))
	assert.True(t, pending)
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL`, rig.user).Scan(&n))
	assert.Equal(t, 1, n)
}

// The driver commits durably, then loses the acknowledgement. SQL assertions use
// the independent original pool so an HTTP success cannot hide an unknown outcome.
type terminalAmbiguousConnector struct{ driver.Connector }

func (c terminalAmbiguousConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return terminalAmbiguousConn{conn}, nil
}

type terminalAmbiguousConn struct{ driver.Conn }

func (c terminalAmbiguousConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	begin, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("fixture driver lacks BeginTx")
	}
	tx, err := begin.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return terminalAmbiguousTx{tx}, nil
}
func (c terminalAmbiguousConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	exec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return exec.ExecContext(ctx, q, args)
}
func (c terminalAmbiguousConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	query, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return query.QueryContext(ctx, q, args)
}

type terminalAmbiguousTx struct{ driver.Tx }

func (tx terminalAmbiguousTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return errors.New("fixture lost terminal commit acknowledgement")
}
func TestLoginFamilyTerminalUnknownCommitEmitsNoSuccess(t *testing.T) {
	rig := newFamilyTestRig(t)
	pair, family, _ := terminalMint(t, rig)
	connector, err := pq.NewConnector(dbtest.DatabaseURL())
	require.NoError(t, err)
	ambiguous := sql.OpenDB(terminalAmbiguousConnector{connector})
	defer func() { require.NoError(t, ambiguous.Close()) }()
	h := NewHandler(ambiguous, rig.h.redis, logger.NewWithWriter(io.Discard), familyTestSecret, nil)
	c, w := familyTestContext()
	c.Request.Header.Set("Authorization", "Bearer "+pair.Access)
	c.Request.Header.Set("X-Refresh-Token", pair.Refresh)
	h.Logout(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "successfully")
	at, _ := terminalState(t, rig, family)
	assert.True(t, at.Valid, "fixture must prove a real commit despite failed acknowledgement")
}

// Sequence advancement survives rollback, proving fresh INSERT was attempted
// even if all normal SQL rows are later rolled back.
func terminalDisplacementInsertProbe(t *testing.T, rig familyTestRig, oldFamily string) func() bool {
	t.Helper()
	_, err := rig.db.Exec(`CREATE SEQUENCE terminal_displacement_probe_seq`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := rig.db.Exec(`DROP FUNCTION terminal_displacement_probe_guard() CASCADE; DROP TABLE terminal_displacement_probe_targets; DROP SEQUENCE terminal_displacement_probe_seq`)
		require.NoError(t, err)
	})
	_, err = rig.db.Exec(`CREATE TABLE terminal_displacement_probe_targets(family_id UUID PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = rig.db.Exec(`INSERT INTO terminal_displacement_probe_targets VALUES($1)`, oldFamily)
	require.NoError(t, err)
	_, err = rig.db.Exec(`CREATE FUNCTION terminal_displacement_probe_guard() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
 PERFORM nextval('terminal_displacement_probe_seq');
 IF EXISTS (SELECT 1 FROM terminal_displacement_probe_targets p LEFT JOIN login_families f USING(family_id) WHERE f.terminal_at IS NULL) THEN
 RAISE EXCEPTION 'old matched family was live at fresh family insert';
 END IF;
 RETURN NEW;
 END $$`)
	require.NoError(t, err)
	_, err = rig.db.Exec(`CREATE TRIGGER terminal_displacement_probe BEFORE INSERT ON login_families FOR EACH ROW EXECUTE FUNCTION terminal_displacement_probe_guard()`)
	require.NoError(t, err)
	return func() bool {
		var called bool
		require.NoError(t, rig.db.QueryRow(`SELECT is_called FROM terminal_displacement_probe_seq`).Scan(&called))
		return called
	}
}

func TestLoginFamilyTerminalDisplacementUnmappedMatchFailsWithoutPartialRevocation(t *testing.T) {
	for _, mode := range []string{"machine", "ip-and-agent"} {
		t.Run(mode, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			_, family, sid := terminalMint(t, rig)
			_, err := rig.db.Exec(`UPDATE refresh_tokens SET machine_id='terminal-displacement',ip_address='192.0.2.10',user_agent='logical-family-fixture' WHERE id=$1`, sid)
			require.NoError(t, err)
			legacySID := uuid.NewString()
			// Deliberately unmapped historical row, never used as a successful mint fixture.
			_, err = rig.db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,machine_id,ip_address,user_agent,expires_at) VALUES($1,$2,$3,'terminal-displacement','192.0.2.10','logical-family-fixture',NOW()+INTERVAL '1 hour')`, legacySID, rig.user, HashRefreshToken("terminal-unmapped-match"))
			require.NoError(t, err)
			attempted := terminalDisplacementInsertProbe(t, rig, family)
			c, w := familyTestContext()
			if mode == "machine" {
				c.Request.Header.Set("X-Machine-Id", "terminal-displacement")
				c.Request.RemoteAddr = "192.0.2.99:1234"
			}
			require.False(t, rig.h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword))
			assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			assert.JSONEq(t, `{"error":"`+errMsgLoginFailed+`"}`, w.Body.String())
			assert.Empty(t, w.Header().Values("Set-Cookie"))
			assert.False(t, attempted(), "mixed capture must fail before fresh-family INSERT")
			at, owner := terminalState(t, rig, family)
			assert.False(t, at.Valid)
			assert.False(t, owner.Valid)
			var count int
			require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE id IN($1,$2) AND revoked_at IS NULL`, sid, legacySID).Scan(&count))
			assert.Equal(t, 2, count)
			require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`, rig.user).Scan(&count))
			assert.Zero(t, count)
			require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_families WHERE user_id=$1`, rig.user).Scan(&count))
			assert.Equal(t, 1, count)
		})
	}
}

func TestLoginFamilyTerminalDisplacementCaptureDependencyFailure503(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, family, sid := terminalMint(t, rig)
	_, err := rig.db.Exec(`UPDATE refresh_tokens SET machine_id='terminal-displacement' WHERE id=$1`, sid)
	require.NoError(t, err)
	attempted := terminalDisplacementInsertProbe(t, rig, family)
	_, err = rig.db.Exec(`ALTER TABLE login_family_sids RENAME TO terminal_displacement_hidden_sids`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := rig.db.Exec(`ALTER TABLE terminal_displacement_hidden_sids RENAME TO login_family_sids`)
		require.NoError(t, err)
	})
	c, w := familyTestContext()
	c.Request.Header.Set("X-Machine-Id", "terminal-displacement")
	require.False(t, rig.h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Empty(t, w.Header().Values("Set-Cookie"))
	assert.NotContains(t, w.Body.String(), "access_token")
	assert.NotContains(t, w.Body.String(), "refresh_token")
	assert.False(t, attempted(), "capture dependency must fail before fresh-family INSERT")
	at, owner := terminalState(t, rig, family)
	assert.False(t, at.Valid)
	assert.False(t, owner.Valid)
	var revoked sql.NullTime
	require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, sid).Scan(&revoked))
	assert.False(t, revoked.Valid)
}

func TestLoginFamilyTerminalDisplacementTerminalBeforeFreshFamilyInsert(t *testing.T) {
	for _, mode := range []string{"machine", "ip-and-agent"} {
		t.Run(mode, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			_, family, sid := terminalMint(t, rig)
			_, err := rig.db.Exec(`UPDATE refresh_tokens SET machine_id='terminal-displacement',ip_address='192.0.2.10',user_agent='logical-family-fixture' WHERE id=$1`, sid)
			require.NoError(t, err)
			_, secondFamily, secondSID := terminalMint(t, rig)
			_, err = rig.db.Exec(`UPDATE refresh_tokens SET machine_id='terminal-displacement',ip_address='192.0.2.10',user_agent='logical-family-fixture' WHERE id=$1`, secondSID)
			require.NoError(t, err)
			attempted := terminalDisplacementInsertProbe(t, rig, family)
			_, err = rig.db.Exec(`INSERT INTO terminal_displacement_probe_targets VALUES($1)`, secondFamily)
			require.NoError(t, err)
			c, w := familyTestContext()
			if mode == "machine" {
				c.Request.Header.Set("X-Machine-Id", "terminal-displacement")
				c.Request.RemoteAddr = "192.0.2.99:1234"
			}
			require.True(t, rig.h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword))
			pair := familyResponse(t, w)
			assert.True(t, attempted(), "actual BEFORE INSERT guard must run")
			fresh, _ := assertFamilyAccess(t, rig, pair.Access)
			assert.NotEqual(t, family, fresh)
			at, _ := terminalState(t, rig, family)
			assert.True(t, at.Valid)
			secondAt, _ := terminalState(t, rig, secondFamily)
			assert.True(t, secondAt.Valid)
		})
	}
}
