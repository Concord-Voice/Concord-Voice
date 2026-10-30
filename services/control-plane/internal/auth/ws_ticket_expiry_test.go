package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

// A fresh access-only family binds the prepared token's exact expiry before signing.
func shortDeadlineTicketAccess(t *testing.T, rig familyTestRig) (string, *Claims) {
	t.Helper()
	ctx := t.Context()
	tx, err := rig.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := tx.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	}()
	user, err := lockMintUserTx(ctx, tx, rig.user)
	require.NoError(t, err)
	var epoch string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COALESCE(credential_epoch, '') FROM users WHERE id=$1`, rig.user).Scan(&epoch))
	family, err := lf.CreateFamilyTx(ctx, tx, user, epoch)
	require.NoError(t, err)
	sid := uuid.New()
	prepared, err := PrepareAccessToken(AccessTokenInput{UserID: uuid.MustParse(rig.user), SID: sid, CredentialEpoch: epoch, EmailVerified: true}, time.Now().Add(-AccessTokenTTL+5*time.Second))
	require.NoError(t, err)
	require.NoError(t, lf.BindSIDTx(ctx, tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	access, err := prepared.Sign(familyTestSecret)
	require.NoError(t, err)
	claims, err := ValidateAccessToken(access, familyTestSecret)
	require.NoError(t, err)
	var exact bool
	require.NoError(t, rig.db.QueryRowContext(ctx, `SELECT access_expires_at=$2 FROM login_family_sids WHERE sid=$1 AND user_id=$3`, sid, claims.ExpiresAt.Time, rig.user).Scan(&exact))
	require.True(t, exact)
	return access, claims
}

func expiryTicketRequest(rig familyTestRig, access string, handler *WSTicketHandler) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/ticket", middleware.AuthRequired(familyTestSecret, rig.h.redis, rig.db, nil), handler.IssueTicket)
	req := httptest.NewRequest(http.MethodPost, "/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestIssueTicketRetainsAccessTokenDeadline(t *testing.T) {
	rig := newFamilyTestRig(t)
	access, claims := shortDeadlineTicketAccess(t, rig)
	handler := NewWSTicketHandler(rig.h.redis, familyTestSecret)
	var storageNow time.Time
	checks := 0
	handler.now = func() time.Time { checks++; storageNow = time.Now(); return storageNow }
	w := expiryTicketRequest(rig, access, handler)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, checks, "real admission must reach the final TTL clock")
	var body struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Ticket, 64)
	key := wsTicketPrefix + body.Ticket
	stored, err := rig.h.redis.Get(t.Context(), key).Result()
	require.NoError(t, err)
	require.Len(t, strings.Split(stored, ":"), 4)
	require.Equal(t, fmt.Sprintf("%s:%s:%s:%d", rig.user, claims.SessionID, claims.CredentialEpoch, claims.ExpiresAt.Unix()), stored)
	ttl, err := rig.h.redis.PTTL(t.Context(), key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, 30*time.Second)
	require.LessOrEqual(t, ttl, claims.ExpiresAt.Sub(storageNow))
	require.LessOrEqual(t, ttl, 5*time.Second)
	user, sid, epoch, deadline, err := ValidateTicket(t.Context(), rig.h.redis, body.Ticket)
	require.NoError(t, err)
	require.Equal(t, rig.user, user)
	require.Equal(t, claims.SessionID, sid)
	require.Equal(t, claims.CredentialEpoch, epoch)
	require.Equal(t, claims.ExpiresAt.Time, deadline)
	_, _, _, _, err = ValidateTicket(t.Context(), rig.h.redis, body.Ticket)
	require.Error(t, err, "ticket is consumed exactly once")
}

func TestIssueTicketRejectsUntrustedPublicClaimsContexts(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	for _, test := range []struct {
		name   string
		claims any
	}{
		{name: "wrong claims type", claims: "not verified claims"},
		{name: "missing deadline", claims: jwt.MapClaims{}},
		{name: "expired deadline", claims: jwt.MapClaims{"exp": float64(time.Now().Add(-time.Second).Unix())}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/auth/ws-ticket", nil)
			c.Set("user_id", "user-1")
			c.Set(middleware.JWTClaimsContextKey, test.claims)
			NewWSTicketHandler(rc, "unused").IssueTicket(c)
			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.Empty(t, mr.Keys())
		})
	}
}

func TestIssueTicketRejectsDeadlineCrossedBeforeStorage(t *testing.T) {
	rig := newFamilyTestRig(t)
	access, claims := shortDeadlineTicketAccess(t, rig)
	handler := NewWSTicketHandler(rig.h.redis, familyTestSecret)
	checks := 0
	handler.now = func() time.Time {
		checks++
		return claims.ExpiresAt.Add(time.Nanosecond)
	}
	w := expiryTicketRequest(rig, access, handler)
	require.Equal(t, 1, checks, "must reach final storage TTL check after real admission and family recheck")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
	keys, err := rig.h.redis.Keys(t.Context(), wsTicketPrefix+"*").Result()
	require.NoError(t, err)
	require.Empty(t, keys, "an expired token must never create a persistent ticket")
}

func TestValidateTicketRequiresUnexpiredDeadline(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	for _, value := range []string{"user-1::epoch", "user-1:::not-a-time", "user-1:::0",
		fmt.Sprintf("user-1:::%d", time.Now().Add(-time.Second).Unix())} {
		require.NoError(t, rc.Set(context.Background(), wsTicketPrefix+"deadline", value, time.Minute).Err())
		_, _, _, _, err := ValidateTicket(context.Background(), rc, "deadline")
		require.Error(t, err)
		require.False(t, mr.Exists(wsTicketPrefix+"deadline"), "even invalid tickets are single-use")
	}
	expires := time.Now().Add(time.Minute).Truncate(time.Second)
	require.NoError(t, rc.Set(context.Background(), wsTicketPrefix+"deadline",
		fmt.Sprintf("user-1:session:epoch:%d", expires.Unix()), time.Minute).Err())
	user, session, epoch, deadline, err := ValidateTicket(context.Background(), rc, "deadline")
	require.NoError(t, err)
	require.Equal(t, "user-1", user)
	require.Equal(t, "session", session)
	require.Equal(t, "epoch", epoch)
	require.Equal(t, expires, deadline)
}
