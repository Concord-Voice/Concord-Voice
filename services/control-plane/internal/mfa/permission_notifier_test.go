package mfa

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #3456: each of the four P1 write sites, after its commit, bumps the user's
// permission generation and THEN tells the user's own clients to re-read. The
// bump-only half is permission_invalidation_test.go; this file pins the second
// half and its order.
//
// "Bump before event" is proven by ONE recorder implementing both interfaces,
// so the two calls land in a single ordered log. "After the commit" is proven
// as in the sibling file: the recorder reads the factor tables through a pool
// the handler never uses, which sees only committed rows.

type hookCall struct {
	kind   string // "bump" or "notify"
	userID string
	state  p1Snapshot
	ctxErr error
	marker any
}

// orderedPermissionHooks is both the PermissionInvalidator and the
// PermissionChangeNotifier, logging every call in arrival order.
type orderedPermissionHooks struct {
	observer *sql.DB
	bumpErr  error // returned from every bump

	mu    sync.Mutex
	calls []hookCall
}

type notifyMarker struct{}

func (o *orderedPermissionHooks) record(ctx context.Context, kind, userID string) {
	state, _ := queryP1Snapshot(o.observer, userID)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, hookCall{
		kind: kind, userID: userID, state: state, ctxErr: ctx.Err(), marker: ctx.Value(notifyMarker{}),
	})
}

func (o *orderedPermissionHooks) BumpUserPermissionGeneration(ctx context.Context, userID string) error {
	o.record(ctx, "bump", userID)
	return o.bumpErr
}

func (o *orderedPermissionHooks) NotifyPermissionsChanged(ctx context.Context, userID string) {
	o.record(ctx, "notify", userID)
}

func (o *orderedPermissionHooks) snapshot() []hookCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]hookCall(nil), o.calls...)
}

func (o *orderedPermissionHooks) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = nil
}

// kinds returns the call kinds in order, e.g. ["bump", "notify"].
func (o *orderedPermissionHooks) kinds() []string {
	calls := o.snapshot()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.kind)
	}
	return out
}

// requireBumpThenNotify asserts the exact sequence bump, notify, both for
// userID, and returns the notify call (whose state is the committed state).
func (o *orderedPermissionHooks) requireBumpThenNotify(t *testing.T, userID string) hookCall {
	t.Helper()
	calls := o.snapshot()
	require.Len(t, calls, 2, "a committed P1 write must bump once and notify once, got %v", o.kinds())
	require.Equal(t, "bump", calls[0].kind, "the bump must run before the event")
	require.Equal(t, "notify", calls[1].kind)
	require.Equal(t, userID, calls[0].userID)
	require.Equal(t, userID, calls[1].userID, "the event goes to the actor and to no one else")
	return calls[1]
}

// eventAfterCommit is the shared failure message for the ordering assertions:
// each checks that the committed state is visible when the event is sent.
const eventAfterCommit = "the event must follow the commit"

func newNotifierFixture(
	t *testing.T, db *sql.DB, log *logger.Logger,
) (*Handler, *orderedPermissionHooks, *sql.DB, *Keyring) {
	t.Helper()
	h, _, observer, kr := newInvalidationFixture(t, db, log)
	hooks := &orderedPermissionHooks{observer: observer}
	h.SetPermissionInvalidator(hooks)
	h.SetPermissionChangeNotifier(hooks)
	return h, hooks, observer, kr
}

// Kills: the notify call removed from invalidatePermissionState (the commit arm
// sees only a bump), moved above the bump (the order), sent to another user id,
// and the hook moved before tx.Commit() (the notify arm observes an absent row).
func TestNotifyPermissionsChanged_WithMFAFactorWriteTx(t *testing.T) {
	db := iuNewTestDB(t)
	h, hooks, observer, _ := newNotifierFixture(t, db, logger.New("test"))
	ctx := context.Background()

	t.Run("a committed write bumps, then notifies, after the commit", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		err := h.withMFAFactorWriteTx(ctx, userID, emailSmsState{known: true}, func(tx *sql.Tx) error {
			return insertWebAuthnCredentialTx(ctx, tx, userID)
		})
		require.NoError(t, err)
		notify := hooks.requireBumpThenNotify(t, userID)
		assert.Equal(t, 1, notify.state.webauthn, eventAfterCommit)
	})

	t.Run("a write that fails before the commit notifies no one", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		refused := errors.New("refused before commit")
		err := h.withMFAFactorWriteTx(ctx, userID, emailSmsState{known: true}, func(tx *sql.Tx) error {
			if err := insertWebAuthnCredentialTx(ctx, tx, userID); err != nil {
				return err
			}
			return refused
		})
		require.ErrorIs(t, err, refused)
		assert.Empty(t, hooks.snapshot(), "a rolled-back write must neither bump nor notify")
		assert.Equal(t, 0, readP1Snapshot(t, observer, userID).webauthn)
	})
}

