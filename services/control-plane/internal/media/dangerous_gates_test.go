package media_test

// #3454 T3: the dangerous-action gate on the server icon and banner uploads
// (POST /api/v1/media/upload/server-icon and .../server-banner), driven over
// HTTP through the real router and the real MFA verifier, with an in-memory
// object store the tests can pause, fail and inspect. Each test names the
// mutant that kills it.
//
// This file is an external test package: the internal media tests cannot
// import internal/testhelpers (it imports internal/api, which imports media).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// imgBackupCode is each enrolled persona's single backup code, at most 20
	// characters so the real verifier checks it as a TOTP/backup code and not as
	// a WebAuthn inline token.
	imgBackupCode = "MFAENF01"
	// imgWrongCode is never a valid code for anyone.
	imgWrongCode = "O2CODE7Q"

	imgForbiddenBody   = `{"error":"Insufficient permissions"}`
	imgMFARequiredBody = `{"error":"MFA verification required","mfa_required":true,"methods":["totp"]}`
	imgInvalidCodeBody = `{"error":"Invalid MFA code"}`
	imgStoreFailBody   = `{"error":"Failed to store image"}`
	imgRecordFailBody  = `{"error":"Failed to record media metadata"}`
	imgBadRequestBody  = `{"error":"Invalid request body"}`

	// imgLockProofBound bounds every wait on another goroutine, so a hang fails
	// with a named cause instead of the suite's timeout.
	imgLockProofBound = 10 * time.Second
)

// The submissions the per-route table sends.
const (
	imgNoCode        = "no code"
	imgWrongSubmit   = "wrong code"
	imgValidCode     = "valid code"
	imgOtherWebAuthn = "WebAuthn token minted for another purpose"
	imgOwnWebAuthn   = "WebAuthn token minted for this purpose"
)

// The answers a gated route can give, classified.
const (
	imgOK          = "ok"
	imgMFARequired = "mfa required"
	imgInvalid     = "invalid code"
	imgEnrollment  = "enrollment required"
)

var errImgStoreUnused = errors.New("imgGateStore: operation not used by these tests")

// imgGateStore is an in-memory media.ObjectStore. It records every PutObject
// and DeleteObject, can fail a put, and can run a hook inside it, which is how
// a test holds the upload's gate transaction open.
type imgGateStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
	putErr  error
	atPut   func(ctx context.Context, key string) error
}

var _ media.ObjectStore = (*imgGateStore)(nil)

func newImgGateStore() *imgGateStore { return &imgGateStore{objects: map[string][]byte{}} }

func (s *imgGateStore) PutObject(ctx context.Context, key string, r io.Reader, _ int64, _ string) error {
	s.mu.Lock()
	hook, putErr := s.atPut, s.putErr
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, key); err != nil {
			return err
		}
	}
	if putErr != nil {
		return putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	return nil
}

func (s *imgGateStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, key)
	delete(s.objects, key)
	return nil
}

func (s *imgGateStore) GetObject(context.Context, string) (io.ReadCloser, string, error) {
	return nil, "", storage.ErrObjectNotFound
}

func (s *imgGateStore) PresignedGetURL(context.Context, string, time.Duration) (string, error) {
	return "", errImgStoreUnused
}

func (s *imgGateStore) NewMultipartUpload(context.Context, string, string) (string, error) {
	return "", errImgStoreUnused
}

func (s *imgGateStore) PutObjectPart(context.Context, string, string, int, io.Reader, int64) (storage.ObjectPartInfo, error) {
	return storage.ObjectPartInfo{}, errImgStoreUnused
}

func (s *imgGateStore) ListObjectParts(context.Context, string, string) ([]storage.ObjectPartInfo, error) {
	return nil, errImgStoreUnused
}

func (s *imgGateStore) CompleteMultipartUpload(context.Context, string, string, []storage.ObjectPartInfo) error {
	return errImgStoreUnused
}

func (s *imgGateStore) AbortMultipartUpload(context.Context, string, string) error {
	return errImgStoreUnused
}

func (s *imgGateStore) ListIncompleteUploads(context.Context, time.Time) ([]storage.IncompleteUpload, error) {
	return nil, nil
}

func (s *imgGateStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

func (s *imgGateStore) object(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.objects[key]...)
}

func (s *imgGateStore) seed(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
}

func (s *imgGateStore) deletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

func (s *imgGateStore) setPutErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putErr = err
}

