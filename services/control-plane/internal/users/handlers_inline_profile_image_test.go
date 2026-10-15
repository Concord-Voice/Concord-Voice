package users_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	concordws "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
)

// Regression suite for #3856: PATCH /api/v1/users/me must not accept a NEW
// inline data:image/ avatar or banner. The inline path stored the bytes
// verbatim, so a free account could set an animated GIF that every client
// renders with <img> and animates — bypassing the Supersonic-only upload gate
// (animated_profile_premium). The refusal is tier-independent.

const inlineProfileImageCode = "inline_profile_image_unsupported"

// inlineProfileField is one inline-capable profile column and the upload route
// the refusal must point the caller at.
type inlineProfileField struct {
	name        string
	jsonKey     string
	column      string
	uploadRoute string
}

var inlineProfileFields = []inlineProfileField{
	{name: "avatar", jsonKey: "avatar_url", column: "avatar_url", uploadRoute: "POST /api/v1/media/upload/avatar"},
	{name: "banner", jsonKey: "header_image_url", column: "header_image_url", uploadRoute: "POST /api/v1/media/upload/banner"},
}

// storedProfileImage reads a profile image column; nil means NULL.
func storedProfileImage(t *testing.T, ts *testhelpers.TestServer, userID, column string) *string {
	t.Helper()
	var stored *string
	// column comes from the fixed inlineProfileFields table above, never from input.
	require.NoError(t, ts.DB.QueryRow(
		fmt.Sprintf("SELECT %s FROM users WHERE id = $1", column), userID,
	).Scan(&stored))
	return stored
}

// seedInlineProfileImage writes an inline value straight into the column, the
// way a pre-#3856 row looks.
func seedInlineProfileImage(t *testing.T, ts *testhelpers.TestServer, userID, column, value string) {
	t.Helper()
	_, err := ts.DB.Exec(fmt.Sprintf("UPDATE users SET %s = $1 WHERE id = $2", column), value, userID)
	require.NoError(t, err)
}

func inlineProfilePayloads(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"static png":          testhelpers.DataURL("image/png", testhelpers.StaticPNGBytes(t)),
		"animated gif":        testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3)),
		"animated gif as png": testhelpers.DataURL("image/png", testhelpers.GIFBytes(t, 3)),
	}
}

func TestUpdateMeRefusesNewInlineProfileImageAtEveryTier(t *testing.T) {
	ts := setupTS(t)
	free := ts.CreateTestUser(t, "inlinefree")
	premium := ts.CreateTestUser(t, "inlineprem")
	grantPremium(t, ts, premium.ID)

	for _, acct := range []struct {
		tier string
		user testhelpers.TestUser
	}{
		{"free", free},
		{"premium", premium},
	} {
		for _, field := range inlineProfileFields {
			for label, dataURL := range inlineProfilePayloads(t) {
				t.Run(acct.tier+"/"+field.name+"/"+label, func(t *testing.T) {
					w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
						field.jsonKey: dataURL,
					}, testhelpers.AuthHeaders(acct.user.AccessToken))

					require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
					var body map[string]interface{}
					testhelpers.ParseJSON(t, w, &body)
					assert.Equal(t, inlineProfileImageCode, body["code"])
					assert.Contains(t, body["error"], field.uploadRoute)
					assert.Nil(t, storedProfileImage(t, ts, acct.user.ID, field.column),
						"a refused inline image must not be stored")
				})
			}
		}
	}
}

func TestUpdateMeRefusedInlineImageRollsBackTheRestOfTheEdit(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlineatomic")

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"bio":        "must not land",
		"avatar_url": testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3)),
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	var bio *string
	require.NoError(t, ts.DB.QueryRow(`SELECT bio FROM users WHERE id = $1`, user.ID).Scan(&bio))
	assert.Nil(t, bio, "the whole PATCH is refused, not just the image field")
}

