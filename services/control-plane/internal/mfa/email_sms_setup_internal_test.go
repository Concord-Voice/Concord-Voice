package mfa

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
)

func setupEmailSmsContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

func TestSendEmailSmsSetupEmailSkipsWhenNoEmailCode(t *testing.T) {
	c, w := setupEmailSmsContext()
	h := &Handler{log: logger.New("test")}

	ok := h.sendEmailSmsSetupEmail(c, "user-1", "user@example.com", map[string]string{"sms": "123456"})

	assert.True(t, ok)
	assert.Empty(t, w.Body.String())
}

func TestSendEmailSmsSetupEmailRejectsMissingService(t *testing.T) {
	c, w := setupEmailSmsContext()
	h := &Handler{log: logger.New("test")}

	ok := h.sendEmailSmsSetupEmail(c, "user-1", "user@example.com", map[string]string{"email": "123456"})

	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Email delivery is not configured")
}

func TestSendEmailSmsSetupEmailReturnsSendFailure(t *testing.T) {
	c, w := setupEmailSmsContext()
	svc := email.NewService(&config.Config{
		SMTPHost: "127.0.0.1",
		SMTPPort: 1,
		SMTPFrom: "",
	}, logger.New("test"))
	h := &Handler{emailSvc: svc, log: logger.New("test")}

	ok := h.sendEmailSmsSetupEmail(c, "user-1", "user@example.com", map[string]string{"email": "123456"})

	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Failed to send verification email")
}