func (s *imgGateStore) setAtPut(hook func(ctx context.Context, key string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.atPut = hook
}

// imgGateEnv is the real router, built with imgGateStore as its object store
// (testhelpers.SetupTestServer builds none, which leaves the upload routes
// unregistered).
type imgGateEnv struct {
	ts    *testhelpers.TestServer
	store *imgGateStore
}

func setupImgGateEnv(t *testing.T) *imgGateEnv {
	t.Helper()
	t.Setenv("CONCORD_ENV", "test")
	db, dbCleanup := testhelpers.SetupTestDB(t)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	cfg := &config.Config{
		Environment:                      "test",
		Port:                             "0",
		JWTSecret:                        testhelpers.TestJWTSecret,
		AllowedOrigins:                   []string{"*"},
		InstanceType:                     os.Getenv("INSTANCE_TYPE"),
		MFAEncryptionKey:                 strings.Repeat("00", 32),
		MFAEncryptionKeyVersion:          1,
		WebAuthnRPID:                     "localhost",
		WebAuthnRPOrigins:                []string{"http://localhost:3001"},
		ActivityHistoryClusterEnabled:    true,
		ControlPlaneReplicaCount:         1,
		ControlPlaneReplicaCountExplicit: true,
		NATSUrl:                          os.Getenv("NATS_URL"),
	}
	store := newImgGateStore()
	history := presencehistory.NewService(db,
		presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}), true)
	t.Cleanup(func() {
		redisCleanup()
		dbCleanup()
	})
	router, hub, natsClient, opsRuntime, enforcer, _, closePresence, _, _, _, err := api.NewRouter(
		t.Context(), db, rdb, cfg, nil, logger.NewWithWriter(io.Discard),
		api.RouterDependencies{Store: store, PresenceHistory: history},
	)
	require.NoError(t, err)
	if natsClient != nil {
		t.Cleanup(func() { _ = natsClient.Close() })
	}
	t.Cleanup(enforcer.Close)
	t.Cleanup(func() { require.NoError(t, opsRuntime.Stop(context.Background())) })
	t.Cleanup(func() { hub.Shutdown() })
	t.Cleanup(closePresence)
	return &imgGateEnv{
		ts:    &testhelpers.TestServer{Router: router, Hub: hub, DB: db, Redis: rdb, PresenceHistory: history},
		store: store,
	}
}

// imgRoute is one gated upload route.
type imgRoute struct {
	name string
	path string
	// purpose is the route's own step-up purpose; otherPurpose is its sibling's,
	// the most plausible token to replay.
	purpose, otherPurpose stepup.Purpose
	keyPrefix             string
}

func (r imgRoute) key(serverID string) string { return r.keyPrefix + serverID }

func imgRoutes() []imgRoute {
	return []imgRoute{
		{
			name: "server icon", path: "/api/v1/media/upload/server-icon",
			purpose: stepup.PurposeServerIconUpload, otherPurpose: stepup.PurposeServerBannerUpload,
			keyPrefix: "server-icons/",
		},
		{
			name: "server banner", path: "/api/v1/media/upload/server-banner",
			purpose: stepup.PurposeServerBannerUpload, otherPurpose: stepup.PurposeServerIconUpload,
			keyPrefix: "server-banners/",
		},
	}
}

func imgPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 64))))
	return buf.Bytes()
}

func imgMultipart(t *testing.T, fields map[string]string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", "image.png")
	require.NoError(t, err)
	_, err = part.Write(file)
	require.NoError(t, err)
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	require.NoError(t, w.Close())
	return &buf, w.FormDataContentType()
}

// uploadRequest builds one multipart upload for route, as the holder of token.
// A non-empty code travels as the multipart field mfa_code. It is built on the
// test's own goroutine, so a request that runs on another one never calls
// require.
func uploadRequest(t *testing.T, r imgRoute, token, serverID, code string) *http.Request {
	t.Helper()
	fields := map[string]string{"server_id": serverID}
	if code != "" {
		fields["mfa_code"] = code
	}
	body, contentType := imgMultipart(t, fields, imgPNG(t))
	req := httptest.NewRequest(http.MethodPost, r.path, body)
	req.Header = testhelpers.AuthHeaders(token)
	req.Header.Set("Content-Type", contentType)
	return req
}

func (e *imgGateEnv) serve(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.ts.Router.ServeHTTP(w, req)
	return w
}

func (e *imgGateEnv) upload(t *testing.T, r imgRoute, token, serverID, code string) *httptest.ResponseRecorder {
	t.Helper()
	return e.serve(uploadRequest(t, r, token, serverID, code))
}

// imgGateFixture is a fresh owner and server per case, so no case inherits a
// rate-limit bucket, a spent code or a stored object from another.
type imgGateFixture struct {
	owner    testhelpers.TestUser
	serverID string
}

func newImgGateFixture(t *testing.T, env *imgGateEnv, enforcing, enrolled bool) imgGateFixture {
	t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	f := imgGateFixture{owner: env.ts.CreateTestUser(t, "i"+tag)}
	f.serverID = env.ts.CreateTestServer(t, f.owner.ID, "Img "+tag)
	if enrolled {
		enrollImgTOTP(t, env, f.owner.ID)
	}
	setImgEnforcement(t, env, f.serverID, enforcing)
	return f
}