func TestUpdateMeCanonicalAndClearedProfileImagesStillWork(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlinecanonical")
	seedLiveTier1ProfileMedia(t, ts, user.ID, "avatars/"+user.ID)
	seedLiveTier1ProfileMedia(t, ts, user.ID, "banners/"+user.ID)

	canonical := map[string]interface{}{
		"avatar_url":       fmt.Sprintf("/api/v1/media/avatars/%s", user.ID),
		"header_image_url": fmt.Sprintf("/api/v1/media/banners/%s", user.ID),
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, canonical, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for _, field := range inlineProfileFields {
		stored := storedProfileImage(t, ts, user.ID, field.column)
		require.NotNil(t, stored)
		assert.Equal(t, canonical[field.jsonKey], *stored)
	}

	w = ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url":       "",
		"header_image_url": "",
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for _, field := range inlineProfileFields {
		assert.Nil(t, storedProfileImage(t, ts, user.ID, field.column))
	}
}

func TestUpdateMeClearsAStoredInlineProfileImage(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlineclearstored")
	for _, field := range inlineProfileFields {
		seedInlineProfileImage(t, ts, user.ID, field.column, testhelpers.DataURL("image/png", testhelpers.StaticPNGBytes(t)))
	}

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url":       "",
		"header_image_url": "",
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for _, field := range inlineProfileFields {
		assert.Nil(t, storedProfileImage(t, ts, user.ID, field.column), field.name)
	}
}

func TestUpdateMeExactResendOfStoredInlineImageIsANoOp(t *testing.T) {
	for _, field := range inlineProfileFields {
		t.Run(field.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "inlineresend"+field.name)
			// A pre-#3856 row can hold an animated blob; re-sending it must not
			// turn into a 400 for a client that echoes its whole profile back.
			stored := testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3))
			seedInlineProfileImage(t, ts, user.ID, field.column, stored)

			w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
				field.jsonKey: stored,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			current := storedProfileImage(t, ts, user.ID, field.column)
			require.NotNil(t, current)
			assert.Equal(t, stored, *current, "re-send must leave the stored value untouched")
		})
	}
}

func TestUpdateMeResendAlongsideARealEditAppliesTheEdit(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlineresendbio")
	stored := testhelpers.DataURL("image/png", testhelpers.StaticPNGBytes(t))
	seedInlineProfileImage(t, ts, user.ID, "avatar_url", stored)

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url": stored,
		"bio":        "edited with the stored avatar echoed back",
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := testhelpers.JSONField[map[string]interface{}](t, body, "user")
	assert.Equal(t, "edited with the stored avatar echoed back", userData["bio"])
	assert.Equal(t, stored, userData["avatar_url"])
}

func TestUpdateMeDifferentInlineImageWhenOneIsStoredIsRefused(t *testing.T) {
	for _, field := range inlineProfileFields {
		t.Run(field.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "inlinedifferent"+field.name)
			stored := testhelpers.DataURL("image/png", testhelpers.StaticPNGBytes(t))
			seedInlineProfileImage(t, ts, user.ID, field.column, stored)

			w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
				field.jsonKey: testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3)),
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

			var body map[string]interface{}
			testhelpers.ParseJSON(t, w, &body)
			assert.Equal(t, inlineProfileImageCode, body["code"])

			current := storedProfileImage(t, ts, user.ID, field.column)
			require.NotNil(t, current)
			assert.Equal(t, stored, *current, "the stored value must survive a refused replacement")
		})
	}
}

