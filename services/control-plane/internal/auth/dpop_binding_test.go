package auth

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dpopRefreshHTU = dpopTestOrigin + "/api/v1/auth/refresh"

type dpopRefreshResult struct {
	Refresh   string `json:"refresh_token"`
	TokenType string `json:"token_type"`
	Code      string `json:"code"`
	Server    int64  `json:"server_time"`
}

func dpopRefresh(t *testing.T, rig familyTestRig, refresh, proof string) (*httptest.ResponseRecorder, dpopRefreshResult) {
	t.Helper()
	c, w := familyTestContext()
	c.Request.Header.Set("X-Refresh-Token", refresh)
	if proof != "" {
		c.Request.Header.Set(dpopHeader, proof)
	}
	rig.h.Refresh(c)
	var out dpopRefreshResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return w, out
}

func rowJKT(t *testing.T, rig familyTestRig, refresh string) (jkt sql.NullString, revoked bool) {
	t.Helper()
	require.NoError(t, rig.db.QueryRow(
		`SELECT pop_jkt, revoked_at IS NOT NULL FROM refresh_tokens WHERE token_hash = $1`,
		HashRefreshToken(refresh)).Scan(&jkt, &revoked))
	return jkt, revoked
}

// bindSession mints an unbound session and binds it on its first refresh.
func bindSession(t *testing.T, rig familyTestRig, key dpopTestKey) (unbound, bound string) {
	t.Helper()
	mint, _ := familyTestContext()
	_, unbound, _, err := rig.h.IssueTokenPair(mint, rig.user, "")
	require.NoError(t, err)
	w, out := dpopRefresh(t, rig, unbound, key.proof(t, "POST", dpopRefreshHTU, &unbound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, tokenTypeDPoP, out.TokenType)
	jkt, _ := rowJKT(t, rig, out.Refresh)
	require.Equal(t, key.jkt, jkt.String)
	return unbound, out.Refresh
}

func TestSessionBindingBindsAndInherits(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	_, bound := bindSession(t, rig, key)

	w, out := dpopRefresh(t, rig, bound, key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
	jkt, _ := rowJKT(t, rig, out.Refresh)
	assert.Equal(t, key.jkt, jkt.String, "a bound row's successor inherits its key")
	var alg string
	require.NoError(t, rig.db.QueryRow(`SELECT pop_alg FROM refresh_tokens WHERE token_hash = $1`, HashRefreshToken(out.Refresh)).Scan(&alg))
	assert.Equal(t, dpopAlgES256, alg)
}

func TestSessionBindingRefusalsLeaveSessionLive(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	_, bound := bindSession(t, rig, key)
	other := newDPoPTestKey(t)

	cases := map[string]struct {
		proof func() string
		code  string
	}{
		"no proof":    {func() string { return "" }, errCodePoPRequired},
		"other key":   {func() string { return other.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()) }, errCodePoPInvalid},
		"wrong token": {func() string { s := "x"; return key.proof(t, "POST", dpopRefreshHTU, &s, time.Now()) }, errCodePoPInvalid},
		"stale iat":   {func() string { return key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now().Add(-2*time.Minute)) }, errCodePoPStaleIAT},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Even with binding switched off, a bound row stays enforced.
			rig.h.SetSessionBinding(name != "no proof")
			w, out := dpopRefresh(t, rig, bound, tc.proof())
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			assert.Equal(t, tc.code, out.Code)
			if tc.code == errCodePoPStaleIAT {
				assert.InDelta(t, time.Now().Unix(), out.Server, 5)
			} else {
				assert.Zero(t, out.Server)
			}
			_, revoked := rowJKT(t, rig, bound)
			assert.False(t, revoked, "a refused proof must leave the session live")
			assert.NotContains(t, w.Body.String(), key.jkt)
		})
	}
	var live int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, rig.user).Scan(&live))
	assert.Equal(t, 1, live, "no refusal revoked or minted anything")
}

