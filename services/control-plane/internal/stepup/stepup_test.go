package stepup

// Hermetic unit tests for the shared step-up decision table (#2765). No
// database: VerifyPasswordFactor takes the stored hash as a struct field, and
// the MFA halves go through the narrow verifier interfaces, so both are
// exercised with a fake. Ported from internal/dm/purge_stepup_test.go, whose
// copy of this table was the only one offering an actionable response when no
// password factor was available — the others simply rejected.
//
// The no-password-factor cases below are defence-in-depth, not a live path:
// users.password_hash is TEXT NOT NULL and SSO accounts carry a real hash. See
// the package comment; an earlier revision framed this as a NULL-handling bug,
// which was false.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
)

// testCopy mirrors the DM purge strings so the ported cases assert the exact
// bytes the client already discriminates on.
var testCopy = Copy{
	NoFactors:          "Bulk deletion requires verification, but this account has no password and no MFA method. Set a password, enable MFA, or turn off \"Require authentication before purging\" in Privacy & Security.",
	CredentialRequired: "Current password required to purge messages",
}

// fakeMFAVerifier is a stub verifier with scriptable behaviour. It implements
// the full MFAVerifier surface so one fake serves both the pool-scoped and the
// transaction-scoped entry points; usedTx records which one was reached.
type fakeMFAVerifier struct {
	methodsErr   error
	methodsCalls int
	enabled      bool
	valid        bool
	verifyErr    error
	methods      []string
	usedTx       bool
	usedPool     bool
}

func (f *fakeMFAVerifier) IsEnabled(context.Context, string) bool { return f.enabled }

func (f *fakeMFAVerifier) VerifyCode(context.Context, string, string) (bool, error) {
	f.usedPool = true
	return f.valid, f.verifyErr
}

func (f *fakeMFAVerifier) VerifyCodeTx(context.Context, *sql.Tx, string, string) (bool, error) {
	f.usedTx = true
	return f.valid, f.verifyErr
}

func (f *fakeMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	f.methodsCalls++
	return f.methods, f.methodsErr
}

// fakeMFAVerifier must satisfy the union contract Task 3 consumes.
var _ MFAVerifier = (*fakeMFAVerifier)(nil)

func TestVerifyPasswordFactor_PasswordlessWithoutMFAIsActionable400(t *testing.T) {
	// No usable password factor and no MFA: must be an actionable 400, not a 500.
	//
	// This state is defence-in-depth, not a state production can currently
	// reach: users.password_hash is TEXT NOT NULL and SSO accounts carry a real
	// hash (they are marked by password_login_disabled instead). The test pins
	// the fail-safe's behaviour so a future schema change or auth method
	// inherits a defined answer rather than a 500. Do not read it as evidence
	// that an SSO account has no hash.
	err := VerifyPasswordFactor(Subject{PasswordHash: "", MFAEnabled: false}, "", testCopy)

	require.NotNil(t, err)
	require.Equal(t, http.StatusBadRequest, err.Status)
	require.Equal(t, testCopy.NoFactors, err.Body["error"])
	require.NotContains(t, err.Body, "password_required")
}

func TestVerifyPasswordFactor_PasswordlessWithMFAPasses(t *testing.T) {
	// No usable password factor but MFA enabled: MFA carries the step-up.
	// Same defence-in-depth framing as the case above — unreachable today.
	require.Nil(t, VerifyPasswordFactor(Subject{PasswordHash: "", MFAEnabled: true}, "", testCopy))
}

func TestVerifyPasswordFactor_MissingPasswordIs403(t *testing.T) {
	// Bound to a non-credential-shaped identifier: gosec G101 flags a quoted
	// literal sitting directly in a `PasswordHash:` field, and an inline
	// literal here would need a nolint pragma to survive the linter.
	const truncatedHash = "$argon2id$vali"

	err := VerifyPasswordFactor(Subject{PasswordHash: truncatedHash, MFAEnabled: false}, "", testCopy)

	require.NotNil(t, err)
	require.Equal(t, http.StatusForbidden, err.Status)
	require.Equal(t, testCopy.CredentialRequired, err.Body["error"])
	require.Equal(t, true, err.Body["password_required"])
}

