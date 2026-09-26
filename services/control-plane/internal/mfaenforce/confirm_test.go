package mfaenforce_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// spyVerifier records every call. valid is its verdict for a code.
type spyVerifier struct {
	valid bool
	calls int
}

func (s *spyVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	s.calls++
	return nil, nil
}

func (s *spyVerifier) VerifyCodeTx(context.Context, *sql.Tx, string, string) (bool, error) {
	s.calls++
	return s.valid, nil
}

var _ stepup.MFATxCodeVerifier = (*spyVerifier)(nil)

var enrolledTOTP = stepup.Subject{MFAEnabled: true, MFAMethods: []string{"totp"}}

func requireEnrollmentRequired(t *testing.T, e *stepup.Error) {
	t.Helper()
	require.NotNil(t, e, "an actor with no inline factor must be refused")
	require.Equal(t, http.StatusForbidden, e.Status)
	require.Equal(t, stepup.ErrMsgMFAEnrollmentRequired, e.Body["error"])
	require.Equal(t, true, e.Body["mfa_enrollment_required"])
	require.NotContains(t, e.Body, "methods", "there is no factor to prompt for")
	require.Nil(t, e.Cause)
}

// Invariant I8. The unenrolled branch is where the purge-fence gate returns nil
// (a password carries that step-up); copied here, it would let an unenrolled
// actor turn enforcement off with no factor at all.
func TestConfirmTx_RefusesAnActorWithNoInlineFactor(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	g, tx := lockGate(t, db, createServer(t, db, owner, true), owner)
	require.False(t, g.Subject.MFAEnabled, "precondition: the actor is unenrolled")

	t.Run("a subject from LockGateTx", func(t *testing.T) {
		spy := &spyVerifier{valid: true}

		requireEnrollmentRequired(t, mfaenforce.ConfirmTx(context.Background(), tx, g.Subject, spy, owner, "123456"))
		require.Zero(t, spy.calls, "the verifier must not be asked")
	})

	// MFAEnabled without methods would reach VerifyMFAFactorTx with a nil
	// slice, whose fallback reads users.mfa_methods on a second pooled
	// connection while tx holds the users row.
	t.Run("a hand-built subject claiming MFA with no methods", func(t *testing.T) {
		spy := &spyVerifier{valid: true}

		requireEnrollmentRequired(t, mfaenforce.ConfirmTx(context.Background(), tx,
			stepup.Subject{MFAEnabled: true}, spy, owner, ""))
		require.Zero(t, spy.calls)
	})
}

func TestConfirmTx_NilVerifierIs500(t *testing.T) {
	e := mfaenforce.ConfirmTx(context.Background(), nil, enrolledTOTP, nil, "user-1", "123456")

	require.NotNil(t, e)
	require.Equal(t, http.StatusInternalServerError, e.Status)
	require.Equal(t, stepup.ErrMsgVerificationFailed, e.Body["error"])
	require.Error(t, e.Cause, "a 500 carries a Cause for the caller to log")
}

func TestConfirmTx_VerifiesTheCodeOnTheTransaction(t *testing.T) {
	db := gateTestDB(t)
	verifier, ring := newVerifier(t, db)
	owner := createUser(t, db)
	secret := enrollTOTP(t, db, ring, owner)
	serverID := createServer(t, db, owner, true)
	ctx := context.Background()

	t.Run("no code asks for one and offers the P1 methods", func(t *testing.T) {
		g, tx := lockGate(t, db, serverID, owner)

		e := mfaenforce.ConfirmTx(ctx, tx, g.Subject, verifier, owner, "")

		require.NotNil(t, e)
		require.Equal(t, http.StatusForbidden, e.Status)
		require.Equal(t, true, e.Body["mfa_required"])
		require.Equal(t, []string{"totp"}, e.Body["methods"])
	})

	t.Run("a wrong code is refused", func(t *testing.T) {
		g, tx := lockGate(t, db, serverID, owner)

		e := mfaenforce.ConfirmTx(ctx, tx, g.Subject, verifier, owner, "not-a-code")

		require.NotNil(t, e)
		require.Equal(t, http.StatusForbidden, e.Status)
		require.Equal(t, stepup.ErrMsgInvalidMFACode, e.Body["error"])
	})

	t.Run("a valid TOTP code confirms", func(t *testing.T) {
		g, tx := lockGate(t, db, serverID, owner)
		code, err := totp.GenerateCode(secret, time.Now())
		require.NoError(t, err)

		require.Nil(t, mfaenforce.ConfirmTx(ctx, tx, g.Subject, verifier, owner, code))
	})
}

