package friends_test

// End-to-end coverage for the #1240 privacy gate: the eligibility endpoint
// (spec T7) and the SendRequest enforcement merge (spec T5, T6, T13).
//
// External package for the same reason as presence_capture_db_test.go:
// internal/testhelpers builds the whole router and therefore imports
// internal/friends, so an in-package test importing it would cycle. The gate
// helper's own truth table lives in privacy_internal_test.go, which CAN see it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const (
	eligibilityURLSuffix = "/friend-request-eligibility"
	usersURLPrefix       = "/api/v1/users/"

	wireModeEveryone      = "everyone"
	wireModeMutualServers = "mutual_servers"
	wireModeNobody        = "nobody"
)

// eligibilityPath builds the probe URL. Not a fmt.Sprintf so a malformed target
// (the 400 case) survives unmangled.
func eligibilityPath(targetID string) string {
	return usersURLPrefix + targetID + eligibilityURLSuffix
}

func probeEligibility(
	t *testing.T, ts *testhelpers.TestServer, caller testhelpers.TestUser, targetID string,
) *httptest.ResponseRecorder {
	t.Helper()
	return ts.DoRequest(http.MethodGet, eligibilityPath(targetID), nil,
		testhelpers.AuthHeaders(caller.AccessToken))
}

// eligibleFlag decodes the one bit the endpoint is allowed to emit, and fails
// if the key is absent — a body of {} would otherwise decode to false and make
// every "expect false" assertion vacuous.
func eligibleFlag(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body: %s", w.Body.String())
	raw, present := body["eligible"]
	require.True(t, present, `the 200 body must carry an "eligible" key; got %s`, w.Body.String())
	flag, isBool := raw.(bool)
	require.True(t, isBool, `"eligible" must be a JSON boolean; got %T`, raw)
	return flag
}

// setPrivacyMode writes the target's allow_friend_requests_from directly.
//
// Direct SQL rather than a PATCH through /users/me/privacy on purpose: these
// tests are about the READER. Driving the writer would couple every case here
// to internal/users' step-up rules and make a users-side regression fail as a
// friends-side failure.
func setPrivacyMode(t *testing.T, ts *testhelpers.TestServer, userID, mode string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO privacy_settings (user_id, allow_friend_requests_from) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET allow_friend_requests_from = EXCLUDED.allow_friend_requests_from`,
		userID, mode,
	)
	require.NoError(t, err)
}

// joinSameServer puts both users in one freshly created server, which is the
// only thing that makes mutual_servers evaluate true.
func joinSameServer(t *testing.T, ts *testhelpers.TestServer, ownerID, memberID string) {
	t.Helper()
	serverID := ts.CreateTestServer(t, ownerID, "privacy-gate-server")
	ts.AddMemberToServer(t, serverID, memberID, "member")
}

func sendFriendRequest(
	t *testing.T, ts *testhelpers.TestServer, from testhelpers.TestUser, toID string,
) *httptest.ResponseRecorder {
	t.Helper()
	return ts.DoRequest(http.MethodPost, "/api/v1/friends/request",
		map[string]interface{}{"user_id": toID}, testhelpers.AuthHeaders(from.AccessToken))
}

func friendshipRowCount(t *testing.T, ts *testhelpers.TestServer, a, b string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT count(*) FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)`, a, b).Scan(&n))
	return n
}

func headerNames(w *httptest.ResponseRecorder) []string {
	names := make([]string, 0, len(w.Header()))
	for name := range w.Header() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ── The eligibility endpoint (spec T7 / §6.1) ────────────────────────────────

// TestFriendRequestEligibilityReflectsTheTargetsMode drives the endpoint's two
// 200 shapes through the full router, including auth and the rate limiter.
//
// Only the two extreme modes are exercised HERE; the six-cell matrix belongs to
// privacy_internal_test.go, because over HTTP the endpoint is contractually
// forbidden from distinguishing which mode produced a false (§2).
func TestFriendRequestEligibilityReflectsTheTargetsMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want bool
	}{
		{"everyone accepts a stranger", wireModeEveryone, true},
		{"nobody refuses a stranger", wireModeNobody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			caller := ts.CreateTestUser(t, "eligcaller")
			target := ts.CreateTestUser(t, "eligtarget")
			setPrivacyMode(t, ts, target.ID, tc.mode)

			w := probeEligibility(t, ts, caller, target.ID)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, tc.want, eligibleFlag(t, w))
		})
	}
}

