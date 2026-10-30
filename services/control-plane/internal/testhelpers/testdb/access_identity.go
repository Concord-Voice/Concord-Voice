package testdb

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
)

// ActorContext returns ctx carrying a live access identity for userID, the
// value middleware.AuthRequired attaches and credepoch.GuardFamilyTx
// revalidates. It commits a fresh family and SID binding (no refresh row), so
// it is for tests that call writers below the HTTP layer.
func ActorContext(ctx context.Context, t testing.TB, db *sql.DB, userID string) context.Context {
	t.Helper()
	id, err := uuid.Parse(userID)
	if err != nil {
		t.Fatalf("testdb: actor identity user: %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("testdb: begin actor identity: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		t.Fatalf("testdb: actor identity barrier: %v", err)
	}
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{id}, loginfamily.UserNoKeyUpdate)
	if err != nil {
		t.Fatalf("testdb: actor identity user lock: %v", err)
	}
	user, err := users.ForUser(id)
	if err != nil {
		t.Fatalf("testdb: actor identity authority: %v", err)
	}
	var epoch string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`, id).Scan(&epoch); err != nil {
		t.Fatalf("testdb: actor identity epoch: %v", err)
	}
	family, err := loginfamily.CreateFamilyTx(ctx, tx, user, epoch)
	if err != nil {
		t.Fatalf("testdb: actor identity family: %v", err)
	}
	sid := uuid.New()
	expiry := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	if err := loginfamily.BindSIDTx(ctx, tx, family, sid, expiry); err != nil {
		t.Fatalf("testdb: actor identity SID: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("testdb: commit actor identity: %v", err)
	}
	return loginfamily.WithAccessIdentity(ctx, loginfamily.AccessIdentity{UserID: id, SID: sid, CredentialEpoch: epoch, AccessExpiresAt: expiry})
}

// rememberedActors holds one identity per user RememberActor minted.
var rememberedActors sync.Map

// RememberActor mints userID one live identity and keeps it, so a hand-built
// request context that knows only the user ID can carry it. Call it when the
// user is created, before anything locks the users row: minting locks it.
func RememberActor(t testing.TB, db *sql.DB, userID string) {
	t.Helper()
	identity, _ := loginfamily.AccessIdentityFromContext(ActorContext(context.Background(), t, db, userID))
	rememberedActors.Store(userID, identity)
}

// WithRememberedActor attaches userID's remembered identity. An unknown ID
// gets none, so a guarded writer refuses it as AuthRequired would have.
func WithRememberedActor(ctx context.Context, userID string) context.Context {
	identity, ok := rememberedActors.Load(userID)
	if !ok {
		return ctx
	}
	return loginfamily.WithAccessIdentity(ctx, identity.(loginfamily.AccessIdentity))
}

// AttachRememberedActor is WithRememberedActor for a gin test context. Call
// it after c.Request is set.
func AttachRememberedActor(c *gin.Context, userID string) {
	if c.Request == nil {
		c.Request = &http.Request{}
	}
	c.Request = c.Request.WithContext(WithRememberedActor(c.Request.Context(), userID))
}