// Redeeming a backup code is a write. It must ride the gate's transaction, so a
// rollback leaves the code usable. The last two steps are the control: the
// code IS spent by a commit and IS single use, so "still unused after the
// rollback" cannot pass merely because nothing ever spends it.
func TestConfirmTx_BackupCodeIsSpentOnlyWhenTheTransactionCommits(t *testing.T) {
	db := gateTestDB(t)
	verifier, ring := newVerifier(t, db)
	owner := createUser(t, db)
	enrollTOTP(t, db, ring, owner)
	serverID := createServer(t, db, owner, true)
	ctx := context.Background()

	g, tx := lockGate(t, db, serverID, owner)
	require.Nil(t, mfaenforce.ConfirmTx(ctx, tx, g.Subject, verifier, owner, testBackupCode))
	require.NoError(t, tx.Rollback())
	require.False(t, backupCodeUsed(t, db, owner), "a rolled-back gate must not spend the backup code")

	g, tx = lockGate(t, db, serverID, owner)
	require.Nil(t, mfaenforce.ConfirmTx(ctx, tx, g.Subject, verifier, owner, testBackupCode),
		"the code must still be usable after the rollback")
	require.NoError(t, tx.Commit())
	require.True(t, backupCodeUsed(t, db, owner), "a committed gate spends the backup code")

	g, tx = lockGate(t, db, serverID, owner)
	e := mfaenforce.ConfirmTx(ctx, tx, g.Subject, verifier, owner, testBackupCode)
	require.NotNil(t, e, "a backup code is single use")
	require.Equal(t, stepup.ErrMsgInvalidMFACode, e.Body["error"])
}

func TestRequireConfirmationTx(t *testing.T) {
	ctx := context.Background()

	t.Run("a server that does not enforce asks nothing of an unenrolled actor", func(t *testing.T) {
		spy := &spyVerifier{}

		require.Nil(t, mfaenforce.RequireConfirmationTx(ctx, nil, mfaenforce.Gate{}, spy, "user-1", ""))
		require.Zero(t, spy.calls)
	})

	t.Run("a server that does not enforce does not consult the verifier for an enrolled actor", func(t *testing.T) {
		spy := &spyVerifier{valid: false}

		require.Nil(t, mfaenforce.RequireConfirmationTx(ctx, nil,
			mfaenforce.Gate{Subject: enrolledTOTP}, spy, "user-1", "123456"))
		require.Zero(t, spy.calls, "a code sent to a non-enforcing server must not be checked, or spent")
	})

	t.Run("an enforcing server refuses an unenrolled actor", func(t *testing.T) {
		spy := &spyVerifier{valid: true}

		requireEnrollmentRequired(t, mfaenforce.RequireConfirmationTx(ctx, nil,
			mfaenforce.Gate{Enforcing: true}, spy, "user-1", "123456"))
	})

	t.Run("an enforcing server asks an enrolled actor for a code", func(t *testing.T) {
		e := mfaenforce.RequireConfirmationTx(ctx, nil,
			mfaenforce.Gate{Enforcing: true, Subject: enrolledTOTP}, &spyVerifier{}, "user-1", "")

		require.NotNil(t, e)
		require.Equal(t, true, e.Body["mfa_required"])
	})

	t.Run("an enforcing server accepts an enrolled actor's valid code", func(t *testing.T) {
		spy := &spyVerifier{valid: true}

		require.Nil(t, mfaenforce.RequireConfirmationTx(ctx, nil,
			mfaenforce.Gate{Enforcing: true, Subject: enrolledTOTP}, spy, "user-1", "123456"))
		require.Equal(t, 1, spy.calls)
	})
}
