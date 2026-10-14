package channels_test

// #3459 §7 (D-B, AC7): turning "Include pinned messages" ON over a policy
// whose effective value was false makes kept pins deletable, so on a server
// that enforces MFA on dangerous actions it asks for the same factor a
// shortening does, under purpose channels.expiration_shorten. Turning it off,
// omitting it, or re-affirming true stays ungated.

import (
	"database/sql"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// chanPinnedPolicy is the channel's raw include column, its effective value E
// and its revision.
type chanPinnedPolicy struct {
	raw, effective bool
	revision       int64
}

func readChanPinnedPolicy(t *testing.T, db *sql.DB, channelID string) chanPinnedPolicy {
	t.Helper()
	var p chanPinnedPolicy
	require.NoError(t, db.QueryRow(`SELECT expiration_include_pinned,
	        expiration_window_seconds IS NOT NULL AND expiration_include_pinned, expiration_revision
	   FROM channels WHERE id = $1`, channelID).Scan(&p.raw, &p.effective, &p.revision))
	return p
}

// pinnedSetBody is a same-window new_only set: never a shortening, so only the
// include_pinned predicate can fire the gate. include nil omits the field.
func pinnedSetBody(window int, include *bool, code string) map[string]any {
	body := map[string]any{"mode": "set", "window_seconds": window, "retroactive": "new_only"}
	if include != nil {
		body["include_pinned"] = *include
	}
	return withChanGateCode(body, code)
}

func boolRef(v bool) *bool { return &v }

// AC7: false -> true on an enforcing server asks for MFA and writes nothing;
// the confirmed retry turns it on.
//
// Mutant killed: dropping enablesPinnedExpiry from expirationGateFires (the
// no-code request is admitted and E becomes true).
func TestChannelExpirationGate_EnablingPinnedExpiryIsGated(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	path := pathChannelsPrefix + f.channelID + "/expiration"
	auth := testhelpers.AuthHeaders(f.mod.AccessToken)

	w := ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, nil, ""), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	before := readChanPinnedPolicy(t, ts.DB, f.channelID)
	require.False(t, before.effective, "a new timer with the field omitted keeps pins")

	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)

	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, boolRef(true), ""), auth)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String())
	assert.Equal(t, before, readChanPinnedPolicy(t, ts.DB, f.channelID), "a refusal writes nothing")
	assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID), "no code, no charge")

	token := mintChanGateToken(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten)
	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, boolRef(true), token), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	after := readChanPinnedPolicy(t, ts.DB, f.channelID)
	assert.True(t, after.effective)
	assert.Equal(t, before.revision+1, after.revision)
	assert.False(t, chanGateTokenLive(t, ts.DB, f.mod.ID, stepup.PurposeChannelExpirationShorten), "the confirmation spent the token")
}

// AC7: true -> false and an omitted field never fire; re-affirming true over
// an effective true does not either. None charges the budget or spends a token.
func TestChannelExpirationGate_DisablingOrOmittingPinnedStaysUngated(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	path := pathChannelsPrefix + f.channelID + "/expiration"
	auth := testhelpers.AuthHeaders(f.mod.AccessToken)

	w := ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, boolRef(true), ""), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, readChanPinnedPolicy(t, ts.DB, f.channelID).effective)

	testhelpers.SetServerMFAEnforcement(t, ts.DB, f.serverID, true)
	enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)

	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, boolRef(true), ""), auth)
	require.Equal(t, http.StatusOK, w.Code, "true over an effective true: %s", w.Body.String())

	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(604800, nil, ""), auth)
	require.Equal(t, http.StatusOK, w.Code, "omitted, longer window: %s", w.Body.String())
	assert.True(t, readChanPinnedPolicy(t, ts.DB, f.channelID).effective, "an omitted field keeps the value")

	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(604800, boolRef(false), ""), auth)
	require.Equal(t, http.StatusOK, w.Code, "turning it off: %s", w.Body.String())
	assert.False(t, readChanPinnedPolicy(t, ts.DB, f.channelID).effective)

	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(604800, nil, ""), auth)
	require.Equal(t, http.StatusOK, w.Code, "omitted over an effective false: %s", w.Body.String())
	assert.False(t, readChanPinnedPolicy(t, ts.DB, f.channelID).effective)
	assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID), "no ungated request charged")

	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(604800, boolRef(true), ""), auth)
	require.Equal(t, http.StatusForbidden, w.Code, "and back on is gated again: %s", w.Body.String())
	assert.JSONEq(t, chanGateBodyMFARequired, w.Body.String())
}

// AC7, non-enforcing server: a request with no code that turns pinned expiry
// on is answered exactly as any other ungated set — 200, no Redis charge, and
// the response is the policy with no gate field in it.
func TestChannelExpirationGate_PinnedOnNonEnforcingServerIsUnchanged(t *testing.T) {
	ts := setupTS(t)
	f := newChanGateFixture(t, ts)
	path := pathChannelsPrefix + f.channelID + "/expiration"
	auth := testhelpers.AuthHeaders(f.mod.AccessToken)

	w := ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, nil, ""), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	enrollChanGateWebAuthn(t, ts.DB, f.mod.ID)

	w = ts.DoRequest(http.MethodPatch, path, pinnedSetBody(86400, boolRef(true), ""), auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var policy map[string]any
	testhelpers.ParseJSON(t, w, &policy)
	assert.ElementsMatch(t, []string{"window_seconds", "updated_at", "revision", "backfill_pending", "include_pinned"},
		slices.Collect(maps.Keys(policy)))
	assert.Equal(t, true, policy["include_pinned"])
	assert.Equal(t, "absent", chanGateBudget(t, ts, f.mod.ID))

	// The list rows carry E too (§6.3): this channel is the fixture's only
	// one with pinned expiry on, so its true can only come from this row.
	for _, read := range []string{pathChannelsPrefix + f.channelID, "/api/v1/servers/" + f.serverID + "/channels"} {
		w = ts.DoRequest(http.MethodGet, read, nil, auth)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"expiration_include_pinned":true`, read)
	}
}