// TestFriendRequestEligibilityHonoursMutualServers is the one cell where the
// endpoint's answer depends on state other than the enum. Both directions are
// asserted with the SAME mode so the difference can only come from the join.
func TestFriendRequestEligibilityHonoursMutualServers(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "mutualcaller")
	shared := ts.CreateTestUser(t, "mutualshared")
	stranger := ts.CreateTestUser(t, "mutualstranger")

	setPrivacyMode(t, ts, shared.ID, wireModeMutualServers)
	setPrivacyMode(t, ts, stranger.ID, wireModeMutualServers)
	joinSameServer(t, ts, shared.ID, caller.ID)

	sharedProbe := probeEligibility(t, ts, caller, shared.ID)
	require.Equal(t, http.StatusOK, sharedProbe.Code, sharedProbe.Body.String())
	assert.True(t, eligibleFlag(t, sharedProbe), "a co-member must be eligible under mutual_servers")

	strangerProbe := probeEligibility(t, ts, caller, stranger.ID)
	require.Equal(t, http.StatusOK, strangerProbe.Code, strangerProbe.Body.String())
	assert.False(t, eligibleFlag(t, strangerProbe), "a non-co-member must not be")
}

// TestFriendRequestEligibilityRejectsAMalformedUserID locks the 400 arm.
//
// The bad values are all things a real client sends: a truncated id, a
// username pasted into a path that wants a uuid, and an empty-ish segment.
func TestFriendRequestEligibilityRejectsAMalformedUserID(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "badparamcaller")

	for _, bad := range []string{"not-a-uuid", "12345", "someusername"} {
		t.Run(bad, func(t *testing.T) {
			w := probeEligibility(t, ts, caller, bad)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, "Invalid user_id", body["error"])
			assert.NotContains(t, body, "eligible",
				"a 400 must not also emit a verdict — a client that reads the bit "+
					"without checking the status would treat the absent key as false and "+
					"hide the affordance")
		})
	}
}

// TestFriendRequestEligibilityIs404ForAnUnknownUser locks AR-1: account
// existence stays distinguishable, deliberately and in writing.
func TestFriendRequestEligibilityIs404ForAnUnknownUser(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "unknowncaller")

	w := probeEligibility(t, ts, caller, uuid.New().String())
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "User not found", body["error"])
	assert.NotContains(t, body, "eligible", "no verdict may accompany a 404")
}

// TestFriendRequestEligibilityForSelfIsFalse locks the §6.1 self short-circuit.
//
// This is a CORRECTNESS requirement, not an optimization: the shared-server
// self-join matches trivially for anyone in at least one server, so without the
// short-circuit a self-probe returns true and the client offers a user the
// affordance to friend themselves. The caller is deliberately put in a server
// first — that is the exact state in which the unguarded query goes wrong, and
// a test with a serverless caller would pass either way.
func TestFriendRequestEligibilityForSelfIsFalse(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "selfprobe")
	ts.CreateTestServer(t, caller.ID, "self-probe-server")

	w := probeEligibility(t, ts, caller, caller.ID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, eligibleFlag(t, w),
		"a self-probe must answer false; the endpoint answers \"would a request be "+
			"accepted\", and for self it would not")
}

