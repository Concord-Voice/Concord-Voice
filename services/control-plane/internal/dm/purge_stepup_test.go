package dm

// Wiring tests for the DM/group purge step-up binding (#2765). The ten
// decision-table cases that used to live here moved to internal/stepup, which
// now owns the policy; what remains proves the DM *binding* — that
// verifyPurgeStepUp delegates to that package, writes its typed error onto the
// gin context, and supplies the DM wording byte-for-byte.
//
// The full handler path (require_auth gate → step-up → purge) is covered
// against a real PostgreSQL in purge_integration_test.go.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// fakeMFAVerifier is a stub mfa.Verifier with scriptable behaviour.
type fakeMFAVerifier struct {
	enabled   bool
	valid     bool
	verifyErr error
	methods   []string
}

func (f *fakeMFAVerifier) IsEnabled(context.Context, string) bool { return f.enabled }
func (f *fakeMFAVerifier) VerifyCode(context.Context, string, string) (bool, error) {
	return f.valid, f.verifyErr
}
func (f *fakeMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return f.methods, nil
}

// errNoDatabase is returned by every connection attempt of failingConnector.
var errNoDatabase = errors.New("no database")

// failingConnector yields an *sql.DB whose first query fails deterministically,
// with no network and no driver registration. It is how the subject-load
// failure branch is exercised hermetically.
type failingConnector struct{}

func (failingConnector) Connect(context.Context) (driver.Conn, error) { return nil, errNoDatabase }
func (failingConnector) Driver() driver.Driver                        { return failingDriver{} }

type failingDriver struct{}

func (failingDriver) Open(string) (driver.Conn, error) { return nil, errNoDatabase }

func stepUpHandler(v *fakeMFAVerifier, db *sql.DB) *Handler {
	return &Handler{log: logger.NewWithWriter(io.Discard), mfaVerifier: v, db: db}
}

// testCtx returns a gin context wired to a recorder, with a request whose context
// the handler helpers read.
func testCtx() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	return c, w
}

// TestVerifyPurgeStepUp_SubjectLoadFailureIs500 proves the binding actually
// reaches stepup.LoadSubject and writes the returned *stepup.Error onto the
// gin context rather than swallowing it — the delegation, not the policy.
func TestVerifyPurgeStepUp_SubjectLoadFailureIs500(t *testing.T) {
	db := sql.OpenDB(failingConnector{})
	t.Cleanup(func() { _ = db.Close() })

	h := stepUpHandler(&fakeMFAVerifier{}, db)
	c, w := testCtx()

	ok := h.verifyPurgeStepUp(c, "user-1", "anything", "")

	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), stepup.ErrMsgVerificationFailed)
}

// TestPurgeStepUpCopy_PasswordlessWithoutMFAIsActionable400 locks the DM
// wording: an SSO/passwordless account with no MFA must get the actionable 400,
// not a raw 500 (review finding S1), and the bytes must not drift — the desktop
// client discriminates on them by string.
func TestPurgeStepUpCopy_PasswordlessWithoutMFAIsActionable400(t *testing.T) {
	err := stepup.VerifyPasswordFactor(
		stepup.Subject{PasswordHash: "", MFAEnabled: false}, "", purgeStepUpCopy)

	require.NotNil(t, err)
	assert.Equal(t, http.StatusBadRequest, err.Status)
	assert.Equal(t,
		"Bulk deletion requires verification, but this account has no password and no MFA method. "+
			"Set a password, enable MFA, or turn off \"Require authentication before purging\" in Privacy & Security.",
		err.Body["error"])
}

// TestPurgeStepUpCopy_MissingPasswordIs403 locks the other half of the DM
// wording plus the password_required discriminator the client keys on.
func TestPurgeStepUpCopy_MissingPasswordIs403(t *testing.T) {
	const truncatedHash = "$argon2id$vali"

	err := stepup.VerifyPasswordFactor(
		stepup.Subject{PasswordHash: truncatedHash}, "", purgeStepUpCopy)

	require.NotNil(t, err)
	assert.Equal(t, http.StatusForbidden, err.Status)
	assert.Equal(t, "Current password required to purge messages", err.Body["error"])
	assert.Equal(t, true, err.Body["password_required"])
}

// TestPurgeStepUpCopy_CorrectPasswordWithoutMFAPasses is the pass-through case:
// a valid password on an MFA-disabled account clears the gate, so the handler
// never enters the MFA branch.
func TestPurgeStepUpCopy_CorrectPasswordWithoutMFAPasses(t *testing.T) {
	const plaintext = "correct-horse-battery"
	hash, hashErr := auth.HashPassword(plaintext)
	require.NoError(t, hashErr)

	assert.Nil(t, stepup.VerifyPasswordFactor(
		stepup.Subject{PasswordHash: hash, MFAEnabled: false}, plaintext, purgeStepUpCopy))
}
