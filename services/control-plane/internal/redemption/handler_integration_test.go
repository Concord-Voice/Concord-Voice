package redemption_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAdminToken = "test-admin-token-at-least-32-characters-long" //nolint:gosec // test fixture, not a real secret

func newTestHandler(t *testing.T, adminToken string) (*redemption.Handler, *gin.Engine, func()) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	eng := redemption.NewEngine(db, redemption.NewCatalog(), &recordingNotifier{})
	iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())
	h := redemption.NewHandler(eng, iss, adminToken, logger.New("test"))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	return h, r, func() { cleanup() }
}

// authAs injects a user_id into the gin context, mimicking AuthRequired.
func authAs(userID uuid.UUID) gin.HandlerFunc {
	return func(c *gin.Context) { c.Set("user_id", userID.String()) }
}

// TestHandler_RedeemHappyPath drives the full HTTP redeem of a premium code.
func TestHandler_RedeemHappyPath(t *testing.T) {
	h, r, cleanup := newTestHandler(t, testAdminToken)
	defer cleanup()
	db, _ := testhelpers.SetupTestDB(t) // separate handle for fixtures on same DB
	user := testhelpers.CreateUser(t, db)
	code := issueOne(t, db, redemption.IssueSpec{
		GrantKind: redemption.GrantPremiumSubscription, GrantParams: map[string]any{"months": 12},
		Count: 1, SingleUse: true, MaxRedeems: intPtr(1),
	})

	r.POST("/redeem", authAs(user), h.Redeem)
	body, _ := json.Marshal(map[string]string{"code": code})
	w := doJSON(r, http.MethodPost, "/redeem", body)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["success"])
	assert.Contains(t, resp["description"], "12 months")
}

// TestHandler_RedeemNoOracle: invalid and not-found inputs both return the SAME
// generic message + 400 — the client cannot distinguish them.
func TestHandler_RedeemNoOracle(t *testing.T) {
	h, r, cleanup := newTestHandler(t, testAdminToken)
	defer cleanup()
	db, _ := testhelpers.SetupTestDB(t)
	user := testhelpers.CreateUser(t, db)
	r.POST("/redeem", authAs(user), h.Redeem)

	// (a) checksum-invalid garbage.
	body, _ := json.Marshal(map[string]string{"code": "NOPE-NOPE-NOPE"})
	wGarbage := doJSON(r, http.MethodPost, "/redeem", body)

	// (b) well-formed but never issued: issue then delete so the lookup misses.
	code := issueOne(t, db, redemption.IssueSpec{
		GrantKind: redemption.GrantPremiumSubscription, Count: 1, SingleUse: true, MaxRedeems: intPtr(1),
	})
	_, err := db.Exec(`DELETE FROM redemption_codes`)
	require.NoError(t, err)
	body2, _ := json.Marshal(map[string]string{"code": code})
	wMissing := doJSON(r, http.MethodPost, "/redeem", body2)

	// Both: identical status AND identical body (no oracle).
	assert.Equal(t, http.StatusBadRequest, wGarbage.Code)
	assert.Equal(t, http.StatusBadRequest, wMissing.Code)
	assert.JSONEq(t, wGarbage.Body.String(), wMissing.Body.String(),
		"invalid and not-found must be byte-identical (no oracle)")
}

// TestHandler_AdminGate covers the three issuer-authz branches.
func TestHandler_AdminGate(t *testing.T) {
	user := uuid.New()

	t.Run("missing token → 403", func(t *testing.T) {
		h, r, cleanup := newTestHandler(t, testAdminToken)
		defer cleanup()
		r.POST("/admin/redemption/codes", authAs(user), h.AdminGate(), h.Generate)
		w := doJSON(r, http.MethodPost, "/admin/redemption/codes", []byte(`{}`))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("wrong token → 403", func(t *testing.T) {
		h, r, cleanup := newTestHandler(t, testAdminToken)
		defer cleanup()
		r.POST("/admin/redemption/codes", authAs(user), h.AdminGate(), h.Generate)
		req := httptest.NewRequest(http.MethodPost, "/admin/redemption/codes", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("X-Admin-Token", "wrong-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("empty configured token → 503 (endpoint disabled)", func(t *testing.T) {
		h, r, cleanup := newTestHandler(t, "") // no admin token configured
		defer cleanup()
		r.POST("/admin/redemption/codes", authAs(user), h.AdminGate(), h.Generate)
		req := httptest.NewRequest(http.MethodPost, "/admin/redemption/codes", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("X-Admin-Token", "anything")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
}

// TestHandler_GenerateHappyPath: a correctly-authorized admin request mints
// codes and returns the plaintext once.
func TestHandler_GenerateHappyPath(t *testing.T) {
	h, r, cleanup := newTestHandler(t, testAdminToken)
	defer cleanup()
	admin := testhelpers.CreateUser(t, mustDB(t))

	r.POST("/admin/redemption/codes", authAs(admin), h.AdminGate(), h.Generate)

	reqBody, _ := json.Marshal(map[string]any{
		"grant_kind":      redemption.GrantPremiumSubscription,
		"grant_params":    map[string]any{"months": 6},
		"count":           3,
		"prefix":          "KS",
		"single_use":      true,
		"max_redemptions": 1,
		"batch_id":        "http-batch",
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/redemption/codes", bytes.NewReader(reqBody))
	req.Header.Set("X-Admin-Token", testAdminToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp struct {
		Count int      `json:"count"`
		Codes []string `json:"codes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.Count)
	require.Len(t, resp.Codes, 3)
	for _, c := range resp.Codes {
		assert.Contains(t, c, "KS-")
	}
}

// TestHandler_GenerateRejectsUnknownKind: an admin request for an unsupported
// grant_kind returns a 400 with a precise (non-oracle — this is the admin side)
// message.
func TestHandler_GenerateRejectsUnknownKind(t *testing.T) {
	h, r, cleanup := newTestHandler(t, testAdminToken)
	defer cleanup()
	admin := testhelpers.CreateUser(t, mustDB(t))
	r.POST("/admin/redemption/codes", authAs(admin), h.AdminGate(), h.Generate)

	reqBody, _ := json.Marshal(map[string]any{"grant_kind": "bogus:thing", "count": 1})
	req := httptest.NewRequest(http.MethodPost, "/admin/redemption/codes", bytes.NewReader(reqBody))
	req.Header.Set("X-Admin-Token", testAdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── helpers ──────────────────────────────────────────────────────────────

func doJSON(r *gin.Engine, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// mustDB opens a throwaway DB handle for fixtures (the handler holds its own).
func mustDB(t *testing.T) *sql.DB {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	return db
}
