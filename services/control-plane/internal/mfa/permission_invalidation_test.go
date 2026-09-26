package mfa

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// I-5 (#3453 spec §13): each of the four P1 write sites bumps the user's
// permission generation exactly once, AFTER its commit, and never when the
// request fails before the commit. The static half, that these are the only
// P1 writers, is p1_writer_pin_test.go.
//
// "After the commit" is proven by the spy: when it is called it reads the
// factor tables through a pool the handler never uses, which sees only
// committed rows. A bump that ran before the commit would record the state
// the commit was about to replace.

// p1Snapshot is the committed factor state the spy observes.
type p1Snapshot struct {
	totpRow     bool
	totpEnabled bool
	webauthn    int
}

type permissionBump struct {
	userID   string
	state    p1Snapshot
	stateErr error
	ctxErr   error
	marker   any
}

// permissionBumpSpy records every bump with the committed factor state it
// read through observer at the moment it was called.
type permissionBumpSpy struct {
	observer *sql.DB
	err      error // returned from every call

	mu    sync.Mutex
	calls []permissionBump
}

type permissionBumpMarker struct{}

// i5BodyFmt prints a handler's decoded response body beside a status mismatch.
const i5BodyFmt = "body: %v"

func (s *permissionBumpSpy) BumpUserPermissionGeneration(ctx context.Context, userID string) error {
	state, stateErr := queryP1Snapshot(s.observer, userID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, permissionBump{
		userID: userID, state: state, stateErr: stateErr, ctxErr: ctx.Err(), marker: ctx.Value(permissionBumpMarker{}),
	})
	return s.err
}

func (s *permissionBumpSpy) bumps() []permissionBump {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]permissionBump(nil), s.calls...)
}

func (s *permissionBumpSpy) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

// requireOneBump asserts exactly one bump, for userID, and returns it.
func (s *permissionBumpSpy) requireOneBump(t *testing.T, userID string) permissionBump {
	t.Helper()
	bumps := s.bumps()
	require.Len(t, bumps, 1, "a committed P1 write must bump exactly once")
	require.Equal(t, userID, bumps[0].userID)
	require.NoError(t, bumps[0].stateErr, "the spy could not read the committed state")
	return bumps[0]
}

func queryP1Snapshot(db *sql.DB, userID string) (p1Snapshot, error) {
	var s p1Snapshot
	err := db.QueryRowContext(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = $1),
		       COALESCE((SELECT enabled FROM user_mfa_totp WHERE user_id = $1), FALSE),
		       (SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1)`, userID,
	).Scan(&s.totpRow, &s.totpEnabled, &s.webauthn)
	return s, err
}

func readP1Snapshot(t *testing.T, db *sql.DB, userID string) p1Snapshot {
	t.Helper()
	s, err := queryP1Snapshot(db, userID)
	require.NoError(t, err)
	return s
}

// newInvalidationFixture builds a handler over handlerDB and a private Redis
// database, wired to a spy that observes through a separate pool.
func newInvalidationFixture(t *testing.T, handlerDB *sql.DB, log *logger.Logger) (*Handler, *permissionBumpSpy, *sql.DB, *Keyring) {
	t.Helper()
	observer, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = observer.Close() })
	kr := iuKeyring(t)
	h := NewHandler(handlerDB, redistest.Client(t), log, kr, "test", nil, "test")
	spy := &permissionBumpSpy{observer: observer}
	h.SetPermissionInvalidator(spy)
	return h, spy, observer, kr
}

func insertWebAuthnCredentialTx(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key, sign_count, credential_name, credential_type)
		VALUES ($1, $2, $3, 0, 'Key', 'hardware')`, userID, []byte("i5-cred-"+uuid.NewString()), []byte("i5-public-key"))
	return err
}

