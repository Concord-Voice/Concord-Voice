package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"net/http"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Treating Header.Get's empty value as absence wrongly spends ambient authority.
// Positive controls prove those same credentials still select the intended family
// when the explicit headers really are absent, and preserve selection precedence.
func TestLogoutTargetHeaderPresence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		refreshPresent  bool
		refreshValid    bool
		sidPresent      bool
		sidValid        bool
		cookie          bool
		bearer          bool
		wantFamily      string
		wantCookieClear bool
		wantBlacklist   bool
	}{
		{name: "empty refresh blocks cookie", refreshPresent: true, cookie: true},
		{name: "empty refresh blocks bearer", refreshPresent: true, bearer: true},
		{name: "empty refresh blocks both ambient credentials", refreshPresent: true, cookie: true, bearer: true},
		{name: "empty refresh precedes valid explicit SID", refreshPresent: true, sidPresent: true, sidValid: true, cookie: true, bearer: true},
		{name: "empty SID blocks cookie", sidPresent: true, cookie: true},
		{name: "empty SID blocks bearer", sidPresent: true, bearer: true},
		{name: "empty SID blocks both ambient credentials", sidPresent: true, cookie: true, bearer: true},
		{name: "absent headers allow cookie before bearer", cookie: true, bearer: true, wantFamily: "cookie", wantCookieClear: true},
		{name: "absent headers allow bearer alone", bearer: true, wantFamily: "bearer", wantBlacklist: true},
		{name: "valid refresh precedes SID and ambient credentials", refreshPresent: true, refreshValid: true, sidPresent: true, sidValid: true, cookie: true, bearer: true, wantFamily: "cookie"},
		{name: "valid SID precedes ambient cookie", sidPresent: true, sidValid: true, cookie: true, bearer: true, wantFamily: "bearer", wantBlacklist: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			cookiePair, cookieFamily, cookieSID := terminalMint(t, rig)
			bearerPair, bearerFamily, bearerSID := terminalMint(t, rig)
			foreignRig := rig
			foreignRig.user = dbtest.CreateUser(t, rig.db).String()
			foreignPair, foreignFamily, foreignSID := terminalMint(t, foreignRig)
			families := []struct {
				name, family, sid, user string
				pair                    familyPair
			}{
				{"cookie", cookieFamily, cookieSID, rig.user, cookiePair},
				{"bearer", bearerFamily, bearerSID, rig.user, bearerPair},
				{"foreign", foreignFamily, foreignSID, foreignRig.user, foreignPair},
			}
			before := make(map[string][32]byte, len(families))
			for _, family := range families {
				before[family.name] = logoutPresenceSnapshot(t, rig, family.family)
				at, owner := terminalState(t, rig, family.family)
				require.False(t, at.Valid)
				require.False(t, owner.Valid)
			}
			logoutPresenceAssertOperationCounts(t, rig, foreignRig.user, 0)

			c, w := familyTestContext()
			c.Request.URL.Path = "/api/v1/auth/logout"
			c.Request.Header.Set("X-Forwarded-Proto", "https")
			if tc.cookie {
				c.Request.AddCookie(&http.Cookie{Name: "refresh_token", Value: cookiePair.Refresh, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			}
			if tc.bearer {
				c.Request.Header.Set("Authorization", "Bearer "+bearerPair.Access)
			}
			if tc.refreshPresent {
				value := ""
				if tc.refreshValid {
					value = cookiePair.Refresh
				}
				c.Request.Header.Set("X-Refresh-Token", value)
			}
			if tc.sidPresent {
				value := ""
				if tc.sidValid {
					value = bearerSID
				}
				c.Request.Header.Set("X-Session-ID", value)
			}
			// Pin actual map presence: Header.Get alone collapses these cases.
			_, hasRefresh := c.Request.Header[http.CanonicalHeaderKey("X-Refresh-Token")]
			_, hasSID := c.Request.Header[http.CanonicalHeaderKey("X-Session-ID")]
			require.Equal(t, tc.refreshPresent, hasRefresh)
			require.Equal(t, tc.sidPresent, hasSID)
			rig.h.Logout(c)

			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			wantOperations := 0
			if tc.wantFamily == "" {
				assert.JSONEq(t, `{"message":"Already logged out"}`, w.Body.String())
			} else {
				wantOperations = 1
				assert.JSONEq(t, `{"message":"Logged out successfully"}`, w.Body.String())
			}
			logoutPresenceAssertOperationCounts(t, rig, foreignRig.user, wantOperations)
			for _, family := range families {
				at, owner := terminalState(t, rig, family.family)
				var revoked sql.NullTime
				require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, family.sid).Scan(&revoked))
				selected := family.name == tc.wantFamily
				assert.Equal(t, selected, at.Valid, family.name+" terminal state")
				assert.Equal(t, selected, owner.Valid, family.name+" durable owner")
				assert.Equal(t, selected, revoked.Valid, family.name+" refresh revocation")
				if !selected {
					assert.Equal(t, before[family.name], logoutPresenceSnapshot(t, rig, family.family), family.name+" rows changed")
				} else if owner.Valid {
					var user, scope, reason, targetFamily, targetUser string
					var sealed, pending bool
					require.NoError(t, rig.db.QueryRow(`SELECT o.user_id,o.scope,o.reason,o.sealed_at IS NOT NULL,o.completed_at IS NULL,t.family_id,t.user_id
FROM login_family_revocations o JOIN login_family_revocation_targets t USING(operation_id)
WHERE o.operation_id=$1`, owner.String).Scan(&user, &scope, &reason, &sealed, &pending, &targetFamily, &targetUser))
					assert.Equal(t, family.user, user)
					assert.Equal(t, "session", scope)
					assert.Equal(t, "logout", reason)
					assert.True(t, sealed)
					assert.True(t, pending, "HTTP success records delivery obligation, not remote completion")
					assert.Equal(t, family.family, targetFamily)
					assert.Equal(t, family.user, targetUser)
				}
				claims, err := ValidateAccessToken(family.pair.Access, familyTestSecret)
				require.NoError(t, err)
				exists, err := rig.h.redis.Exists(context.Background(), "blacklist:"+claims.ID).Result()
				require.NoError(t, err)
				wantBlacklist := int64(0)
				if family.name == "bearer" && tc.wantBlacklist {
					wantBlacklist = 1
				}
				assert.Equal(t, wantBlacklist, exists, family.name+" JTI blacklist")
			}
			if !tc.wantCookieClear {
				assert.Empty(t, w.Header().Values("Set-Cookie"))
			} else {
				cookies := w.Result().Cookies()
				require.Len(t, cookies, 1)
				assert.Equal(t, "refresh_token", cookies[0].Name)
				assert.Empty(t, cookies[0].Value)
				assert.Less(t, cookies[0].MaxAge, 0)
				assert.Equal(t, "/", cookies[0].Path)
				assert.True(t, cookies[0].Secure)
				assert.True(t, cookies[0].HttpOnly)
				assert.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
			}
		})
	}
}

// Hash complete rows so failed equality checks never print credential-bearing data.
func logoutPresenceSnapshot(t *testing.T, rig familyTestRig, family string) [32]byte {
	t.Helper()
	var snapshot string
	require.NoError(t, rig.db.QueryRow(`SELECT jsonb_build_object(
'family',to_jsonb(f),
'sids',(SELECT jsonb_agg(to_jsonb(s) ORDER BY s.sid) FROM login_family_sids s WHERE s.family_id=f.family_id),
'refresh',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM refresh_tokens r JOIN login_family_sids s ON s.sid=r.id WHERE s.family_id=f.family_id))::text
FROM login_families f WHERE f.family_id=$1`, family).Scan(&snapshot))
	return sha256.Sum256([]byte(snapshot))
}

func logoutPresenceAssertOperationCounts(t *testing.T, rig familyTestRig, foreignUser string, want int) {
	t.Helper()
	var operations, targets int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id IN ($1,$2)`, rig.user, foreignUser).Scan(&operations))
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_revocation_targets WHERE user_id IN ($1,$2)`, rig.user, foreignUser).Scan(&targets))
	assert.Equal(t, want, operations, "durable operation count")
	assert.Equal(t, want, targets, "durable target count")
}
