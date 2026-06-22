package admin

import (
	"context"
	"database/sql"
	"io"

	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

// export_test.go exposes a few unexported helpers to the external `admin_test`
// package so behavioral tests can exercise them without widening the public API
// surface. This is the idiomatic Go "export for test" bridge; it compiles only
// under `go test` (the _test.go suffix), so the production API stays minimal.

// CheckAAGUIDForTest is a test-only wrapper around the unexported checkAAGUID,
// the AAGUID allow-list gate enforced at admin WebAuthn enrollment (#1688 §8).
func CheckAAGUIDForTest(aaguid []byte, allowed []string) error {
	return checkAAGUID(aaguid, allowed)
}

// EnrollBaseURLForTest exposes the unexported enrollBaseURL helper to the
// external admin_test package so table tests can exercise both branches
// (configured origin vs. localhost fallback) without changing the production API.
func EnrollBaseURLForTest(cfg *config.Config) string {
	return enrollBaseURL(cfg)
}

// RunAdminCtlForTest drives the unexported runAdminCtl with fully injected deps
// (isolated DB + Redis + in-memory stdin/stdout), so adminctl_test.go can
// exercise the bootstrap / reset-enrollment verbs without a real TTY or the
// production config.Load + db/redis-open path. Returns the verb's exit code.
func RunAdminCtlForTest(
	ctx context.Context,
	db *sql.DB,
	rdb *redis.Client,
	stdin io.Reader,
	stdout io.Writer,
	enrollBaseURL string,
	args []string,
) int {
	deps := adminCtlDeps{
		repo:          NewAdminRepo(db),
		audit:         NewAuditLog(db),
		enroll:        NewEnrollmentStore(rdb),
		enrollBaseURL: enrollBaseURL,
		stdin:         stdin,
		stdout:        stdout,
	}
	return runAdminCtl(ctx, deps, args)
}