// Kills: the notify call removed, moved above the bump, or placed before the
// UPDATE (the success arm observes enabled=FALSE; the wrong-code arm notifies).
func TestNotifyPermissionsChanged_TOTPVerifySetup(t *testing.T) {
	t.Run("a committed verify bumps, then notifies, after the commit", func(t *testing.T) {
		db := iuNewTestDB(t)
		h, hooks, _, kr := newNotifierFixture(t, db, logger.New("test"))
		userID := iuCreateUser(t, db, iuPassword)
		seedPendingTOTP(t, db, kr, userID)

		code, body := runTOTPVerifySetup(t, h, userID)
		require.Equal(t, http.StatusOK, code, i5BodyFmt, body)
		notify := hooks.requireBumpThenNotify(t, userID)
		assert.True(t, notify.state.totpEnabled, eventAfterCommit)
	})

	t.Run("a wrong code notifies no one", func(t *testing.T) {
		db := iuNewTestDB(t)
		h, hooks, observer, kr := newNotifierFixture(t, db, logger.New("test"))
		userID := iuCreateUser(t, db, iuPassword)
		seedPendingTOTP(t, db, kr, userID)
		wrong := "000000"
		if wrong == iuTOTPCode(t, iuTOTPSecret) {
			wrong = "111111"
		}
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/verify-setup", fmt.Sprintf(`{"code":%q}`, wrong), userID, "")
		h.TOTPVerifySetup(c)
		require.Equal(t, http.StatusForbidden, w.Code, "%s", w.Body.String())
		assert.Empty(t, hooks.snapshot())
		assert.False(t, readP1Snapshot(t, observer, userID).totpEnabled)
	})
}

// Kills: the notify call removed from TOTPDisable, moved above tx.Commit() (the
// success arm observes the TOTP row still present), or moved into
// verifyAndDeleteTOTPTx after the DELETE (the D1 arm, which rolls back, notifies).
func TestNotifyPermissionsChanged_TOTPDisable(t *testing.T) {
	db := iuNewTestDB(t)
	h, hooks, observer, kr := newNotifierFixture(t, db, logger.New("test"))

	t.Run("a committed disable bumps, then notifies, after the commit", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		iuEnrollTOTP(t, db, kr, userID)

		code, body := runTOTPDisable(t, h, userID)
		require.Equal(t, http.StatusOK, code, i5BodyFmt, body)
		notify := hooks.requireBumpThenNotify(t, userID)
		assert.False(t, notify.state.totpRow, eventAfterCommit)
	})

	t.Run("a disable refused after its DELETE notifies no one", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		iuEnrollTOTP(t, db, kr, userID)
		enableEmailFactor(t, h, userID)

		code, body := runTOTPDisable(t, h, userID)
		require.Equal(t, http.StatusConflict, code, i5BodyFmt, body)
		require.Equal(t, true, body["inline_factor_required"], "D1 must be what refused")
		assert.Empty(t, hooks.snapshot(), "a rolled-back disable must neither bump nor notify")
		assert.True(t, readP1Snapshot(t, observer, userID).totpRow)
	})
}

// Kills: the notify call removed from WebAuthnDeleteCredential, moved above
// tx.Commit() (the success arm observes the credential still present), or moved
// into verifyAndDeleteWebAuthnTx after the DELETE (the D1 arm notifies).
func TestNotifyPermissionsChanged_WebAuthnDeleteCredential(t *testing.T) {
	db := iuNewTestDB(t)
	h, hooks, observer, _ := newNotifierFixture(t, db, logger.New("test"))

	t.Run("a committed delete bumps, then notifies, after the commit", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		credentialID := insertWebAuthnCredential(t, db, userID)

		code, body := runWebAuthnDelete(t, h, userID, credentialID)
		require.Equal(t, http.StatusOK, code, i5BodyFmt, body)
		notify := hooks.requireBumpThenNotify(t, userID)
		assert.Equal(t, 0, notify.state.webauthn, eventAfterCommit)
	})

	t.Run("a delete refused after its DELETE notifies no one", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		credentialID := insertWebAuthnCredential(t, db, userID)
		enableEmailFactor(t, h, userID)

		code, body := runWebAuthnDelete(t, h, userID, credentialID)
		require.Equal(t, http.StatusConflict, code, i5BodyFmt, body)
		require.Equal(t, true, body["inline_factor_required"], "D1 must be what refused")
		assert.Empty(t, hooks.snapshot(), "a rolled-back delete must neither bump nor notify")
		assert.Equal(t, 1, readP1Snapshot(t, observer, userID).webauthn)
	})
}

