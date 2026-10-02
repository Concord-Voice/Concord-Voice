package stepup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// serveRequirements runs the handler for one request as the given user and
// returns the recorder plus everything it logged.
func serveRequirements(ctx context.Context, t *testing.T, q RowQuerier, userID string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/mfa/step-up", nil).WithContext(ctx)
	c.Set("user_id", userID)
	RequirementsHandler(q, logger.NewWithWriter(&logs))(c)
	return w, logs.String()
}

func TestRequirementsHandler(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	t.Run("documented shape, no-store, and nothing logged", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestTOTP(t, db, userID, true, true)
		factorsSetTOTP(t, db, userID, base.Unix()/totpStepSeconds, []string{"h1"}, []bool{false})
		factorsAddKey(t, db, userID, base.Add(-time.Hour), base.Add(-time.Minute*30))

		w, logs := serveRequirements(ctx, t, db, userID)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		require.JSONEq(t, `{"methods":["totp","webauthn"],"default_method":"totp","backup_code_available":true}`, w.Body.String())
		require.Empty(t, logs, "factor posture must never reach a log")
	})

	t.Run("no inline factor: default_method is JSON null, not an empty string", func(t *testing.T) {
		userID := subjectTestUser(t, db)

		w, _ := serveRequirements(ctx, t, db, userID)

		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "null", string(body["default_method"]))
		require.Equal(t, "[]", string(body["methods"]), "methods is an empty array, never null")
		require.Equal(t, "false", string(body["backup_code_available"]))
	})

	t.Run("deleted user: 401, never 404, never an empty 200", func(t *testing.T) {
		w, logs := serveRequirements(ctx, t, db, uuid.New().String())

		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		require.JSONEq(t, `{"error":"`+ErrMsgSessionNoLongerValid+`"}`, w.Body.String())
		require.Empty(t, logs, "a 401 is an outcome, not a fault")
	})

	t.Run("read failure: 500, logged with its cause", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		canceled, cancel := context.WithCancel(ctx)
		cancel()

		w, logs := serveRequirements(canceled, t, db, userID)

		require.Equal(t, http.StatusInternalServerError, w.Code)
		require.JSONEq(t, `{"error":"`+ErrMsgVerificationFailed+`"}`, w.Body.String())
		require.Contains(t, logs, "Failed to read step-up requirements")
	})
}

// TestRequirementsRoute_DeletedUserThroughAuthChain is the spec's chain case
// (PR 1 server tests): the auth gates the router mounts GET /mfa/step-up
// behind, with the real credential-epoch fence, admit a live token whose user
// was deleted after minting, and the handler's users-row anchor answers 401.
// The fence reads an unknown user as having no epoch, so the anchor is what
// stops the request.
func TestRequirementsRoute_DeletedUserThroughAuthChain(t *testing.T) {
	db := subjectTestDB(t)
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	log := logger.NewWithWriter(io.Discard)
	secret := uuid.NewString() // a per-run signing key, never a literal

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/mfa/step-up",
		middleware.AuthRequired(secret, rdb, credepoch.New(db, rdb, log)),
		middleware.RequireVerifiedEmail(),
		middleware.RateLimitByUser(rdb, 20, time.Minute),
		RequirementsHandler(db, log),
	)
	get := func(token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/step-up", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("no token: 401 before the handler", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, get("").Code)
	})

	t.Run("live token, user deleted after minting: 401 from the anchor", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		token, err := auth.GenerateAccessToken(userID, secret, true, "", uuid.NewString())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, get(token).Code, "control: the chain admits the live user")

		_, err = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
		require.NoError(t, err)

		w := get(token)
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.JSONEq(t, `{"error":"`+ErrMsgSessionNoLongerValid+`"}`, w.Body.String())
	})
}

func TestDefaultMethodReader(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	read := DefaultMethodReader(db)

	userID := subjectTestUser(t, db)
	subjectTestTOTP(t, db, userID, true, true)
	factorsSetTOTP(t, db, userID, base.Unix()/totpStepSeconds, []string{}, []bool{})
	factorsAddKey(t, db, userID, base.Add(-time.Hour), base.Add(-30*time.Minute))

	got, err := read(ctx, userID, []string{"totp", "webauthn", "email"})
	require.NoError(t, err)
	require.Equal(t, MethodTOTP, got)

	got, err = read(ctx, userID, []string{"webauthn"})
	require.NoError(t, err)
	require.Equal(t, MethodWebAuthn, got, "restricted to what the challenge offers")

	got, err = read(ctx, userID, []string{"email"})
	require.NoError(t, err)
	require.Equal(t, "", got, "an email-only challenge gets no default")

	got, err = read(ctx, uuid.New().String(), []string{"totp"})
	require.Error(t, err, "a missing user is an error the caller omits on")
	require.Equal(t, "", got)
}