func setImgEnforcement(t *testing.T, env *imgGateEnv, serverID string, on bool) {
	t.Helper()
	_, err := env.ts.DB.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = $2 WHERE id = $1`, serverID, on)
	require.NoError(t, err)
}

// enrollImgTOTP gives userID a confirmed TOTP factor sealed under the router's
// keyring, plus one unused backup code (imgBackupCode).
func enrollImgTOTP(t *testing.T, env *imgGateEnv, userID string) {
	t.Helper()
	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	key, err := mfa.GenerateSecret(userID + "@image-gate.test")
	require.NoError(t, err)
	enc, nonce, version, err := ring.Seal([]byte(key.Secret()))
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(imgBackupCode))
	_, err = env.ts.DB.Exec(`INSERT INTO user_mfa_totp
		(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
		VALUES ($1, $2, $3, $4, TRUE, TRUE, $5, $6)`,
		userID, enc, nonce, version, pq.Array([]string{hex.EncodeToString(digest[:])}), pq.Array([]bool{false}))
	require.NoError(t, err)
}

func imgBackupCodeSpent(t *testing.T, env *imgGateEnv, userID string) bool {
	t.Helper()
	var used []bool
	require.NoError(t, env.ts.DB.QueryRow(
		`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(pq.Array(&used)))
	require.Len(t, used, 1)
	return used[0]
}

// imgBudget reads the attempt counter the gate charges; 0 when absent.
func imgBudget(t *testing.T, env *imgGateEnv, userID string) int {
	t.Helper()
	n, err := env.ts.Redis.Get(context.Background(), stepup.DangerousActionBudget(nil).Key(userID)).Int()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	return n
}

func imgGraceKeys(t *testing.T, env *imgGateEnv, userID string) []string {
	t.Helper()
	keys, err := env.ts.Redis.Keys(context.Background(), "stepup:grace:"+userID+":*").Result()
	require.NoError(t, err)
	return keys
}

func mintImgToken(t *testing.T, env *imgGateEnv, userID string, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), env.ts.DB, userID, stepup.FactorWebAuthn, purpose, "")
	require.Nil(t, e)
	return token
}

func imgUnspentTokens(t *testing.T, env *imgGateEnv, userID string) int {
	t.Helper()
	return imgCount(t, env.ts.DB, `SELECT count(*) FROM step_up_tokens WHERE user_id = $1`, userID)
}

func imgCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

// imgMediaRows counts the media_files rows for a storage key.
func imgMediaRows(t *testing.T, env *imgGateEnv, key string) int {
	t.Helper()
	return imgCount(t, env.ts.DB, `SELECT count(*) FROM media_files WHERE storage_key = $1`, key)
}

// requireNothingStored is "a refusal writes nothing": no object, no
// media_files row, and no delete call either.
func requireNothingStored(t *testing.T, env *imgGateEnv, r imgRoute, serverID, msg string) {
	t.Helper()
	assert.False(t, env.store.has(r.key(serverID)), "%s: no object", msg)
	assert.Zero(t, imgMediaRows(t, env, r.key(serverID)), "%s: no media_files row", msg)
}

func classifyImgGate(t *testing.T, r imgRoute, serverID string, w *httptest.ResponseRecorder, msg string) string {
	t.Helper()
	body := w.Body.String()
	switch {
	case w.Code == http.StatusCreated:
		var resp map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), msg)
		require.Equal(t, r.key(serverID), resp["storage_key"], msg)
		return imgOK
	case w.Code == http.StatusForbidden && imgSameJSON(t, body, imgMFARequiredBody):
		return imgMFARequired
	case w.Code == http.StatusForbidden && imgSameJSON(t, body, imgInvalidCodeBody):
		return imgInvalid
	case w.Code == http.StatusForbidden && strings.Contains(body, `"mfa_enrollment_required":true`):
		requireImgEnrollment(t, w, msg)
		return imgEnrollment
	}
	return fmt.Sprintf("unexpected %d %s", w.Code, body)
}

func imgSameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil {
		return false
	}
	require.NoError(t, json.Unmarshal([]byte(want), &w))
	return assert.ObjectsAreEqual(w, g)
}

func requireImgEnrollment(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, w.Code, msg)
	require.JSONEq(t, fmt.Sprintf(`{"error":%q,"mfa_enrollment_required":true}`, stepup.ErrMsgMFAEnrollmentRequired),
		w.Body.String(), msg)
}

func wantImgGate(enforcing, enrolled bool, submission string) string {
	switch {
	case !enforcing:
		return imgOK
	case !enrolled:
		return imgEnrollment
	case submission == imgNoCode:
		return imgMFARequired
	case submission == imgValidCode, submission == imgOwnWebAuthn:
		return imgOK
	default:
		return imgInvalid
	}
}

