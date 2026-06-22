package admin

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
)

// Admin auth rate-limit budgets (per IP). The password/webauthn/enroll steps are
// the brute-force surface; lockout (per-account + per-IP, Task 9) is the primary
// velocity control, and RateLimitByIP is the coarse outer cap that stops a flood
// from ever reaching the lockout bookkeeping.
const (
	adminAuthRateLimit   = 20
	adminAuthRateWindow  = 15 * time.Minute
	adminEnrollRateLimit = 10
)

// RegisterRoutes mounts the admin auth surface under the given router group
// (typically the bare engine, since admin lives at top-level `/admin`, NOT under
// `/api/v1`). Pre-auth routes (password/webauthn/logout/enroll) carry per-IP rate
// limiting; everything else is gated by AdminAuthRequired (deny-by-default,
// verified by the reflective routes_test).
//
// rdb is needed for the RateLimitByIP middleware; h.sessions backs AdminAuthRequired.
func RegisterRoutes(rg *gin.RouterGroup, h *Handler, rdb *redis.Client) {
	grp := rg.Group("/admin")

	// --- Pre-auth API routes (rate-limited; the token/password/key IS the auth) ---
	api := grp.Group("/api/v1")
	{
		authRoutes := api.Group("/auth")
		authRoutes.Use(middleware.RateLimitByIP(rdb, adminAuthRateLimit, adminAuthRateWindow))
		{
			authRoutes.POST("/password", h.PasswordLogin)
			authRoutes.POST("/webauthn", h.WebAuthnLogin)
			authRoutes.POST("/logout", h.Logout)
		}

		enrollRoutes := api.Group("/enroll")
		enrollRoutes.Use(middleware.RateLimitByIP(rdb, adminEnrollRateLimit, adminAuthRateWindow))
		{
			enrollRoutes.POST("/begin", h.EnrollBegin)
			enrollRoutes.POST("/finish", h.EnrollFinish)
		}

		// --- Gated routes (AdminAuthRequired — deny-by-default) ---
		gated := api.Group("/")
		gated.Use(AdminAuthRequired(h.sessions))
		{
			gated.POST("/admins", h.CreateAdmin)
			// Future read routes (#1690/#1692) mount here, behind the same gate.
		}
	}

	// --- The enrollment HTML page (pre-auth; the JS ceremony calls the pre-auth
	//     /enroll API). Rate-limited as a page fetch. ---
	grp.GET("/enroll",
		middleware.RateLimitByIP(rdb, adminEnrollRateLimit, adminAuthRateWindow),
		h.EnrollPage,
	)
}

// preAuthRoutePaths is the explicit allowlist of admin routes that are
// intentionally reachable WITHOUT an admin session. The reflective deny-by-default
// test (routes_test.go) asserts every registered /admin route NOT in this set
// rejects an unauthenticated request. Keep it in lockstep with RegisterRoutes.
var preAuthRoutePaths = map[string]struct{}{
	"/admin/api/v1/auth/password": {},
	"/admin/api/v1/auth/webauthn": {},
	"/admin/api/v1/auth/logout":   {},
	"/admin/api/v1/enroll/begin":  {},
	"/admin/api/v1/enroll/finish": {},
	"/admin/enroll":               {},
}

// IsPreAuthRoute reports whether the given route path is on the pre-auth
// allowlist. Exported for the reflective routes_test (it lives in admin_test).
func IsPreAuthRoute(path string) bool {
	_, ok := preAuthRoutePaths[path]
	return ok
}
