package mfa_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupEmailSmsSetupEndpoint(t *testing.T, emailSvc *email.Service) (*mfa.Handler, *gin.Context, *httptest.ResponseRecorder, *redis.Client, string) {
	t.Helper()

	ts := setupTS(t)
	user := ts.CreateTestUser(t, "emailsmsdelivery")
	secret, _ := enrollTOTP(t, ts, user)

	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	handler := mfa.NewHandler(ts.DB, ts.Redis, logger.New("test"), ring, testhelpers.TestJWTSecret, nil, "test")
	if emailSvc != nil {
		handler.SetEmailService(emailSvc)
	}

	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", user.ID)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		urlEmailSmsSetup,
		strings.NewReader(fmt.Sprintf(`{"password":%q,"mfa_code":%q,"methods":["email"]}`, testhelpers.TestAuthPlaintext, code)),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	return handler, c, w, ts.Redis, user.ID
}

func TestEmailSmsSetupRejectsEmailWhenServiceMissingBeforeGeneratingCode(t *testing.T) {
	handler, c, w, redisClient, userID := setupEmailSmsSetupEndpoint(t, nil)

	handler.EmailSmsSetup(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Email delivery is not configured")
	assert.Equal(t, int64(0), redisClient.Exists(context.Background(), fmt.Sprintf(redisEmailSmsSetup, userID)).Val())
}

func TestEmailSmsSetupReturnsWhenEmailSendFails(t *testing.T) {
	svc := email.NewService(&config.Config{
		SMTPHost: "127.0.0.1",
		SMTPPort: 1,
		SMTPFrom: "",
	}, logger.New("test"))
	handler, c, w, redisClient, userID := setupEmailSmsSetupEndpoint(t, svc)

	handler.EmailSmsSetup(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to send verification email")
	assert.Equal(t, int64(0), redisClient.Exists(context.Background(), fmt.Sprintf(redisEmailSmsSetup, userID)).Val())
}

// SMS cannot be enrolled outside development and test, so hardened mode's SMS
// half is dormant there: an account carrying the default TRUE flag must still
// activate email MFA with the email code alone. Before this, every production
// activation answered 400 "Hardened mode requires both email and SMS codes".
func TestEmailSmsVerifyHardenedIsDormantWithoutSms(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "harddormant")
	enrollTOTP(t, ts, user)
	_, err := ts.DB.Exec(`UPDATE users SET recovery_hardened = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Set(context.Background(), fmt.Sprintf(redisEmailSmsSetup, user.ID), "123456", 10*time.Minute).Err())

	ring, err := mfa.ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	handler := mfa.NewHandler(ts.DB, ts.Redis, logger.New("test"), ring, testhelpers.TestJWTSecret, nil, "production")

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", user.ID)
	c.Request = httptest.NewRequest(http.MethodPost, urlEmailSmsVerify, strings.NewReader(`{"codes":{"email":"123456"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.EmailSmsVerify(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "Hardened mode")
}
