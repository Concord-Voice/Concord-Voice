//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"database/sql"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/privacy"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// buildPrivacyHandler constructs the privacy handler wired to the
// account-deletion service. Telemetry-related wiring was removed in #758 (sub-epic G);
// the handler now exposes only POST /api/v1/privacy/erase-account.
func buildPrivacyHandler(db *sql.DB, log *logger.Logger) *privacy.Handler {
	account := users.NewAccountService(db, log)
	return privacy.NewHandler(account, log)
}
