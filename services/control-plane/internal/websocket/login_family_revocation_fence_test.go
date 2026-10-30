package websocket

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/sessions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// revocationFencePassword is a throwaway fixture credential for the revoke step-up.
const revocationFencePassword = "Fence-fixture-password-123!" //nolint:gosec // test fixture, not a real secret // pragma: allowlist secret

// Regression for the PR #4135 red-team PoC: DELETE /sessions/:id answered 200
// while the revoked family's open control socket kept full send/receive
// authority until the 5s reconciler ticker. No reconciler runs here, so the
// only thing that can close the socket is the handler's synchronous fence.
func TestRevokeSessionFencesRevokedSocketBeforeResponse(t *testing.T) {
	r := loginFamilyRig(t)
	_, err := r.db.Exec(`INSERT INTO login_family_rollout(id) VALUES (TRUE) ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	hash, err := auth.HashPassword(revocationFencePassword)
	require.NoError(t, err)
	_, err = r.db.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, r.user, hash)
	require.NoError(t, err)
	// An unrelated second login of the same user must survive the revoke.
	siblingFamily, siblingSID := uuid.New(), uuid.New()
	_, err = r.db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, siblingFamily, r.user)
	require.NoError(t, err)
	_, err = r.db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, siblingSID, siblingFamily, r.user, r.expiry)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws", r.h.HandleWebSocket)
	srv := httptest.NewServer(router)
	defer srv.Close()
	go r.h.hub.Run()
	t.Cleanup(r.h.hub.Shutdown)

	dial := func(sid uuid.UUID) *ws.Conn {
		t.Helper()
		conn, resp, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
			http.Header{"Authorization": []string{"Bearer " + r.token(t, r.user, sid, "", r.expiry)}})
		require.NoError(t, err)
		_ = resp.Body.Close()
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	// awaitType reads until the wanted frame; any read error means the socket closed.
	awaitType := func(conn *ws.Conn, want string, within time.Duration) error {
		deadline := time.Now().Add(within)
		for {
			require.NoError(t, conn.SetReadDeadline(deadline))
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return err
			}
			var msg struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &msg)
			if msg.Type == want {
				return nil
			}
		}
	}
	revoked, sibling := dial(r.sid), dial(siblingSID)
	require.NoError(t, awaitType(revoked, "connected", 5*time.Second))
	require.NoError(t, awaitType(sibling, "connected", 5*time.Second))

	// The real DELETE /sessions/:id handler, authenticated as the sibling login.
	handler := sessions.NewHandler(r.db, r.h.redis, logger.NewWithWriter(io.Discard), r.h.hub, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/sessions/"+r.sid.String(), strings.NewReader(`{"password":"`+revocationFencePassword+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: r.sid.String()}}
	c.Set("user_id", r.user.String())
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"user_id": r.user.String(), "sid": siblingSID.String(), "cred_epoch": "", "exp": float64(r.expiry.Unix())})
	// AuthRequired attaches the requester's signed identity; the revoke revalidates it.
	c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(),
		loginfamily.AccessIdentity{UserID: r.user, SID: siblingSID, AccessExpiresAt: r.expiry}))
	handler.RevokeSession(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Immediately after the 200 — far inside the reconciler's 5s tick — the
	// revoked socket must neither answer a frame nor stay open.
	_ = revoked.WriteJSON(map[string]any{"type": "connection_ready_probe"})
	require.Error(t, awaitType(revoked, "connection_ready", 2*time.Second),
		"revoked family's socket must be closed before the revoke response, not answer frames")

	// The unrelated login keeps full authority.
	require.NoError(t, sibling.WriteJSON(map[string]any{"type": "connection_ready_probe"}))
	require.NoError(t, awaitType(sibling, "connection_ready", 3*time.Second),
		"an unrelated session of the same user must stay live")
}
