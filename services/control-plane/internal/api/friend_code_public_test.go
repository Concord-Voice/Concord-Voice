package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// friendCodeUUIDRe matches the leading two groups of a canonical UUID. Kept
// deliberately loose (a prefix, not a full-form anchor) so a truncated or
// re-cased leak still trips it.
var friendCodeUUIDRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-`)

const (
	friendCodePreviewSuffix = "/preview"
	friendCodeAvatarSuffix  = "/avatar"
	friendCodesPath         = "/api/v1/friends/codes/"

	// fixedRequestID replaces the per-request UUID that middleware.RequestID
	// generates and echoes back in X-Request-ID. Supplying a non-UUID value
	// lets the header sweep run with NO exclusions: any UUID it then finds is
	// a genuine leak rather than the random correlation ID every route carries.
	fixedRequestID = "friend-code-public-test"
)

// getPublicFriendCode issues an anonymous GET against one of the two public
// friend-code routes. requestID, when non-empty, is sent as X-Request-ID.
func getPublicFriendCode(
	t *testing.T,
	ts *testhelpers.TestServer,
	code, suffix, requestID string,
) *httptest.ResponseRecorder {
	t.Helper()
	return getPublicFriendCodePath(t, ts, friendCodesPath+code+suffix, requestID)
}

// getPublicFriendCodePath is the raw-path form of getPublicFriendCode: the
// caller supplies the complete wire path, so percent-encoded cases can be
// expressed exactly as an attacker would send them.
func getPublicFriendCodePath(
	t *testing.T,
	ts *testhelpers.TestServer,
	path, requestID string,
) *httptest.ResponseRecorder {
	t.Helper()
	var headers http.Header
	if requestID != "" {
		headers = http.Header{}
		headers.Set("X-Request-ID", requestID)
	}
	return ts.DoRequest(http.MethodGet, path, nil, headers)
}

// percentEncodeFirstByte returns code with its leading byte percent-encoded.
// The DECODED path is byte-identical to the canonical one, so gin routes it to
// the same handler with the same :code param; the only difference lives in
// URL.RawPath — which is precisely what the edge rate-limit rule matches on and
// the origin must therefore reject (#945, VULN-001).
func percentEncodeFirstByte(code string) string {
	return fmt.Sprintf("%%%02X%s", code[0], code[1:])
}

// assertNoUUIDInHeaders sweeps every response header value for a UUID.
func assertNoUUIDInHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for name, values := range w.Result().Header {
		for _, v := range values {
			assert.NotRegexp(t, friendCodeUUIDRe, v, "header %s leaked a UUID", name)
		}
	}
}

func TestPublicFriendCodePreview(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	valid := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{
		Username: "fcalice", DisplayName: "Alice A", AvatarURL: "/api/v1/media/avatars/x",
	})
	expired := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{Username: "fcbob", Expired: true})
	revoked := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{Username: "fccarol", Revoked: true})
	maxed := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{
		Username: "fcdave", MaxUses: 1, UseCount: 1,
	})

	t.Run("every invalid class returns byte-identical output", func(t *testing.T) {
		// Ordered, not a map: the reference body must be a fixed case rather
		// than whichever one Go's randomized map iteration happened to visit
		// first.
		//
		// The last three carry percent-encoding and use the VALID code on
		// purpose. Gin routes on the DECODED URL.Path while the edge
		// rate-limit rule matches the RAW wire path, so these reach the
		// handler with no managed challenge and no edge bucket. They must land
		// in the same uniform invalid shape as every other rejected class —
		// rejecting them with anything distinguishable would trade the
		// rate-limit bypass for an enumeration oracle (#945, VULN-001).
		cases := []struct{ name, path string }{
			{"malformed charset", friendCodesPath + "AAAA000I" + friendCodePreviewSuffix},
			{"length 7", friendCodesPath + "AAAAAAA" + friendCodePreviewSuffix},
			{"length 9", friendCodesPath + "AAAAAAAAA" + friendCodePreviewSuffix},
			{"unknown", friendCodesPath + "ZZZZZZZZ" + friendCodePreviewSuffix},
			{"expired", friendCodesPath + expired.Code + friendCodePreviewSuffix},
			{"revoked", friendCodesPath + revoked.Code + friendCodePreviewSuffix},
			{"max used", friendCodesPath + maxed.Code + friendCodePreviewSuffix},
			{"encoded separator, uppercase", friendCodesPath + valid.Code + "%2Fpreview"},
			{"encoded separator, lowercase", friendCodesPath + valid.Code + "%2fpreview"},
			{
				"encoded character inside the code",
				friendCodesPath + percentEncodeFirstByte(valid.Code) + friendCodePreviewSuffix,
			},
		}

		reference := getPublicFriendCodePath(t, ts, cases[0].path, "")
		require.Equal(t, http.StatusOK, reference.Code)
		require.JSONEq(t, `{"valid":false}`, reference.Body.String())

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				w := getPublicFriendCodePath(t, ts, tc.path, "")
				assert.Equal(t, http.StatusOK, w.Code)
				// Byte equality, not field equality: a whitespace, key-order,
				// or extra-key difference between classes is itself an oracle.
				assert.Equal(t, reference.Body.Bytes(), w.Body.Bytes(),
					"invalid classes must be byte-indistinguishable")
			})
		}
	})

	t.Run("the canonical path still previews while its encoded twin does not", func(t *testing.T) {
		// The pairing is the point: the guard must reject the encoded form
		// WITHOUT costing the canonical form its preview.
		canonical := getPublicFriendCode(t, ts, valid.Code, friendCodePreviewSuffix, "")
		require.Equal(t, http.StatusOK, canonical.Code)
		require.Contains(t, canonical.Body.String(), `"valid":true`)
		require.Contains(t, canonical.Body.String(), "fcalice")

		encoded := getPublicFriendCodePath(t, ts, friendCodesPath+valid.Code+"%2Fpreview", "")
		assert.Equal(t, http.StatusOK, encoded.Code)
		assert.NotContains(t, encoded.Body.String(), "fcalice",
			"the encoded path must disclose nothing about the code's owner")
	})

	t.Run("valid preview omits user_id and avatar_url entirely", func(t *testing.T) {
		w := getPublicFriendCode(t, ts, valid.Code, friendCodePreviewSuffix, "")
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, true, body["valid"])
		assert.Equal(t, "fcalice", body["username"])
		assert.Equal(t, "Alice A", body["display_name"])

		// Key ABSENCE, not emptiness: assert.Empty passes on a present-but-empty
		// key, which would still hand the caller a field to grow a branch on.
		_, hasUserID := body["user_id"]
		assert.False(t, hasUserID, "user_id must not be present")
		_, hasAvatar := body["avatar_url"]
		assert.False(t, hasAvatar, "avatar_url must not be present")

		// The owner's real UUID must not appear anywhere in the body.
		assert.NotContains(t, w.Body.String(), valid.Owner.ID)
	})

	t.Run("valid preview omits display_name when the owner has none", func(t *testing.T) {
		noDisplayName := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{Username: "fcerin"})
		w := getPublicFriendCode(t, ts, noDisplayName.Code, friendCodePreviewSuffix, "")
		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t, `{"valid":true,"username":"fcerin"}`, w.Body.String())
	})

	t.Run("no UUID in the body or any response header", func(t *testing.T) {
		// With a caller-supplied non-UUID X-Request-ID the sweep needs no
		// exclusion list at all.
		w := getPublicFriendCode(t, ts, valid.Code, friendCodePreviewSuffix, fixedRequestID)
		require.Equal(t, http.StatusOK, w.Code)
		assert.NotRegexp(t, friendCodeUUIDRe, w.Body.String())
		assertNoUUIDInHeaders(t, w)
	})

	t.Run("public route needs no Authorization but the authenticated one still does", func(t *testing.T) {
		assert.Equal(t, http.StatusOK,
			getPublicFriendCode(t, ts, valid.Code, friendCodePreviewSuffix, "").Code)

		// Same :code param node, different route: proof that registering the
		// public suffixes did not accidentally open the protected leaf.
		w := getPublicFriendCode(t, ts, valid.Code, "", "")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestPublicFriendCodeAvatar(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	noAvatar := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{Username: "fcfrank"})
	revoked := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{
		Username: "fcgrace", AvatarURL: "/api/v1/media/avatars/y", Revoked: true,
	})

	t.Run("all fallback classes are byte-identical including headers", func(t *testing.T) {
		reference := getPublicFriendCode(t, ts, noAvatar.Code, friendCodeAvatarSuffix, "")
		require.Equal(t, http.StatusOK, reference.Code)
		require.Equal(t, "image/svg+xml; charset=utf-8", reference.Header().Get("Content-Type"))
		require.Equal(t, "public, max-age=60, must-revalidate", reference.Header().Get("Cache-Control"))

		for _, tc := range []struct{ name, code string }{
			{"malformed charset", "AAAA000I"},
			{"length 7", "AAAAAAA"},
			{"length 9", "AAAAAAAAA"},
			{"unknown", "ZZZZZZZZ"},
			{"revoked", revoked.Code},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := getPublicFriendCode(t, ts, tc.code, friendCodeAvatarSuffix, "")
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Equal(t, reference.Body.Bytes(), w.Body.Bytes())
				assert.Equal(t, reference.Header().Get("Content-Type"), w.Header().Get("Content-Type"))
				assert.Equal(t, reference.Header().Get("Cache-Control"), w.Header().Get("Cache-Control"))
			})
		}
	})

	t.Run("fallback matches the shipped invite icon fallback byte for byte", func(t *testing.T) {
		// The two arms of publicFriendAvatarHandler must be indistinguishable
		// from each other AND from the invite pair they mirror; a divergence in
		// either direction turns the fallback itself into a signal.
		friend := getPublicFriendCode(t, ts, noAvatar.Code, friendCodeAvatarSuffix, "")
		invite := ts.DoRequest(http.MethodGet, "/api/v1/invites/ZZZZZZZZ/icon", nil, nil)
		require.Equal(t, http.StatusOK, invite.Code)
		assert.Equal(t, invite.Body.Bytes(), friend.Body.Bytes())
		assert.Equal(t, invite.Header().Get("Content-Type"), friend.Header().Get("Content-Type"))
		assert.Equal(t, invite.Header().Get("Cache-Control"), friend.Header().Get("Cache-Control"))
	})

	t.Run("no UUID in the body or any response header", func(t *testing.T) {
		w := getPublicFriendCode(t, ts, revoked.Code, friendCodeAvatarSuffix, fixedRequestID)
		require.Equal(t, http.StatusOK, w.Code)
		assert.NotRegexp(t, friendCodeUUIDRe, w.Body.String())
		assertNoUUIDInHeaders(t, w)
		assert.NotContains(t, w.Body.String(), revoked.Owner.ID)
	})
}

// breakFriendCodesTable renames friend_codes out from under the running router
// so the handler's QueryRow fails with a real driver error, then restores it.
//
// Renaming rather than closing ts.DB: the shared pool is also what
// SetupTestServer's cleanup truncates through, so closing it would turn this
// test's teardown into an unrelated failure. The restore is registered as a
// t.Cleanup from the test body, so LIFO runs it before that truncation.
func breakFriendCodesTable(t *testing.T, ts *testhelpers.TestServer) {
	t.Helper()
	_, err := ts.DB.Exec(`ALTER TABLE friend_codes RENAME TO friend_codes_945_broken`)
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec(`ALTER TABLE friend_codes_945_broken RENAME TO friend_codes`); err != nil {
			t.Errorf("failed to restore friend_codes: %v", err)
		}
	})
}

// handlerLogLines returns only the captured lines emitted by the handler under
// test. The gin access log (middleware.Logger) records the raw request URL path
// and a random request_id on EVERY route — shipped, route-independent behavior
// that this handler neither introduces nor can influence — so scoping to the
// handler's own line is what makes the assertion about the handler.
func handlerLogLines(t *testing.T, captured, msg string) string {
	t.Helper()
	var matched []string
	for _, line := range strings.Split(captured, "\n") {
		if strings.Contains(line, msg) {
			matched = append(matched, line)
		}
	}
	require.NotEmpty(t, matched, "expected a log line containing %q, got:\n%s", msg, captured)
	return strings.Join(matched, "\n")
}

// TestPublicFriendCodePreviewDBError covers the one permitted divergence from
// the uniform invalid shape, and proves it still leaks nothing.
func TestPublicFriendCodePreviewDBError(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	seeded := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{Username: "fcheidi"})

	logs := ts.CaptureLogs(t)
	breakFriendCodesTable(t, ts)

	w := getPublicFriendCode(t, ts, seeded.Code, friendCodePreviewSuffix, fixedRequestID)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"error":"Failed to fetch friend code preview"}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), seeded.Code, "the code is bearer material")
	assert.NotRegexp(t, friendCodeUUIDRe, w.Body.String())
	assertNoUUIDInHeaders(t, w)

	line := handlerLogLines(t, logs.String(), "Failed to fetch public friend code preview")
	assert.NotContains(t, line, seeded.Code, "the code is bearer material")
	assert.NotContains(t, line, seeded.Owner.ID)
	assert.NotRegexp(t, friendCodeUUIDRe, line)
}

// TestAuthenticatedPreviewFriendCodeLeaksNothingOnInvalid is the regression
// guard for the two boy-scout fixes to the AUTHENTICATED PreviewFriendCode.
func TestAuthenticatedPreviewFriendCodeLeaksNothingOnInvalid(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	revoked := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{
		Username: "fcivan", DisplayName: "Ivan I", AvatarURL: "/api/v1/media/avatars/z", Revoked: true,
	})
	caller := ts.CreateTestUser(t, "fccaller")

	get := func(t *testing.T, code string) *httptest.ResponseRecorder {
		t.Helper()
		return ts.DoRequest(http.MethodGet, friendCodesPath+code, nil,
			testhelpers.AuthHeaders(caller.AccessToken))
	}

	t.Run("invalid code discloses no identity", func(t *testing.T) {
		w := get(t, revoked.Code)
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, false, body["valid"])
		for _, key := range []string{"username", "display_name", "avatar_url"} {
			_, present := body[key]
			assert.Falsef(t, present, "%s must not be present when valid is false", key)
		}
		assert.NotContains(t, w.Body.String(), "Ivan I")
	})

	t.Run("wrong charset still returns the existing 400", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, get(t, "AAAA000I").Code)
	})
}

// stubAvatarStore is a media.ObjectStore that answers exactly one key with a
// distinctive body. Any response carrying that body proves the handler reached
// the object store instead of serving the uniform fallback — which is what
// makes the encoded-path assertions below able to tell exploited from fixed.
// The wider TestPublicFriendCodeAvatar above runs against the store-less arm
// (SetupTestServer passes a nil ObjectStore, so the router selects
// friends.GetPublicFriendAvatarFallback), and that arm answers everything with
// the fallback — it cannot distinguish the two states at all.
type stubAvatarStore struct {
	key  string
	body []byte
}

func (s *stubAvatarStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	if key != s.key {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(s.body)), "image/png", nil
}

var errStubAvatarStoreUnused = errors.New("stubAvatarStore: operation not used by this test")

func (s *stubAvatarStore) PutObject(context.Context, string, io.Reader, int64, string) error {
	return errStubAvatarStoreUnused
}

func (s *stubAvatarStore) PresignedGetURL(context.Context, string, time.Duration) (string, error) {
	return "", errStubAvatarStoreUnused
}

func (s *stubAvatarStore) DeleteObject(context.Context, string) error {
	return errStubAvatarStoreUnused
}

// newFriendCodeAvatarRouter mounts media.ProxyFriendCodeAvatar on the real
// route pattern, backed by store. The production router selects this handler
// whenever an object store is configured; the shared test server has none, so
// exercising it needs its own engine.
func newFriendCodeAvatarRouter(t *testing.T, ts *testhelpers.TestServer, store media.ObjectStore) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	handler := media.NewHandler(ts.DB, store, logger.NewWithWriter(io.Discard), &config.Config{}, nil, nil)
	router := gin.New()
	router.GET(friendCodesPath+":code"+friendCodeAvatarSuffix, handler.ProxyFriendCodeAvatar)
	return router
}

// TestPublicFriendCodeAvatarEncodedPath proves the proxying arm of the public
// avatar route rejects percent-encoded paths, and does so by serving the exact
// fallback bytes and headers every other invalid class serves.
//
// The edge rate-limit rule matches the RAW wire path, so /…/CODE%2Favatar
// carries no managed challenge and no edge bucket while still reaching this
// handler — which, unlike the preview, has no RateLimitGlobal companion and
// drives unbounded DB and object-store work (#945, VULN-001).
func TestPublicFriendCodeAvatarEncodedPath(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	seeded := ts.SeedFriendCode(t, testhelpers.FriendCodeSeed{
		Username: "fcjudy", AvatarURL: "/api/v1/media/avatars/judy",
	})
	store := &stubAvatarStore{
		key:  "avatars/" + seeded.Owner.ID,
		body: []byte("proxied-avatar-bytes-945"),
	}
	router := newFriendCodeAvatarRouter(t, ts, store)

	do := func(t *testing.T, path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	// The uniform shape every invalid class must be indistinguishable from.
	fallback := do(t, friendCodesPath+"ZZZZZZZZ"+friendCodeAvatarSuffix)
	require.Equal(t, http.StatusOK, fallback.Code)
	require.NotEqual(t, store.body, fallback.Body.Bytes(),
		"the fallback must not coincide with the proxied bytes, or nothing below can distinguish them")

	t.Run("canonical path still proxies the owner's avatar", func(t *testing.T) {
		w := do(t, friendCodesPath+seeded.Code+friendCodeAvatarSuffix)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, store.body, w.Body.Bytes(),
			"the guard must not cost the canonical path its avatar")
	})

	for _, tc := range []struct{ name, path string }{
		{"encoded separator, uppercase", friendCodesPath + seeded.Code + "%2Favatar"},
		{"encoded separator, lowercase", friendCodesPath + seeded.Code + "%2favatar"},
		{
			"encoded character inside the code",
			friendCodesPath + percentEncodeFirstByte(seeded.Code) + friendCodeAvatarSuffix,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, tc.path)
			// Bytes, not status: the fallback also answers 200, so a status
			// assertion alone cannot tell exploited from fixed.
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, fallback.Body.Bytes(), w.Body.Bytes(),
				"an encoded path must serve the fallback, byte for byte")
			assert.Equal(t, fallback.Header().Get("Content-Type"), w.Header().Get("Content-Type"))
			assert.Equal(t, fallback.Header().Get("Cache-Control"), w.Header().Get("Cache-Control"))
			assert.NotContains(t, w.Body.String(), seeded.Owner.ID)
		})
	}
}

// Multipart upload: interface satisfaction only. This stub serves the public
// friend-code avatar route, which never touches the attachment session.
func (s *stubAvatarStore) NewMultipartUpload(_ context.Context, _, _ string) (string, error) {
	return "", errStubAvatarStoreUnused
}

func (s *stubAvatarStore) PutObjectPart(
	_ context.Context, _, _ string, _ int, _ io.Reader, _ int64,
) (storage.ObjectPartInfo, error) {
	return storage.ObjectPartInfo{}, errStubAvatarStoreUnused
}

func (s *stubAvatarStore) ListObjectParts(_ context.Context, _, _ string) ([]storage.ObjectPartInfo, error) {
	return nil, errStubAvatarStoreUnused
}

func (s *stubAvatarStore) CompleteMultipartUpload(
	_ context.Context, _, _ string, _ []storage.ObjectPartInfo,
) error {
	return errStubAvatarStoreUnused
}

func (s *stubAvatarStore) AbortMultipartUpload(_ context.Context, _, _ string) error {
	return errStubAvatarStoreUnused
}

func (s *stubAvatarStore) ListIncompleteUploads(
	_ context.Context, _ time.Time,
) ([]storage.IncompleteUpload, error) {
	return nil, errStubAvatarStoreUnused
}
