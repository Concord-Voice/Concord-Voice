//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// TestBuildFeedbackHandler_DevStub verifies that an empty PAT / repo lands the
// handler on the log-only dev-stub path (handlers.go's `nil` GitHubIssueCreator
// branch). Production guard at config.go fatal-exits before we reach this with
// empty values in `ENVIRONMENT=production`, so the dev-stub is a
// dev / self-hosted convenience.
func TestBuildFeedbackHandler_DevStub(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{
			Token: "",
			Repo:  "",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h, "buildFeedbackHandler must return a non-nil handler in dev-stub mode")
}

// TestBuildFeedbackHandler_GitHubWired pins the production wiring: both fields
// set → NewClient is constructed and injected into the handler.
func TestBuildFeedbackHandler_GitHubWired(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture with fake PAT
			Token: "ghp_fake_test_pat", // #nosec G101 -- test fixture
			Repo:  "Concord-Voice/Concord-Voice-Feedback",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h, "buildFeedbackHandler must return a non-nil handler in github-wired mode")
}

// TestBuildFeedbackHandler_OnlyTokenStillDevStub verifies that a partial
// configuration (token set but repo empty) does NOT construct the GitHub
// client — both fields are required. Mirrors the config.go validate() rule
// that fatal-exits production when either is empty.
func TestBuildFeedbackHandler_OnlyTokenStillDevStub(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture
			Token: "ghp_only_token", // #nosec G101 -- test fixture
			Repo:  "",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h)
}

func TestBuildFeedbackHandler_OnlyRepoStillDevStub(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{
			Token: "",
			Repo:  "owner/repo",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h)
}

const feedbackBoundaryBody = `{"type":"bug","title":"owned-feedback-title-canary","description":"owned-feedback-description-canary","diagnostics":{"appVersion":"owned","platform":"owned","logs":"owned-feedback-diagnostic-canary"}}`

type feedbackBoundaryRequestBody struct {
	reader *strings.Reader
	reads  int
}

func (b *feedbackBoundaryRequestBody) Read(data []byte) (int, error) {
	b.reads++
	return b.reader.Read(data)
}

func (b *feedbackBoundaryRequestBody) Close() error { return nil }

// Every GitHub attempt terminates at this owned in-memory transport. No test
// server, socket, DNS lookup or external service participates in this boundary.
type feedbackBoundaryTransport struct {
	calls    int
	requests []*http.Request
	bodies   []string
}