// The per-route table: setting x enrollment x submission, for both uploads. A
// refusal writes nothing (no object, no media_files row); a server that does
// not enforce verifies nothing (the backup code and a token minted for this
// very purpose stay unspent); a WebAuthn token minted for the sibling upload's
// purpose is refused and stays unspent.
// Kills: either route's Require call removed (enforcing + enrolled + no code
// answers 201); Require run with the wrong purpose (the own-purpose token is
// refused, or the other-purpose token accepted); the PutObject moved before
// Require (a refusal leaves an object).
func TestServerImageGate_PerRouteTable(t *testing.T) {
	env := setupImgGateEnv(t)
	submissions := []string{imgNoCode, imgWrongSubmit, imgValidCode, imgOtherWebAuthn, imgOwnWebAuthn}
	for _, r := range imgRoutes() {
		for _, enforcing := range []bool{false, true} {
			for _, enrolled := range []bool{false, true} {
				for _, submission := range submissions {
					name := fmt.Sprintf("%s/enforcing=%t/enrolled=%t/%s", r.name, enforcing, enrolled, submission)
					t.Run(name, func(t *testing.T) {
						runImgGateCase(t, env, r, enforcing, enrolled, submission)
					})
				}
			}
		}
	}
}

func runImgGateCase(t *testing.T, env *imgGateEnv, r imgRoute, enforcing, enrolled bool, submission string) {
	f := newImgGateFixture(t, env, enforcing, enrolled)
	code := ""
	switch submission {
	case imgWrongSubmit:
		code = imgWrongCode
	case imgValidCode:
		code = imgBackupCode
	case imgOtherWebAuthn:
		code = mintImgToken(t, env, f.owner.ID, r.otherPurpose)
	case imgOwnWebAuthn:
		code = mintImgToken(t, env, f.owner.ID, r.purpose)
	}
	tokensBefore := imgUnspentTokens(t, env, f.owner.ID)

	w := env.upload(t, r, f.owner.AccessToken, f.serverID, code)
	want := wantImgGate(enforcing, enrolled, submission)
	require.Equal(t, want, classifyImgGate(t, r, f.serverID, w, submission), w.Body.String())
	if want == imgOK {
		assert.True(t, env.store.has(r.key(f.serverID)), "the object lands exactly when the gate admits")
		assert.Equal(t, 1, imgMediaRows(t, env, r.key(f.serverID)))
	} else {
		requireNothingStored(t, env, r, f.serverID, submission)
	}

	verified := enforcing && enrolled
	if enrolled {
		assert.Equal(t, verified && submission == imgValidCode, imgBackupCodeSpent(t, env, f.owner.ID),
			"the backup code is spent only by a verified, committed confirmation")
	}
	wantTokens := tokensBefore
	if verified && submission == imgOwnWebAuthn {
		wantTokens--
	}
	assert.Equal(t, wantTokens, imgUnspentTokens(t, env, f.owner.ID),
		"a token is spent only under its own purpose on an enforcing server")
}

// With the setting off, a request that carries no code is answered exactly as
// before #3454: the same status and body shape, no budget charge, and no grace
// written.
// Kills: Charge run without a code; a grace granted on an unverified success.
func TestServerImageGate_SettingOffAndNoCodeIsUnchanged(t *testing.T) {
	env := setupImgGateEnv(t)
	for _, r := range imgRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newImgGateFixture(t, env, false, true)
			w := env.upload(t, r, f.owner.AccessToken, f.serverID, "")
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var resp map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			keys := make([]string, 0, len(resp))
			for k := range resp {
				keys = append(keys, k)
			}
			assert.ElementsMatch(t, []string{"file_id", "storage_key", "url", "file_size", "width", "height"}, keys,
				"the success body keeps exactly the keys it always had")
			assert.Equal(t, 1, imgMediaRows(t, env, r.key(f.serverID)))
			assert.Zero(t, imgBudget(t, env, f.owner.ID), "a request with no code is never charged")
			assert.Empty(t, imgGraceKeys(t, env, f.owner.ID), "an unverified success grants nothing")
		})
	}
}

