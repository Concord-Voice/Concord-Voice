package testhelpers

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// AccessIdentityForToken returns the signed access identity of a fixture
// token, the value middleware.AuthRequired verifies and attaches.
func AccessIdentityForToken(t testing.TB, token string) loginfamily.AccessIdentity {
	t.Helper()
	claims, err := auth.ValidateAccessToken(token, TestJWTSecret)
	if err != nil || claims.ExpiresAt == nil {
		t.Fatalf("testhelpers: fixture access token: %v", err)
	}
	user, err := uuid.Parse(claims.UserID)
	if err != nil {
		t.Fatalf("testhelpers: fixture token user: %v", err)
	}
	sid, err := uuid.Parse(claims.SessionID)
	if err != nil {
		t.Fatalf("testhelpers: fixture token sid: %v", err)
	}
	return loginfamily.AccessIdentity{UserID: user, SID: sid, CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time}
}

// WithAccessIdentity attaches user's signed identity to ctx, as AuthRequired
// does for the request context that writers' credepoch.GuardFamilyTx reads.
func WithAccessIdentity(ctx context.Context, t testing.TB, user TestUser) context.Context {
	t.Helper()
	return loginfamily.WithAccessIdentity(ctx, AccessIdentityForToken(t, user.AccessToken))
}

// AttachAccessIdentity does the same for a hand-built gin test context. Call
// it after c.Request is set.
func AttachAccessIdentity(t testing.TB, c *gin.Context, user TestUser) {
	t.Helper()
	if c.Request == nil {
		c.Request = &http.Request{}
	}
	c.Request = c.Request.WithContext(WithAccessIdentity(c.Request.Context(), t, user))
}

// issuedAccessTokens maps each user CreateTestUser made to its access token,
// for test-only auth stand-ins that know only the user ID.
var issuedAccessTokens sync.Map

// UserIdentityContext attaches the signed identity of a user CreateTestUser
// made. An unknown ID gets none, so a guarded writer refuses it as
// AuthRequired would have.
func UserIdentityContext(ctx context.Context, t testing.TB, userID string) context.Context {
	t.Helper()
	token, ok := issuedAccessTokens.Load(userID)
	if !ok {
		return testdb.WithRememberedActor(ctx, userID)
	}
	return WithAccessIdentity(ctx, t, TestUser{ID: userID, AccessToken: token.(string)})
}

// AttachUserIdentity is UserIdentityContext for a gin context. Call it after
// c.Request is set.
func AttachUserIdentity(t testing.TB, c *gin.Context, userID string) {
	t.Helper()
	if c.Request == nil {
		c.Request = &http.Request{}
	}
	c.Request = c.Request.WithContext(UserIdentityContext(c.Request.Context(), t, userID))
}

// ActorContext returns ctx carrying a fresh live signed identity for userID
// (a new access-only family, so session counts are unchanged).
func (ts *TestServer) ActorContext(ctx context.Context, t *testing.T, userID string) context.Context {
	t.Helper()
	return ActorContextForDB(ctx, t, ts.DB, userID)
}

// ActorContextForDB is ActorContext for tests that hold only the database.
func ActorContextForDB(ctx context.Context, t testing.TB, db *sql.DB, userID string) context.Context {
	t.Helper()
	token := createAccessOnlyFamilyFixtureDB(t, db, userID, false)
	return WithAccessIdentity(ctx, t, TestUser{ID: userID, AccessToken: token})
}

// AttachActorIdentity attaches a fresh live identity for userID to a
// hand-built gin test context. Call it after c.Request is set.
func (ts *TestServer) AttachActorIdentity(t *testing.T, c *gin.Context, userID string) {
	t.Helper()
	if c.Request == nil {
		c.Request = &http.Request{}
	}
	c.Request = c.Request.WithContext(ts.ActorContext(c.Request.Context(), t, userID))
}