// An inline value longer than the retired inline cap cannot be a stored one, so
// it is refused before the database comparison. Seeding the column with the same
// value is what proves it: at the cap the comparison answers 200, and one byte
// past it only the length guard can answer 400.
func TestUpdateMeInlineImageLengthBoundary(t *testing.T) {
	const prefix = "data:image/png;base64,"
	atCap := prefix + strings.Repeat("A", users.MaxInlineResendLen-len(prefix))
	for _, tc := range []struct {
		name  string
		value string
		want  int
	}{
		{"at the retired cap", atCap, http.StatusOK},
		{"one byte past it", atCap + "A", http.StatusBadRequest},
	} {
		for _, field := range inlineProfileFields {
			t.Run(tc.name+"/"+field.name, func(t *testing.T) {
				ts := setupTS(t)
				user := ts.CreateTestUser(t, "inlinecap"+field.name)
				seedInlineProfileImage(t, ts, user.ID, field.column, tc.value)

				w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
					field.jsonKey: tc.value,
				}, testhelpers.AuthHeaders(user.AccessToken))
				require.Equal(t, tc.want, w.Code)
				if tc.want == http.StatusBadRequest {
					var body map[string]interface{}
					testhelpers.ParseJSON(t, w, &body)
					assert.Equal(t, inlineProfileImageCode, body["code"])
				}
			})
		}
	}
}

// A NUL byte cannot be in a stored text column, and sending one to the
// comparison query would fail it as a database error. It is the typed 400.
func TestUpdateMeInlineImageWithNULByteIsRefused(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlinenul")
	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url": "data:image/png;base64,AA\x00AA",
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, inlineProfileImageCode, body["code"])
}

// Every pending inline value is checked: a matching avatar re-send does not
// carry a different inline banner through, and the whole PATCH rolls back.
func TestUpdateMeMatchingResendDoesNotCarryAMismatchedField(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlinemixed")
	stored := testhelpers.DataURL("image/png", testhelpers.StaticPNGBytes(t))
	seedInlineProfileImage(t, ts, user.ID, "avatar_url", stored)

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url":       stored,
		"header_image_url": testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3)),
		"bio":              "must not land",
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, inlineProfileImageCode, body["code"])
	assert.Nil(t, storedProfileImage(t, ts, user.ID, "header_image_url"))

	var bio *string
	require.NoError(t, ts.DB.QueryRow(`SELECT bio FROM users WHERE id = $1`, user.ID).Scan(&bio))
	assert.Nil(t, bio, "the refused PATCH must roll back the bio edit")
}

// A lone matching re-send changes nothing, so it must not broadcast
// profile_updated (a legacy inline value can be megabytes, sent to every client).
// A real edit by the same user is the positive control: the observer does see it.
func TestUpdateMeLoneMatchingResendDoesNotBroadcast(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlinequiet")
	observer := ts.CreateTestUser(t, "inlinequietobs")
	stored := testhelpers.DataURL("image/png", testhelpers.StaticPNGBytes(t))
	seedInlineProfileImage(t, ts, user.ID, "avatar_url", stored)
	conn := dialProfileObserver(t, ts, observer.ID)

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{"avatar_url": stored},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	ts.Hub.BroadcastToAll(concordws.OutgoingMessage{Type: "inline_quiet_sentinel", Data: map[string]interface{}{}})
	assert.NotContains(t, readProfileWireUntil(t, conn, "inline_quiet_sentinel"), "profile_updated",
		"a lone matching re-send must not broadcast")

	w = ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{"bio": "a real edit"},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	readProfileWireUntil(t, conn, "profile_updated")
}

func dialProfileObserver(t *testing.T, ts *testhelpers.TestServer, userID string) *gorillaWS.Conn {
	t.Helper()
	ticket := "inline-quiet-" + uuid.NewString()
	require.NoError(t, ts.Redis.Set(t.Context(), "ws_ticket:"+ticket, userID+":inline-quiet", time.Minute).Err())
	srv := httptest.NewServer(ts.Router)
	t.Cleanup(srv.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial("ws"+srv.URL[4:]+"/api/v1/ws?ticket="+ticket, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(userID)) > 0
	}, 2*time.Second, 10*time.Millisecond)
	return conn
}

// readProfileWireUntil reads frames until one of type want arrives and returns
// the types seen before it.
func readProfileWireUntil(t *testing.T, conn *gorillaWS.Conn, want string) []string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	var seen []string
	for {
		var frame struct {
			Type string `json:"type"`
		}
		require.NoError(t, conn.ReadJSON(&frame))
		if frame.Type == want {
			return seen
		}
		seen = append(seen, frame.Type)
	}
}
