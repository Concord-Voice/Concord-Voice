package testhelpers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// StaleBoundary schedules a real epoch rotation between successful HTTP family
// admission and the route handler. Hook independently observes the route's own
// guarded SQL; callers must assert that observation as well as RequireRotated.
// Fixtures and rotation always use the ordinary pool, never the hooked router pool.
type StaleBoundary struct {
	Hook             *stmthook.Hook
	server           *TestServer
	mu               sync.Mutex
	identity         loginfamily.AccessIdentity
	rotationExpected bool
	revokeExpected   bool
	epoch            string
	rotate           func(context.Context, string) (string, error)
}

// SetupStaleBoundaryServer uses the original API router and a separate wrapped
// pool, with no production seam or fabricated middleware context.
func SetupStaleBoundaryServer(t *testing.T) (*TestServer, *StaleBoundary) {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	hook, wrapped := stmthook.Open(t)
	ts := SetupTestServerWithRouterDB(t, wrapped)
	boundary := &StaleBoundary{Hook: hook, server: ts}
	boundary.rotate = func(ctx context.Context, userID string) (string, error) {
		return rotateCredentialEpoch(ctx, ts, userID)
	}
	return ts, boundary
}

// Arm accepts the exact signed JWT the subsequent request will present. It
// proves the SID/expiry/family binding is current before arranging the race.
func (b *StaleBoundary) Arm(token string) error { return b.arm(token, true) }

// ArmRevocation commits a revoke-one of the presented session's login family at
// the same boundary instead of rotating the epoch (#2272): the epoch stays
// current, so only a family guard can refuse the in-flight write.
func (b *StaleBoundary) ArmRevocation(token string) error {
	if err := b.arm(token, true); err != nil {
		return err
	}
	b.mu.Lock()
	b.revokeExpected = true
	b.mu.Unlock()
	return nil
}

// ArmControl observes the same commit boundary without changing authority.
func (b *StaleBoundary) ArmControl(token string) error { return b.arm(token, false) }

