package dm

import (
	"context"
	"database/sql"
	"testing"

	"github.com/gin-gonic/gin"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// actorCtx gives ctx the live signed identity AuthRequired would attach for
// userID, so writers' credepoch.GuardFamilyTx can revalidate it (#2272).
func actorCtx(ctx context.Context, t testing.TB, db *sql.DB, userID string) context.Context {
	t.Helper()
	return dbtest.ActorContext(ctx, t, db, userID)
}

// withActor does the same for a hand-built gin context. Call it after
// c.Request is set.
func withActor(t testing.TB, db *sql.DB, c *gin.Context, userID string) {
	t.Helper()
	c.Request = c.Request.WithContext(actorCtx(c.Request.Context(), t, db, userID))
}