func insertWebAuthnCredential(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO user_mfa_webauthn (id, user_id, credential_id, public_key, sign_count, credential_name, credential_type)
		VALUES ($1, $2, $3, $4, 0, 'Key', 'hardware')`, id, userID, []byte("i5-cred-"+id), []byte("i5-public-key"))
	require.NoError(t, err)
	return id
}

// enableEmailFactor turns email codes on, which makes removing the last inline
// factor a D1 refusal: the delete runs, then the transaction rolls back.
func enableEmailFactor(t *testing.T, h *Handler, userID string) {
	t.Helper()
	require.NoError(t, h.redis.Set(context.Background(), fmt.Sprintf(redisKeyEmailSmsEnabledEmail, userID), "1", 0).Err())
}

// Kills: the hook removed from withMFAFactorWriteTx (the commit arm sees no
// bump), the hook moved before tx.Commit() (the commit arm observes the
// uncommitted insert as absent), and the hook moved above the write or its
// error return (the failure arm bumps).
func TestInvalidatePermissionState_WithMFAFactorWriteTx(t *testing.T) {
	db := iuNewTestDB(t)
	h, spy, observer, _ := newInvalidationFixture(t, db, logger.New("test"))
	ctx := context.Background()

	t.Run("a committed write bumps once, after the commit", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		err := h.withMFAFactorWriteTx(ctx, userID, emailSmsState{known: true}, func(tx *sql.Tx) error {
			return insertWebAuthnCredentialTx(ctx, tx, userID)
		})
		require.NoError(t, err)
		bump := spy.requireOneBump(t, userID)
		assert.Equal(t, 1, bump.state.webauthn, "the bump must see the committed credential")
	})

	t.Run("a write that fails before the commit does not bump", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		refused := errors.New("refused before commit")
		err := h.withMFAFactorWriteTx(ctx, userID, emailSmsState{known: true}, func(tx *sql.Tx) error {
			if err := insertWebAuthnCredentialTx(ctx, tx, userID); err != nil {
				return err
			}
			return refused
		})
		require.ErrorIs(t, err, refused)
		assert.Empty(t, spy.bumps(), "a rolled-back write must not bump")
		assert.Equal(t, 0, readP1Snapshot(t, observer, userID).webauthn, "the write rolled back")
	})
}

// seedPendingTOTP inserts the row TOTPSetup leaves behind: enabled and
// confirmed FALSE, awaiting verify-setup.
func seedPendingTOTP(t *testing.T, db *sql.DB, kr *Keyring, userID string) {
	t.Helper()
	enc, nonce, keyVersion, err := kr.Seal([]byte(iuTOTPSecret))
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		VALUES ($1, $2, $3, $4, FALSE, FALSE)`, userID, enc, nonce, keyVersion)
	require.NoError(t, err)
}

func runTOTPVerifySetup(t *testing.T, h *Handler, userID string) (int, map[string]interface{}) {
	t.Helper()
	body := fmt.Sprintf(`{"code":%q}`, iuTOTPCode(t, iuTOTPSecret))
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/verify-setup", body, userID, "")
	h.TOTPVerifySetup(c)
	return w.Code, iuBody(t, w)
}

// Kills: the hook removed from TOTPVerifySetup (the success arm sees no bump),
// the hook moved above the UPDATE (the success arm observes enabled=FALSE, and
// the failure arm bumps).
func TestInvalidatePermissionState_TOTPVerifySetup(t *testing.T) {
	t.Run("a committed verify bumps once, after the commit", func(t *testing.T) {
		db := iuNewTestDB(t)
		h, spy, _, kr := newInvalidationFixture(t, db, logger.New("test"))
		userID := iuCreateUser(t, db, iuPassword)
		seedPendingTOTP(t, db, kr, userID)

		code, body := runTOTPVerifySetup(t, h, userID)
		require.Equal(t, http.StatusOK, code, i5BodyFmt, body)
		bump := spy.requireOneBump(t, userID)
		assert.True(t, bump.state.totpEnabled, "the bump must see the committed enabled=TRUE")
	})

	t.Run("an UPDATE that fails does not bump", func(t *testing.T) {
		setupDB := iuNewTestDB(t)
		userID := iuCreateUser(t, setupDB, iuPassword)
		seedPendingTOTP(t, setupDB, iuKeyring(t), userID)

		// A single-connection pool with a short lock_timeout, while another
		// transaction holds the TOTP row: the SELECT reads through the lock and
		// the code checks out, so the UPDATE is the first thing that fails.
		lockedDB, err := sql.Open("postgres", dbtest.DatabaseURL())
		require.NoError(t, err)
		t.Cleanup(func() { _ = lockedDB.Close() })
		lockedDB.SetMaxOpenConns(1)
		lockedDB.SetMaxIdleConns(1)
		_, err = lockedDB.Exec(`SET lock_timeout = '100ms'`)
		require.NoError(t, err)
		holder, err := setupDB.Begin()
		require.NoError(t, err)
		t.Cleanup(func() { _ = holder.Rollback() })
		_, err = holder.Exec(`SELECT 1 FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE`, userID)
		require.NoError(t, err)

		h, spy, observer, _ := newInvalidationFixture(t, lockedDB, logger.New("test"))
		code, body := runTOTPVerifySetup(t, h, userID)
		require.Equal(t, http.StatusInternalServerError, code)
		require.Equal(t, "Failed to complete verification", body["error"], "the UPDATE must be what failed")
		assert.Empty(t, spy.bumps(), "a failed UPDATE must not bump")
		assert.False(t, readP1Snapshot(t, observer, userID).totpEnabled)
	})
}

