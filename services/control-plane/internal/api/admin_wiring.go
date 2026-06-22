//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"database/sql"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// wireAdminRoutes builds the platform-admin auth Handler (#1688) and mounts its
// routes at the top-level `/admin` group (separate from `/api/v1`). The admin
// surface is a fully isolated identity (separate WebAuthn RP, opaque Redis
// sessions, append-only audit) — it never shares the user JWT/auth middleware.
//
// On a Handler construction failure (e.g. an invalid admin WebAuthn RP config),
// the error is fatal: a control-plane that cannot stand up the admin console is
// misconfigured and must fail loudly at startup, mirroring the user-facing
// WebAuthn service's `log.Fatal` in NewRouter. The admin RP config carries dev
// defaults (config.go), so this only fires on a genuinely broken override.
//
// Mirrors the build*Handler / wiring siblings (age_wiring.go, feedback_wiring.go)
// and is invoked once from NewRouter.
//
// The surface is DORMANT by default (#1688 ships the auth backend before the
// console UI #1691 / network gating #1692/#1693): when ADMIN_CONSOLE_ENABLED is
// false the /admin routes are not mounted at all, so no inert public auth
// endpoints exist and the admin WebAuthn config is not required. The operator
// flips ADMIN_CONSOLE_ENABLED=true once the console is ready (validate() then
// requires a real RP config).
func wireAdminRoutes(router *gin.Engine, db *sql.DB, rdb *redis.Client, cfg *config.Config, log *logger.Logger) {
	if !cfg.AdminConsoleEnabled {
		log.Info("Admin console disabled (ADMIN_CONSOLE_ENABLED=false); /admin routes not mounted")
		return
	}
	handler, err := admin.NewHandler(db, rdb, log, cfg)
	if err != nil {
		log.Fatal("Failed to create admin auth handler", "error", err)
	}
	admin.RegisterRoutes(&router.RouterGroup, handler, rdb)
}
