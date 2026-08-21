package media

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	invitecodes "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The friend-code avatar route answers every invalid class with one shared
// silhouette so an anonymous caller cannot use it to test whether a code exists.
// That uniformity held only while the object store was healthy: the success arm
// delegated to proxyInviteIcon, which answers a non-not-found storage error with
// 500 and a nil store with 503 — and BOTH of those arms are reachable only by a
// code that is well-formed, exists, is live, and whose owner has an avatar.
//
// So during a storage incident the route became a clean binary classifier
// (fault status => live code with avatar, 200 SVG => everything else), and the
// nil-store arm makes that reachable permanently in any deployment with no
// object store configured, not just during an incident. CWE-203.
//
// The shipped tests could not have caught it: the api-package suite builds its
// router with a nil ObjectStore, so every case it compares already takes the
// fallback arm and the comparison is a tautology.

type friendAvatarCase struct {
	name string
	code string
}

// seedFriendAvatarFixtures creates one live code whose owner HAS an avatar —
// the only input that reaches the object store — plus the invalid classes that
// must stay indistinguishable from it.
func seedFriendAvatarFixtures(t *testing.T, ts *testSetup) (live string, cases []friendAvatarCase) {
	t.Helper()

	withAvatar := ts.createTestUser(t, "faoalice")
	_, err := ts.db.Exec(`UPDATE users SET avatar_url = $1 WHERE id = $2`,
		"/api/v1/media/avatars/"+withAvatar, withAvatar)
	require.NoError(t, err)
	noAvatar := ts.createTestUser(t, "faobob")

	insert := func(owner, code string, revoked bool) {
		t.Helper()
		_, err := ts.db.Exec(
			`INSERT INTO friend_codes (id, user_id, code, max_uses, use_count, is_revoked)
			 VALUES ($1, $2, $3, 0, 0, $4)`,
			uuid.New().String(), owner, code, revoked)
		require.NoError(t, err)
	}

	live = "FAVBLZE2"
	// Load-bearing guard, and the reason this test is not vacuous. The code
	// charset excludes I, l, O, 0 and 1; the first draft of this file used codes
	// containing "O", so EVERY fixture was charset-rejected and served the shared
	// fallback. The suite then compared six identical fallbacks and passed against
	// the unfixed tree. Assert the fixtures mean what their names claim.
	require.True(t, invitecodes.IsValidCode(live),
		"the live fixture must pass the charset guard, or it never reaches the object store "+
			"and this whole test degenerates into comparing fallbacks to each other")
	insert(withAvatar, live, false)
	insert(withAvatar, "FAVBRVK3", true)
	insert(noAvatar, "FAVBNAV4", false)

	return live, []friendAvatarCase{
		{"revoked code, owner has avatar", "FAVBRVK3"},
		{"live code, owner has NO avatar", "FAVBNAV4"},
		{"code that does not exist", "FAVBGNE5"},
		{"malformed — outside the code charset", "AAAAAAA1"}, // '1' is excluded
		{"malformed — wrong length", "SHORT"},
	}
}

func serveFriendAvatar(t *testing.T, h *Handler, code string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "code", Value: code}}
	c.Request = httptest.NewRequest(http.MethodGet,
		"/api/v1/friends/codes/"+code+"/avatar", nil)
	h.ProxyFriendCodeAvatar(c)
	return w
}

// shape is everything an anonymous caller can actually observe.
//
// Body BYTES, not just length, and Cache-Control alongside Content-Type: a
// length-only comparison passes for two different fallback bodies that happen to
// match in size, and omitting the cache directive would let the oracle survive in
// a header — which is the very reason this fix also drops the success TTL to
// max-age=60. Asserting parity while ignoring the header the fix equalizes would
// have been a test that agreed with the change instead of checking it.
func shape(w *httptest.ResponseRecorder) string {
	return fmt.Sprintf("status=%d content-type=%q cache-control=%q body=%q",
		w.Code,
		w.Header().Get("Content-Type"),
		w.Header().Get("Cache-Control"),
		w.Body.String())
}

func TestFriendAvatarIsNotAValidityClassifierDuringAnOutage(t *testing.T) {
	ts := setupMediaTest(t)
	live, cases := seedFriendAvatarFixtures(t, ts)

	// A live backend fault, NOT ErrObjectNotFound — the branch proxyInviteIcon
	// answers with 500 rather than the shared fallback.
	ts.store.getErr = errors.New("storage backend unavailable")

	liveShape := shape(serveFriendAvatar(t, ts.handler, live))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shape(serveFriendAvatar(t, ts.handler, tc.code))
			require.Equal(t, liveShape, got,
				"during a storage outage this response is distinguishable from a LIVE code whose owner "+
					"has an avatar, so the route answers \"does this friend code exist?\" for an anonymous "+
					"caller (CWE-203). live=%s / %s=%s", liveShape, tc.name, got)
		})
	}
}

// The store hands back a reader that yields bytes and THEN fails. Before the fix
// the handler had already sent 200 plus its headers and was streaming with
// io.Copy, so this left a truncated body on the wire that no invalid class could
// produce — the same oracle, one step later in the flow, and unretractable once
// the status line is out.
func TestFriendAvatarIsNotAValidityClassifierOnAMidStreamReadFailure(t *testing.T) {
	ts := setupMediaTest(t)
	live, cases := seedFriendAvatarFixtures(t, ts)

	ts.store.getPartial = []byte("\x89PNG\r\n\x1a\n truncated avatar bytes")

	liveShape := shape(serveFriendAvatar(t, ts.handler, live))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, liveShape, shape(serveFriendAvatar(t, ts.handler, tc.code)),
				"a read that fails PART WAY through leaves a response no invalid class can "+
					"produce, so the route still answers \"does this friend code exist?\" (CWE-203)")
		})
	}
}

func TestFriendAvatarIsNotAValidityClassifierWithoutAnObjectStore(t *testing.T) {
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	ts := &testSetup{
		handler: NewHandler(db, newMockStore(), logger.New("test"),
			&config.Config{UploadMaxSize: 25 * 1024 * 1024}, nil, freeTierStub{}),
		store: newMockStore(),
		db:    db,
	}
	live, cases := seedFriendAvatarFixtures(t, ts)

	// No object store configured at all. Unlike the outage above this is a
	// PERMANENT deployment state, so the classifier is not incident-scoped.
	noStore := NewHandler(db, nil, logger.New("test"),
		&config.Config{UploadMaxSize: 25 * 1024 * 1024}, nil, freeTierStub{})

	liveShape := shape(serveFriendAvatar(t, noStore, live))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shape(serveFriendAvatar(t, noStore, tc.code))
			require.Equal(t, liveShape, got,
				"with no object store configured this response is distinguishable from a LIVE code whose "+
					"owner has an avatar — a permanent validity oracle, not an incident-only one. "+
					"live=%s / %s=%s", liveShape, tc.name, got)
		})
	}
}