func TestFriendRequestEligibilityRequiresAuth(t *testing.T) {
	ts := setupTS(t)
	target := ts.CreateTestUser(t, "unauthtarget")

	w := ts.DoRequest(http.MethodGet, eligibilityPath(target.ID), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// ── SendRequest indistinguishability (spec T5 / §2 / AC-9) ───────────────────

// TestSendRequestPrivacyRejectionIsByteIdenticalToTheBlockRejection is the
// whole point of #1240.
//
// It compares the two recorders against EACH OTHER rather than against a
// literal, because a literal is a constant this test would also have written:
// if someone changed friendshipConflictResponse's copy, a literal-based test
// would fail on the copy while a divergence between the two paths — the thing
// that actually matters — went unnoticed.
//
// Three guards keep the comparison from being vacuous:
//   - the block path's 403 is asserted absolutely, so a pair of matching 500s
//     cannot pass;
//   - the body is asserted non-empty, so two empty bodies cannot pass;
//   - the privacy target is asserted to have NO friendship row, so the
//     "privacy" response is proven to come from the gate rather than from a
//     second block.
func TestSendRequestPrivacyRejectionIsByteIdenticalToTheBlockRejection(t *testing.T) {
	ts := setupTS(t)
	requester := ts.CreateTestUser(t, "indistreq")
	blocker := ts.CreateTestUser(t, "indistblock")
	private := ts.CreateTestUser(t, "indistpriv")

	// Path A — user-blocked: a blocked friendship row, no privacy setting.
	ts.CreateFriendship(t, requester.ID, blocker.ID, "blocked")
	// Path B — privacy-blocked: no friendship row at all, target on 'nobody'.
	setPrivacyMode(t, ts, private.ID, wireModeNobody)
	require.Zero(t, friendshipRowCount(t, ts, requester.ID, private.ID),
		"precondition: the privacy target must have NO friendship row, or this test "+
			"would be comparing the block path against itself")

	blockedResp := sendFriendRequest(t, ts, requester, blocker.ID)
	privacyResp := sendFriendRequest(t, ts, requester, private.ID)

	require.Equal(t, http.StatusForbidden, blockedResp.Code,
		"reference path: a blocked user must still 403. body: %s", blockedResp.Body.String())
	require.NotEmpty(t, blockedResp.Body.Bytes(),
		"positive control: an empty reference body would make the comparison below vacuous")

	assert.Equal(t, blockedResp.Code, privacyResp.Code,
		"a privacy-blocked rejection must carry the SAME status as a user-blocked one (§2)")
	assert.Equal(t, blockedResp.Body.Bytes(), privacyResp.Body.Bytes(),
		"the two rejection bodies must be byte-identical (§2). block=%q privacy=%q",
		blockedResp.Body.String(), privacyResp.Body.String())
	assert.Equal(t, headerNames(blockedResp), headerNames(privacyResp),
		"no header may distinguish the two rejections — not a Content-Type, and "+
			"certainly not a diagnostic one")
	assert.Equal(t,
		blockedResp.Header().Get("Content-Type"), privacyResp.Header().Get("Content-Type"))

	assert.Zero(t, friendshipRowCount(t, ts, requester.ID, private.ID),
		"a privacy-blocked request must write nothing")
}

// TestSendRequestPrivacyRejectionEmitsNoLogLine is spec T6 / AC-10.
//
// The enum never becomes a Go value in package friends — that is a property of
// the single-statement helper, not a convention — so the strongest available
// assertion is that no mode string and no eligibility wording reaches the log
// on the rejection path. A rejection that logged "target is on nobody" would
// hand an operator-log reader exactly the bit §2 withholds from the requester.
func TestSendRequestPrivacyRejectionEmitsNoLogLine(t *testing.T) {
	ts := setupTS(t)
	requester := ts.CreateTestUser(t, "nologreq")
	private := ts.CreateTestUser(t, "nologtarget")
	setPrivacyMode(t, ts, private.ID, wireModeNobody)

	logs := ts.CaptureLogs(t)
	w := sendFriendRequest(t, ts, requester, private.ID)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	captured := logs.String()
	for _, forbidden := range []string{
		wireModeNobody, wireModeMutualServers, "allow_friend_requests_from", "eligib",
	} {
		assert.NotContains(t, captured, forbidden,
			"the privacy rejection path must emit no log line naming %q", forbidden)
	}
}

// TestSendRequestSucceedsWhenTheTargetAcceptsEveryone is the positive control
// for the two tests above: it proves the 403s they assert are caused by the
// gate rather than by anything else in this fixture's setup.
func TestSendRequestSucceedsWhenTheTargetAcceptsEveryone(t *testing.T) {
	ts := setupTS(t)
	requester := ts.CreateTestUser(t, "eligiblereq")
	target := ts.CreateTestUser(t, "eligibletarget")
	setPrivacyMode(t, ts, target.ID, wireModeEveryone)

	w := sendFriendRequest(t, ts, requester, target.ID)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, 1, friendshipRowCount(t, ts, requester.ID, target.ID))
}

// TestSendRequestGateHonoursMutualServers proves the SendRequest path reads the
// same three-valued gate the endpoint does, not just a nobody/everyone boolean.
func TestSendRequestGateHonoursMutualServers(t *testing.T) {
	ts := setupTS(t)
	requester := ts.CreateTestUser(t, "sendmutualreq")
	coMember := ts.CreateTestUser(t, "sendmutualco")
	stranger := ts.CreateTestUser(t, "sendmutualstranger")

	setPrivacyMode(t, ts, coMember.ID, wireModeMutualServers)
	setPrivacyMode(t, ts, stranger.ID, wireModeMutualServers)
	joinSameServer(t, ts, coMember.ID, requester.ID)

	accepted := sendFriendRequest(t, ts, requester, coMember.ID)
	assert.Equal(t, http.StatusCreated, accepted.Code, accepted.Body.String())

	refused := sendFriendRequest(t, ts, requester, stranger.ID)
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
	assert.Zero(t, friendshipRowCount(t, ts, requester.ID, stranger.ID))
}

// TestSendRequestPreExistingOutcomesSurviveTheEligibilityGate is the regression
// half of the merge described in §6.3.
//
// The eligibility read REPLACED the bare users-EXISTS probe rather than being
// added after it, so every outcome that probe used to produce has to be
// re-proven against the new statement. The existing TestSendRequest_* cases
// cover these outcomes in general; what this adds is that they still hold with
// the target explicitly on 'everyone', i.e. that the gate does not shadow them.
func TestSendRequestPreExistingOutcomesSurviveTheEligibilityGate(t *testing.T) {
	t.Run("unknown target still 404s", func(t *testing.T) {
		ts := setupTS(t)
		requester := ts.CreateTestUser(t, "survive404")
		w := sendFriendRequest(t, ts, requester, uuid.New().String())
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "User not found", body["error"])
	})

	t.Run("already friends still 409s", func(t *testing.T) {
		ts := setupTS(t)
		requester := ts.CreateTestUser(t, "survivefriend1")
		target := ts.CreateTestUser(t, "survivefriend2")
		setPrivacyMode(t, ts, target.ID, wireModeEveryone)
		ts.CreateFriendship(t, requester.ID, target.ID, "accepted")

		w := sendFriendRequest(t, ts, requester, target.ID)
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "Already friends", body["error"])
	})

	t.Run("pending request still 409s", func(t *testing.T) {
		ts := setupTS(t)
		requester := ts.CreateTestUser(t, "survivepend1")
		target := ts.CreateTestUser(t, "survivepend2")
		setPrivacyMode(t, ts, target.ID, wireModeEveryone)
		ts.CreateFriendship(t, requester.ID, target.ID, "pending")

		w := sendFriendRequest(t, ts, requester, target.ID)
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "Friend request already pending", body["error"])
	})

	t.Run("self target still 400s before the gate", func(t *testing.T) {
		ts := setupTS(t)
		requester := ts.CreateTestUser(t, "surviveself")
		// Deliberately asymmetric with the eligibility endpoint, which answers
		// 200 {"eligible": false} for self. Both behaviours are specified.
		w := sendFriendRequest(t, ts, requester, requester.ID)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	})
}

