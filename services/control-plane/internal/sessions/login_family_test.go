package sessions_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/sessions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const terminalPassword = "Family-test-password-123!"

type terminalSessionRig struct {
	db     *sql.DB
	h      *sessions.Handler
	mint   *auth.Handler
	user   string
	claims *auth.Claims
	hub    *terminalSessionHub
}

// terminalSessionHub records each exact family fence and whether the HTTP
// response had already been written when the fence was reached.
type terminalSessionHub struct {
	mu            sync.Mutex
	families      []uuid.UUID
	users         []uuid.UUID
	writtenBefore []bool
	response      *httptest.ResponseRecorder
}

func (h *terminalSessionHub) DisconnectFamilyContext(_ context.Context, user, family uuid.UUID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.users = append(h.users, user)
	h.families = append(h.families, family)
	h.writtenBefore = append(h.writtenBefore, h.response != nil && h.response.Body.Len() > 0)
	return nil
}

func (h *terminalSessionHub) recorded() ([]uuid.UUID, []uuid.UUID, []bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]uuid.UUID(nil), h.users...), append([]uuid.UUID(nil), h.families...), append([]bool(nil), h.writtenBefore...)
}
func newTerminalSessionRig(t *testing.T) terminalSessionRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	user := dbtest.CreateUser(t, db).String()
	hash, err := auth.HashPassword(terminalPassword)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, user, hash)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO user_keys(user_id,wrapped_private_key,key_derivation_salt,key_version,key_derivation_alg) VALUES($1,$2,$3,1,'argon2id')`, user, []byte{1}, []byte{2})
	require.NoError(t, err)
	rdb := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), rdb))
	log := logger.NewWithWriter(io.Discard)
	mint := auth.NewHandler(db, rdb, log, "terminal-session-test-secret", nil)
	access, _, _, err := mint.IssueAccessAndRefresh(context.Background(), user)
	require.NoError(t, err)
	claims, err := auth.ValidateAccessToken(access, "terminal-session-test-secret")
	require.NoError(t, err)
	hub := &terminalSessionHub{}
	return terminalSessionRig{db, sessions.NewHandler(db, rdb, log, hub, nil), mint, user, claims, hub}
}
func (r terminalSessionRig) request(t *testing.T, target string, all bool, claims *auth.Claims) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.hub.mu.Lock()
	r.hub.response = w
	r.hub.mu.Unlock()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"password":"`+terminalPassword+`","include_current":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", r.user)
	if claims != nil {
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"user_id": r.user, "sid": claims.SessionID, "cred_epoch": claims.CredentialEpoch, "exp": float64(claims.ExpiresAt.Unix())})
		c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(), loginfamily.AccessIdentity{
			UserID: uuid.MustParse(r.user), SID: uuid.MustParse(claims.SessionID),
			CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time,
		}))
	}
	if all {
		r.h.RevokeAllSessions(c)
	} else {
		c.Params = gin.Params{{Key: "id", Value: target}}
		r.h.RevokeSession(c)
	}
	return w
}
func TestLoginFamilyTerminalSessionUnknownAndForeignAre404(t *testing.T) {
	r := newTerminalSessionRig(t)
	for _, target := range []string{uuid.NewString(), uuid.Nil.String()} {
		w := r.request(t, target, false, r.claims)
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	}
	other := dbtest.CreateUser(t, r.db).String()
	_, err := r.db.Exec(`INSERT INTO user_keys(user_id,wrapped_private_key,key_derivation_salt,key_version,key_derivation_alg) VALUES($1,$2,$3,1,'argon2id')`, other, []byte{1}, []byte{2})
	require.NoError(t, err)
	_, _, sid, err := r.mint.IssueAccessAndRefresh(context.Background(), other)
	require.NoError(t, err)
	w := r.request(t, sid, false, r.claims)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}
func TestLoginFamilyTerminalSessionIdempotentOwnedTarget(t *testing.T) {
	r := newTerminalSessionRig(t)
	_, _, sid, err := r.mint.IssueAccessAndRefresh(context.Background(), r.user)
	require.NoError(t, err)
	w := r.request(t, sid, false, r.claims)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var before, after string
	require.NoError(t, r.db.QueryRow(`SELECT terminal_at::text || auth_operation_id::text FROM login_families WHERE family_id=(SELECT family_id FROM login_family_sids WHERE sid=$1)`, sid).Scan(&before))
	w = r.request(t, sid, false, r.claims)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, r.db.QueryRow(`SELECT terminal_at::text || auth_operation_id::text FROM login_families WHERE family_id=(SELECT family_id FROM login_family_sids WHERE sid=$1)`, sid).Scan(&after))
	assert.Equal(t, before, after)
}
func TestLoginFamilyTerminalSessionUnknownPreserve400(t *testing.T) {
	r := newTerminalSessionRig(t)
	w := r.request(t, "", true, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var n int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM login_families WHERE user_id=$1 AND terminal_at IS NOT NULL`, r.user).Scan(&n))
	assert.Zero(t, n)
}
func TestLoginFamilyTerminalSessionAuthorityUnavailable503(t *testing.T) {
	r := newTerminalSessionRig(t)
	// Leave users/password policy available; only the family authority read fails.
	_, err := r.db.Exec(`ALTER TABLE login_families RENAME TO terminal_fixture_hidden_families`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := r.db.Exec(`ALTER TABLE terminal_fixture_hidden_families RENAME TO login_families`)
		require.NoError(t, err)
	})
	w := r.request(t, "", true, r.claims)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

