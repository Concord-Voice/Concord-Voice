package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func ticketFamilyRevoke(t *testing.T, rig familyTestRig, sid string) {
	t.Helper()
	ctx := context.Background()
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
	targets, err := lf.ResolveTargetSIDTx(ctx, tx, user, uuid.MustParse(sid))
	require.NoError(t, err)
	_, err = lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}
func ticketFamilyRequest(t *testing.T, rig familyTestRig, access string, between gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	chain := []gin.HandlerFunc{middleware.AuthRequired(familyTestSecret, rig.h.redis, rig.db, nil)}
	if between != nil {
		chain = append(chain, between)
	}
	chain = append(chain, NewWSTicketHandler(rig.h.redis, familyTestSecret).IssueTicket)
	r.POST("/ticket", chain...)
	req := httptest.NewRequest(http.MethodPost, "/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("X-Session-ID", uuid.NewString())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func ticketFamilyValue(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Ticket, 64)
	return body.Ticket
}

func TestLoginFamilyTicketRealMintRetainsFourPartIdentityAndSingleUse(t *testing.T) {
	rig := newFamilyTestRig(t)
	access, _, sid, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
	require.NoError(t, err)
	claims, err := ValidateAccessToken(access, familyTestSecret)
	require.NoError(t, err)
	ticket := ticketFamilyValue(t, ticketFamilyRequest(t, rig, access, nil))
	stored, err := rig.h.redis.Get(t.Context(), wsTicketPrefix+ticket).Result()
	require.NoError(t, err)
	parts := strings.Split(stored, ":")
	require.Len(t, parts, 4, "family ID must be re-resolved, never added as a fifth ticket field")
	require.Equal(t, rig.user, parts[0])
	require.Equal(t, sid, parts[1])
	require.Equal(t, claims.CredentialEpoch, parts[2])
	ttl, err := rig.h.redis.TTL(t.Context(), wsTicketPrefix+ticket).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, 30*time.Second)
	// Concurrent consumers reach the actual Redis Lua consume boundary.
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user, session, epoch, expiry, err := ValidateTicket(context.Background(), rig.h.redis, ticket)
			if err == nil {
				if user != rig.user || session != sid || epoch != claims.CredentialEpoch || !expiry.Equal(claims.ExpiresAt.Time) {
					err = errors.New("ticket identity changed")
				}
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
}

func TestLoginFamilyTicketRechecksFamilyAfterHTTPAdmission(t *testing.T) {
	rig := newFamilyTestRig(t)
	access, _, sid, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
	require.NoError(t, err)
	w := ticketFamilyRequest(t, rig, access, func(c *gin.Context) { ticketFamilyRevoke(t, rig, sid); c.Next() })
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
	keys, err := rig.h.redis.Keys(t.Context(), wsTicketPrefix+"*").Result()
	require.NoError(t, err)
	require.Empty(t, keys, "no credential may be published from a stale HTTP family snapshot")
}

func TestLoginFamilyTicketDoesNotTrustMutatedHTTPClaims(t *testing.T) {
	rig := newFamilyTestRig(t)
	access, _, _, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
	require.NoError(t, err)
	w := ticketFamilyRequest(t, rig, access, func(c *gin.Context) { c.Set("user_id", uuid.NewString()); c.Next() })
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	keys, err := rig.h.redis.Keys(t.Context(), wsTicketPrefix+"*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestLoginFamilyTicketUnavailableRecheckPublishesNothing(t *testing.T) {
	rig := newFamilyTestRig(t)
	access, _, _, err := rig.h.IssueAccessAndRefresh(t.Context(), rig.user)
	require.NoError(t, err)
	w := ticketFamilyRequest(t, rig, access, func(c *gin.Context) {
		_, err := rig.db.Exec(`ALTER TABLE login_family_sids RENAME TO ticket_family_fixture_unavailable`)
		require.NoError(t, err)
		defer func() {
			_, err := rig.db.Exec(`ALTER TABLE ticket_family_fixture_unavailable RENAME TO login_family_sids`)
			require.NoError(t, err)
		}()
		c.Next()
	})
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, w.Body.String())
	keys, err := rig.h.redis.Keys(t.Context(), wsTicketPrefix+"*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
}