func (transport *feedbackBoundaryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls++
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}
	transport.requests = append(transport.requests, request)
	transport.bodies = append(transport.bodies, string(body))
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"number":7,"html_url":"https://example.invalid/owned-feedback/7"}`)),
		Request:    request,
	}, nil
}

func feedbackBoundarySubmit(t *testing.T, cfg *config.Config, payload, userID string) (*httptest.ResponseRecorder, *feedbackBoundaryRequestBody, *feedbackBoundaryTransport, string) {
	t.Helper()
	transport := &feedbackBoundaryTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	var logs bytes.Buffer
	handler := buildFeedbackHandler(cfg, logger.NewWithWriter(&logs))
	require.NotNil(t, handler)
	body := &feedbackBoundaryRequestBody{reader: strings.NewReader(payload)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	if userID != "" {
		context.Set("user_id", userID)
	}
	handler.Submit(context)
	return recorder, body, transport, logs.String()
}

func TestBuildFeedbackHandler_SelfHostDisabled(t *testing.T) {
	type boundaryCase struct {
		name, instance, repo, payload, userID string
		status                                int
		errorCode                             string
	}
	cases := make([]boundaryCase, 0, 12)
	for _, instance := range []string{"self-hosted", "SELF-HOSTED", " Self-Hosted "} {
		for _, repo := range []string{"selfhost/disabled", "SELFHOST/DISABLED", "SelfHost/Disabled"} {
			cases = append(cases, boundaryCase{
				name: instance + "/" + repo, instance: instance, repo: repo,
				payload: feedbackBoundaryBody, userID: "owned-feedback-reporter",
				status: http.StatusServiceUnavailable, errorCode: "feedback_disabled",
			})
		}
	}
	cases = append(cases,
		boundaryCase{name: "malformed body remains unread", instance: "self-hosted", repo: "selfhost/disabled",
			payload: "owned-feedback-malformed-canary", userID: "owned-feedback-reporter",
			status: http.StatusServiceUnavailable, errorCode: "feedback_disabled"},
		boundaryCase{name: "oversize body remains unread", instance: "self-hosted", repo: "selfhost/disabled",
			payload: strings.Repeat("owned-feedback-oversize-canary", 6000), userID: "owned-feedback-reporter",
			status: http.StatusServiceUnavailable, errorCode: "feedback_disabled"},
		boundaryCase{name: "authentication retained", instance: "self-hosted", repo: "selfhost/disabled",
			payload: feedbackBoundaryBody, status: http.StatusUnauthorized, errorCode: "unauthorized"},
	)
	for _, cell := range cases {
		t.Run(cell.name, func(t *testing.T) {
			cfg := &config.Config{
				Environment: "production", InstanceType: cell.instance,
				JWTSecret: "owned-feedback-correlation-input", // #nosec G101 -- synthetic correlation input
				GitHubFeedback: config.GitHubFeedbackConfig{
					Token: "disabled-owned-feedback-token", // #nosec G101 -- inert installer-shaped placeholder
					Repo:  cell.repo,
				},
			}
			response, body, transport, logs := feedbackBoundarySubmit(t, cfg, cell.payload, cell.userID)
			assert.Equal(t, cell.status, response.Code)
			assert.JSONEq(t, `{"error":"`+cell.errorCode+`"}`, response.Body.String())
			assert.Zero(t, body.reads, "disabled mode must refuse before decoding any body bytes")
			assert.Zero(t, transport.calls, "disabled feedback must not construct an outbound submission")
			assert.Empty(t, transport.bodies)
			assert.Empty(t, logs, "disabled/authentication refusal must not log a report or diagnostics")
			for _, canary := range []string{"owned-feedback-title-canary", "owned-feedback-description-canary",
				"owned-feedback-diagnostic-canary", "owned-feedback-reporter", "owned-feedback-malformed-canary",
				"owned-feedback-oversize-canary", "disabled-owned-feedback-token"} {
				assert.NotContains(t, logs+response.Body.String(), canary)
			}
		})
	}
}

func TestBuildFeedbackHandler_FeedbackModeControls(t *testing.T) {
	cases := []struct {
		name, environment, instance, repo string
	}{
		{name: "enabled self-host", environment: "production", instance: "self-hosted", repo: "owned/feedback"},
		{name: "near sentinel self-host", environment: "production", instance: "self-hosted", repo: "selfhost/disabled-extra"},
		{name: "managed same slug", environment: "production", instance: "saas", repo: "selfhost/disabled"},
		{name: "development same slug", environment: "development", instance: "saas", repo: "SELFHOST/DISABLED"},
	}
	for _, cell := range cases {
		t.Run(cell.name, func(t *testing.T) {
			cfg := &config.Config{
				Environment: cell.environment, InstanceType: cell.instance,
				JWTSecret: "owned-feedback-correlation-input", // #nosec G101 -- synthetic correlation input
				GitHubFeedback: config.GitHubFeedbackConfig{
					Token: "owned-enabled-feedback-token", // #nosec G101 -- inert explicit enabled fixture
					Repo:  cell.repo,
				},
			}
			response, body, transport, _ := feedbackBoundarySubmit(t, cfg, feedbackBoundaryBody, "owned-feedback-reporter")
			require.Equal(t, http.StatusOK, response.Code)
			assert.JSONEq(t, `{"issueUrl":"https://example.invalid/owned-feedback/7","dev":false}`, response.Body.String())
			require.Equal(t, 1, transport.calls)
			require.Len(t, transport.requests, 1)
			require.Len(t, transport.bodies, 1)
			assert.Positive(t, body.reads)
			assert.Equal(t, http.MethodPost, transport.requests[0].Method)
			assert.Equal(t, "https", transport.requests[0].URL.Scheme)
			assert.Equal(t, "api.github.com", transport.requests[0].URL.Host)
			assert.Equal(t, "/repos/"+cell.repo+"/issues", transport.requests[0].URL.Path)
			assert.Contains(t, transport.bodies[0], "owned-feedback-title-canary")
			assert.Contains(t, transport.bodies[0], "owned-feedback-description-canary")
			assert.Contains(t, transport.bodies[0], "owned-feedback-diagnostic-canary")
		})
	}
	t.Run("empty development config retains stub", func(t *testing.T) {
		cfg := &config.Config{Environment: "development", InstanceType: "saas"}
		response, body, transport, _ := feedbackBoundarySubmit(t, cfg, feedbackBoundaryBody, "owned-feedback-reporter")
		require.Equal(t, http.StatusOK, response.Code)
		assert.JSONEq(t, `{"dev":true}`, response.Body.String())
		assert.Positive(t, body.reads)
		assert.Zero(t, transport.calls)
	})
}

const (
	feedbackScreenshotUploadPath = "/api/v1/media/upload/feedback-screenshot"
	feedbackScreenshotPublicPath = "/api/v1/media/feedback-screenshots/22222222-2222-4222-8222-222222222222"
	feedbackScreenshotObjectKey  = "feedback-screenshots/22222222-2222-4222-8222-222222222222"
	feedbackScreenshotUserID     = "33333333-3333-4333-8333-333333333333"
)

// The production router and media handler run unchanged. Only their external
// persistence boundaries are owned by the fixture: no developer database,
// object-storage service, GitHub submission or NATS connection is involved.
type feedbackScreenshotStore struct {
	media.ObjectStore   // Unused multipart APIs deliberately have no implementation.
	mu                  sync.Mutex
	objects             map[string][]byte
	puts, gets, deletes int
}

func (s *feedbackScreenshotStore) PutObject(_ context.Context, key string, reader io.Reader, _ int64, contentType string) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.objects[key] = data
	if contentType != "image/jpeg" {
		return errors.New("feedback screenshot must be processed as JPEG")
	}
	return nil
}

func (s *feedbackScreenshotStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	data, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), "image/jpeg", nil
}

func (s *feedbackScreenshotStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	delete(s.objects, key)
	return nil
}

func (s *feedbackScreenshotStore) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts, s.gets, s.deletes
}

type feedbackScreenshotDB struct {
	inserts atomic.Int64
	writes  atomic.Int64
}
type feedbackScreenshotConnector struct{ state *feedbackScreenshotDB }
type feedbackScreenshotDriver struct{}
type feedbackScreenshotConn struct{ state *feedbackScreenshotDB }
type feedbackScreenshotRows struct {
	value driver.Value
	done  bool
}

func (c feedbackScreenshotConnector) Connect(context.Context) (driver.Conn, error) {
	return &feedbackScreenshotConn{state: c.state}, nil
}
func (feedbackScreenshotConnector) Driver() driver.Driver { return feedbackScreenshotDriver{} }
func (feedbackScreenshotDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use fixture connector")
}
func (*feedbackScreenshotConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*feedbackScreenshotConn) Close() error                        { return nil }
func (*feedbackScreenshotConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c *feedbackScreenshotConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.state.writes.Add(1)
	return nil, errors.New("unexpected persistence write")
}
func (c *feedbackScreenshotConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(strings.TrimSpace(query), "INSERT INTO media_files") && len(args) == 7 {
		c.state.inserts.Add(1)
		return &feedbackScreenshotRows{value: args[0].Value}, nil
	}
	return nil, errors.New("query outside screenshot metadata boundary")
}
func (*feedbackScreenshotRows) Columns() []string { return []string{"id"} }
func (*feedbackScreenshotRows) Close() error      { return nil }
func (r *feedbackScreenshotRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

type feedbackScreenshotRouterFixture struct {
	router  http.Handler
	rdb     *redis.Client
	store   *feedbackScreenshotStore
	dbState *feedbackScreenshotDB
	secret  string
}

func newFeedbackScreenshotRouterFixture(t *testing.T, instance, repo string) *feedbackScreenshotRouterFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t) // Private keyspace and frozen limiter clock; no shared Redis flush.
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	state := &feedbackScreenshotDB{}
	db := sql.OpenDB(feedbackScreenshotConnector{state: state})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := &feedbackScreenshotStore{objects: map[string][]byte{feedbackScreenshotObjectKey: []byte("owned-previous-screenshot")}}
	cfg := &config.Config{
		Environment: "test", InstanceType: instance,
		JWTSecret:        "owned-feedback-screenshot-signing-input", // #nosec G101 -- synthetic fixture signing input
		NATSUrl:          "nats://127.0.0.1:1",                      // Refused loopback port; no NATS daemon or DNS.
		AllowedOrigins:   []string{"*"},
		MFAEncryptionKey: strings.Repeat("0", 64), MFAEncryptionKeyVersion: 1,
		WebAuthnRPID: "localhost", WebAuthnRPOrigins: []string{"http://localhost:3001"},
		GitHubFeedback: config.GitHubFeedbackConfig{Repo: repo},
	}
	router, hub, natsClient, opsRuntime, enforcer, _, closePresence, _, _, _, err := NewRouter(
		t.Context(), db, rdb, cfg, nil, logger.NewWithWriter(io.Discard),
		RouterDependencies{Store: store, PresenceHistory: presencehistory.NewService(db,
			presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: instance}), true)},
	)
	require.NoError(t, err)
	require.NotNil(t, natsClient, "the router's erasure publisher boot guard requires a handle")
	require.False(t, natsClient.IsConnected(), "no NATS service participates in this fixture")
	t.Cleanup(func() {
		closePresence()
		hub.Shutdown()
		enforcer.Close()
		if natsClient != nil {
			// Drain closes a reconnecting connection and returns this documented
			// sentinel. There is no connected service or buffered publish to drain.
			if closeErr := natsClient.Close(); closeErr != nil &&
				!errors.Is(closeErr, natsclient.ErrDrainSkippedReconnecting) &&
				!errors.Is(closeErr, natsclient.ErrDrainNothingToDo) {
				require.NoError(t, closeErr)
			}
		}
		require.NoError(t, opsRuntime.Stop(context.Background()))
	})
	require.NoError(t, rdb.Set(t.Context(), credepoch.Key(feedbackScreenshotUserID), "none", time.Minute).Err())
	return &feedbackScreenshotRouterFixture{router: router, rdb: rdb, store: store, dbState: state, secret: cfg.JWTSecret}
}

func (f *feedbackScreenshotRouterFixture) token(t *testing.T, verified bool) string {
	t.Helper()
	value, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": middleware.AccessTokenIssuer, "user_id": feedbackScreenshotUserID,
		"email_verified": verified, "exp": time.Now().Add(time.Minute).Unix(), "jti": "owned-feedback-screenshot-token",
	}).SignedString([]byte(f.secret))
	require.NoError(t, err)
	return value
}

func feedbackScreenshotMultipart(t *testing.T) (string, string) {
	t.Helper()
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, err := writer.CreateFormFile("file", "owned-screenshot.png")
	require.NoError(t, err)
	_, err = part.Write(imageBytes.Bytes())
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return payload.String(), writer.FormDataContentType()
}

func (f *feedbackScreenshotRouterFixture) request(method, path, payload, contentType, token string) (*httptest.ResponseRecorder, *feedbackBoundaryRequestBody) {
	body := &feedbackBoundaryRequestBody{reader: strings.NewReader(payload)}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Content-Type", contentType)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response, body
}

func TestNewRouter_FeedbackScreenshotDisabledBoundary(t *testing.T) {
	for _, cell := range []struct{ name, instance, repo string }{
		{"self-hosted", "self-hosted", "selfhost/disabled"},
		{"normalized sentinel", "  SELF-HOSTED  ", "  SelfHost/Disabled  "},
	} {
		t.Run(cell.name, func(t *testing.T) {
			f := newFeedbackScreenshotRouterFixture(t, cell.instance, cell.repo)
			payload, contentType := feedbackScreenshotMultipart(t)
			response, body := f.request(http.MethodPost, feedbackScreenshotUploadPath, payload, contentType, f.token(t, true))
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.JSONEq(t, `{"error":"feedback_disabled"}`, response.Body.String())
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Zero(t, body.reads, "disabled screenshots must refuse before multipart parsing")
			puts, gets, deletes := f.store.counts()
			assert.Zero(t, puts, "no plaintext screenshot may reach object storage")
			assert.Zero(t, gets)
			assert.Zero(t, deletes)
			assert.Zero(t, f.dbState.inserts.Load(), "disabled upload must create no media metadata")
			assert.Zero(t, f.dbState.writes.Load())

			// Previously stored screenshot bytes must also stay behind the
			// disabled boundary, even on the existing public proxy route.
			proxy, _ := f.request(http.MethodGet, feedbackScreenshotPublicPath, "", "", "")
			assert.Equal(t, http.StatusServiceUnavailable, proxy.Code)
			assert.JSONEq(t, `{"error":"feedback_disabled"}`, proxy.Body.String())
			assert.Equal(t, "no-store", proxy.Header().Get("Cache-Control"))
			_, gets, _ = f.store.counts()
			assert.Zero(t, gets, "disabled public proxy must not read an existing screenshot")
			assert.NotContains(t, proxy.Body.String(), "owned-previous-screenshot")
		})
	}
}

func TestNewRouter_FeedbackScreenshotModeControls(t *testing.T) {
	for _, cell := range []struct{ name, instance, repo string }{
		{"enabled self-host", "self-hosted", "owned/feedback"},
		{"near sentinel self-host", "self-hosted", "selfhost/disabled-extra"},
		{"managed same slug", "saas", "selfhost/disabled"},
		{"development stub", "saas", ""},
	} {
		t.Run(cell.name, func(t *testing.T) {
			f := newFeedbackScreenshotRouterFixture(t, cell.instance, cell.repo)
			payload, contentType := feedbackScreenshotMultipart(t)
			response, body := f.request(http.MethodPost, feedbackScreenshotUploadPath, payload, contentType, f.token(t, true))
			require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
			assert.Positive(t, body.reads, "the same real handler must consume enabled multipart uploads")
			puts, _, deletes := f.store.counts()
			assert.Equal(t, 1, puts)
			assert.Zero(t, deletes)
			assert.EqualValues(t, 1, f.dbState.inserts.Load(), "genuine media handler must persist metadata")
			var uploaded struct {
				StorageKey string `json:"storage_key"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &uploaded))
			assert.True(t, strings.HasPrefix(uploaded.StorageKey, "feedback-screenshots/"))
			f.store.mu.Lock()
			stored := append([]byte(nil), f.store.objects[uploaded.StorageKey]...)
			f.store.mu.Unlock()
			_, format, err := image.Decode(bytes.NewReader(stored))
			require.NoError(t, err, "assert against the bytes actually sent to storage")
			assert.Equal(t, "jpeg", format)
			proxy, _ := f.request(http.MethodGet, feedbackScreenshotPublicPath, "", "", "")
			require.Equal(t, http.StatusOK, proxy.Code)
			assert.Equal(t, "owned-previous-screenshot", proxy.Body.String())
			_, gets, _ := f.store.counts()
			assert.Equal(t, 1, gets)
		})
	}
}