func TestVerifyPasswordFactor_WrongPasswordIs403(t *testing.T) {
	hash, hashErr := auth.HashPassword("correct-horse-battery")
	require.NoError(t, hashErr)

	err := VerifyPasswordFactor(Subject{PasswordHash: hash}, "wrong-password", testCopy)

	require.NotNil(t, err)
	require.Equal(t, http.StatusForbidden, err.Status)
	require.Equal(t, "Invalid password", err.Body["error"])
}

func TestVerifyPasswordFactor_CorrectPasswordPasses(t *testing.T) {
	const plaintext = "correct-horse-battery"
	hash, hashErr := auth.HashPassword(plaintext)
	require.NoError(t, hashErr)

	require.Nil(t, VerifyPasswordFactor(Subject{PasswordHash: hash}, plaintext, testCopy))
}

func TestVerifyPasswordFactor_MalformedHashIs500(t *testing.T) {
	// A non-empty but undecodable hash is a server-side data problem, not a
	// user error — surface 500, not a misleading "invalid password".
	const malformedHash = "not-a-valid-argon2-hash"

	err := VerifyPasswordFactor(Subject{PasswordHash: malformedHash}, "anything", testCopy)

	require.NotNil(t, err)
	require.Equal(t, http.StatusInternalServerError, err.Status)
	require.Equal(t, ErrMsgVerificationFailed, err.Body["error"])
}

func TestVerifyMFAFactor_MissingCodeIs403WithMethods(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, methods: []string{"totp"}}

	err := VerifyMFAFactor(context.Background(), v, "user-1", "")

	require.NotNil(t, err)
	require.Equal(t, http.StatusForbidden, err.Status)
	require.Equal(t, "MFA verification required", err.Body["error"])
	require.Equal(t, true, err.Body["mfa_required"])
	require.Equal(t, []string{"totp"}, err.Body["methods"])
}

func TestVerifyMFAFactor_InvalidCodeIs403(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, valid: false}

	err := VerifyMFAFactor(context.Background(), v, "user-1", "000000")

	require.NotNil(t, err)
	require.Equal(t, http.StatusForbidden, err.Status)
	require.Equal(t, "Invalid MFA code", err.Body["error"])
}

func TestVerifyMFAFactor_VerifyErrorIs500(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, verifyErr: errors.New("totp backend down")}

	err := VerifyMFAFactor(context.Background(), v, "user-1", "123456")

	require.NotNil(t, err)
	require.Equal(t, http.StatusInternalServerError, err.Status)
	require.Equal(t, ErrMsgVerificationFailed, err.Body["error"],
		"internal error text must not leak into the response body")
}

func TestVerifyMFAFactor_ValidCodePasses(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, valid: true}

	require.Nil(t, VerifyMFAFactor(context.Background(), v, "user-1", "123456"))
	require.True(t, v.usedPool, "the non-tx form must reach VerifyCode")
}

// TestVerifyMFAFactorTx_ReachesTxVerifier proves the transaction form is wired
// to VerifyCodeTx and not silently to the pool form — a rollback must not be
// able to burn a single-use backup code. The nil *sql.Tx is deliberate: the
// fake never dereferences it, and this asserts the routing, not the SQL.
func TestVerifyMFAFactorTx_ReachesTxVerifier(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, valid: true}

	require.Nil(t, VerifyMFAFactorTx(context.Background(), nil, v, "user-1", "123456", nil))
	require.True(t, v.usedTx, "the tx form must reach VerifyCodeTx")
	require.False(t, v.usedPool, "the tx form must not fall back to the pool verifier")
}

// --- Error.Cause: root-cause diagnostics without credential leakage --------
//
// The pre-extraction DM handler logged the MFA-verify error. Dropping it left a
// 500 from a broken MFA backend with no diagnostic anywhere, so Cause restores
// it — but NOT for the password factor, whose underlying argon2 error can embed
// the malformed hash ([internal]rules/observability.md Core principle #1).

