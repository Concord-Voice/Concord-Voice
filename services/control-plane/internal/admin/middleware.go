package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// adminIDContextKey is the gin.Context key under which AdminAuthRequired stores
// the resolved admin id for downstream handlers. Handlers read it via
// c.GetString(adminIDContextKey) — it is the ADMIN identity, never a user_id.
const adminIDContextKey = "admin_id"

// AdminAuthRequired gates admin-console routes on a valid opaque admin session.
// It reads ONLY the `__Host-cv_admin_sid` cookie and resolves it through the
// SessionStore — it NEVER consults the user-facing Authorization header / JWT,
// keeping the admin identity fully isolated from end-user accounts (#1688 §6).
//
// On a valid session it sets c.Set("admin_id", <adminID>) and calls c.Next().
// On any failure (no cookie, unknown/expired/malformed sid) it aborts with 401
// and never reaches the wrapped handler — the deny-by-default posture verified by
// the reflective routes_test.
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see types.go header).
func AdminAuthRequired(sessions *SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		sid, err := c.Cookie(adminCookieName)
		if err != nil || sid == "" {
			// No admin session cookie present.
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		sess, err := sessions.Get(c.Request.Context(), sid)
		if err != nil {
			// Unknown, expired (idle or absolute), or malformed session id.
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		c.Set(adminIDContextKey, sess.AdminID)
		c.Next()
	}
}
