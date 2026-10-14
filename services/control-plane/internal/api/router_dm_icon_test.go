package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	rawnats "github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	dmIconRouterPrefix = "/api/v1/media/dm-icons/"
	dmIconPrivateCache = "private, no-store"
	dmIconPublicCache  = "public, max-age=3600, must-revalidate"
)

// Only the remote object boundary is replaced. The constructor, PostgreSQL,
// bearer/epoch checks, recorded attestation and route middleware remain real.
// Unused write methods inherit the existing fail-closed test-store methods.
type dmIconRouterStore struct {
	stubAvatarStore
	objects map[string][]byte
	getErr  error
	gets    atomic.Int64
	closes  atomic.Int64
}

func (s *dmIconRouterStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	s.gets.Add(1)
	if s.getErr != nil {
		return nil, "", s.getErr
	}
	body, exists := s.objects[key]
	if !exists {
		return nil, "", storage.ErrObjectNotFound
	}
	return &dmIconRouterReader{Reader: bytes.NewReader(body), closes: &s.closes}, "image/png", nil
}

type dmIconRouterReader struct {
	*bytes.Reader
	closes *atomic.Int64
}

func (r *dmIconRouterReader) Close() error { r.closes.Add(1); return nil }

type dmIconRouterActor struct {
	user    testhelpers.TestUser
	headers http.Header
}

type dmIconRouterFixture struct {
	db                                         *sql.DB
	rdb                                        *redis.Client
	mr                                         *miniredis.Miniredis
	router                                     http.Handler
	store                                      *dmIconRouterStore
	configured                                 bool
	redisClosed                                bool
	member, outsider, unverified, removed      dmIconRouterActor
	conversation, secondConversation, serverID string
}

// Discovery is a constructor prerequisite. This fixture never verifies a
// publisher token; DM access uses the real recorded-attestation middleware.
func dmIconRouterDiscovery(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	body, err := json.Marshal(map[string]any{
		"issuer": server.URL, "jwks_uri": server.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
	require.NoError(t, err)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(body); err != nil {
			t.Errorf("write local OIDC discovery: %v", err)
		}
	})
	return server.URL
}

