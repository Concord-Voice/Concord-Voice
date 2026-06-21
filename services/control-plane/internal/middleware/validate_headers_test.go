package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/stretchr/testify/assert"
)

const (
	testHeaderDeviceName = "X-Device-Name"
	testHeaderMachineID  = "X-Machine-Id"
)

func headersRouter() *gin.Engine {
	r := gin.New()
	r.Use(middleware.ValidateCustomHeaders())
	r.GET("/test", func(c *gin.Context) { c.String(200, "ok") })
	return r
}

func doHeaders(r *gin.Engine, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/test", nil)
	if headers != nil {
		req.Header = headers
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestValidateHeadersNoHeadersOK(t *testing.T) {
	w := doHeaders(headersRouter(), nil)
	assert.Equal(t, 200, w.Code)
}

func TestValidateHeadersValidMachineId(t *testing.T) {
	h := http.Header{}
	h.Set(testHeaderMachineID, "550e8400-e29b-41d4-a716-446655440000")
	w := doHeaders(headersRouter(), h)
	assert.Equal(t, 200, w.Code)
}

func TestValidateHeadersInvalidMachineId(t *testing.T) {
	h := http.Header{}
	h.Set(testHeaderMachineID, "not-a-uuid")
	w := doHeaders(headersRouter(), h)
	assert.Equal(t, 400, w.Code)
}

func TestValidateHeadersDeviceNameValid(t *testing.T) {
	h := http.Header{}
	h.Set(testHeaderDeviceName, "Mark's MacBook Pro")
	w := doHeaders(headersRouter(), h)
	assert.Equal(t, 200, w.Code)
}

func TestValidateHeadersDeviceNameUTF8(t *testing.T) {
	// 255 Japanese characters = 765 bytes, should pass character limit
	h := http.Header{}
	h.Set(testHeaderDeviceName, strings.Repeat("あ", 255))
	w := doHeaders(headersRouter(), h)
	assert.Equal(t, 200, w.Code)
}

func TestValidateHeadersDeviceNameTooLong(t *testing.T) {
	h := http.Header{}
	h.Set(testHeaderDeviceName, strings.Repeat("A", 256))
	w := doHeaders(headersRouter(), h)
	assert.Equal(t, 400, w.Code)
}

func TestValidateHeadersDeviceNameInvalidUTF8(t *testing.T) {
	h := http.Header{}
	h.Set(testHeaderDeviceName, "bad\xff\xfebytes")
	w := doHeaders(headersRouter(), h)
	assert.Equal(t, 400, w.Code)
}

func TestValidateHeadersDeviceNameControlChars(t *testing.T) {
	h := http.Header{}
	h.Set(testHeaderDeviceName, "My\x00Device")
	w := doHeaders(headersRouter(), h)
	assert.Equal(t, 400, w.Code)
}