// A Commit error does not prove a rollback, so each transaction site bumps
// after Commit whatever it returned; the notification rides the same helper and
// follows the bump. A client that re-reads after a lost acknowledgement sees the
// true state either way.
//
// Kills: the notify call placed after the commit-error return at any of the
// three transaction sites (that arm then records only a bump).
func TestNotifyPermissionsChanged_CommitErrorStillNotifies(t *testing.T) {
	db := iuNewTestDB(t)
	h, hooks, _, kr := newNotifierFixture(t, db, logger.New("test"))
	ctx := context.Background()

	t.Run("withMFAFactorWriteTx", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		failCommitsOn(t, db, i5FailCommitWebAuthn, i5FailCommitWebAuthnDrop)
		err := h.withMFAFactorWriteTx(ctx, userID, emailSmsState{known: true}, func(tx *sql.Tx) error {
			return insertWebAuthnCredentialTx(ctx, tx, userID)
		})
		require.ErrorContains(t, err, "commit MFA factor write", "Commit itself must be what failed")
		hooks.requireBumpThenNotify(t, userID)
	})

	t.Run("TOTPDisable", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		iuEnrollTOTP(t, db, kr, userID)
		failCommitsOn(t, db, i5FailCommitTOTP, i5FailCommitTOTPDrop)
		code, body := runTOTPDisable(t, h, userID)
		require.Equal(t, http.StatusInternalServerError, code, i5BodyFmt, body)
		hooks.requireBumpThenNotify(t, userID)
	})

	t.Run("WebAuthnDeleteCredential", func(t *testing.T) {
		hooks.reset()
		userID := iuCreateUser(t, db, iuPassword)
		credentialID := insertWebAuthnCredential(t, db, userID)
		failCommitsOn(t, db, i5FailCommitWebAuthn, i5FailCommitWebAuthnDrop)
		code, body := runWebAuthnDelete(t, h, userID, credentialID)
		require.Equal(t, http.StatusInternalServerError, code, i5BodyFmt, body)
		hooks.requireBumpThenNotify(t, userID)
	})
}

// The helper's own contract for the notification: it follows the bump whatever
// the bump returned, it is detached from cancellation but not from values, and
// the two unwired shapes are inert.
//
// Kills: the notify guarded on the bump's error (the failed-bump arm sees no
// event), context.WithoutCancel dropped or moved after the bump (the notifier's
// ctx is Canceled), the context replaced with context.Background() (the marker
// is lost), the nil-notifier guard dropped (panic), and the nil-invalidator
// early return moved below the notify (the unwired arm notifies).
func TestInvalidatePermissionState_NotifierContract(t *testing.T) {
	newHandler := func(t *testing.T, log *logger.Logger) (*Handler, *orderedPermissionHooks) {
		t.Helper()
		db := iuNewTestDB(t)
		h, hooks, _, _ := newNotifierFixture(t, db, log)
		return h, hooks
	}

	t.Run("a failed bump is still followed by the event", func(t *testing.T) {
		var buf bytes.Buffer
		h, hooks := newHandler(t, &logger.Logger{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
		hooks.bumpErr = errors.New("redis unavailable")
		userID := uuid.NewString()

		h.invalidatePermissionState(context.Background(), userID)

		hooks.requireBumpThenNotify(t, userID)
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		assert.Len(t, lines, 1, "the failed bump logs its one line and the notification adds none: %q", buf.String())
	})

	t.Run("the notifier is detached from cancellation and keeps values", func(t *testing.T) {
		h, hooks := newHandler(t, logger.New("test"))
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), notifyMarker{}, "kept"))
		cancel()
		userID := uuid.NewString()

		h.invalidatePermissionState(ctx, userID)

		notify := hooks.requireBumpThenNotify(t, userID)
		assert.NoError(t, notify.ctxErr, "a hung-up client must not cancel the notification")
		assert.Equal(t, "kept", notify.marker, "the request's values must reach the notifier")
	})

	t.Run("an unwired notifier leaves the bump alone", func(t *testing.T) {
		h, hooks := newHandler(t, logger.New("test"))
		h.SetPermissionChangeNotifier(nil)
		userID := uuid.NewString()

		assert.NotPanics(t, func() { h.invalidatePermissionState(context.Background(), userID) })

		assert.Equal(t, []string{"bump"}, hooks.kinds())
	})

	t.Run("an unwired invalidator notifies no one", func(t *testing.T) {
		h, hooks := newHandler(t, logger.New("test"))
		h.SetPermissionInvalidator(nil)

		h.invalidatePermissionState(context.Background(), uuid.NewString())

		assert.Empty(t, hooks.snapshot(),
			"documented: the helper is a no-op without an invalidator, and the boot guard makes it unreachable")
	})
}