func newDMIconRouterFixture(t *testing.T, configured bool) *dmIconRouterFixture {
	t.Helper()
	t.Setenv("CONCORD_ENV", "test")
	db, _ := testhelpers.SetupTestDB(t)
	// Suite-local miniredis is the frozen-clock exception used by the router
	// delete tests: no shared Redis state and no wall-clock window sleeps.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	f := &dmIconRouterFixture{db: db, rdb: rdb, mr: mr, configured: configured,
		store: &dmIconRouterStore{objects: make(map[string][]byte)}}
	t.Cleanup(func() {
		if !f.redisClosed {
			assert.NoError(t, rdb.Close())
		}
	})
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = rawnats.DefaultURL
	}
	cfg := &config.Config{
		Environment: "test", Port: "0", JWTSecret: testhelpers.TestJWTSecret,
		NATSUrl: natsURL, AllowedOrigins: []string{"*"},
		MFAEncryptionKey:        "0000000000000000000000000000000000000000000000000000000000000000",
		MFAEncryptionKeyVersion: 1, WebAuthnRPID: "localhost",
		WebAuthnRPOrigins:             []string{"http://localhost:3001"},
		ActivityHistoryClusterEnabled: true, ControlPlaneReplicaCount: 1,
		ControlPlaneReplicaCountExplicit: true,
		ClientMinVersion:                 "0.2.44", RequireClientAttestation: true,
		OIDCIssuer: dmIconRouterDiscovery(t), OIDCAudience: "icon-router-tests",
		OIDCSubjectPrefix: "repo:Concord-Voice/Concord-Voice-Alpha:",
		OIDCSPAWorkflow:   "main-cd.yml", OIDCSPARef: "refs/heads/main",
		OIDCBinaryWorkflow: "build-desktop.yml", OIDCBinaryRef: "refs/heads/main",
	}
	var store media.ObjectStore
	if configured {
		store = f.store
	}
	lifecycleCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	router, hub, nc, ops, permissions, _, closePresence, _, _, _, err := api.NewRouter(
		lifecycleCtx, db, rdb, cfg, nil, logger.NewWithWriter(io.Discard),
		api.RouterDependencies{Store: store, PresenceHistory: presencehistory.NewService(
			db, presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}), true)},
	)
	// Register acquired-resource cleanup before any assertion can abort setup.
	t.Cleanup(func() {
		cancel()
		if closePresence != nil {
			closePresence()
		}
		if hub != nil {
			hub.Shutdown()
		}
		if permissions != nil {
			permissions.Close()
		}
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if ops != nil {
			assert.NoError(t, ops.Stop(shutdownCtx))
		}
		if nc != nil {
			assert.NoError(t, nc.Close())
		}
	})
	require.NoError(t, err, "actual NewRouter setup must succeed")
	require.NotNil(t, router)
	require.NotNil(t, hub)
	require.NotNil(t, permissions)
	// Operations metrics are disabled in this fixture; that constructor
	// handle is legitimately nil and is cleaned up only when acquired.
	require.NotNil(t, closePresence)
	f.router = router
	f.member = f.actor(t, "icon_member", true)
	f.outsider = f.actor(t, "icon_outsider", true)
	f.unverified = f.actor(t, "icon_unverified", false)
	f.removed = f.actor(t, "icon_removed", true)
	ts := &testhelpers.TestServer{DB: db}
	f.conversation = ts.CreateDMConversation(t, f.member.user.ID, f.unverified.user.ID)
	f.secondConversation = ts.CreateDMConversation(t, f.member.user.ID, f.outsider.user.ID)
	_, err = db.Exec(`UPDATE dm_conversations SET is_group = TRUE WHERE id IN ($1, $2)`, f.conversation, f.secondConversation)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, f.conversation, f.removed.user.ID)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, f.conversation, f.removed.user.ID)
	require.NoError(t, err)
	f.serverID = ts.CreateTestServer(t, f.member.user.ID, "icon router public control")
	f.store.objects["dm-icons/"+f.conversation] = []byte("seeded first group icon bytes")
	f.store.objects["dm-icons/"+f.secondConversation] = []byte("seeded second group icon bytes")
	for _, prefix := range []string{"server-icons/", "server-banners/"} {
		f.store.objects[prefix+f.serverID] = []byte("seeded public server image bytes")
	}
	for _, slot := range []string{"avatar", "banner"} {
		key := slot + "s/" + f.member.user.ID
		f.store.objects[key] = []byte("seeded public profile image bytes")
		_, err = db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
			VALUES (gen_random_uuid(), $1, 'photo', 1, 'image/png', $2, $3, $4)`,
			f.member.user.ID, len(f.store.objects[key]), key, slot)
		require.NoError(t, err)
	}
	return f
}

func (f *dmIconRouterFixture) actor(t *testing.T, name string, verified bool) dmIconRouterActor {
	t.Helper()
	ts := &testhelpers.TestServer{DB: f.db}
	var user testhelpers.TestUser
	if verified {
		user = ts.CreateTestUser(t, name)
	} else {
		user = ts.CreateTestUserUnverified(t, name)
	}
	epoch, session, machine, recordedToken := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := f.db.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, epoch, user.ID)
	require.NoError(t, err)
	// Mint through the production access-token generator after persisting its
	// exact epoch. No fake authentication or legacy epoch exemption is used.
	user.AccessToken, err = auth.GenerateAccessToken(user.ID, testhelpers.TestJWTSecret, verified, epoch, session)
	require.NoError(t, err)
	record, err := json.Marshal(attestation.TokenRecord{
		Token: recordedToken, Version: "0.2.44", SpaVersion: "20261013",
		IssuedAt: time.Now(), UserID: attestation.HashUserID(user.ID),
	})
	require.NoError(t, err)
	require.NoError(t, f.rdb.Set(t.Context(), "attestation:"+session+":"+machine, record, 2*time.Hour).Err())
	headers := testhelpers.AuthHeaders(user.AccessToken)
	headers.Set(middleware.ClientVersionHeader, "0.2.44")
	headers.Set("X-Attestation-Token", recordedToken)
	headers.Set("X-Session-ID", session)
	headers.Set("X-Machine-Id", machine)
	return dmIconRouterActor{user: user, headers: headers}
}

func (f *dmIconRouterFixture) get(path string, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if headers != nil {
		req.Header = headers.Clone()
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func (f *dmIconRouterFixture) controls(t *testing.T) {
	t.Helper()
	w := f.get("/api/v1/users/me", f.member.headers)
	require.Equal(t, http.StatusOK, w.Code, "persisted current-epoch full-gate identity control: %s", w.Body.String())
	var profile struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &profile))
	require.Equal(t, f.member.user.ID, profile.User.ID)
	// This route is in the verified protected tier, unlike /users/me.
	w = f.get("/api/v1/dm/conversations", f.member.headers)
	require.Equal(t, http.StatusOK, w.Code, "full protected-chain control: %s", w.Body.String())
	if f.configured {
		before := f.store.gets.Load()
		closed := f.store.closes.Load()
		w = f.get("/api/v1/media/server-icons/"+f.serverID, nil)
		require.Equal(t, http.StatusOK, w.Code, "anonymous seeded public-media control: %s", w.Body.String())
		require.Equal(t, []byte("seeded public server image bytes"), w.Body.Bytes())
		require.Equal(t, "image/png", w.Header().Get("Content-Type"))
		require.Equal(t, dmIconPublicCache, w.Header().Get("Cache-Control"))
		require.Equal(t, before+1, f.store.gets.Load())
		require.Equal(t, closed+1, f.store.closes.Load())
	}
}

func TestNewRouterDMIconConstructorControls(t *testing.T) {
	for _, configured := range []bool{true, false} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) { newDMIconRouterFixture(t, configured).controls(t) })
	}
}

func TestNewRouterDMIconPolicyAndNilStorage(t *testing.T) {
	for _, configured := range []bool{true, false} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			f := newDMIconRouterFixture(t, configured)
			f.controls(t)
			memberStatus, memberBody := http.StatusServiceUnavailable, `{"error":"Object storage unavailable"}`
			if configured {
				memberStatus, memberBody = http.StatusOK, "seeded first group icon bytes"
			}
			cases := []struct {
				name, id string
				actor    dmIconRouterActor
				status   int
				body     string
				gets     int64
			}{
				{"member seeded bytes", f.conversation, f.member, memberStatus, memberBody, 0},
				{"malformed", "not-a-conversation", f.member, http.StatusBadRequest, `{"error":"Invalid conversation ID"}`, 0},
				{"unknown", uuid.NewString(), f.member, http.StatusNotFound, `{"error":"Not found"}`, 0},
				{"nonmember", f.conversation, f.outsider, http.StatusNotFound, `{"error":"Not found"}`, 0},
				{"removed member", f.conversation, f.removed, http.StatusNotFound, `{"error":"Not found"}`, 0},
				{"unverified email", f.conversation, f.unverified, http.StatusForbidden, `{"error":"Email verification required","code":"EMAIL_NOT_VERIFIED"}`, 0},
			}
			if configured {
				cases[0].gets = 1
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					before := f.store.gets.Load()
					closed := f.store.closes.Load()
					w := f.get(dmIconRouterPrefix+tc.id, tc.actor.headers)
					assert.Equal(t, tc.status, w.Code, "body: %s", w.Body.String())
					if tc.status == http.StatusOK {
						assert.Equal(t, tc.body, w.Body.String())
						assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
					} else {
						assert.JSONEq(t, tc.body, w.Body.String())
						if tc.status == http.StatusNotFound {
							assert.Equal(t, `{"error":"Not found"}`, w.Body.String(), "unknown, nonmember and removed responses must be byte-identical")
						}
					}
					assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"))
					assert.Equal(t, before+tc.gets, f.store.gets.Load())
					assert.Equal(t, closed+tc.gets, f.store.closes.Load())
				})
			}
			if configured {
				t.Run("storage failure", func(t *testing.T) {
					f.store.getErr = errors.New("injected icon acquisition failure")
					t.Cleanup(func() { f.store.getErr = nil })
					before, closed := f.store.gets.Load(), f.store.closes.Load()
					w := f.get(dmIconRouterPrefix+f.conversation, f.member.headers)
					assert.Equal(t, http.StatusInternalServerError, w.Code)
					assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
					assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"))
					assert.Equal(t, before+1, f.store.gets.Load())
					assert.Equal(t, closed, f.store.closes.Load())
				})
				t.Run("authorized missing object", func(t *testing.T) {
					delete(f.store.objects, "dm-icons/"+f.conversation)
					before := f.store.gets.Load()
					w := f.get(dmIconRouterPrefix+f.conversation, f.member.headers)
					assert.Equal(t, http.StatusNotFound, w.Code)
					assert.JSONEq(t, `{"error":"Not found"}`, w.Body.String())
					assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"))
					assert.Equal(t, before+1, f.store.gets.Load())
				})
			}
		})
	}
}

func TestNewRouterDMIconCacheBeforeGates(t *testing.T) {
	for _, configured := range []bool{true, false} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			f := newDMIconRouterFixture(t, configured)
			f.controls(t)
			cases := []struct {
				name          string
				actor         dmIconRouterActor
				header, value string
				status        int
				body          string
			}{
				{"missing bearer before version and attestation", f.member, "Authorization", "", 401, `{"error":"Authentication required"}`},
				{"invalid bearer", f.member, "Authorization", "Bearer invalid-access-fixture", 401, `{"error":"Authentication required"}`},
				{"old version before attestation and email", f.unverified, middleware.ClientVersionHeader, "0.2.43", 403, `{"error":"Client version too old","code":"CLIENT_VERSION_TOO_OLD","updateAvailable":true,"requiredMinVersion":"0.2.44","downloadHelpUrl":"https://concordvoice.com/download"}`},
				{"missing attestation before email", f.unverified, "X-Attestation-Token", "", 403, `{"error":"Attestation failed","code":"ATTESTATION_MISSING"}`},
				{"rejected attestation before email", f.unverified, "X-Attestation-Token", uuid.NewString(), 403, `{"error":"Attestation failed","code":"ATTESTATION_INVALID"}`},
				{"global custom header before bearer", f.member, "X-Machine-Id", "not-a-uuid", 400, `{"error":"X-Machine-Id must be a valid UUID"}`},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					headers := tc.actor.headers.Clone()
					headers.Set(tc.header, tc.value)
					if tc.name == "missing bearer before version and attestation" {
						headers.Del(middleware.ClientVersionHeader)
						headers.Del("X-Attestation-Token")
					}
					if tc.name == "old version before attestation and email" {
						headers.Del("X-Attestation-Token")
					}
					if tc.name == "global custom header before bearer" {
						headers.Del("Authorization")
					}
					before := f.store.gets.Load()
					w := f.get(dmIconRouterPrefix+f.conversation, headers)
					assert.Equal(t, tc.status, w.Code, "body: %s", w.Body.String())
					assert.JSONEq(t, tc.body, w.Body.String())
					assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"), "cache boundary must precede the named abort")
					assert.Equal(t, before, f.store.gets.Load())
					assert.False(t, f.mr.Exists("ratelimit:ip:192.0.2.1:GET:/api/v1/media/dm-icons/:conversationId"), "gate refusals must not consume the protected route budget")
				})
				// Clear only this isolated clock's route key between independent
				// cases, so the assertion identifies the gate's own admission.
				require.NoError(t, f.rdb.Del(t.Context(), "ratelimit:ip:192.0.2.1:GET:/api/v1/media/dm-icons/:conversationId").Err())
			}
		})
	}
}

func TestNewRouterDMIconRateLimit(t *testing.T) {
	f := newDMIconRouterFixture(t, true)
	f.controls(t)
	before := f.store.gets.Load()
	closed := f.store.closes.Load()
	for i := 0; i < 60; i++ {
		// Both resource IDs share IP + GET + matched route-template budget.
		id, body := f.conversation, "seeded first group icon bytes"
		if i%2 == 1 {
			id, body = f.secondConversation, "seeded second group icon bytes"
		}
		w := f.get(dmIconRouterPrefix+id, f.member.headers)
		assert.Equal(t, http.StatusOK, w.Code, "authorized call %d: %s", i+1, w.Body.String())
		assert.Equal(t, body, w.Body.String())
		assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"))
		assert.Equal(t, "60", w.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, strconv.Itoa(59-i), w.Header().Get("X-RateLimit-Remaining"))
	}
	assert.Equal(t, before+60, f.store.gets.Load(), "every admitted authorized call acquires exactly one object")
	assert.Equal(t, closed+60, f.store.closes.Load(), "all admitted readers close")
	before = f.store.gets.Load()
	w := f.get(dmIconRouterPrefix+f.secondConversation, f.member.headers)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "60", w.Header().Get("Retry-After"))
	assert.Equal(t, "60", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "0", w.Header().Get("X-RateLimit-Remaining"))
	reset, err := strconv.ParseInt(w.Header().Get("X-RateLimit-Reset"), 10, 64)
	assert.NoError(t, err)
	assert.Greater(t, reset, time.Now().Unix())
	assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"))
	assert.Equal(t, before, f.store.gets.Load(), "429 must not acquire an object")
	f.mr.FastForward(time.Minute + time.Millisecond)
	w = f.get(dmIconRouterPrefix+f.conversation, f.member.headers)
	assert.Equal(t, http.StatusOK, w.Code, "window reset: %s", w.Body.String())
	assert.Equal(t, "seeded first group icon bytes", w.Body.String())
	assert.Equal(t, "59", w.Header().Get("X-RateLimit-Remaining"))
	assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"))
	assert.Equal(t, before+1, f.store.gets.Load())
	assert.Equal(t, closed+61, f.store.closes.Load(), "the reset request closes its reader")
}

func TestNewRouterDMIconRedisOutageBoundary(t *testing.T) {
	for _, configured := range []bool{true, false} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			f := newDMIconRouterFixture(t, configured)
			f.controls(t)
			require.NoError(t, f.rdb.Close())
			f.redisClosed = true
			t.Run("protected auth failure", func(t *testing.T) {
				before := f.store.gets.Load()
				w := f.get(dmIconRouterPrefix+f.conversation, f.member.headers)
				assert.Equal(t, http.StatusServiceUnavailable, w.Code)
				assert.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, w.Body.String())
				assert.Equal(t, dmIconPrivateCache, w.Header().Get("Cache-Control"))
				assert.Equal(t, before, f.store.gets.Load(), "protected Redis failure is fail-closed before the limiter")
			})
			if configured {
				// The existing limiter's fail-open contract is observable where
				// AuthRequired does not first reject the unavailable dependency.
				t.Run("anonymous public limiter fail open", func(t *testing.T) {
					before := f.store.gets.Load()
					w := f.get("/api/v1/media/server-icons/"+f.serverID, nil)
					assert.Equal(t, http.StatusOK, w.Code, "anonymous public limiter fail-open: %s", w.Body.String())
					assert.Equal(t, "seeded public server image bytes", w.Body.String())
					assert.Equal(t, dmIconPublicCache, w.Header().Get("Cache-Control"))
					assert.Equal(t, before+1, f.store.gets.Load())
				})
			}
		})
	}
}

func TestNewRouterDMIconPublicMediaRegression(t *testing.T) {
	for _, configured := range []bool{true, false} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			f := newDMIconRouterFixture(t, configured)
			f.controls(t)
			cases := []struct{ path, body, cache string }{
				{"/api/v1/media/avatars/" + f.member.user.ID, "seeded public profile image bytes", "no-store"},
				{"/api/v1/media/banners/" + f.member.user.ID, "seeded public profile image bytes", "no-store"},
				{"/api/v1/media/server-icons/" + f.serverID, "seeded public server image bytes", dmIconPublicCache},
				{"/api/v1/media/server-banners/" + f.serverID, "seeded public server image bytes", dmIconPublicCache},
			}
			for _, tc := range cases {
				t.Run(tc.path, func(t *testing.T) {
					before := f.store.gets.Load()
					w := f.get(tc.path, nil)
					if configured {
						assert.Equal(t, http.StatusOK, w.Code, "anonymous public-media bytes: %s", w.Body.String())
						assert.Equal(t, tc.body, w.Body.String())
						assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
						assert.Equal(t, tc.cache, w.Header().Get("Cache-Control"))
						assert.Equal(t, before+1, f.store.gets.Load())
					} else {
						assert.Equal(t, http.StatusServiceUnavailable, w.Code)
						assert.JSONEq(t, `{"error":"media storage not configured"}`, w.Body.String())
						assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
						assert.Equal(t, before, f.store.gets.Load())
					}
				})
			}
		})
	}
}