func TestNewRouter_FeedbackScreenshotExistingGuards(t *testing.T) {
	for _, repo := range []string{"selfhost/disabled", "owned/feedback"} {
		t.Run(repo, func(t *testing.T) {
			f := newFeedbackScreenshotRouterFixture(t, "self-hosted", repo)
			payload, contentType := feedbackScreenshotMultipart(t)
			for _, cell := range []struct {
				name, token string
				status      int
			}{
				{"unauthenticated", "", http.StatusUnauthorized},
				{"invalid token", "owned-invalid-token", http.StatusUnauthorized},
				{"email unverified", f.token(t, false), http.StatusForbidden},
			} {
				t.Run(cell.name, func(t *testing.T) {
					response, body := f.request(http.MethodPost, feedbackScreenshotUploadPath, payload, contentType, cell.token)
					assert.Equal(t, cell.status, response.Code)
					assert.Zero(t, body.reads)
				})
			}
			for _, cell := range []struct {
				method, path, key string
				limit             int
			}{
				{http.MethodPost, feedbackScreenshotUploadPath, "ratelimit:user:" + feedbackScreenshotUserID + ":POST:" + feedbackScreenshotUploadPath, 12},
				{http.MethodGet, feedbackScreenshotPublicPath, "ratelimit:ip:192.0.2.1:GET:/api/v1/media/feedback-screenshots/:id", 120},
			} {
				require.NoError(t, f.rdb.Set(t.Context(), cell.key, cell.limit, time.Minute).Err())
				response, body := f.request(cell.method, cell.path, payload, contentType, f.token(t, true))
				assert.Equal(t, http.StatusTooManyRequests, response.Code, "existing limiter must run before mode refusal")
				assert.Zero(t, body.reads)
			}
			// Ordinary media is not feedback: it must still reach its existing
			// multipart parser while feedback is disabled (an empty part yields
			// the actual media handler's 400, not a route-local test handler).
			ordinary, body := f.request(http.MethodPost, "/api/v1/media/upload/avatar", "owned-malformed-upload", "multipart/form-data; boundary=owned", f.token(t, true))
			assert.Equal(t, http.StatusBadRequest, ordinary.Code)
			assert.Contains(t, ordinary.Body.String(), "Missing file in request")
			assert.Positive(t, body.reads)
			puts, gets, deletes := f.store.counts()
			assert.Zero(t, puts)
			assert.Zero(t, gets)
			assert.Zero(t, deletes)
			assert.Zero(t, f.dbState.inserts.Load())
		})
	}
}
