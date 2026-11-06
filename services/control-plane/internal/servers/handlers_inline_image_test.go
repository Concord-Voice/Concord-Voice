package servers_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regression suite for #3856 Part B: CreateServer and UpdateServer accepted an
// inline data:image/ icon or banner and stored the bytes verbatim, so an
// animated image rendered animated in <img> without passing the upload route's
// gates (an animated icon or banner needs a Mach server).
// Static inline images keep working — the desktop CreateServerModal sends them
// inline at create time.

const (
	codeAnimatedIconMach   = "animated_server_icon_mach"
	codeAnimatedBannerMach = "animated_server_banner_mach"
	// inlineFrameCapPlusOne is one frame over media's maxGifFrames (120).
	inlineFrameCapPlusOne = 121
)

// animatedInlineImages are inline images every refusal path must catch,
// including a GIF whose declared MIME type lies (detection is by content).
func animatedInlineImages(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"animated gif":         testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3)),
		"animated apng":        testhelpers.DataURL("image/png", testhelpers.APNGBytes(t, true)),
		"animated webp":        testhelpers.DataURL("image/webp", testhelpers.WebPBytes(true)),
		"animated gif as png":  testhelpers.DataURL("image/png", testhelpers.GIFBytes(t, 3)),
		"animated webp as gif": testhelpers.DataURL("image/gif", testhelpers.WebPBytes(true)),
		// Go's image/gif decodes one frame here; browsers animate two.
		"gif hiding a frame from go": testhelpers.DataURL("image/gif", testhelpers.GIFWithFrameHiddenFromGoDecoder(t)),
	}
}

func staticInlineImages(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"png":              testhelpers.DataURL("image/png", testhelpers.StaticPNGBytes(t)),
		"jpeg":             testhelpers.DataURL("image/jpeg", testhelpers.StaticJPEGBytes(t)),
		"static gif":       testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 1)),
		"png labelled gif": testhelpers.DataURL("image/gif", testhelpers.APNGBytes(t, false)),
		"static webp":      testhelpers.DataURL("image/webp", testhelpers.WebPBytes(false)),
	}
}