func (r terminalSessionRig) familyOf(t *testing.T, sid string) uuid.UUID {
	t.Helper()
	var family uuid.UUID
	require.NoError(t, r.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, sid).Scan(&family))
	return family
}

// A request whose own login was revoked while it waited must not revoke
// another live login: the epoch is unchanged, so only the family check stops it.
func TestLoginFamilyRevokedRequesterCannotRevokeSibling(t *testing.T) {
	r := newTerminalSessionRig(t)
	access, _, sibling, err := r.mint.IssueAccessAndRefresh(context.Background(), r.user)
	require.NoError(t, err)
	siblingClaims, err := auth.ValidateAccessToken(access, "terminal-session-test-secret")
	require.NoError(t, err)
	w := r.request(t, r.claims.SessionID, false, siblingClaims)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = r.request(t, sibling, false, r.claims)
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var terminal bool
	require.NoError(t, r.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, r.familyOf(t, sibling)).Scan(&terminal))
	assert.False(t, terminal, "the revoked requester must not terminalize the sibling login")

	w = r.request(t, "", true, r.claims)
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NoError(t, r.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, r.familyOf(t, sibling)).Scan(&terminal))
	assert.False(t, terminal, "revoke-all from the revoked requester must not run either")
}

// The revoked family's sockets are fenced before the 200 is written; the
// caller's own sibling family is never touched.
func TestLoginFamilyRevokeSessionFencesOnlyTargetBeforeResponse(t *testing.T) {
	r := newTerminalSessionRig(t)
	_, _, sid, err := r.mint.IssueAccessAndRefresh(context.Background(), r.user)
	require.NoError(t, err)
	target, current := r.familyOf(t, sid), r.familyOf(t, r.claims.SessionID)
	require.NotEqual(t, target, current)

	w := r.request(t, sid, false, r.claims)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	users, families, writtenBefore := r.hub.recorded()
	assert.Equal(t, []uuid.UUID{target}, families, "exactly the revoked family is fenced")
	assert.NotContains(t, families, current, "the sibling (current) family must stay connected")
	assert.Equal(t, []uuid.UUID{uuid.MustParse(r.user)}, users)
	assert.Equal(t, []bool{false}, writtenBefore, "the fence must be reached before the response is written")
}

// Revoke-all fences every terminalized family of the user except the preserved one.
func TestLoginFamilyRevokeAllSessionsFencesEveryOtherFamily(t *testing.T) {
	r := newTerminalSessionRig(t)
	others := make([]uuid.UUID, 0, 3)
	for range 3 {
		_, _, sid, err := r.mint.IssueAccessAndRefresh(context.Background(), r.user)
		require.NoError(t, err)
		others = append(others, r.familyOf(t, sid))
	}
	w := r.request(t, "", true, r.claims)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, families, writtenBefore := r.hub.recorded()
	assert.ElementsMatch(t, others, families)
	assert.NotContains(t, families, r.familyOf(t, r.claims.SessionID), "the preserved current family must stay connected")
	for _, written := range writtenBefore {
		assert.False(t, written, "every fence must precede the response")
	}
}

// failingFamilyHub proves a failed synchronous close never turns a committed
// revocation into an error response; the durable owner keeps the obligation.
type failingFamilyHub struct{ calls int }

func (h *failingFamilyHub) DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error {
	h.calls++
	return errors.New("injected hub failure")
}

func TestLoginFamilyRevokeSessionCloseFailureKeepsCommittedSuccess(t *testing.T) {
	r := newTerminalSessionRig(t)
	failing := &failingFamilyHub{}
	rdb := redistest.Client(t)
	r.h = sessions.NewHandler(r.db, rdb, logger.NewWithWriter(io.Discard), failing, nil)
	_, _, sid, err := r.mint.IssueAccessAndRefresh(context.Background(), r.user)
	require.NoError(t, err)
	w := r.request(t, sid, false, r.claims)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, failing.calls)
	var pending bool
	require.NoError(t, r.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM login_family_revocations o
 JOIN login_families f ON f.auth_operation_id=o.operation_id
 WHERE f.family_id=$1 AND o.sealed_at IS NOT NULL AND o.completed_at IS NULL)`, r.familyOf(t, sid)).Scan(&pending))
	assert.True(t, pending, "the sealed durable owner remains for reconciliation")
}