func TestSessionBindingReplayedProofAndWindowFreeRecovery(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	_, bound := bindSession(t, rig, key)

	proof := key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now())
	w, lost := dpopRefresh(t, rig, bound, proof)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The same proof again reaches the grace path and is refused as replayed.
	w, out := dpopRefresh(t, rig, bound, proof)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPReplayed, out.Code)

	// Long after the 30 s window, a fresh proof by the bound key recovers.
	_, err := rig.db.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() - INTERVAL '1 hour' WHERE token_hash = $1`, HashRefreshToken(bound))
	require.NoError(t, err)
	w, out = dpopRefresh(t, rig, bound, "")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPRequired, out.Code)
	w, out = dpopRefresh(t, rig, bound, key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
	_, lostRevoked := rowJKT(t, rig, lost.Refresh)
	assert.True(t, lostRevoked, "recovery consumes the undelivered successor")
	jkt, _ := rowJKT(t, rig, out.Refresh)
	assert.Equal(t, key.jkt, jkt.String)
}

func TestSessionBindingUnboundReplayCannotTakeBoundSuccessor(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	unbound, _ := bindSession(t, rig, key)

	// Inside the grace window, same IP and user-agent: the old unbound token
	// would recover, but its successor is bound, so only the key can.
	w, out := dpopRefresh(t, rig, unbound, "")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPRequired, out.Code)
	w, out = dpopRefresh(t, rig, unbound, key.proof(t, "POST", dpopRefreshHTU, &unbound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
}

func TestSessionBindingDormantIgnoresProofs(t *testing.T) {
	rig := newFamilyTestRig(t)
	key := newDPoPTestKey(t)
	mint, _ := familyTestContext()
	_, refresh, _, err := rig.h.IssueTokenPair(mint, rig.user, "")
	require.NoError(t, err)
	w, out := dpopRefresh(t, rig, refresh, key.proof(t, "POST", dpopRefreshHTU, &refresh, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeBearer, out.TokenType)
	jkt, _ := rowJKT(t, rig, out.Refresh)
	assert.False(t, jkt.Valid, "no new session is bound while binding is off")

	w, out = dpopRefresh(t, rig, out.Refresh, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeBearer, out.TokenType, "unbound sessions keep working")
}

func TestSessionBindingAtLogin(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	var epoch string
	require.NoError(t, rig.db.QueryRow(`SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`, rig.user).Scan(&epoch))

	login := func(proof string) (*httptest.ResponseRecorder, dpopRefreshResult) {
		c, w := familyTestContext()
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		c.Request.Header.Set(dpopHeader, proof)
		rig.h.CompleteLogin(c, rig.user, true, epoch, securityevent.AuthPassword)
		var out dpopRefreshResult
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
		return w, out
	}
	w, out := login(key.proof(t, "POST", dpopTestOrigin+"/api/v1/auth/login", nil, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
	jkt, _ := rowJKT(t, rig, out.Refresh)
	assert.Equal(t, key.jkt, jkt.String)

	refresh := "x"
	w, out = login(key.proof(t, "POST", dpopTestOrigin+"/api/v1/auth/login", &refresh, time.Now()))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPInvalid, out.Code, "a login proof must not carry rth")
}

// liveTokens counts the user's unrevoked refresh tokens.
func liveTokens(t *testing.T, rig familyTestRig) int {
	t.Helper()
	var live int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, rig.user).Scan(&live))
	return live
}

func TestSessionBindingUnboundReplayWrongKeyLeavesBoundSuccessor(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	unbound, bound := bindSession(t, rig, key)
	other := newDPoPTestKey(t)

	// A valid proof, by a key that is not the successor's.
	w, out := dpopRefresh(t, rig, unbound, other.proof(t, "POST", dpopRefreshHTU, &unbound, time.Now()))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPRequired, out.Code)
	jkt, revoked := rowJKT(t, rig, bound)
	assert.False(t, revoked, "the bound successor stays live")
	assert.Equal(t, key.jkt, jkt.String, "and keeps its key")
	assert.Equal(t, 1, liveTokens(t, rig), "nothing was minted")

	// Positive control: the successor's own key recovers it.
	w, out = dpopRefresh(t, rig, unbound, key.proof(t, "POST", dpopRefreshHTU, &unbound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
}

func TestSessionBindingProofRefusalPrecedesTheftCheck(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	_, bound := bindSession(t, rig, key)
	other := newDPoPTestKey(t)
	_, err := rig.db.Exec(`UPDATE refresh_tokens SET machine_id = 'stored-machine' WHERE token_hash = $1`, HashRefreshToken(bound))
	require.NoError(t, err)

	// A different machine ID and a different IP would be theft, but the proof
	// is refused first, so neither revocation nor a theft event follows.
	theftRefresh := func(proof string) *httptest.ResponseRecorder {
		c, w := familyTestContext()
		c.Request.RemoteAddr = "198.51.100.7:1234"
		c.Request.Header.Set("X-Refresh-Token", bound)
		c.Request.Header.Set(headerMachineID, "other-machine")
		if proof != "" {
			c.Request.Header.Set(dpopHeader, proof)
		}
		rig.h.Refresh(c)
		return w
	}
	for name, tc := range map[string]struct {
		proof func() string
		code  string
	}{
		"no proof":  {func() string { return "" }, errCodePoPRequired},
		"wrong key": {func() string { return other.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()) }, errCodePoPInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			w := theftRefresh(tc.proof())
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			var out dpopRefreshResult
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
			assert.Equal(t, tc.code, out.Code, "a proof refusal, not a theft refusal")
			_, revoked := rowJKT(t, rig, bound)
			assert.False(t, revoked)
			assert.Equal(t, 1, liveTokens(t, rig))
		})
	}

	// Positive control: with a valid proof the same request reaches the
	// theft check, which revokes the session.
	w := theftRefresh(key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	_, revoked := rowJKT(t, rig, bound)
	assert.True(t, revoked, "the control must reach theft revocation")
}

func TestSessionBindingGraceRecoversBoundSuccessorWhileDormant(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	unbound, bound := bindSession(t, rig, key)

	// Binding is switched off after the bound successor's response was lost.
	rig.h.SetSessionBinding(false)
	w, out := dpopRefresh(t, rig, unbound, "")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPRequired, out.Code, "a bound successor still needs its key")
	w, out = dpopRefresh(t, rig, unbound, key.proof(t, "POST", dpopRefreshHTU, &unbound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
	_, revoked := rowJKT(t, rig, bound)
	assert.True(t, revoked, "recovery consumed the undelivered successor")
	jkt, _ := rowJKT(t, rig, out.Refresh)
	assert.Equal(t, key.jkt, jkt.String, "the recovered session keeps the key")
}

func TestSessionBindingDormantGraceNeverBindsNewSession(t *testing.T) {
	rig := newFamilyTestRig(t)
	key := newDPoPTestKey(t)
	mint, _ := familyTestContext()
	_, first, _, err := rig.h.IssueTokenPair(mint, rig.user, "")
	require.NoError(t, err)
	w, _ := dpopRefresh(t, rig, first, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// A valid proof on a dormant replay of an unbound lineage binds nothing.
	w, out := dpopRefresh(t, rig, first, key.proof(t, "POST", dpopRefreshHTU, &first, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeBearer, out.TokenType)
	jkt, _ := rowJKT(t, rig, out.Refresh)
	assert.False(t, jkt.Valid)

	// An invalid proof on a dormant replay of an unbound lineage is ignored,
	// as on the dormant refresh path: the replay still recovers, unbound.
	_, second, _, err := rig.h.IssueTokenPair(mint, rig.user, "")
	require.NoError(t, err)
	w, _ = dpopRefresh(t, rig, second, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	s := "wrong"
	w, out = dpopRefresh(t, rig, second, key.proof(t, "POST", dpopRefreshHTU, &s, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeBearer, out.TokenType)
	jkt, _ = rowJKT(t, rig, out.Refresh)
	assert.False(t, jkt.Valid)
}

func TestSessionBindingDormantGraceReturnsBoundSuccessorProofError(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	unbound, bound := bindSession(t, rig, key)
	rig.h.SetSessionBinding(false)

	// The right key with a skewed clock gets the stale-iat answer and the
	// server's time, not pop_required, so the client can retry once.
	w, out := dpopRefresh(t, rig, unbound, key.proof(t, "POST", dpopRefreshHTU, &unbound, time.Now().Add(-time.Hour)))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPStaleIAT, out.Code)
	assert.NotZero(t, out.Server)
	_, revoked := rowJKT(t, rig, bound)
	assert.False(t, revoked, "the bound successor stays live")
}

func TestSessionBindingJunkProofKeepsReplayDetection(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	mint, _ := familyTestContext()
	_, first, _, err := rig.h.IssueTokenPair(mint, rig.user, "")
	require.NoError(t, err)
	w, _ := dpopRefresh(t, rig, first, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Inside the window a bad proof on an unbound lineage is still refused.
	w, out := dpopRefresh(t, rig, first, "junk.junk.junk")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, errCodePoPInvalid, out.Code)
	assert.Equal(t, 1, liveTokens(t, rig), "nothing was minted")

	// Outside it, a junk proof must not hide the stale replay from detection.
	_, err = rig.db.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() - INTERVAL '5 minutes' WHERE token_hash = $1`, HashRefreshToken(first))
	require.NoError(t, err)
	rec := &securityEventRecorder{}
	rig.h.SetSecurityEvents(rec)
	w, out = dpopRefresh(t, rig, first, "junk.junk.junk")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Empty(t, out.Code, "the replay refusal, not a proof refusal")
	replays := 0
	for _, e := range rec.events {
		if e.ReasonCode == securityevent.ReasonRefreshReplay {
			replays++
		}
	}
	assert.Equal(t, 1, replays)
}

