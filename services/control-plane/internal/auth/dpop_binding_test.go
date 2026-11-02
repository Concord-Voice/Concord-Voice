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