// The attempt budget is charged before the transaction only when a code is
// sent, stands on a refusal and on an unverified success, and is cleared only
// after a verified commit.
// Kills: Charge dropped or run on every request; the budget cleared on a
// refusal or on an unconfirmed success; Settle skipped after a verified commit.
func TestServerImageGate_BudgetIsChargedOnlyWithACode(t *testing.T) {
	env := setupImgGateEnv(t)
	for _, r := range imgRoutes() {
		t.Run(r.name, func(t *testing.T) {
			off := newImgGateFixture(t, env, false, true)
			w := env.upload(t, r, off.owner.AccessToken, off.serverID, imgWrongCode)
			require.Equal(t, http.StatusCreated, w.Code, "not enforcing, wrong code")
			assert.Equal(t, 1, imgBudget(t, env, off.owner.ID),
				"a code is charged whatever the setting, and an unverified success clears nothing")

			on := newImgGateFixture(t, env, true, true)
			w = env.upload(t, r, on.owner.AccessToken, on.serverID, "")
			require.Equal(t, imgMFARequired, classifyImgGate(t, r, on.serverID, w, "no code"))
			assert.Zero(t, imgBudget(t, env, on.owner.ID), "a refusal with no code is not charged")

			w = env.upload(t, r, on.owner.AccessToken, on.serverID, imgWrongCode)
			require.Equal(t, imgInvalid, classifyImgGate(t, r, on.serverID, w, "wrong code"))
			assert.Equal(t, 1, imgBudget(t, env, on.owner.ID), "a refused code stays charged")

			w = env.upload(t, r, on.owner.AccessToken, on.serverID, imgBackupCode)
			require.Equal(t, imgOK, classifyImgGate(t, r, on.serverID, w, "valid code"))
			assert.Zero(t, imgBudget(t, env, on.owner.ID), "a verified commit clears the budget")
		})
	}
}

// mfa_code is at most 256 characters; longer is a 400 before anything is
// charged, processed or stored, and exactly 256 passes the cap.
// Kills: the cap dropped or moved past 256; the field read after the charge.
func TestServerImageGate_MFACodeFieldIsCapped(t *testing.T) {
	env := setupImgGateEnv(t)
	r := imgRoutes()[0]
	f := newImgGateFixture(t, env, false, true)

	w := env.upload(t, r, f.owner.AccessToken, f.serverID, strings.Repeat("9", 257))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.JSONEq(t, imgBadRequestBody, w.Body.String())
	requireNothingStored(t, env, r, f.serverID, "oversize code")
	assert.Zero(t, imgBudget(t, env, f.owner.ID), "an oversize code is refused before the charge")

	w = env.upload(t, r, f.owner.AccessToken, f.serverID, strings.Repeat("9", 256))
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, 1, imgBudget(t, env, f.owner.ID))
}

// RS5 at the ManageServer denial: on an enforcing server, a member who would
// hold ManageServer without the mask (through the bit itself or raw
// Administrator) and has no inline factor is told to enroll; a member without
// the bit, enrolled or not, and a non-member get the unchanged generic 403.
// Nothing is stored.
// Kills: rbac.EnrollmentDenial removed from the pooled denial (the raw holders
// get the generic 403); EnrollmentDenial called with a bit the member lacks.
func TestServerImageGate_RS5(t *testing.T) {
	env := setupImgGateEnv(t)
	ts := env.ts
	f := newImgGateFixture(t, env, true, true)
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	manage := ts.CreateTestRole(t, f.serverID, "manage-"+tag, 4, int64(rbac.PermManageServer))
	admin := ts.CreateTestRole(t, f.serverID, "admin-"+tag, 5, int64(rbac.PermAdministrator))
	other := ts.CreateTestRole(t, f.serverID, "other-"+tag, 3, int64(rbac.PermManageChannels))
	persona := func(name, roleID string, enrolled bool) testhelpers.TestUser {
		u := ts.CreateTestUser(t, name+tag)
		ts.AddMemberToServer(t, f.serverID, u.ID, "member")
		ts.AssignRoleToUser(t, f.serverID, u.ID, roleID)
		if enrolled {
			enrollImgTOTP(t, env, u.ID)
		}
		return u
	}

	for i, r := range imgRoutes() {
		n := strconv.Itoa(i)
		for name, u := range map[string]testhelpers.TestUser{
			"unenrolled ManageServer holder": persona("rsm"+n, manage, false),
			"unenrolled raw Administrator":   persona("rsa"+n, admin, false),
		} {
			requireImgEnrollment(t, env.upload(t, r, u.AccessToken, f.serverID, ""), r.name+" "+name)
		}
		outsider := ts.CreateTestUser(t, "rso"+n+tag)
		for name, u := range map[string]testhelpers.TestUser{
			"unenrolled member without the bit": persona("rsu"+n, other, false),
			"enrolled member without the bit":   persona("rse"+n, other, true),
			"non-member":                        outsider,
		} {
			w := env.upload(t, r, u.AccessToken, f.serverID, imgBackupCode)
			assert.Equal(t, http.StatusForbidden, w.Code, r.name+" "+name)
			assert.Equal(t, imgForbiddenBody, w.Body.String(), r.name+" "+name)
		}
		requireNothingStored(t, env, r, f.serverID, r.name)
	}
}