// TestSendRequestConflictWinsOverAPrivacyRejection pins the ORDER of the two
// checks, which is load-bearing in a way that is easy to lose in a refactor.
//
// The gate read is hoisted above the friendship check but its verdict is
// applied BELOW it, so an existing pending request between two users still
// reports "Friend request already pending" even after the addressee flips to
// nobody. Applying the verdict earlier would convert that 409 into a 403 and
// tell the requester their target's setting had changed — a disclosure channel
// keyed on a state transition.
func TestSendRequestConflictWinsOverAPrivacyRejection(t *testing.T) {
	ts := setupTS(t)
	requester := ts.CreateTestUser(t, "orderreq")
	target := ts.CreateTestUser(t, "ordertarget")
	ts.CreateFriendship(t, requester.ID, target.ID, "pending")
	setPrivacyMode(t, ts, target.ID, wireModeNobody)

	w := sendFriendRequest(t, ts, requester, target.ID)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "Friend request already pending", body["error"],
		"the pre-existing conflict must still win; a 403 here would leak that the "+
			"addressee had changed their setting since the request was sent")
}

// TestPendingRequestStaysAcceptableAfterTheAddresseeChoosesNobody is spec T13,
// the Q1 corollary, proven mechanically rather than asserted in prose.
//
// 'nobody' governs NEW requests only. There is no write path from the setting
// to the friendships table, so this is a property of the design — but it is
// exactly the property a later "auto-decline pending on nobody" change would
// destroy, and the copy deck promises it to users.
func TestPendingRequestStaysAcceptableAfterTheAddresseeChoosesNobody(t *testing.T) {
	ts := setupTS(t)
	requester := ts.CreateTestUser(t, "q1requester")
	addressee := ts.CreateTestUser(t, "q1addressee")

	sent := sendFriendRequest(t, ts, requester, addressee.ID)
	require.Equal(t, http.StatusCreated, sent.Code, sent.Body.String())
	var created map[string]interface{}
	require.NoError(t, json.Unmarshal(sent.Body.Bytes(), &created))
	requestID, ok := created["id"].(string)
	require.True(t, ok, "the 201 body must carry the friendship id: %s", sent.Body.String())

	// The addressee now closes the door.
	setPrivacyMode(t, ts, addressee.ID, wireModeNobody)

	accept := ts.DoRequest(http.MethodPatch, pathFriendRequestSlash+requestID,
		map[string]interface{}{"action": "accept"}, testhelpers.AuthHeaders(addressee.AccessToken))
	require.Equal(t, http.StatusOK, accept.Code,
		"a request already in the inbox must remain acceptable after choosing 'nobody' — "+
			"the setting governs NEW requests only. body: %s", accept.Body.String())

	var status string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT status FROM friendships WHERE id = $1`, requestID).Scan(&status))
	assert.Equal(t, "accepted", status)
}