func (b *StaleBoundary) arm(token string, rotate bool) error {
	claims, err := auth.ValidateAccessToken(token, b.server.Config.JWTSecret)
	if err != nil {
		return fmt.Errorf("stale boundary JWT: %w", err)
	}
	user, err := uuid.Parse(claims.UserID)
	if err != nil || user == uuid.Nil || user.String() != claims.UserID {
		return errors.New("stale boundary: noncanonical user")
	}
	sid, err := uuid.Parse(claims.SessionID)
	if err != nil || sid == uuid.Nil || sid.String() != claims.SessionID {
		return errors.New("stale boundary: noncanonical SID")
	}
	if claims.ExpiresAt == nil {
		return errors.New("stale boundary: missing signed expiry")
	}
	identity := loginfamily.AccessIdentity{UserID: user, SID: sid, CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := loginfamily.ResolveVerifiedAccess(ctx, b.server.DB, identity); err != nil {
		return fmt.Errorf("stale boundary live authority: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	err = b.Hook.ArmAdmission(user.String(), sid.String(), func() error {
		if !rotate {
			return nil
		}
		b.mu.Lock()
		rotation, revoke, armed := b.rotate, b.revokeExpected, b.identity
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if revoke {
			return revokeSessionFamily(ctx, b.server.DB, armed)
		}
		epoch, err := rotation(ctx, user.String())
		b.mu.Lock()
		b.epoch = epoch
		b.mu.Unlock()
		return err
	})
	if err != nil {
		return err
	}
	b.identity, b.rotationExpected, b.revokeExpected, b.epoch = identity, rotate, false, ""
	return nil
}

// Check reports missing/unfinished observations and callback failures explicitly;
// a uniform HTTP 401 can never substitute for this boundary evidence.
func (b *StaleBoundary) Check() error {
	report := b.Hook.AdmissionReport()
	if report.Fired != 1 || !report.Completed || report.Pending || report.Running {
		return errors.New("stale boundary: exact successful auth commit not observed")
	}
	if report.CallbackErr != nil {
		return fmt.Errorf("stale boundary callback: %w", report.CallbackErr)
	}
	b.mu.Lock()
	identity, rotated, revoked, epoch := b.identity, b.rotationExpected, b.revokeExpected, b.epoch
	b.mu.Unlock()
	if report.UserID != identity.UserID.String() || report.SID != identity.SID.String() {
		return errors.New("stale boundary: admission identity differs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stored string
	if err := b.server.DB.QueryRowContext(ctx, `SELECT COALESCE(credential_epoch, '') FROM users WHERE id=$1`, identity.UserID).Scan(&stored); err != nil {
		return err
	}
	if revoked {
		var terminal bool
		if err := b.server.DB.QueryRowContext(ctx, `SELECT f.terminal_at IS NOT NULL FROM login_family_sids s
			JOIN login_families f ON f.user_id=s.user_id AND f.family_id=s.family_id
			WHERE s.user_id=$1 AND s.sid=$2`, identity.UserID, identity.SID).Scan(&terminal); err != nil {
			return err
		}
		if !terminal || stored != identity.CredentialEpoch {
			return errors.New("stale boundary: committed family revocation with an unchanged epoch missing")
		}
		return nil
	}
	if !rotated {
		if stored != identity.CredentialEpoch {
			return errors.New("stale boundary: control changed epoch")
		}
		return nil
	}
	return requireRotatedEpoch(identity.CredentialEpoch, stored, epoch)
}

func requireRotatedEpoch(previous, stored, epoch string) error {
	if len(epoch) != 32 || epoch == previous || stored != epoch {
		return errors.New("stale boundary: distinct durable rotated epoch missing")
	}
	for _, c := range epoch {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errors.New("stale boundary: noncanonical rotated epoch")
		}
	}
	return nil
}

// RequireRotated checks the fixture, independently of each caller's guard and
// domain-state assertions. Control arms must be checked with Check instead.
func (b *StaleBoundary) RequireRotated(t *testing.T) {
	t.Helper()
	require.NoError(t, b.Check())
	b.mu.Lock()
	rotated := b.rotationExpected
	b.mu.Unlock()
	require.True(t, rotated, "a no-rotation control cannot prove an epoch race")
}

// RequireRevoked checks an ArmRevocation fixture: the admitted session's family
// was terminalized after admission while its epoch stayed current.
func (b *StaleBoundary) RequireRevoked(t *testing.T) {
	t.Helper()
	require.NoError(t, b.Check())
	b.mu.Lock()
	revoked := b.revokeExpected
	b.mu.Unlock()
	require.True(t, revoked, "only an ArmRevocation fixture proves a family-revocation race")
}

// revokeSessionFamily commits the revoke-one shape: barrier, users FOR NO KEY
// UPDATE, the SID's family, then the auth-owned terminal operation.
func revokeSessionFamily(ctx context.Context, db *sql.DB, identity loginfamily.AccessIdentity) (retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		return err
	}
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return err
	}
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{identity.UserID}, loginfamily.UserNoKeyUpdate)
	if err != nil {
		return err
	}
	user, err := users.ForUser(identity.UserID)
	if err != nil {
		return err
	}
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, tx, user, identity.SID)
	if err != nil {
		return err
	}
	if _, err := loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonExplicitRevoke}); err != nil {
		return err
	}
	return tx.Commit()
}

// RotateCredentialEpoch is for races that already have their own later body or
// SQL-lock barrier. It does not move those tests' existing linearization point.
func (ts *TestServer) RotateCredentialEpoch(t *testing.T, userID string) string {
	t.Helper()
	epoch, err := rotateCredentialEpoch(t.Context(), ts, userID)
	require.NoError(t, err)
	return epoch
}

func rotateCredentialEpoch(parent context.Context, ts *TestServer, userID string) (epoch string, retErr error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	fence := credepoch.New(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard))
	op, err := fence.Begin(ctx, userID)
	if err != nil {
		return "", err
	}
	tx, err := ts.DB.BeginTx(ctx, nil)
	if err != nil {
		op.Rollback(ctx)
		return "", err
	}
	definite := false
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rotation rollback: %w", rollbackErr))
		}
		if !definite && rollbackErr == nil {
			op.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		return "", err
	}
	if _, err := loginfamily.AcquireWriterBarrierTx(ctx, tx); err != nil {
		return "", err
	}
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&locked); err != nil {
		return "", err
	}
	if _, err := op.RotateTx(ctx, tx); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	definite = true
	op.Commit(context.WithoutCancel(ctx))
	return op.NewEpochValue(), nil
}