// The gate transaction re-checks ManageServer under its locks, and answers
// that denial with RS5 on the transaction. A planted permission-cache entry
// stands in for a grant revoked (or a setting turned on) after the pooled
// check read it.
// Kills: the in-transaction ManageServer check removed (the revoked member's
// upload lands); the in-transaction EnrollmentDenial removed (the unenrolled
// holder gets the generic 403).
func TestServerImageGate_RechecksUnderTheGate(t *testing.T) {
	env := setupImgGateEnv(t)
	ts := env.ts
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	for i, r := range imgRoutes() {
		n := strconv.Itoa(i)
		revoked := newImgGateFixture(t, env, false, false)
		member := ts.CreateTestUser(t, "rv"+n+tag)
		ts.AddMemberToServer(t, revoked.serverID, member.ID, "member")
		testhelpers.PublishPermissionCache(t, ts.Redis, revoked.serverID, member.ID, "", rbac.PermManageServer)
		w := env.upload(t, r, member.AccessToken, revoked.serverID, "")
		assert.Equal(t, http.StatusForbidden, w.Code, r.name)
		assert.Equal(t, imgForbiddenBody, w.Body.String(), r.name)
		requireNothingStored(t, env, r, revoked.serverID, r.name+" revoked")

		masked := newImgGateFixture(t, env, true, false)
		holder := ts.CreateTestUser(t, "mh"+n+tag)
		ts.AddMemberToServer(t, masked.serverID, holder.ID, "member")
		ts.AssignRoleToUser(t, masked.serverID, holder.ID,
			ts.CreateTestRole(t, masked.serverID, "manage-"+tag, 4, int64(rbac.PermManageServer)))
		testhelpers.PublishPermissionCache(t, ts.Redis, masked.serverID, holder.ID, "", rbac.PermManageServer)
		requireImgEnrollment(t, env.upload(t, r, holder.AccessToken, masked.serverID, ""), r.name+" in-transaction RS5")
		requireNothingStored(t, env, r, masked.serverID, r.name+" masked")
	}
}

// A failed PutObject rolls the gate transaction back: nothing is recorded, the
// verified factor stays unspent (a rolled-back spend is restored, A-1), and
// neither the budget nor a grace settles, since nothing committed.
// Kills: the PutObject moved after the commit (the spend and the row survive);
// Settle run on an error; a verified factor spent outside the transaction.
func TestServerImageGate_PutObjectFailureRollsBack(t *testing.T) {
	env := setupImgGateEnv(t)
	for _, r := range imgRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newImgGateFixture(t, env, true, true)
			env.store.setPutErr(errors.New("object store down"))
			t.Cleanup(func() { env.store.setPutErr(nil) })

			w := env.upload(t, r, f.owner.AccessToken, f.serverID, imgBackupCode)
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.JSONEq(t, imgStoreFailBody, w.Body.String(), "the route's own 500 body")
			requireNothingStored(t, env, r, f.serverID, "put failure")
			assert.False(t, imgBackupCodeSpent(t, env, f.owner.ID), "the verified step is restored by the rollback")
			assert.Equal(t, 1, imgBudget(t, env, f.owner.ID), "nothing committed, so the charge stands")
			assert.Empty(t, imgGraceKeys(t, env, f.owner.ID), "nothing committed, so no grace")
		})
	}
}

// failMediaInsert installs a trigger that fails the media_files INSERT for
// storageKey: immediately, or (deferred) at COMMIT, after the INSERT and the
// object write have both succeeded. The trigger is dropped on cleanup.
func failMediaInsert(t *testing.T, env *imgGateEnv, storageKey string, deferred bool) {
	t.Helper()
	_, err := env.ts.DB.Exec(`CREATE OR REPLACE FUNCTION imggate_fail_insert() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'imggate: forced failure'; END $$`)
	require.NoError(t, err)
	var ddl string
	if deferred {
		ddl = `CREATE CONSTRAINT TRIGGER imggate_fail_insert AFTER INSERT ON media_files
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.storage_key = '%s')
			EXECUTE FUNCTION imggate_fail_insert()`
	} else {
		ddl = `CREATE TRIGGER imggate_fail_insert BEFORE INSERT ON media_files
			FOR EACH ROW WHEN (NEW.storage_key = '%s') EXECUTE FUNCTION imggate_fail_insert()`
	}
	// DDL cannot take a bind parameter; the key is built from a uuid this test generated.
	_, err = env.ts.DB.Exec(fmt.Sprintf(ddl, storageKey)) //nolint:gosec // see above
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = env.ts.DB.Exec(`DROP TRIGGER IF EXISTS imggate_fail_insert ON media_files`)
		_, _ = env.ts.DB.Exec(`DROP FUNCTION IF EXISTS imggate_fail_insert()`)
	})
}

