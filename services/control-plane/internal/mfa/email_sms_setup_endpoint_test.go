package mfa_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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

	handler := mfa.NewHandler(ts.DB, ts.Redis, logger.New("test"), make([]byte, 32), testhelpers.TestJWTSecret, nil, "test")
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
