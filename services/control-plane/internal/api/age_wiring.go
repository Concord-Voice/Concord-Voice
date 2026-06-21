//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"database/sql"

	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/age"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// buildAgeHandler constructs the #1623 age-verification claim handler. The WebSocket
// hub satisfies age.SessionDisconnector for the terminal-disable live-session kick.
// Extracted (like build*Handler siblings) so NewRouter's cognitive complexity stays
// under the SonarCloud threshold.
func buildAgeHandler(db *sql.DB, rdb *redis.Client, hub age.SessionDisconnector, log *logger.Logger) *age.Handler {
	return age.NewHandler(db, rdb, hub, log)
}
