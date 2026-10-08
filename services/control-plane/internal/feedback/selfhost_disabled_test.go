package feedback

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const selfhostDisabledBugBody = `{"type":"bug","title":"owned-disabled-title-canary","description":"owned-disabled-description-canary","diagnostics":{"logs":"owned-disabled-diagnostic-canary"}}`
const selfhostDisabledFeatureBody = `{"type":"feature","title":"owned-disabled-title-canary","description":"owned-disabled-description-canary"}`

type selfhostDisabledRequestBody struct {
	reader *strings.Reader
	reads  int
}

func (body *selfhostDisabledRequestBody) Read(buffer []byte) (int, error) {
	body.reads++
	return body.reader.Read(buffer)
}

func (body *selfhostDisabledRequestBody) Close() error { return nil }

func submitSelfhostFeedback(t *testing.T, handler *Handler, payload, userID string) (*httptest.ResponseRecorder, *selfhostDisabledRequestBody) {
	t.Helper()
	body := &selfhostDisabledRequestBody{reader: strings.NewReader(payload)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", body)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestEngine(t, handler, userID).ServeHTTP(response, request)
	require.NoError(t, request.Body.Close())
	return response, body
}

// Exercise the real constructor and consumer inside their own package, so
// ordinary per-package coverage observes the refusal. Effect observers stay
// live even if the disabled Submit branch is removed; no socket or server runs.
func TestNewDisabledHandler_RefusesBeforeReportEffects(t *testing.T) {
	cases := []struct {
		name, payload, userID, errorCode string
		status                           int
	}{
		{name: "valid bug report", payload: selfhostDisabledBugBody, userID: "owned-disabled-reporter",
			status: http.StatusServiceUnavailable, errorCode: "feedback_disabled"},
		{name: "valid feature request", payload: selfhostDisabledFeatureBody, userID: "owned-disabled-reporter",
			status: http.StatusServiceUnavailable, errorCode: "feedback_disabled"},
		{name: "malformed body", payload: "owned-disabled-malformed-canary", userID: "owned-disabled-reporter",
			status: http.StatusServiceUnavailable, errorCode: "feedback_disabled"},
		{name: "oversize body", payload: `{"type":"bug","title":"Owned","description":"owned-disabled-oversize-canary` +
			strings.Repeat("x", MaxRequestBytes) + `"}`, userID: "owned-disabled-reporter",
			status: http.StatusServiceUnavailable, errorCode: "feedback_disabled"},
		{name: "authentication precedes disabled refusal", payload: selfhostDisabledBugBody,
			status: http.StatusUnauthorized, errorCode: "unauthorized"},
	}
	for _, cell := range cases {
		t.Run(cell.name, func(t *testing.T) {
			handler := NewDisabledHandler()
			require.NotNil(t, handler)
			var logs bytes.Buffer
			github := &fakeGitHub{returnErr: errors.New("owned-disabled-github-effect-canary")}
			// Preserve the constructor's mode. Attach observers and valid ordinary
			// inputs only, so a missing refusal reaches named effect assertions.
			handler.github = github
			handler.log = logger.NewWithWriter(&logs)
			handler.corrKey = testCorrKey
			handler.mediaBaseURL = testMediaBaseURL
			response, body := submitSelfhostFeedback(t, handler, cell.payload, cell.userID)
			assert.Equal(t, cell.status, response.Code)
			assert.JSONEq(t, `{"error":"`+cell.errorCode+`"}`, response.Body.String())
			assert.Zero(t, body.reads, "disabled feedback must refuse before any body read")
			assert.Empty(t, github.calls, "disabled feedback must not create an issue")
			assert.Empty(t, logs.String(), "disabled feedback must not log a report or diagnostics")
			for _, canary := range []string{"owned-disabled-title-canary", "owned-disabled-description-canary",
				"owned-disabled-diagnostic-canary", "owned-disabled-malformed-canary", "owned-disabled-oversize-canary",
				"owned-disabled-github-effect-canary", "owned-disabled-reporter"} {
				assert.NotContains(t, logs.String()+response.Body.String(), canary)
			}
		})
	}
}

func TestNewHandler_EnabledFeedbackStillReachesIssueCreator(t *testing.T) {
	cases := []struct {
		name, payload string
		withLogs      bool
	}{
		{name: "bug report", payload: selfhostDisabledBugBody, withLogs: true},
		{name: "feature request", payload: selfhostDisabledFeatureBody},
	}
	for _, cell := range cases {
		t.Run(cell.name, func(t *testing.T) {
			var logs bytes.Buffer
			github := &fakeGitHub{}
			handler := NewHandler(logger.NewWithWriter(&logs), github, testCorrKey, testMediaBaseURL)
			response, body := submitSelfhostFeedback(t, handler, cell.payload, "owned-disabled-reporter")
			require.Equal(t, http.StatusOK, response.Code)
			assert.JSONEq(t, `{"issueUrl":"https://github.com/test/repo/issues/1","dev":false}`, response.Body.String())
			assert.Positive(t, body.reads, "enabled feedback must consume the request")
			require.Len(t, github.calls, 1, "the same issue observer must detect enabled publication")
			assert.Contains(t, github.calls[0].Title, "owned-disabled-title-canary")
			assert.Contains(t, github.calls[0].Body, "owned-disabled-description-canary")
			if cell.withLogs {
				assert.Contains(t, github.calls[0].Body, "owned-disabled-diagnostic-canary")
			}
			assert.Empty(t, logs.String())
		})
	}
}

func TestRequireEnabled_EnforcesModeBeforeMediaEffects(t *testing.T) {
	modes := []struct {
		name    string
		handler *Handler
		allowed bool
	}{
		{name: "disabled", handler: NewDisabledHandler()},
		{name: "enabled", handler: newTestHandler(&fakeGitHub{}), allowed: true},
		{name: "development stub", handler: newTestHandler(nil), allowed: true},
	}
	for _, mode := range modes {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			t.Run(mode.name+"/"+method, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				engine := gin.New()
				effects := 0
				var consumed []byte
				engine.Handle(method, "/media", mode.handler.RequireEnabled, func(c *gin.Context) {
					effects++
					var err error
					consumed, err = io.ReadAll(c.Request.Body)
					require.NoError(t, err)
					c.Status(http.StatusNoContent)
				})
				const payload = "owned-feedback-media-body-canary"
				body := &selfhostDisabledRequestBody{reader: strings.NewReader(payload)}
				request := httptest.NewRequest(method, "/media", body)
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				require.NoError(t, request.Body.Close())
				if mode.allowed {
					require.Equal(t, http.StatusNoContent, response.Code)
					assert.Empty(t, response.Header().Get("Cache-Control"))
					assert.Equal(t, 1, effects, "enabled feedback must reach the media consumer")
					assert.Positive(t, body.reads)
					assert.Equal(t, payload, string(consumed))
					return
				}
				assert.Equal(t, http.StatusServiceUnavailable, response.Code)
				assert.JSONEq(t, `{"error":"feedback_disabled"}`, response.Body.String())
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
				assert.Zero(t, effects, "disabled feedback must abort downstream media work")
				assert.Zero(t, body.reads, "disabled feedback must not consume the request body")
				assert.Empty(t, consumed)
				assert.NotContains(t, response.Body.String(), payload)
			})
		}
	}
}
