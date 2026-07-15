package dm

// Hermetic unit tests for the DM/group purge step-up factors (#1352). No database:
// verifyPurgePasswordFactor takes the stored hash as a parameter, and the MFA half
// goes through the mfa.Verifier interface, so both are exercised with a fake.

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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

func stepUpHandler(v *fakeMFAVerifier) *Handler {
	return &Handler{log: logger.NewWithWriter(io.Discard), mfaVerifier: v}
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

func TestVerifyPurgePasswordFactor_PasswordlessWithoutMFAIsActionable400(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{enabled: false})
	c, w := testCtx()

	// SSO/passwordless account (empty hash) with no MFA: must NOT 500 (review S1).
	ok := h.verifyPurgePasswordFactor(c, "", "", false)

	assert.False(t, ok)
	assert.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "no password and no MFA")
}

func TestVerifyPurgePasswordFactor_PasswordlessWithMFAPasses(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{enabled: true})
	c, w := testCtx()

	// Passwordless but MFA-enabled: the MFA factor carries the step-up.
	ok := h.verifyPurgePasswordFactor(c, "", "", true)

	assert.True(t, ok)
	assert.Equal(t, 200, w.Code, "no error response written")
}

func TestVerifyPurgePasswordFactor_MissingPasswordIs403(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{})
	c, w := testCtx()

	ok := h.verifyPurgePasswordFactor(c, "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA", "", false)

	assert.False(t, ok)
	assert.Equal(t, 403, w.Code)
	assert.Contains(t, w.Body.String(), "password_required")
}

func TestVerifyPurgePasswordFactor_WrongPasswordIs403(t *testing.T) {
	hash, err := auth.HashPassword("correct-horse-battery")
	require.NoError(t, err)

	h := stepUpHandler(&fakeMFAVerifier{})
	c, w := testCtx()

	ok := h.verifyPurgePasswordFactor(c, hash, "wrong-password", false)

	assert.False(t, ok)
	assert.Equal(t, 403, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid password")
}

func TestVerifyPurgePasswordFactor_CorrectPasswordPasses(t *testing.T) {
	const pw = "correct-horse-battery"
	hash, err := auth.HashPassword(pw)
	require.NoError(t, err)

	h := stepUpHandler(&fakeMFAVerifier{})
	c, w := testCtx()

	ok := h.verifyPurgePasswordFactor(c, hash, pw, false)

	assert.True(t, ok)
	assert.Equal(t, 200, w.Code)
}

func TestVerifyPurgePasswordFactor_MalformedHashIs500(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{})
	c, w := testCtx()

	// A non-empty but undecodable hash is a server-side data problem, not a
	// user error — surface 500, not a misleading "invalid password".
	ok := h.verifyPurgePasswordFactor(c, "not-a-valid-argon2-hash", "anything", false)

	assert.False(t, ok)
	assert.Equal(t, 500, w.Code)
}

func TestVerifyPurgeMFAFactor_MissingCodeIs403WithMethods(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{enabled: true, methods: []string{"totp"}})
	c, w := testCtx()

	ok := h.verifyPurgeMFAFactor(c, "user-1", "")

	assert.False(t, ok)
	assert.Equal(t, 403, w.Code)
	assert.Contains(t, w.Body.String(), "mfa_required")
	assert.Contains(t, w.Body.String(), "totp")
}

func TestVerifyPurgeMFAFactor_InvalidCodeIs403(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{enabled: true, valid: false})
	c, w := testCtx()

	ok := h.verifyPurgeMFAFactor(c, "user-1", "000000")

	assert.False(t, ok)
	assert.Equal(t, 403, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid MFA code")
}

func TestVerifyPurgeMFAFactor_VerifyErrorIs500(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{enabled: true, verifyErr: errors.New("totp backend down")})
	c, w := testCtx()

	ok := h.verifyPurgeMFAFactor(c, "user-1", "123456")

	assert.False(t, ok)
	assert.Equal(t, 500, w.Code)
	assert.NotContains(t, w.Body.String(), "totp backend down", "internal error text must not leak")
}

func TestVerifyPurgeMFAFactor_ValidCodePasses(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{enabled: true, valid: true})
	c, w := testCtx()

	ok := h.verifyPurgeMFAFactor(c, "user-1", "123456")

	assert.True(t, ok)
	assert.Equal(t, 200, w.Code)
}
