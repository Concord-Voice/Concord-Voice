package mfa

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// The step-up gate's login-family guard (#2272, credepoch.GuardFamilyTx) needs
// the signed identity AuthRequired attaches. Hand-built gin contexts attach
// one here: the live identity minted for a PostgreSQL user, else a synthetic
// one that the fake databases' dbtest.FakeFamilyGuard answers for.

// liveFamilies holds the identity minted for each user a PostgreSQL test
// created, so a hand-built context can carry it without holding the database.
var liveFamilies sync.Map

// mintLiveFamily mints userID a live family in db. Call it before anything
// locks the users row: minting takes that lock.
func mintLiveFamily(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	identity, ok := loginfamily.AccessIdentityFromContext(dbtest.ActorContext(context.Background(), t, db, userID))
	require.True(t, ok)
	liveFamilies.Store(userID, identity)
}

// attachFamily gives c the identity AuthRequired would. A non-UUID user gets
// none, and the guard refuses it as production would.
func attachFamily(c *gin.Context, userID string) {
	if identity, ok := liveFamilies.Load(userID); ok {
		c.Request = c.Request.WithContext(loginfamily.WithAccessIdentity(c.Request.Context(), identity.(loginfamily.AccessIdentity)))
		return
	}
	if _, err := uuid.Parse(userID); err == nil {
		c.Request = c.Request.WithContext(dbtest.WithFakeFamily(c.Request.Context(), userID, ""))
	}
}