func TestSessionBindingGraceRecoveryOwesPreMFAUpgrade(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	_, bound := bindSession(t, rig, key)
	w, lost := dpopRefresh(t, rig, bound, key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var lostID string
	require.NoError(t, rig.db.QueryRow(`SELECT id FROM refresh_tokens WHERE token_hash = $1`, HashRefreshToken(lost.Refresh)).Scan(&lostID))

	// MFA was enabled after the undelivered successor was created.
	_, err := rig.db.Exec(`UPDATE refresh_tokens SET created_at = NOW() - INTERVAL '1 hour' WHERE user_id = $1`, rig.user)
	require.NoError(t, err)
	_, err = rig.db.Exec(`UPDATE users SET mfa_enabled_at = NOW() WHERE id = $1`, rig.user)
	require.NoError(t, err)
	mfa := &mfaMethodsStub{loginMethods: []string{"totp"}, enabledMethods: []string{"totp"}, challengeTok: "upgrade-token"}
	rig.h.SetMFAChecker(mfa)

	w, _ = dpopRefresh(t, rig, bound, key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "mfa_upgrade_required")
	assert.Equal(t, lostID, mfa.upgradeSession, "the challenge is bound to the successor recovery would replace")
	_, revoked := rowJKT(t, rig, lost.Refresh)
	assert.False(t, revoked, "the successor stays live")
	assert.Equal(t, 1, liveTokens(t, rig), "nothing was minted")

	// A completed upgrade for that successor lets recovery through.
	require.NoError(t, rig.h.redis.Set(t.Context(), MFAUpgradeBypassKey(rig.user, lostID), "1", time.Minute).Err())
	w, out := dpopRefresh(t, rig, bound, key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
}

func TestSessionBindingGraceRecoveryAfterMFAOwesNothing(t *testing.T) {
	rig := newFamilyTestRig(t)
	rig.h.SetSessionBinding(true)
	key := newDPoPTestKey(t)
	_, bound := bindSession(t, rig, key)
	w, _ := dpopRefresh(t, rig, bound, key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// MFA was enabled before the successor was created: no upgrade is owed.
	_, err := rig.db.Exec(`UPDATE users SET mfa_enabled_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, rig.user)
	require.NoError(t, err)
	mfa := &mfaMethodsStub{loginMethods: []string{"totp"}, enabledMethods: []string{"totp"}, challengeTok: "upgrade-token"}
	rig.h.SetMFAChecker(mfa)

	w, out := dpopRefresh(t, rig, bound, key.proof(t, "POST", dpopRefreshHTU, &bound, time.Now()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tokenTypeDPoP, out.TokenType)
	assert.Empty(t, mfa.upgradeSession, "no challenge was issued")
}

func TestUnboundGraceRecoveryOfPreMFASessionEndsQuietly(t *testing.T) {
	rig := newFamilyTestRig(t)
	mint, _ := familyTestContext()
	_, first, _, err := rig.h.IssueTokenPair(mint, rig.user, "")
	require.NoError(t, err)
	w, lost := dpopRefresh(t, rig, first, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	_, err = rig.db.Exec(`UPDATE refresh_tokens SET created_at = NOW() - INTERVAL '1 hour' WHERE user_id = $1`, rig.user)
	require.NoError(t, err)
	_, err = rig.db.Exec(`UPDATE users SET mfa_enabled_at = NOW() WHERE id = $1`, rig.user)
	require.NoError(t, err)
	mfa := &mfaMethodsStub{loginMethods: []string{"totp"}, enabledMethods: []string{"totp"}, challengeTok: "upgrade-token"}
	rig.h.SetMFAChecker(mfa)
	rec := &securityEventRecorder{}
	rig.h.SetSecurityEvents(rec)

	// The 30 s window would close before an MFA ceremony finished, so the
	// session ends with a plain 401: no challenge and no replay alert.
	w, _ = dpopRefresh(t, rig, first, "")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "mfa_upgrade_required")
	assert.Empty(t, mfa.upgradeSession)
	for _, e := range rec.events {
		assert.NotEqual(t, securityevent.ReasonRefreshReplay, e.ReasonCode)
	}
	_, revoked := rowJKT(t, rig, lost.Refresh)
	assert.False(t, revoked, "the successor stays live")
	assert.Equal(t, 1, liveTokens(t, rig), "nothing was minted")
}