func TestError_MFABackendFailurePropagatesCause(t *testing.T) {
	backendDown := errors.New("totp store unreachable")
	err := VerifyMFAFactor(context.Background(),
		&fakeMFAVerifier{enabled: true, verifyErr: backendDown}, "u1", "123456")

	require.NotNil(t, err)
	require.Equal(t, http.StatusInternalServerError, err.Status)
	require.ErrorIs(t, err, backendDown, "a broken MFA backend must stay diagnosable")
	require.Equal(t, ErrMsgVerificationFailed, err.Body["error"], "the client body stays opaque")
	require.NotContains(t, err.Body, "cause", "Cause must never be serialized")
}

func TestError_PasswordFailureCarriesSentinelNotUnderlyingError(t *testing.T) {
	// A malformed hash makes auth.VerifyPassword fail. Its error can embed the
	// hash, so Cause must be the fixed sentinel and nothing else.
	//
	// Bound to a non-credential-shaped identifier: gosec G101 flags a quoted
	// literal assigned to a PasswordHash field, the same hazard that governs
	// the Copy.CredentialRequired naming. Bind, do not nolint.
	unparseable := "not-a-valid-argon2-encoding"
	err := VerifyPasswordFactor(
		Subject{PasswordHash: unparseable, MFAEnabled: false},
		"any-input", testCopy)

	require.NotNil(t, err)
	require.Equal(t, http.StatusInternalServerError, err.Status)
	require.ErrorIs(t, err, ErrPasswordVerification)
	require.Equal(t, ErrPasswordVerification, err.Cause,
		"Cause must be exactly the sentinel — never a wrapper that could carry the hash")
	require.NotContains(t, err.Cause.Error(), "not-a-valid-argon2-encoding")
}

func TestError_RejectionsCarryNoCause(t *testing.T) {
	// A 4xx is an outcome, not a fault. Logging one would turn ordinary user
	// error into error-level noise and hint at which accounts are being probed.
	stubEncoding := "$argon2id$stub" // bound, not inline — gosec G101
	cases := map[string]*Error{
		"no factors available": VerifyPasswordFactor(Subject{}, "", testCopy),
		"password omitted": VerifyPasswordFactor(
			Subject{PasswordHash: stubEncoding}, "", testCopy),
		"mfa code omitted": VerifyMFAFactor(context.Background(),
			&fakeMFAVerifier{enabled: true}, "u1", ""),
		"mfa code invalid": VerifyMFAFactor(context.Background(),
			&fakeMFAVerifier{enabled: true, valid: false}, "u1", "000000"),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, err)
			require.Less(t, err.Status, http.StatusInternalServerError)
			require.Nil(t, err.Cause, "a 4xx must carry no Cause")
		})
	}
}

// TestVerifyMFAFactorTx_PreloadedMethodsSkipTheLookup locks the fix for a
// CodeRabbit review finding on #2792.
//
// The missing-code branch used to call GetEnabledMethods through the shared
// pool while the caller already held a pooled connection AND the users-row
// lock — the second-connection-under-lock hazard [internal]rules/backend.md
// already records for entCache.GetTier. A caller that read mfa_methods from
// its own locked row now passes them in, and the lookup must not run.
func TestVerifyMFAFactorTx_PreloadedMethodsSkipTheLookup(t *testing.T) {
	// listErr makes any GetEnabledMethods call observable: if the preloaded
	// slice were ignored, methods would come back empty instead of preloaded.
	v := &fakeMFAVerifier{enabled: true, methodsErr: errors.New("pool lookup must not happen")}

	err := VerifyMFAFactorTx(context.Background(), nil, v, "user-1", "", []string{"totp"})

	require.NotNil(t, err)
	require.Equal(t, http.StatusForbidden, err.Status)
	require.Equal(t, []string{"totp"}, err.Body["methods"],
		"preloaded methods must be used verbatim rather than re-read from the pool")
	require.Equal(t, 0, v.methodsCalls, "GetEnabledMethods must not be called when methods are preloaded")
}

// A nil slice still falls back to the lookup, so the non-transactional caller
// (internal/dm, which holds no transaction) is unaffected.
func TestVerifyMFAFactor_NilPreloadFallsBackToLookup(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, methods: []string{"webauthn"}}

	err := VerifyMFAFactor(context.Background(), v, "user-1", "")

	require.NotNil(t, err)
	require.Equal(t, []string{"webauthn"}, err.Body["methods"])
	require.Equal(t, 1, v.methodsCalls, "the pool lookup is the fallback when nothing is preloaded")
}
