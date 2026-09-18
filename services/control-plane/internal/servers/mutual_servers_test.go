package servers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func mutualServerIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	raw := testhelpers.JSONField[[]interface{}](t, body, "server_ids")

	ids := make([]string, 0, len(raw))
	for i := range raw {
		ids = append(ids, testhelpers.JSONElem[string](t, raw, i))
	}
	return ids
}

// GET /users/{id}/mutual-servers returns the INTERSECTION and nothing else
// (#2372).
//
// The third fixture is the one that matters. A server the TARGET is in and the
// caller is not must never appear: the endpoint's whole privacy argument is
// that every id it returns names a server the caller is already in, and can
// therefore already read the member list of. Leak that row and this stops being
// an intersection and becomes a general "which servers is this person in?"
// lookup — which is not a thing this API offers.
func TestGetMutualServersReturnsIntersectionOnly(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "mutualcaller")
	target := ts.CreateTestUser(t, "mutualtarget")
	outsider := ts.CreateTestUser(t, "mutualoutsider")

	shared := ts.CreateTestServer(t, caller.ID, "Shared Server")
	ts.AddMemberToServer(t, shared, target.ID, "member")

	callerOnly := ts.CreateTestServer(t, caller.ID, "Caller Only")

	targetOnly := ts.CreateTestServer(t, outsider.ID, "Target Only")
	ts.AddMemberToServer(t, targetOnly, target.ID, "member")

	ids := mutualServerIDs(t, ts.DoRequest(
		"GET", "/api/v1/users/"+target.ID+"/mutual-servers", nil,
		testhelpers.AuthHeaders(caller.AccessToken),
	))

	assert.Equal(t, []string{shared}, ids, "only the server both are in")
	assert.NotContains(t, ids, callerOnly, "a server the target is not in is not mutual")
	assert.NotContains(t, ids, targetOnly,
		"a server the CALLER is not in must never be disclosed — that is the privacy boundary")
}

// No overlap serializes as `[]`, never `null`.
//
// Asserted against the RAW JSON, because Go decodes both into the same nil
// slice and a struct-level Empty check passes either way. The renderer iterates
// this field, where `null` is a runtime error rather than an empty loop.
func TestGetMutualServersEmptyIsNotNull(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "mutualemptycaller")
	target := ts.CreateTestUser(t, "mutualemptytarget")
	ts.CreateTestServer(t, caller.ID, "Caller Alone")

	w := ts.DoRequest("GET", "/api/v1/users/"+target.ID+"/mutual-servers", nil,
		testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Contains(t, raw, "server_ids")
	assert.JSONEq(t, `[]`, string(raw["server_ids"]))
}

// An unknown user is answered with an empty list, NOT a 404.
//
// Deliberately different from the adjacent /friend-request-eligibility route,
// which does 404 there. That route has to look the user up to answer at all;
// this one computes its answer from a join and needs no existence check, so
// adding one would hand out a user-existence oracle in exchange for nothing.
// A caller cannot distinguish "no such user" from "we share no servers", which
// is the point.
func TestGetMutualServersUnknownUserIsIndistinguishableFromNoOverlap(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "mutualunknowncaller")
	stranger := ts.CreateTestUser(t, "mutualknownstranger")
	ts.CreateTestServer(t, caller.ID, "Caller Server")

	nonexistent := ts.DoRequest("GET", "/api/v1/users/"+uuid.New().String()+"/mutual-servers", nil,
		testhelpers.AuthHeaders(caller.AccessToken))
	existing := ts.DoRequest("GET", "/api/v1/users/"+stranger.ID+"/mutual-servers", nil,
		testhelpers.AuthHeaders(caller.AccessToken))

	assert.Equal(t, http.StatusOK, nonexistent.Code)
	assert.Equal(t, existing.Code, nonexistent.Code, "status must not distinguish the two")
	assert.JSONEq(t, existing.Body.String(), nonexistent.Body.String(),
		"body must not distinguish the two either")
}

// A malformed id is refused before it reaches the driver. Left unvalidated it
// surfaces as a pq 22P02 whose message echoes the caller's string into the log.
func TestGetMutualServersRejectsMalformedUserID(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "mutualbadidcaller")

	w := ts.DoRequest("GET", "/api/v1/users/not-a-uuid/mutual-servers", nil,
		testhelpers.AuthHeaders(caller.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetMutualServersRequiresAuth(t *testing.T) {
	ts := setupTS(t)
	target := ts.CreateTestUser(t, "mutualnoauthtarget")

	w := ts.DoRequest("GET", "/api/v1/users/"+target.ID+"/mutual-servers", nil, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// `uuid.Parse` is a parser, not a canonicality check, and the difference is
// reachable here.
//
// It accepts `urn:uuid:<id>`, `{<id>}`, the unhyphenated 32-hex form and any
// case. Measured against this project's Postgres 16: braces and the
// unhyphenated form are accepted by the uuid type and canonicalized, but the
// `urn:uuid:` prefix is REJECTED with 22P02. So before the handler re-serialized
// through `parsed.String()`, that one form passed validation and reached the
// driver — producing exactly the pq error whose message echoes the caller's
// string into the log, which is the thing the validation's own comment says it
// exists to prevent.
//
// The assertion is a 200 carrying the RIGHT intersection rather than merely
// "not a 500": a handler that canonicalized to the wrong id would also avoid
// the 500, and would answer with some other user's shared servers.
func TestGetMutualServersCanonicalizesNonCanonicalUUIDForms(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "mutualcanoncaller")
	target := ts.CreateTestUser(t, "mutualcanontarget")

	shared := ts.CreateTestServer(t, caller.ID, "Shared Canon Server")
	ts.AddMemberToServer(t, shared, target.ID, "member")

	// The control: the canonical form, establishing what the right answer is.
	canonical := mutualServerIDs(t, ts.DoRequest(
		"GET", "/api/v1/users/"+target.ID+"/mutual-servers", nil,
		testhelpers.AuthHeaders(caller.AccessToken),
	))
	require.Equal(t, []string{shared}, canonical, "control: canonical id resolves normally")

	// The form that used to reach the driver.
	w := ts.DoRequest(
		"GET", "/api/v1/users/urn:uuid:"+target.ID+"/mutual-servers", nil,
		testhelpers.AuthHeaders(caller.AccessToken),
	)
	require.NotEqual(t, http.StatusInternalServerError, w.Code,
		"a urn:uuid: id must not reach Postgres as-is — that is a 22P02 with the caller's string in the message")
	assert.Equal(t, canonical, mutualServerIDs(t, w),
		"the urn form names the same user, so it must return the same intersection")
}

// Malformed ids stay refused. Canonicalizing the parseable forms must not have
// widened what parses at all — `uuid.Parse` is still the gate, and this pins
// that the gate did not move.
func TestGetMutualServersStillRejectsGarbageAfterCanonicalization(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "mutualgarbagecaller")

	for _, bad := range []string{"not-a-uuid", "urn:uuid:not-a-uuid", uuid.NewString() + "-extra"} {
		w := ts.DoRequest(
			"GET", "/api/v1/users/"+bad+"/mutual-servers", nil,
			testhelpers.AuthHeaders(caller.AccessToken),
		)
		assert.Equal(t, http.StatusBadRequest, w.Code, "id %q must be refused at the handler", bad)
	}
}