// malformedInlineImages are data URLs the server cannot prove static.
func malformedInlineImages(t *testing.T) map[string]string {
	t.Helper()
	gifBytes := testhelpers.GIFBytes(t, 3)
	return map[string]string{
		"not base64":          "data:image/png;base64,!!!not-base64!!!",
		"no base64 marker":    "data:image/png,rawpayload",
		"no comma":            "data:image/png;base64",
		"unrecognized format": testhelpers.DataURL("image/png", []byte("this is not an image at all")),
		"svg":                 testhelpers.DataURL("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><animate/></svg>`)),
		// A browser decodes by the declared type, so SVG is refused whatever the bytes are.
		"png bytes declared svg": testhelpers.DataURL("image/svg+xml", testhelpers.StaticPNGBytes(t)),
		"gif header only":        testhelpers.DataURL("image/gif", []byte("GIF89a")),
		"truncated gif":          testhelpers.DataURL("image/gif", gifBytes[:len(gifBytes)/2]),
		"png without idat":       testhelpers.DataURL("image/png", testhelpers.APNGBytes(t, false)[:33]),
		"webp truncated chunk":   testhelpers.DataURL("image/webp", testhelpers.WebPBytes(true)[:16]),
		"empty payload":          "data:image/png;base64,",
		"riff but not webp":      testhelpers.DataURL("image/webp", append([]byte("RIFF\x04\x00\x00\x00WAVE"), 0, 0, 0, 0)),
		"png signature only":     testhelpers.DataURL("image/png", []byte("\x89PNG\r\n\x1a\n")),
		"webp riff header only":  testhelpers.DataURL("image/webp", []byte("RIFF\x04\x00\x00\x00WEBP")),
	}
}

// inlineOwnerSeq makes every owner username unique. Server create and update are
// each limited to 10 requests per minute per user, and the table-driven tests
// below fire far more than that, so each request gets its own owner.
var inlineOwnerSeq atomic.Int64

func newInlineOwner(t *testing.T, ts *testhelpers.TestServer) testhelpers.TestUser {
	t.Helper()
	return ts.CreateTestUser(t, fmt.Sprintf("inl%d", inlineOwnerSeq.Add(1)))
}

func serverCountOwnedBy(t *testing.T, ts *testhelpers.TestServer, ownerID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM servers WHERE owner_id = $1`, ownerID).Scan(&n))
	return n
}

func storedServerImage(t *testing.T, ts *testhelpers.TestServer, serverID, column string) *string {
	t.Helper()
	query := map[string]string{
		"icon_url":   `SELECT icon_url FROM servers WHERE id = $1`,
		"banner_url": `SELECT banner_url FROM servers WHERE id = $1`,
	}[column]
	require.NotEmpty(t, query, "unknown column %q", column)
	var stored *string
	require.NoError(t, ts.DB.QueryRow(query, serverID).Scan(&stored))
	return stored
}

// --- CreateServer ---

func TestCreateServerStaticInlineImagesStillWork(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range staticInlineImages(t) {
		t.Run(label, func(t *testing.T) {
			user := newInlineOwner(t, ts)
			w := ts.DoRequest("POST", pathServers, map[string]interface{}{
				"name":       "Static Inline Server",
				"icon_url":   dataURL,
				"banner_url": dataURL,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

			var body map[string]interface{}
			testhelpers.ParseJSON(t, w, &body)
			server := testhelpers.JSONField[map[string]interface{}](t, body, "server")
			assert.Equal(t, dataURL, server["icon_url"])
			assert.Equal(t, dataURL, server["banner_url"])
		})
	}
}

func TestCreateServerRefusesAnimatedInlineIconBelowMach(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range animatedInlineImages(t) {
		t.Run(label, func(t *testing.T) {
			user := newInlineOwner(t, ts)
			w := ts.DoRequest("POST", pathServers, map[string]interface{}{
				"name":     "Animated Icon Server",
				"icon_url": dataURL,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

			var body map[string]interface{}
			testhelpers.ParseJSON(t, w, &body)
			assert.Equal(t, codeAnimatedIconMach, body["code"])
			assert.Equal(t, "Animated server icons require a Mach server boost", body["error"])
			assert.Zero(t, serverCountOwnedBy(t, ts, user.ID), "a refused create must not create a server")
		})
	}
}

func TestCreateServerRefusesAnimatedInlineBannerBelowMach(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range animatedInlineImages(t) {
		t.Run(label, func(t *testing.T) {
			user := newInlineOwner(t, ts)
			w := ts.DoRequest("POST", pathServers, map[string]interface{}{
				"name":       "Animated Banner Server",
				"banner_url": dataURL,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

			var body map[string]interface{}
			testhelpers.ParseJSON(t, w, &body)
			assert.Equal(t, codeAnimatedBannerMach, body["code"])
			assert.Equal(t, "Animated server banners require a Mach server boost", body["error"])
			assert.Zero(t, serverCountOwnedBy(t, ts, user.ID), "a refused create must not create a server")
		})
	}
}

func TestCreateServerRefusesMalformedInlineImage(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range malformedInlineImages(t) {
		for _, field := range []string{"icon_url", "banner_url"} {
			t.Run(label+"/"+field, func(t *testing.T) {
				user := newInlineOwner(t, ts)
				w := ts.DoRequest("POST", pathServers, map[string]interface{}{
					"name": "Malformed Inline Server",
					field:  dataURL,
				}, testhelpers.AuthHeaders(user.AccessToken))
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				assertInlineImageRefusal(t, w)
				assert.Zero(t, serverCountOwnedBy(t, ts, user.ID), "a refused create must not create a server")
			})
		}
	}
}

// --- UpdateServer ---

func TestUpdateServerRefusesAnimatedInlineIconBelowMach(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range animatedInlineImages(t) {
		user := newInlineOwner(t, ts)
		serverID := ts.CreateTestServer(t, user.ID, "Icon Server")
		w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
			"name":     "Icon Server",
			"icon_url": dataURL,
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusForbidden, w.Code, label+": "+w.Body.String())

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, codeAnimatedIconMach, body["code"], label)
		assert.Nil(t, storedServerImage(t, ts, serverID, "icon_url"), label)
	}
}

func TestUpdateServerAcceptsAnimatedInlineIconOnATierThatAllowsIt(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmachanimicon")
	serverID := ts.CreateTestServer(t, user.ID, "Mach Icon Server")

	images := animatedInlineImages(t)
	for _, label := range []string{"animated gif"} {
		dataURL := images[label]
		w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
			"name":     "Mach Icon Server",
			"icon_url": dataURL,
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, label+": "+w.Body.String())

		stored := storedServerImage(t, ts, serverID, "icon_url")
		require.NotNil(t, stored, label)
		assert.Equal(t, dataURL, *stored, label+": the allowed animated icon is stored verbatim")
	}
}

func TestUpdateServerRefusesAnimatedInlineBannerBelowMach(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range animatedInlineImages(t) {
		user := newInlineOwner(t, ts)
		serverID := ts.CreateTestServer(t, user.ID, "Banner Server")
		w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
			"name":       "Banner Server",
			"banner_url": dataURL,
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusForbidden, w.Code, label+": "+w.Body.String())

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, codeAnimatedBannerMach, body["code"], label)
		assert.Nil(t, storedServerImage(t, ts, serverID, "banner_url"), label)
	}
}

func TestUpdateServerAcceptsAnimatedInlineBannerOnATierThatAllowsIt(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmachanimbanner")
	serverID := ts.CreateTestServer(t, user.ID, "Mach Banner Server")

	images := animatedInlineImages(t)
	for _, label := range []string{"animated gif"} {
		dataURL := images[label]
		w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
			"name":       "Mach Banner Server",
			"banner_url": dataURL,
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, label+": "+w.Body.String())

		stored := storedServerImage(t, ts, serverID, "banner_url")
		require.NotNil(t, stored, label)
		assert.Equal(t, dataURL, *stored, label+": the allowed animated banner is stored verbatim")
	}
}

func TestUpdateServerStaticInlineImagesStillWork(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range staticInlineImages(t) {
		user := newInlineOwner(t, ts)
		serverID := ts.CreateTestServer(t, user.ID, "Static Server")
		w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
			"name":       "Static Server",
			"icon_url":   dataURL,
			"banner_url": dataURL,
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, label+": "+w.Body.String())
	}
}

func TestUpdateServerRefusesMalformedInlineImage(t *testing.T) {
	ts := setupTS(t)

	for label, dataURL := range malformedInlineImages(t) {
		for _, field := range []string{"icon_url", "banner_url"} {
			user := newInlineOwner(t, ts)
			serverID := ts.CreateTestServer(t, user.ID, "Malformed Server")
			w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
				"name": "Malformed Server",
				field:  dataURL,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusBadRequest, w.Code, label+"/"+field+": "+w.Body.String())
			assertInlineImageRefusal(t, w)
			assert.Nil(t, storedServerImage(t, ts, serverID, field), label+"/"+field)
		}
	}
}

// assertInlineImageRefusal pins a 400 to the inline-image check rather than any
// other validation branch: both of its messages name the accepted formats.
func assertInlineImageRefusal(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "PNG, JPEG, GIF, or WebP")
}

// A value stored before #3856 may no longer pass the inline checks. Echoing it
// back with another edit leaves that field alone, as on PATCH /users/me; a
// different animated value is still refused.
func TestUpdateServerReSendOfStoredInlineImageIsANoOp(t *testing.T) {
	ts := setupTS(t)
	stored := testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3))
	other := testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 4))
	for _, column := range []string{"icon_url", "banner_url"} {
		t.Run(column, func(t *testing.T) {
			user := newInlineOwner(t, ts)
			serverID := ts.CreateTestServer(t, user.ID, "Legacy Server")
			seedServerImage(t, ts, serverID, column, stored)

			w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
				"name": "Renamed Legacy Server",
				column: stored,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			current := storedServerImage(t, ts, serverID, column)
			require.NotNil(t, current)
			assert.Equal(t, stored, *current)
			var name string
			require.NoError(t, ts.DB.QueryRow(`SELECT name FROM servers WHERE id = $1`, serverID).Scan(&name))
			assert.Equal(t, "Renamed Legacy Server", name)

			w = ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
				"name": "Renamed Legacy Server",
				column: other,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		})
	}
}

// On a self-hosted instance a new server starts at the self-host tier, which
// allows an animated banner, so create must accept what update would.
func TestCreateServerAcceptsAnimatedInlineBannerOnSelfHosted(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := setupTS(t)
	user := newInlineOwner(t, ts)
	banner := testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3))

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":       "Self Hosted Banner",
		"banner_url": banner,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var stored string
	require.NoError(t, ts.DB.QueryRow(`SELECT banner_url FROM servers WHERE owner_id = $1`, user.ID).Scan(&stored))
	assert.Equal(t, banner, stored)
}

func seedServerImage(t *testing.T, ts *testhelpers.TestServer, serverID, column, value string) {
	t.Helper()
	query := map[string]string{
		"icon_url":   `UPDATE servers SET icon_url = $1 WHERE id = $2`,
		"banner_url": `UPDATE servers SET banner_url = $1 WHERE id = $2`,
	}[column]
	require.NotEmpty(t, query, "unknown column %q", column)
	_, err := ts.DB.Exec(query, value, serverID)
	require.NoError(t, err)
}

// An allowed animated inline image is stored verbatim and never reaches the
// upload pipeline's decoder, so it must pass the same decode limits there:
// only GIF is accepted (no budget walker for APNG/WebP), and a GIF over the
// frame budget is refused like an upload would be.
func TestUpdateServerRefusesInlineAnimationsOutsideTheDecodeLimits(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := setupTS(t)
	images := animatedInlineImages(t)
	cases := map[string]string{
		"animated apng":      images["animated apng"],
		"animated webp":      images["animated webp"],
		"gif over frame cap": testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, inlineFrameCapPlusOne)),
	}
	for label, dataURL := range cases {
		for _, column := range []string{"icon_url", "banner_url"} {
			user := newInlineOwner(t, ts)
			serverID := ts.CreateTestServer(t, user.ID, "Budget Server")
			w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
				"name": "Budget Server",
				column: dataURL,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusBadRequest, w.Code, label+" "+column+": "+w.Body.String())
			assert.Nil(t, storedServerImage(t, ts, serverID, column), label+" "+column)
		}
	}
}

// On a self-hosted instance a new server starts at the self-host tier, which
// allows an animated icon, so create must accept what update would.
func TestCreateServerAcceptsAnimatedInlineIconOnSelfHosted(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := setupTS(t)
	user := newInlineOwner(t, ts)
	icon := testhelpers.DataURL("image/gif", testhelpers.GIFBytes(t, 3))

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":     "Self Hosted Icon",
		"icon_url": icon,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var stored string
	require.NoError(t, ts.DB.QueryRow(`SELECT icon_url FROM servers WHERE owner_id = $1`, user.ID).Scan(&stored))
	assert.Equal(t, icon, stored)
}