func runTOTPDisable(t *testing.T, h *Handler, userID string) (int, map[string]interface{}) {
	t.Helper()
	body := fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, iuTOTPCode(t, iuTOTPSecret))
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable", body, userID, "")
	h.TOTPDisable(c)
	return w.Code, iuBody(t, w)
}

// Kills: the hook removed from TOTPDisable (the success arm sees no bump), the
// hook moved above tx.Commit() (the success arm observes the TOTP row still
// present), and the hook moved into verifyAndDeleteTOTPTx after the DELETE (the
// D1 arm, which deletes and then rolls back, bumps).
func TestInvalidatePermissionState_TOTPDisable(t *testing.T) {
	db := iuNewTestDB(t)
	h, spy, observer, kr := newInvalidationFixture(t, db, logger.New("test"))

	t.Run("a committed disable bumps once, after the commit", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		iuEnrollTOTP(t, db, kr, userID)

		code, body := runTOTPDisable(t, h, userID)
		require.Equal(t, http.StatusOK, code, i5BodyFmt, body)
		bump := spy.requireOneBump(t, userID)
		assert.False(t, bump.state.totpRow, "the bump must see the committed DELETE")
	})

	t.Run("a disable refused after its DELETE does not bump", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		iuEnrollTOTP(t, db, kr, userID)
		enableEmailFactor(t, h, userID)

		code, body := runTOTPDisable(t, h, userID)
		require.Equal(t, http.StatusConflict, code, i5BodyFmt, body)
		require.Equal(t, true, body["inline_factor_required"], "D1 must be what refused")
		assert.Empty(t, spy.bumps(), "a rolled-back disable must not bump")
		assert.True(t, readP1Snapshot(t, observer, userID).totpRow, "the DELETE rolled back")
	})
}

func runWebAuthnDelete(t *testing.T, h *Handler, userID, credentialID string) (int, map[string]interface{}) {
	t.Helper()
	body := fmt.Sprintf(`{"password":%q}`, iuPassword)
	c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/webauthn/credentials/"+credentialID, body, userID, "")
	c.Params = gin.Params{{Key: "id", Value: credentialID}}
	h.WebAuthnDeleteCredential(c)
	return w.Code, iuBody(t, w)
}

