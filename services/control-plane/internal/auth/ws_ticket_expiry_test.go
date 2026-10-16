package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

func TestIssueTicketRetainsAccessTokenDeadline(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/ws-ticket", nil)
	c.Set("user_id", "user-1")
	expires := time.Now().Add(5 * time.Second).Unix()
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"exp": float64(expires)})
	NewWSTicketHandler(rc, "unused").IssueTicket(c)
	require.Equal(t, http.StatusOK, w.Code)
	keys := mr.Keys()
	require.Len(t, keys, 1)
	stored, err := rc.Get(context.Background(), keys[0]).Result()
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("user-1:::%d", expires), stored)
	require.LessOrEqual(t, mr.TTL(keys[0]), 5*time.Second)
}

func TestIssueTicketRequiresAccessTokenDeadline(t *testing.T) {
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
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/ws-ticket", nil)
	c.Set("user_id", "user-1")
	deadline := time.Now().Add(time.Minute).Truncate(time.Second)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"exp": float64(deadline.Unix())})
	handler := NewWSTicketHandler(rc, "unused")
	checks := 0
	handler.now = func() time.Time {
		checks++
		if checks == 1 {
			return deadline.Add(-time.Second)
		}
		return deadline.Add(time.Nanosecond)
	}
	handler.IssueTicket(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Empty(t, mr.Keys(), "an expired token must never create a persistent ticket")
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