// A failed COMMIT leaves the fixed-key object in place and runs no
// compensating delete (C4): the key is fixed, so the object just written
// replaced the live icon, and deleting it would leave servers.icon_url
// pointing at nothing. The transaction rolled back, so the verified factor is
// unspent and no row was written.
// Kills: a DeleteObject re-added after a failed INSERT or COMMIT.
func TestServerImageGate_CommitFailureKeepsTheFixedKeyObject(t *testing.T) {
	env := setupImgGateEnv(t)
	for _, r := range imgRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newImgGateFixture(t, env, true, true)
			key := r.key(f.serverID)
			env.store.seed(key, []byte("the live icon"))
			failMediaInsert(t, env, key, true)

			w := env.upload(t, r, f.owner.AccessToken, f.serverID, imgBackupCode)
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.JSONEq(t, imgRecordFailBody, w.Body.String(), "the route's own 500 body")
			assert.Empty(t, env.store.deletes(), "no compensating delete runs")
			assert.True(t, env.store.has(key), "the fixed-key object stays in place")
			assert.NotEqual(t, []byte("the live icon"), env.store.object(key), "PutObject overwrote it before the commit failed")
			assert.Zero(t, imgMediaRows(t, env, key), "the rolled-back transaction wrote no row")
			assert.False(t, imgBackupCodeSpent(t, env, f.owner.ID), "the rollback restored the verified step")
			assert.Empty(t, imgGraceKeys(t, env, f.owner.ID), "nothing committed, so no grace")
		})
	}
}

// A failed media_files INSERT likewise leaves the object and deletes nothing.
// Kills: a DeleteObject re-added after a failed INSERT.
func TestServerImageGate_InsertFailureKeepsTheFixedKeyObject(t *testing.T) {
	env := setupImgGateEnv(t)
	r := imgRoutes()[0]
	f := newImgGateFixture(t, env, false, false)
	key := r.key(f.serverID)
	failMediaInsert(t, env, key, false)

	w := env.upload(t, r, f.owner.AccessToken, f.serverID, "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, imgRecordFailBody, w.Body.String())
	assert.Empty(t, env.store.deletes(), "no compensating delete runs")
	assert.True(t, env.store.has(key))
}

// The group DM icon has the same fixed-key shape (dm-icons/<conversation>) and
// had the same compensating delete: a failed INSERT destroyed the live icon.
// Kills: a DeleteObject re-added after a failed INSERT on the DM path.
func TestDMIconInsertFailureKeepsTheFixedKeyObject(t *testing.T) {
	env := setupImgGateEnv(t)
	ts := env.ts
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	admin := ts.CreateTestUser(t, "dia"+tag)
	peer := ts.CreateTestUser(t, "dip"+tag)
	conv := ts.CreateGroupDMConversation(t, admin.ID, peer.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, conv, admin.ID)
	require.NoError(t, err)
	key := "dm-icons/" + conv
	env.store.seed(key, []byte("the live icon"))
	failMediaInsert(t, env, key, false)

	body, contentType := imgMultipart(t, map[string]string{"conversation_id": conv}, imgPNG(t))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/media/upload/dm-icon", body)
	req.Header = testhelpers.AuthHeaders(admin.AccessToken)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.JSONEq(t, imgRecordFailBody, w.Body.String())
	assert.Empty(t, env.store.deletes(), "no compensating delete runs")
	assert.True(t, env.store.has(key), "the fixed-key object stays in place")
}

// imgInflight is a request running on its own goroutine.
type imgInflight struct {
	done chan *httptest.ResponseRecorder
}

func startImgRequest(do func() *httptest.ResponseRecorder) *imgInflight {
	r := &imgInflight{done: make(chan *httptest.ResponseRecorder, 1)}
	go func() { r.done <- do() }()
	return r
}

func (r *imgInflight) await(t *testing.T, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-r.done:
		return w
	case <-time.After(imgLockProofBound):
		t.Fatalf("%s: the request did not finish within %s", what, imgLockProofBound)
		return nil
	}
}