// Kills: the hook removed from WebAuthnDeleteCredential (the success arm sees
// no bump), the hook moved above tx.Commit() (the success arm observes the
// credential still present), and the hook moved into verifyAndDeleteWebAuthnTx
// after the DELETE (the D1 arm bumps).
func TestInvalidatePermissionState_WebAuthnDeleteCredential(t *testing.T) {
	db := iuNewTestDB(t)
	h, spy, observer, _ := newInvalidationFixture(t, db, logger.New("test"))

	t.Run("a committed delete bumps once, after the commit", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		credentialID := insertWebAuthnCredential(t, db, userID)

		code, body := runWebAuthnDelete(t, h, userID, credentialID)
		require.Equal(t, http.StatusOK, code, i5BodyFmt, body)
		bump := spy.requireOneBump(t, userID)
		assert.Equal(t, 0, bump.state.webauthn, "the bump must see the committed DELETE")
	})

	t.Run("a delete refused after its DELETE does not bump", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		credentialID := insertWebAuthnCredential(t, db, userID)
		enableEmailFactor(t, h, userID)

		code, body := runWebAuthnDelete(t, h, userID, credentialID)
		require.Equal(t, http.StatusConflict, code, i5BodyFmt, body)
		require.Equal(t, true, body["inline_factor_required"], "D1 must be what refused")
		assert.Empty(t, spy.bumps(), "a rolled-back delete must not bump")
		assert.Equal(t, 1, readP1Snapshot(t, observer, userID).webauthn, "the DELETE rolled back")
	})
}

// permBumpLogRecords returns the JSON records in buf whose failure_class is
// perm_generation_bump, without their timestamps.
func permBumpLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		if rec["failure_class"] == "perm_generation_bump" {
			delete(rec, "time")
			out = append(out, rec)
		}
	}
	return out
}

// The helper's own contract: detached from cancellation but not from values,
// a no-op when unwired, and on failure ONE log line that carries nothing about
// which way the factor state moved (I7, observability principle 7).
//
// Kills: context.WithoutCancel dropped (ctxErr is Canceled), the context
// replaced with context.Background() (the marker is lost), the nil guard
// dropped (panic), and any field added to the failure line (the key set).
func TestInvalidatePermissionState_Contract(t *testing.T) {
	t.Run("detaches cancellation and keeps values", func(t *testing.T) {
		db := iuNewTestDB(t)
		h, spy, _, _ := newInvalidationFixture(t, db, logger.New("test"))
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), permissionBumpMarker{}, "kept"))
		cancel()
		userID := uuid.NewString()
		h.invalidatePermissionState(ctx, userID)
		bump := spy.requireOneBump(t, userID)
		assert.NoError(t, bump.ctxErr, "a hung-up client must not cancel the bump")
		assert.Equal(t, "kept", bump.marker, "the request's values must reach the invalidator")
	})

	t.Run("an unwired handler is a no-op", func(t *testing.T) {
		var buf bytes.Buffer
		h := &Handler{log: &logger.Logger{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}}
		assert.NotPanics(t, func() { h.invalidatePermissionState(context.Background(), uuid.NewString()) })
		assert.Empty(t, buf.String())
	})

	t.Run("a failed bump logs one direction-free line", func(t *testing.T) {
		var buf bytes.Buffer
		db := iuNewTestDB(t)
		h, spy, _, _ := newInvalidationFixture(t, db, &logger.Logger{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
		spy.err = errors.New("redis unavailable")

		// A gain, through withMFAFactorWriteTx, then a loss, through
		// WebAuthnDeleteCredential. Their failure lines must be identical.
		gainer := iuCreateUser(t, db, iuPassword)
		require.NoError(t, h.withMFAFactorWriteTx(context.Background(), gainer, emailSmsState{known: true},
			func(tx *sql.Tx) error { return insertWebAuthnCredentialTx(context.Background(), tx, gainer) }))
		loser := iuCreateUser(t, db, iuPassword)
		code, body := runWebAuthnDelete(t, h, loser, insertWebAuthnCredential(t, db, loser))
		require.Equal(t, http.StatusOK, code, "a failed bump must not fail the committed request; "+i5BodyFmt, body)

		records := permBumpLogRecords(t, &buf)
		require.Len(t, records, 2, "one line per failed bump")
		assert.Equal(t, records[0], records[1], "a gain and a loss must log the same line")
		keys := make([]string, 0, len(records[0]))
		for k := range records[0] {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, []string{"level", "msg", "failure_class", "error"}, keys,
			"the line may carry the failure class and the error, nothing that says which way the factor moved")
	})
}

// Constant DDL: a deferred constraint trigger that raises at COMMIT, so every
// statement in the transaction succeeds and tx.Commit() itself returns the
// error. Fixed identifiers, created per subtest and dropped on cleanup.
const (
	i5FailCommitFunc = `CREATE OR REPLACE FUNCTION i5_fail_commit() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'i5 forced commit failure'; END $$`
	i5FailCommitDropFunc = `DROP FUNCTION IF EXISTS i5_fail_commit()`

	i5FailCommitTOTP = `CREATE CONSTRAINT TRIGGER i5_fail_commit AFTER INSERT OR DELETE ON user_mfa_totp
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION i5_fail_commit()`
	i5FailCommitTOTPDrop = `DROP TRIGGER IF EXISTS i5_fail_commit ON user_mfa_totp`

	i5FailCommitWebAuthn = `CREATE CONSTRAINT TRIGGER i5_fail_commit AFTER INSERT OR DELETE ON user_mfa_webauthn
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION i5_fail_commit()`
	i5FailCommitWebAuthnDrop = `DROP TRIGGER IF EXISTS i5_fail_commit ON user_mfa_webauthn`
)

// failCommitsOn makes every later transaction that writes the table fail at
// COMMIT, until the test ends. Seed fixtures BEFORE calling it.
func failCommitsOn(t *testing.T, db *sql.DB, create, drop string) {
	t.Helper()
	_, err := db.Exec(i5FailCommitFunc)
	require.NoError(t, err)
	_, err = db.Exec(drop)
	require.NoError(t, err)
	_, err = db.Exec(create)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(drop)
		_, _ = db.Exec(i5FailCommitDropFunc)
	})
}

// A Commit error does not prove a rollback: the server can commit and the
// acknowledgement be lost, and a factor removal that did apply but skipped the
// bump would leave the user's dangerous bits cached until the TTL. So each
// transaction site bumps after Commit whatever Commit returned. The forced
// failure here really does roll back, which is exactly what the handler cannot
// tell apart from a lost acknowledgement: it must bump anyway.
//
// Kills: the bump placed after the commit-error return at any of the three
// transaction sites (that arm then records no bump).
func TestInvalidatePermissionState_CommitErrorStillBumps(t *testing.T) {
	db := iuNewTestDB(t)
	h, spy, observer, kr := newInvalidationFixture(t, db, logger.New("test"))
	ctx := context.Background()

	t.Run("withMFAFactorWriteTx", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		failCommitsOn(t, db, i5FailCommitWebAuthn, i5FailCommitWebAuthnDrop)

		err := h.withMFAFactorWriteTx(ctx, userID, emailSmsState{known: true}, func(tx *sql.Tx) error {
			return insertWebAuthnCredentialTx(ctx, tx, userID)
		})
		require.ErrorContains(t, err, "commit MFA factor write", "Commit itself must be what failed")
		spy.requireOneBump(t, userID)
		assert.Equal(t, 0, readP1Snapshot(t, observer, userID).webauthn, "the forced failure rolled back")
	})

	t.Run("TOTPDisable", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		iuEnrollTOTP(t, db, kr, userID)
		failCommitsOn(t, db, i5FailCommitTOTP, i5FailCommitTOTPDrop)

		code, body := runTOTPDisable(t, h, userID)
		require.Equal(t, http.StatusInternalServerError, code, i5BodyFmt, body)
		require.Equal(t, errMsgFailedDisableMFA, body["error"])
		spy.requireOneBump(t, userID)
		assert.True(t, readP1Snapshot(t, observer, userID).totpRow, "the forced failure rolled back")
	})

	t.Run("WebAuthnDeleteCredential", func(t *testing.T) {
		spy.reset()
		userID := iuCreateUser(t, db, iuPassword)
		credentialID := insertWebAuthnCredential(t, db, userID)
		failCommitsOn(t, db, i5FailCommitWebAuthn, i5FailCommitWebAuthnDrop)

		code, body := runWebAuthnDelete(t, h, userID, credentialID)
		require.Equal(t, http.StatusInternalServerError, code, i5BodyFmt, body)
		require.Equal(t, errMsgFailedDeleteCredential, body["error"])
		spy.requireOneBump(t, userID)
		assert.Equal(t, 1, readP1Snapshot(t, observer, userID).webauthn, "the forced failure rolled back")
	})
}