// requireImgBackendWaitsOnALock polls pg_stat_activity until backend pid waits
// on a lock. The poll only paces reads of an observed state; nothing is
// inferred from elapsed time.
func requireImgBackendWaitsOnALock(t *testing.T, observer *sql.DB, pid int, finished <-chan error) {
	t.Helper()
	deadline := time.Now().Add(imgLockProofBound)
	for {
		var waitType sql.NullString
		require.NoError(t, observer.QueryRow(
			`SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waitType))
		if waitType.String == "Lock" {
			return
		}
		select {
		case err := <-finished:
			t.Fatalf("the toggle's UPDATE finished without waiting on the gate (err=%v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the toggle's UPDATE never waited on a lock within %s", imgLockProofBound)
		}
		time.Sleep(time.Millisecond)
	}
}

// The toggle's UPDATE waits on the upload's gate: with the upload paused inside
// PutObject, holding the gate's servers lock, the toggle's statement queues on
// a row lock and proceeds only after the upload commits. So the setting cannot
// change between the gate's read and the write it authorized, even though the
// object store cannot roll back.
// Kills: the PutObject moved out of the gate transaction (the toggle's UPDATE
// finishes while the upload is still in PutObject); the gate's servers lock
// weakened.
func TestServerImageGate_ToggleWaitsOnTheGate(t *testing.T) {
	env := setupImgGateEnv(t)
	for _, r := range imgRoutes() {
		t.Run(r.name, func(t *testing.T) {
			f := newImgGateFixture(t, env, false, true)
			reached, release := make(chan struct{}), make(chan struct{})
			env.store.setAtPut(func(ctx context.Context, _ string) error {
				close(reached)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			t.Cleanup(func() { env.store.setAtPut(nil) })

			upload := uploadRequest(t, r, f.owner.AccessToken, f.serverID, "")
			req := startImgRequest(func() *httptest.ResponseRecorder { return env.serve(upload) })
			select {
			case <-reached:
			case w := <-req.done:
				t.Fatalf("the upload finished before reaching PutObject: %d %s", w.Code, w.Body.String())
			case <-time.After(imgLockProofBound):
				t.Fatal("the upload never reached PutObject")
			}

			ctx := context.Background()
			probe, err := env.ts.DB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = probe.Rollback() }()
			_, err = probe.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`)
			require.NoError(t, err)
			var pid int
			require.NoError(t, probe.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			flipped := make(chan error, 1)
			go func() {
				_, err := probe.ExecContext(ctx, `UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, f.serverID)
				flipped <- err
			}()
			requireImgBackendWaitsOnALock(t, env.ts.DB, pid, flipped)
			close(release)

			w := req.await(t, r.name)
			require.Equal(t, http.StatusCreated, w.Code, "the upload read the setting before the flip: %s", w.Body.String())
			select {
			case err := <-flipped:
				require.NoError(t, err, "the toggle's UPDATE proceeds once the gate commits")
			case <-time.After(imgLockProofBound):
				t.Fatal("the toggle's UPDATE did not finish after the gate committed")
			}
		})
	}
}

// Both uploads are grace-eligible and share one grace per (session, server,
// ManageServer bit): a verified icon upload lets the same session's banner
// upload through without a code, and no other session's.
// Kills: Settle not called after the upload's commit, or the grace not read
// before its transaction (the second upload is prompted); the grace scoped
// to a different bit.
func TestServerImageGate_GraceCoversTheNextUploadInTheSameSession(t *testing.T) {
	env := setupImgGateEnv(t)
	icon, banner := imgRoutes()[0], imgRoutes()[1]
	f := newImgGateFixture(t, env, true, true)
	session := func() string {
		token, err := auth.GenerateAccessToken(f.owner.ID, testhelpers.TestJWTSecret, true, "", uuid.NewString())
		require.NoError(t, err)
		return token
	}
	first := session()

	require.Equal(t, imgOK, classifyImgGate(t, icon, f.serverID,
		env.upload(t, icon, first, f.serverID, imgBackupCode), "verified icon upload"))
	require.Equal(t, imgOK, classifyImgGate(t, banner, f.serverID,
		env.upload(t, banner, first, f.serverID, ""), "grace-covered banner upload"))
	assert.Equal(t, imgMFARequired, classifyImgGate(t, banner, f.serverID,
		env.upload(t, banner, session(), f.serverID, ""), "another session"))
}

// A feedback screenshot's key is fresh for every upload (#1747), so a failed
// record leaves an object nothing points at, and the compensating delete must
// still run. This is the other side of C4: only a FIXED key keeps its object.
// Kills: routing the unique-key purposes through the fixed-key path (the
// #3454 x #1747 merge sent screenshots to the DM-icon path and answered 500),
// or dropping the DeleteObject from storeUniqueKeyTier1Image.
func TestFeedbackScreenshotInsertFailureDeletesTheObject(t *testing.T) {
	env := setupImgGateEnv(t)
	ts := env.ts
	user := ts.CreateTestUser(t, "fbs"+strings.ReplaceAll(uuid.NewString(), "-", "")[:10])
	_, err := ts.DB.Exec(`CREATE OR REPLACE FUNCTION imggate_fail_insert() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'imggate: forced failure'; END $$`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`CREATE TRIGGER imggate_fail_insert BEFORE INSERT ON media_files
		FOR EACH ROW WHEN (NEW.storage_key LIKE 'feedback-screenshots/%') EXECUTE FUNCTION imggate_fail_insert()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`DROP TRIGGER IF EXISTS imggate_fail_insert ON media_files`)
		_, _ = ts.DB.Exec(`DROP FUNCTION IF EXISTS imggate_fail_insert()`)
	})

	body, contentType := imgMultipart(t, nil, imgPNG(t))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/media/upload/feedback-screenshot", body)
	req.Header = testhelpers.AuthHeaders(user.AccessToken)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.JSONEq(t, imgRecordFailBody, w.Body.String())
	deleted := env.store.deletes()
	require.Len(t, deleted, 1, "the orphaned unique-key object is deleted")
	assert.True(t, strings.HasPrefix(deleted[0], "feedback-screenshots/"), deleted[0])
	assert.False(t, env.store.has(deleted[0]), "nothing is left at the deleted key")
}
