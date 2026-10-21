package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/feedbackscreenshots"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// fakeGitHub captures CreateIssue calls so handler tests can assert what
// would have been sent to GitHub without making any network call.
type fakeGitHub struct {
	calls       []IssueRequest
	returnIssue *IssueResponse
	returnErr   error
	onCreate    func()
	onCreateCtx func(context.Context)
}

func (f *fakeGitHub) CreateIssue(ctx context.Context, req IssueRequest) (*IssueResponse, error) {
	if f.onCreate != nil {
		f.onCreate()
	}
	if f.onCreateCtx != nil {
		f.onCreateCtx(ctx)
	}
	f.calls = append(f.calls, req)
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	if f.returnIssue != nil {
		return f.returnIssue, nil
	}
	return &IssueResponse{Number: 1, HTMLURL: "https://github.com/test/repo/issues/1"}, nil
}

type fakeScreenshotPinner struct {
	userID           string
	paths            []string
	err              error
	calls            int
	confirmCalls     int
	releaseCalls     int
	onPin            func()
	onPinCtx         func(context.Context)
	receipt          string
	confirmedReceipt string
	releasedReceipt  string
	confirmedPaths   []string
	releasedPaths    []string
	onConfirmCtx     func(context.Context)
	onReleaseCtx     func(context.Context)
	releaseErr       error
}

func (f *fakeScreenshotPinner) Pin(ctx context.Context, userID string, paths []string) (string, error) {
	f.calls++
	f.userID = userID
	f.paths = append([]string(nil), paths...)
	if f.onPin != nil {
		f.onPin()
	}
	if f.onPinCtx != nil {
		f.onPinCtx(ctx)
	}
	if f.err != nil {
		return f.receipt, f.err
	}
	if f.receipt == "" {
		return "test-pin-receipt", nil
	}
	return f.receipt, nil
}

func (f *fakeScreenshotPinner) ConfirmPublished(ctx context.Context, receipt string, paths []string) error {
	f.confirmCalls++
	f.confirmedReceipt = receipt
	f.confirmedPaths = append([]string(nil), paths...)
	if f.onConfirmCtx != nil {
		f.onConfirmCtx(ctx)
	}
	return nil
}

func (f *fakeScreenshotPinner) ReleaseRejected(ctx context.Context, receipt string, paths []string) error {
	f.releaseCalls++
	f.releasedReceipt = receipt
	f.releasedPaths = append([]string(nil), paths...)
	if f.onReleaseCtx != nil {
		f.onReleaseCtx(ctx)
	}
	return f.releaseErr
}

// newTestEngine wires the handler into a Gin engine with the auth
// middleware emulated by setting `user_id` on the context. Returns a func
// the caller invokes to perform the POST request.
func newTestEngine(t *testing.T, h *Handler, userID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/feedback", func(c *gin.Context) {
		if userID != "" {
			c.Set("user_id", userID)
		}
		h.Submit(c)
	})
	return r
}

func doPost(t *testing.T, r *gin.Engine, payload interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// testCorrKey is the fixed HKDF-derived-shaped correlation key the test handler
// uses so reporter tokens are deterministic across a test run.
var testCorrKey = []byte("test-corr-key-0000000000000000000") // pragma: allowlist secret -- test fixture

// testMediaBaseURL is the fixed mediaBaseURL the test handler is constructed
// with — used to author absolute screenshot embed URLs in filed issues (#1747).
const testMediaBaseURL = "https://media.test"

func newTestHandler(github GitHubIssueCreator) *Handler {
	h := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	h.SetScreenshotPinner(&fakeScreenshotPinner{})
	return h
}

// ─── Auth ─────────────────────────────────────────────────────────────────

func TestSubmit_RequiresUserID(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "") // no user_id on context

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Crash",
		"description": "Repro: ...",
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ─── Validation ───────────────────────────────────────────────────────────

func TestSubmit_RejectsInvalidType(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "spam",
		"title":       "Hi",
		"description": "Hello",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid report type")
}

func TestSubmit_RejectsEmptyTitle(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "   ",
		"description": "Hello",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "title is required")
}

func TestSubmit_RejectsEmptyDescription(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Crash",
		"description": "  ",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "description is required")
}

func TestSubmit_RejectsOversizeTitle(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       strings.Repeat("a", maxTitleBytes+1),
		"description": "Repro: ...",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─── Sanitization ─────────────────────────────────────────────────────────

// PII in title/description must be scrubbed before being sent to GitHub.
func TestSubmit_SanitizesUserSuppliedFields(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Email leak from alice@example.com",
		"description": "Tried to /Users/alice/Documents/foo.txt and got refused by 10.0.0.5",
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	assert.NotContains(t, gh.calls[0].Title, "alice@example.com")
	assert.Contains(t, gh.calls[0].Title, "<email>")
	assert.NotContains(t, gh.calls[0].Body, "/Users/alice")
	assert.NotContains(t, gh.calls[0].Body, "10.0.0.5")
}

// ─── Issue assembly ───────────────────────────────────────────────────────

func TestSubmit_BugReport_AssemblesIssueShape(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-42")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Crash on send",
		"description": "Steps to reproduce: 1...",
		"diagnostics": map[string]interface{}{
			"appVersion":      "0.1.60",
			"platform":        "darwin",
			"machineIdPrefix": "4c33734c",
			"connectionPhase": "stable",
			"gpu":             map[string]string{"vendor": "Apple", "renderer": "M1 Pro"},
			"display":         map[string]interface{}{"width": 3024, "height": 1964, "scaleFactor": 2},
			"logs":            "2026-06-12T00:00:00Z  [warn]  network hiccup",
		},
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	call := gh.calls[0]
	assert.Equal(t, "[Bug Report] Crash on send", call.Title)
	assert.Equal(t, []string{"type: bug"}, call.Labels)
	// The body carries the non-reversible reporter token, never the raw ID.
	assert.NotContains(t, call.Body, "user-42", "raw user ID must not appear in the body")
	assert.Contains(t, call.Body, "**Reported by:** report `"+DeriveCorrelationToken(testCorrKey, "user-42")+"`")
	// Metadata values are wrapped in inline code spans (post-#1547 Fix 2/4/5).
	assert.Contains(t, call.Body, "**App Version:** `0.1.60`")
	assert.Contains(t, call.Body, "**Platform:** `darwin`")
	assert.Contains(t, call.Body, "## Description")
	assert.Contains(t, call.Body, "Steps to reproduce")
	assert.Contains(t, call.Body, "## Diagnostics")
	assert.Contains(t, call.Body, "`Apple` / `M1 Pro`")
	// Display dimensions are not user free-text — rendered raw, not fenced.
	assert.Contains(t, call.Body, "3024x1964")
	assert.Contains(t, call.Body, "Recent logs (sanitized)")
}

func TestSubmit_FeatureRequest_AssemblesIssueShape(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-9")

	w := doPost(t, r, map[string]interface{}{
		"type":        "feature",
		"title":       "Add dark high-contrast preset",
		"description": "Would help low-vision users.",
		"category":    "Improvement to Existing Feature",
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	call := gh.calls[0]
	assert.Equal(t, "[Feature Request] Add dark high-contrast preset", call.Title)
	assert.Equal(t, []string{"type: feature"}, call.Labels)
	// The body carries the non-reversible reporter token, never the raw ID.
	assert.NotContains(t, call.Body, "user-9", "raw user ID must not appear in the body")
	assert.Contains(t, call.Body, "**Requested by:** report `"+DeriveCorrelationToken(testCorrKey, "user-9")+"`")
	assert.Contains(t, call.Body, "**Category:** `Improvement to Existing Feature`")
	assert.NotContains(t, call.Body, "## Diagnostics", "feature requests must not emit diagnostics section")
}

// ─── Response ─────────────────────────────────────────────────────────────

func TestSubmit_SuccessReturnsIssueURL(t *testing.T) {
	gh := &fakeGitHub{
		returnIssue: &IssueResponse{Number: 7, HTMLURL: "https://github.com/x/y/issues/7"},
	}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "feature",
		"title":       "X",
		"description": "Y",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var resp submitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "https://github.com/x/y/issues/7", resp.IssueURL)
	assert.False(t, resp.Dev)
}

func TestSubmit_DevStubReturnsDevTrue(t *testing.T) {
	// Nil github client → handler.github == nil → log-only stub.
	h := newTestHandler(nil)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var resp submitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Dev)
	assert.Empty(t, resp.IssueURL)
}

func TestSubmit_GitHubErrorReturns502(t *testing.T) {
	gh := &fakeGitHub{returnErr: assertGitHubError("validation failed")}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
	})
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestSubmit_AttachmentsRequirePinningBeforeIssueCreation(t *testing.T) {
	github := &fakeGitHub{}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": feedbackScreenshotPathPrefix + testAttachmentUUID}},
	})

	assert.Equal(t, http.StatusServiceUnavailable, response.Code,
		"a screenshot-bearing issue must not be published unless its attachment was pinned")
	assert.Empty(t, github.calls, "GitHub must not receive an issue with an unpinned screenshot")
}

func TestSubmit_PinsAttachmentsBeforeIssueCreation(t *testing.T) {
	var events []string
	github := &fakeGitHub{onCreate: func() { events = append(events, "issue") }}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{receipt: "published-report-receipt", onPin: func() { events = append(events, "pin") }}
	handler.SetScreenshotPinner(pinner)
	path := feedbackScreenshotPathPrefix + testAttachmentUUID
	response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshots.",
		"attachments": []map[string]string{{"url": path}, {"url": feedbackScreenshotPathPrefix + "550e8400-e29b-41d4-a716-446655440001"}},
	})

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "user-1", pinner.userID)
	assert.Equal(t, []string{path, feedbackScreenshotPathPrefix + "550e8400-e29b-41d4-a716-446655440001"}, pinner.paths)
	assert.Equal(t, []string{"pin", "issue"}, events)
	assert.Equal(t, 1, pinner.confirmCalls)
	assert.Equal(t, "published-report-receipt", pinner.confirmedReceipt, "publication must settle the exact pin attempt, not the uploader identity")
	assert.Equal(t, pinner.paths, pinner.confirmedPaths)
	assert.Zero(t, pinner.releaseCalls)
}

func TestSubmit_SuccessSettlesReceiptAfterClientDisconnect(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	github := &fakeGitHub{onCreate: cancel}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{
		receipt: "published-after-disconnect",
		onConfirmCtx: func(ctx context.Context) {
			require.NoError(t, ctx.Err(), "publication confirmation must survive client cancellation")
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.WithinDuration(t, time.Now().Add(2*time.Second), deadline, time.Second)
		},
	}
	handler.SetScreenshotPinner(pinner)
	path := feedbackScreenshotPathPrefix + testAttachmentUUID
	body, err := json.Marshal(map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": path}},
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", bytes.NewReader(body)).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestEngine(t, handler, "user-1").ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.Equal(t, 1, pinner.confirmCalls)
	require.Equal(t, "published-after-disconnect", pinner.confirmedReceipt)
	require.Equal(t, []string{path}, pinner.confirmedPaths)
}

func TestSubmit_AmbiguousPinCommitReleasesReceiptWithoutPublishing(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	github := &fakeGitHub{}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{
		receipt: "ambiguous-commit-receipt",
		err:     errors.New("commit acknowledgement lost"),
		onPin:   cancel,
		onReleaseCtx: func(ctx context.Context) {
			require.NoError(t, ctx.Err(), "ambiguous commit cleanup must survive client cancellation")
		},
	}
	handler.SetScreenshotPinner(pinner)
	path := feedbackScreenshotPathPrefix + testAttachmentUUID
	body, err := json.Marshal(map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": path}},
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", bytes.NewReader(body)).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestEngine(t, handler, "user-1").ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Empty(t, github.calls)
	require.Equal(t, 1, pinner.releaseCalls)
	require.Equal(t, "ambiguous-commit-receipt", pinner.releasedReceipt)
	require.Equal(t, []string{path}, pinner.releasedPaths)
}

func TestSubmit_DefinitiveGitHubRejectionReleasesPin(t *testing.T) {
	github := &fakeGitHub{returnErr: &issueAPIStatusError{status: http.StatusUnprocessableEntity}}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{receipt: "rejected-report-receipt"}
	handler.SetScreenshotPinner(pinner)
	response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": feedbackScreenshotPathPrefix + testAttachmentUUID}},
	})
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Equal(t, 1, pinner.calls)
	require.Equal(t, 1, pinner.releaseCalls)
	require.Equal(t, "rejected-report-receipt", pinner.releasedReceipt, "rejection must release only the attempt returned by Pin")
	require.Equal(t, pinner.paths, pinner.releasedPaths)
	require.Zero(t, pinner.confirmCalls)
}

func TestSubmit_DefinitiveRejectionSettlesReceiptAfterClientDisconnect(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	github := &fakeGitHub{
		returnErr: &issueAPIStatusError{status: http.StatusUnprocessableEntity},
		onCreate:  cancel,
	}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{
		receipt: "disconnected-report-receipt",
		onReleaseCtx: func(ctx context.Context) {
			require.NoError(t, ctx.Err(), "the rejected attempt must settle despite client cancellation")
			deadline, ok := ctx.Deadline()
			require.True(t, ok, "detached rejection settlement must remain bounded")
			require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, time.Second)
		},
	}
	handler.SetScreenshotPinner(pinner)
	path := feedbackScreenshotPathPrefix + testAttachmentUUID
	body, err := json.Marshal(map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": path}},
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", bytes.NewReader(body)).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestEngine(t, handler, "user-1").ServeHTTP(response, request)

	require.Equal(t, http.StatusBadGateway, response.Code)
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.Equal(t, 1, pinner.releaseCalls)
	require.Equal(t, "disconnected-report-receipt", pinner.releasedReceipt)
	require.Equal(t, []string{path}, pinner.releasedPaths)
	require.Zero(t, pinner.confirmCalls)
}

func TestSubmit_DevStubLeavesScreenshotPending(t *testing.T) {
	handler := NewHandler(logger.New("test"), nil, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{}
	handler.SetScreenshotPinner(pinner)
	response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": feedbackScreenshotPathPrefix + testAttachmentUUID}},
	})
	require.Equal(t, http.StatusOK, response.Code)
	require.JSONEq(t, `{"dev":true}`, response.Body.String())
	require.Zero(t, pinner.calls, "no external issue exists to justify retaining this image")
	require.Zero(t, pinner.confirmCalls)
}

func TestSubmit_PinAndIssueShareResponseDeadline(t *testing.T) {
	var pinDeadline, issueDeadline time.Time
	github := &fakeGitHub{onCreateCtx: func(ctx context.Context) {
		issueDeadline, _ = ctx.Deadline()
	}}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	handler.SetScreenshotPinner(&fakeScreenshotPinner{onPinCtx: func(ctx context.Context) {
		pinDeadline, _ = ctx.Deadline()
	}})
	response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": feedbackScreenshotPathPrefix + testAttachmentUUID}},
	})
	require.Equal(t, http.StatusOK, response.Code)
	require.False(t, pinDeadline.IsZero())
	require.Equal(t, pinDeadline, issueDeadline, "pinning must not grant GitHub a fresh deadline")
}

func TestSubmit_PinErrorsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pinErr     error
		wantStatus int
		wantError  string
	}{
		{name: "invalid ownership or lifecycle state", pinErr: feedbackscreenshots.ErrInvalid, wantStatus: http.StatusBadRequest, wantError: "invalid screenshot attachments"},
		{name: "storage failure", pinErr: errors.New("database details must stay private"), wantStatus: http.StatusServiceUnavailable, wantError: "screenshot storage unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := &fakeGitHub{}
			handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
			handler.SetScreenshotPinner(&fakeScreenshotPinner{err: tc.pinErr})
			response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
				"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
				"attachments": []map[string]string{{"url": feedbackScreenshotPathPrefix + testAttachmentUUID}},
			})

			assert.Equal(t, tc.wantStatus, response.Code)
			assert.Contains(t, response.Body.String(), tc.wantError)
			assert.NotContains(t, response.Body.String(), "database details")
			assert.Empty(t, github.calls, "pin errors must prevent issue creation")
		})
	}
}

func TestSubmit_GitHubErrorKeepsScreenshotPinned(t *testing.T) {
	var events []string
	github := &fakeGitHub{returnErr: assertGitHubError("timeout"), onCreate: func() { events = append(events, "issue") }}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{onPin: func() { events = append(events, "pin") }}
	handler.SetScreenshotPinner(pinner)
	response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
		"type": "bug", "title": "Screenshot report", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": feedbackScreenshotPathPrefix + testAttachmentUUID}},
	})

	assert.Equal(t, http.StatusBadGateway, response.Code)
	assert.Equal(t, []string{"pin", "issue"}, events, "the handler has no unpin operation after an ambiguous GitHub error")
	assert.Equal(t, 1, pinner.calls, "the screenshot remains pinned after GitHub returns an ambiguous error")
	assert.Zero(t, pinner.releaseCalls)
}

func TestSubmit_ScreenshotPinSkippedWhenNoEmbedWillBeCreated(t *testing.T) {
	t.Run("no attachments", func(t *testing.T) {
		pinner := &fakeScreenshotPinner{}
		handler := NewHandler(logger.New("test"), &fakeGitHub{}, testCorrKey, testMediaBaseURL)
		handler.SetScreenshotPinner(pinner)
		response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
			"type": "bug", "title": "Report", "description": "No screenshot.",
		})
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Zero(t, pinner.calls)
	})

	t.Run("empty media origin", func(t *testing.T) {
		pinner := &fakeScreenshotPinner{}
		handler := NewHandler(logger.New("test"), &fakeGitHub{}, testCorrKey, "")
		handler.SetScreenshotPinner(pinner)
		response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
			"type": "bug", "title": "Report", "description": "Screenshot embeds are disabled.",
			"attachments": []map[string]string{{"url": feedbackScreenshotPathPrefix + testAttachmentUUID}},
		})
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Zero(t, pinner.calls)
	})
}

// ─── PR #1547 review fixes ────────────────────────────────────────────────

// Category is bounded — Gitar / @security-reviewer finding on the
// originally-unbounded field. (Title/Description had existing caps.)
func TestSubmit_RejectsOversizeCategory(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "feature",
		"title":       "X",
		"description": "Y",
		"category":    strings.Repeat("a", maxCategoryBytes+1),
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "category exceeds")
}

// Defense-in-depth sanitization on ALL free-text diagnostics fields
// (Gitar finding: previously only Logs was sanitized server-side).
func TestSubmit_SanitizesAllDiagnosticsFields(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-42")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
		"diagnostics": map[string]interface{}{
			"appVersion":      "v1.0 leaked alice@example.com",
			"platform":        "darwin path /Users/alice/bin",
			"machineIdPrefix": "ip 192.168.1.5",
			"connectionPhase": "stable from 2001:db8:abcd:1234:5678:9abc:def0:1",
			"gpu":             map[string]string{"vendor": "vendor@evil.com", "renderer": "M1 /Users/bob/gpu"},
			"logs":            "ok",
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	body := gh.calls[0].Body
	assert.NotContains(t, body, "alice@example.com")
	assert.NotContains(t, body, "vendor@evil.com")
	assert.NotContains(t, body, "/Users/alice")
	assert.NotContains(t, body, "/Users/bob")
	assert.NotContains(t, body, "192.168.1.5")
	// IPv6 full-form is also matched by the IP pattern.
	assert.NotContains(t, body, "2001:db8:abcd")
}

// Markdown @-mentions and #-issue-refs in the TITLE are neutralized via ZWSP
// so a hostile client cannot ping arbitrary GitHub users. The title is
// title-context (GitHub renders no markdown links/HTML there), so ZWSP is the
// right tool for the `@`/`#` autolinks that DO fire in titles. (Post-#1547
// Fixes 2/4/5: BODY free-text is now code-fenced instead — see
// TestSubmit_DescriptionInjectionRenderedInert — so the body assertions here
// changed from "ZWSP present" to "inert via fence".)
func TestSubmit_NeutralizesMarkdownAutolinks(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "Ping @Concord-Voice/security please",
		"description": "Refs #1547 and exfil ![pwn](https://attacker.example/?u=1)",
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)

	// Title: the bare "@Concord" must NOT appear without the ZWSP. The
	// rendered token is "@\u200bConcord-Voice/security" — looks identical
	// to a triager but won't autolink. We assert by checking the ZWSP is
	// present immediately after `@`.
	assert.Contains(t, gh.calls[0].Title, "@\u200bConcord-Voice/security")
	// Body: the image-embed + masked-link payload is now inert inside a code
	// fence \u2014 the `](` trigger must NOT be present and the description block
	// opens with a fence. (Stronger than the old ZWSP-in-`![` neutralization,
	// which left the masked `](url)` link live.)
	assertNoActiveMarkup(t, gh.calls[0].Body)
	assert.Contains(t, gh.calls[0].Body, "## Description\n\n```")
}

// Injection payloads in free-text diagnostics fields (a compromised client
// could place them in e.g. the GPU vendor string) are rendered inert via the
// inline code span \u2014 superseding the old GPU-vendor ZWSP neutralization.
func TestSubmit_NeutralizesMarkdownInDiagnostics(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "T",
		"description": "D",
		"diagnostics": map[string]interface{}{
			"appVersion":      "v1.0",
			"platform":        "darwin",
			"machineIdPrefix": "4c33734c",
			"connectionPhase": "stable",
			"gpu":             map[string]string{"vendor": "@evil-team [x](https://evil.example)", "renderer": "M1"},
			"logs":            "",
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	body := gh.calls[0].Body
	// The vendor value is inside an inline code span (rendered verbatim, no
	// autolink / no live masked link).
	assert.Contains(t, body, "- **GPU:** `")
	assertNoActiveMarkup(t, body)
}

// User-supplied logs containing backtick runs cannot escape the fenced
// code block — writeDiagnostics picks a fence whose length exceeds the
// longest backtick run in the content.
func TestSubmit_LogsCannotEscapeCodeFence(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	// Logs contain a closing 3-backtick sequence followed by an injection
	// attempt. The handler must pick a 4+-backtick fence so the injection
	// stays inside the code block.
	logs := "log line 1\n```\n@evil-injection\n```\nlog line 2"

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "X",
		"description": "Y",
		"diagnostics": map[string]interface{}{
			"appVersion":      "v",
			"platform":        "p",
			"machineIdPrefix": "m",
			"connectionPhase": "s",
			"logs":            logs,
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	body := gh.calls[0].Body

	// The body must use a 4-backtick fence around the logs (3 in content + 1).
	assert.Contains(t, body, "\n````\n")
	// The 3-backtick run inside the logs is preserved verbatim — it does
	// NOT close the surrounding 4-backtick fence.
	assert.Contains(t, body, "```")
}

// 413 response when the request body exceeds MaxRequestBytes — distinct
// from the 400 "invalid request body" used for malformed JSON.
func TestSubmit_OversizeBodyReturns413(t *testing.T) {
	h := newTestHandler(&fakeGitHub{})
	r := newTestEngine(t, h, "user-1")

	// Build a JSON-valid body larger than MaxRequestBytes (128 KB) by
	// putting a long string into `description` — the JSON parser reads
	// the whole body before deciding it's malformed, so MaxBytesReader
	// trips first.
	body := strings.Builder{}
	body.WriteString(`{"type":"bug","title":"X","description":"`)
	body.WriteString(strings.Repeat("a", MaxRequestBytes+100))
	body.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "request body too large")
}

// The assembled GitHub issue body must stay under maxBodyBytes even when
// every per-field cap is saturated — GitHub rejects bodies > 65,536 chars.
// Verifies the lower maxLogsBytes (40_000) + per-field caps mathematically
// keep the body under GitHub's limit.
func TestSubmit_AssembledBodyStaysUnderGitHubLimit(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       strings.Repeat("a", maxTitleBytes),
		"description": strings.Repeat("b", maxDescriptionBytes),
		"diagnostics": map[string]interface{}{
			"appVersion":      "v0.1.60",
			"platform":        "darwin",
			"machineIdPrefix": "4c33734c",
			"connectionPhase": "stable",
			"gpu":             map[string]string{"vendor": "Apple", "renderer": "M1 Pro"},
			"display":         map[string]interface{}{"width": 3024, "height": 1964, "scaleFactor": 2},
			"logs":            strings.Repeat("c", maxLogsBytes),
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	// GitHub's REST API caps issue bodies at 65,536 chars; maxBodyBytes
	// is 60,000 — assembled body must stay under maxBodyBytes too.
	assert.LessOrEqual(t, len(gh.calls[0].Body), maxBodyBytes,
		"assembled body %d bytes exceeds maxBodyBytes %d — would be rejected by GitHub",
		len(gh.calls[0].Body), maxBodyBytes)
}

func TestSubmit_OverflowingDiagnosticsKeepPinnedScreenshotsInFinalIssue(t *testing.T) {
	github := &fakeGitHub{}
	handler := NewHandler(logger.New("test"), github, testCorrKey, testMediaBaseURL)
	pinner := &fakeScreenshotPinner{}
	handler.SetScreenshotPinner(pinner)
	path := feedbackScreenshotPathPrefix + testAttachmentUUID
	response := doPost(t, newTestEngine(t, handler, "user-1"), map[string]interface{}{
		"type": "bug", "title": "Oversized metadata", "description": "Report with screenshot.",
		"attachments": []map[string]string{{"url": path}},
		"diagnostics": map[string]interface{}{
			"gpu": map[string]string{"vendor": strings.Repeat("v ", maxBodyBytes/2), "renderer": "test"},
		},
	})
	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, github.calls, 1)
	require.LessOrEqual(t, len(github.calls[0].Body), maxBodyBytes)
	require.Contains(t, github.calls[0].Body, "![screenshot-1]("+testMediaBaseURL+path+")")
	require.Contains(t, github.calls[0].Body, truncatedBodyMarker)
	require.Equal(t, []string{path}, pinner.paths)
	require.Equal(t, []string{path}, pinner.confirmedPaths)
}

// ─── Public-repo injection hardening (PR #1547 Fixes 2/4/5) ───────────────

// injectionPayloads are the four user-controlled sequences that MUST NOT
// render as an active link / image / raw HTML in the produced issue body.
// Each appears in BOTH a multi-line free-text field (Description) and a short
// metadata field (GPU vendor) below.
var injectionPayloads = []struct {
	name    string
	payload string
}{
	{"masked markdown link", "[pwn](https://evil.example)"},
	{"raw anchor html", `<a href="https://evil.example">x</a>`},
	{"raw img html", `<img src="https://evil.example/p.gif">`},
	{"bare url", "https://evil.example/track"},
}

// onlyBackticks reports whether trimmed line is a non-empty run of only
// backtick characters (a fenced-code-block delimiter line).
func onlyBackticks(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	for _, r := range t {
		if r != '`' {
			return false
		}
	}
	return true
}

// stripCodeRegions removes every fenced code block (a ``` delimiter line, its
// contents, and the closing delimiter line) and every inline code span
// (`...`, any backtick-run length) from s, modelling how GitHub treats their
// contents as inert literal text. What remains is the "live markdown" —
// anything that WOULD be parsed as links/HTML/autolinks. The injection
// assertions check that no trigger survives into this residue, which is the
// actual security property (payloads MAY appear verbatim inside a fence/span —
// that is the whole point).
//
// Go's regexp (RE2) has no backreferences, so fenced blocks are stripped with
// a line-state toggle rather than a `\1`-style same-length match; inline spans
// are then stripped longest-run-first.
func stripCodeRegions(s string) string {
	// 1. Strip fenced code blocks via a line-by-line in/out toggle. Any line
	//    that is purely backticks toggles fenced state; lines inside a fence
	//    (and the delimiter lines themselves) are dropped.
	var out []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if onlyBackticks(line) {
			inFence = !inFence
			continue // drop the delimiter line itself
		}
		if inFence {
			continue // drop fenced content
		}
		out = append(out, line)
	}
	s = strings.Join(out, "\n")
	// 2. Strip inline code spans: a run of N backticks ... next run of N.
	//    Longest runs first so a longer span isn't mis-split by a nested
	//    shorter run. The upper bound is the longest backtick run present in s:
	//    inlineCode emits a fence of longestBacktickRun(value)+1, which exceeds
	//    3 when the wrapped value itself contains a 3+-backtick run, so a fixed
	//    {3,2,1} would miss those — derive the bound dynamically.
	for n := longestBacktickRun(s); n >= 1; n-- {
		fence := strings.Repeat("`", n)
		spanRE := regexp.MustCompile("(?s)" + regexp.QuoteMeta(fence) + ".*?" + regexp.QuoteMeta(fence))
		s = spanRE.ReplaceAllString(s, "")
	}
	return s
}

// assertNoActiveMarkup asserts no parseable active markup survives outside the
// code fences/spans. The raw injection bytes MAY appear verbatim inside a
// fence/span (rendered as literal text); what must NOT survive into the
// fence-stripped residue is any markup trigger: a masked-link `](`, raw
// `<a `/`<img ` HTML, or a bare autolinked `https://` URL.
func assertNoActiveMarkup(t *testing.T, body string) {
	t.Helper()
	live := stripCodeRegions(body)
	assert.NotContains(t, live, "](", "masked-link trigger `](` must not survive outside a code fence/span")
	assert.NotContains(t, live, "<a ", "raw <a ...> anchor must not survive outside a code fence/span")
	assert.NotContains(t, live, "<img ", "raw <img ...> must not survive outside a code fence/span")
	assert.NotContains(t, live, "https://", "bare URL must not survive outside a code fence/span (would autolink)")
}

// Description containing each injection payload must be rendered inert (inside
// a fenced code block), so none parse as active markup.
func TestSubmit_DescriptionInjectionRenderedInert(t *testing.T) {
	for _, p := range injectionPayloads {
		t.Run(p.name, func(t *testing.T) {
			gh := &fakeGitHub{}
			h := newTestHandler(gh)
			r := newTestEngine(t, h, "user-1")

			w := doPost(t, r, map[string]interface{}{
				"type":        "bug",
				"title":       "T",
				"description": "before " + p.payload + " after",
			})
			require.Equal(t, http.StatusOK, w.Code)
			require.Len(t, gh.calls, 1)
			body := gh.calls[0].Body

			assertNoActiveMarkup(t, body)
			// The payload bytes are preserved verbatim inside the fence so
			// triagers can still read what was submitted; only the surrounding
			// text and the fence opener are asserted directly.
			assert.Contains(t, body, "before ")
			assert.Contains(t, body, " after")
			// A fenced code block opens the Description section.
			assert.Contains(t, body, "## Description\n\n```")
		})
	}
}

// A metadata field (GPU vendor) containing each injection payload must be
// rendered inert (inside an inline code span), so none parse as active markup.
func TestSubmit_MetadataInjectionRenderedInert(t *testing.T) {
	for _, p := range injectionPayloads {
		t.Run(p.name, func(t *testing.T) {
			gh := &fakeGitHub{}
			h := newTestHandler(gh)
			r := newTestEngine(t, h, "user-1")

			w := doPost(t, r, map[string]interface{}{
				"type":        "bug",
				"title":       "T",
				"description": "D",
				"diagnostics": map[string]interface{}{
					"appVersion":      "v1.0",
					"platform":        "darwin",
					"machineIdPrefix": "4c33734c",
					"connectionPhase": "stable",
					"gpu":             map[string]string{"vendor": p.payload, "renderer": "M1"},
					"logs":            "",
				},
			})
			require.Equal(t, http.StatusOK, w.Code)
			require.Len(t, gh.calls, 1)
			body := gh.calls[0].Body

			assertNoActiveMarkup(t, body)
			// The GPU bullet is present and the value is wrapped in a code span.
			assert.Contains(t, body, "- **GPU:** `")
		})
	}
}

// A metadata value containing a backtick run must not be able to terminate its
// own inline code span early — the span fence must exceed the longest run.
func TestSubmit_MetadataBacktickCannotEscapeSpan(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	// Vendor contains a single backtick then a masked-link attempt. A naive
	// single-backtick span would close at the first backtick and expose the
	// `](` trigger; inlineCode must pick a 2+-backtick span.
	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "T",
		"description": "D",
		"diagnostics": map[string]interface{}{
			"appVersion":      "v1.0",
			"platform":        "darwin",
			"machineIdPrefix": "4c33734c",
			"connectionPhase": "stable",
			"gpu":             map[string]string{"vendor": "x`y[pwn](https://evil.example)", "renderer": "M1"},
			"logs":            "",
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	assertNoActiveMarkup(t, gh.calls[0].Body)
}

// The Description is now fenced (Fix 2) — a bare `@mention` / `#123` inside it
// is inert by virtue of the code fence, NOT ZWSP insertion. The Title still
// uses neutralizeAutolinks (title-context), so the existing Title autolink
// assertion in TestSubmit_NeutralizesMarkdownAutolinks remains valid.
func TestSubmit_DescriptionFencedNotZWSP(t *testing.T) {
	gh := &fakeGitHub{}
	h := newTestHandler(gh)
	r := newTestEngine(t, h, "user-1")

	w := doPost(t, r, map[string]interface{}{
		"type":        "bug",
		"title":       "T",
		"description": "ping @someone and ref #1547",
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, gh.calls, 1)
	body := gh.calls[0].Body
	// The literal text survives verbatim inside the fence (no ZWSP injected
	// into the body free-text any more).
	zwsp := string(rune(0x200b)) // U+200B ZERO WIDTH SPACE, built without a literal char
	assert.Contains(t, body, "ping @someone and ref #1547")
	assert.NotContains(t, body, "@"+zwsp+"someone", "body free-text must be fenced, not ZWSP-mangled")
	assert.Contains(t, body, "## Description\n\n```")
}

// ─── Rune-aware truncation (PR #1547 Fix 8) ───────────────────────────────

// Adversarial @-heavy input run through neutralizeAutolinks expands each `@`
// into `@`+ZWSP (a 3-byte multi-byte rune). When such expanded content overruns
// maxBodyBytes, truncateBody's slice point can land mid-ZWSP — the regression
// truncateBody (Fix 8) walks back to a rune boundary. We construct the
// post-neutralization body directly (the realistic source of multi-byte
// inflation, mirroring how neutralizeAutolinks inflates @-heavy strings) and
// assert the result is valid UTF-8 and still marked.
func TestTruncateBody_AtHeavyZWSPInflation(t *testing.T) {
	// 40k '@' chars → after neutralizeAutolinks each becomes '@' followed by a
	// 3-byte ZWSP (U+200B), ~160k bytes total: well past maxBodyBytes, densely
	// packed with the multi-byte rune so the cut point is virtually guaranteed
	// to fall inside a rune.
	atHeavy := neutralizeAutolinks(strings.Repeat("@", 40_000))
	body := "header\n\n" + atHeavy
	require.Greater(t, len(body), maxBodyBytes, "precondition: body must overrun maxBodyBytes")

	out := truncateBody(body)

	assert.LessOrEqual(t, len(out), maxBodyBytes, "truncated body must respect maxBodyBytes")
	assert.True(t, utf8.ValidString(out), "truncated body must be valid UTF-8 — no rune split into U+FFFD")
	assert.False(t, strings.ContainsRune(out, '�'), "truncated body must not contain U+FFFD replacement char")
	assert.True(t, strings.HasSuffix(out, "..._\n"), "truncated body must end with the truncation marker")
}

// truncateBody unit: a multi-byte rune straddling the keep boundary is walked
// back so the slice never splits it (happy-path: under-cap is a no-op; error/
// edge-path: over-cap with a rune at the boundary stays valid).
func TestTruncateBody_RuneBoundary(t *testing.T) {
	t.Run("under cap is unchanged", func(t *testing.T) {
		in := "short body"
		assert.Equal(t, in, truncateBody(in))
	})

	t.Run("over cap with rune at boundary stays valid UTF-8", func(t *testing.T) {
		// Build a body of all 3-byte ZWSPs longer than maxBodyBytes so the
		// keep boundary is virtually guaranteed to fall inside a rune.
		zwsp := string(rune(0x200b))             // U+200B, built without a literal char
		in := strings.Repeat(zwsp, maxBodyBytes) // 3*maxBodyBytes bytes
		out := truncateBody(in)
		assert.LessOrEqual(t, len(out), maxBodyBytes)
		assert.True(t, utf8.ValidString(out), "must be valid UTF-8 after rune-boundary walk-back")
		assert.True(t, strings.HasSuffix(out, "..._\n"))
	})
}

func TestBuildIssue_NoRawUserID(t *testing.T) {
	req := &submitRequest{Type: reportTypeBug, Title: "x", Description: "y"}
	const uid = "550e8400-e29b-41d4-a716-446655440000"
	token := DeriveCorrelationToken([]byte("s"), uid)

	_, body, _, _ := buildIssue(req, token, testMediaBaseURL)

	assert.NotContains(t, body, uid, "raw UUID must never appear in the issue body")
	assert.Contains(t, body, "**Reported by:** report `"+token+"`")
}

// ─── helpers ──────────────────────────────────────────────────────────────

type fakeError string

func (e fakeError) Error() string { return string(e) }

func assertGitHubError(msg string) error { return fakeError("github: " + msg) }

// ─── Bug-report screenshot attachments (#1747) ────────────────────────────

const testAttachmentUUID = "550e8400-e29b-41d4-a716-446655440000"

// mk builds a minimal, otherwise-valid submitRequest with the given type and
// attachments — the shared table-test fixture for TestValidate_AttachmentReferences.
func mk(typ reportType, atts []attachmentRef) *submitRequest {
	return &submitRequest{Type: typ, Title: "title", Description: "description", Attachments: atts}
}

func ref(u string) attachmentRef { return attachmentRef{URL: u} }

func TestValidate_AttachmentReferences(t *testing.T) {
	validURL := feedbackScreenshotPathPrefix + testAttachmentUUID

	fourValid := make([]attachmentRef, 0, maxAttachmentCount)
	for i := 0; i < maxAttachmentCount; i++ {
		fourValid = append(fourValid, ref(validURL))
	}
	fiveValid := append(append([]attachmentRef{}, fourValid...), ref(validURL))

	cases := []struct {
		name    string
		req     *submitRequest
		wantErr bool
	}{
		{"valid screenshot path", mk(reportTypeBug, []attachmentRef{ref(validURL)}), false},
		{"no attachments (nil)", mk(reportTypeBug, nil), false},
		{"max count (4) is allowed", mk(reportTypeBug, fourValid), false},
		{"over max count (5) is rejected", mk(reportTypeBug, fiveValid), true},
		{"attachments on a feature request are rejected", mk(reportTypeFeature, []attachmentRef{ref(validURL)}), true},
		{"absolute URL is rejected", mk(reportTypeBug, []attachmentRef{ref("https://evil.example/x.png")}), true},
		{"wrong path prefix is rejected", mk(reportTypeBug, []attachmentRef{ref("/api/v1/media/avatars/" + testAttachmentUUID)}), true},
		{"non-uuid suffix is rejected", mk(reportTypeBug, []attachmentRef{ref(feedbackScreenshotPathPrefix + "not-a-uuid")}), true},
		{"path traversal is rejected", mk(reportTypeBug, []attachmentRef{ref(feedbackScreenshotPathPrefix + "../../../etc/passwd")}), true},
		// uuid.Parse is not a validator: it also accepts braces, urn:uuid:,
		// 32-hex, and uppercase forms. isValidScreenshotPath must reject every
		// non-canonical form via the parsed.String() round-trip (#1747 review).
		{"braces-wrapped uuid is rejected", mk(reportTypeBug, []attachmentRef{ref(feedbackScreenshotPathPrefix + "{" + testAttachmentUUID + "}")}), true},
		{"urn:uuid: form is rejected", mk(reportTypeBug, []attachmentRef{ref(feedbackScreenshotPathPrefix + "urn:uuid:" + testAttachmentUUID)}), true},
		{"32-hex unhyphenated form is rejected", mk(reportTypeBug, []attachmentRef{ref(feedbackScreenshotPathPrefix + strings.ReplaceAll(testAttachmentUUID, "-", ""))}), true},
		{"uppercase canonical form is rejected", mk(reportTypeBug, []attachmentRef{ref(feedbackScreenshotPathPrefix + strings.ToUpper(testAttachmentUUID))}), true},
		// The 38-byte braces form only examines the middle 36 bytes, so the
		// first/last byte can be ANYTHING that parses -- including a raw
		// newline, which would otherwise reach the public GitHub issue body.
		{"trailing-newline braces form is rejected", mk(reportTypeBug, []attachmentRef{ref(feedbackScreenshotPathPrefix + "{" + testAttachmentUUID + "\n")}), true},
		{"newline injection is rejected", mk(reportTypeBug, []attachmentRef{ref(validURL + "\n![x](http://evil)")}), true},
		{"query suffix is rejected", mk(reportTypeBug, []attachmentRef{ref(validURL + "?x=1")}), true},
		{"empty url is rejected", mk(reportTypeBug, []attachmentRef{ref("")}), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validate(c.req)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBuildIssue_Screenshots(t *testing.T) {
	id1 := testAttachmentUUID
	id2 := "660e8400-e29b-41d4-a716-446655440001"

	bugReqWithAttachments := &submitRequest{
		Type:        reportTypeBug,
		Title:       "Crash on send",
		Description: "Steps to reproduce: 1...",
		Attachments: []attachmentRef{
			ref(feedbackScreenshotPathPrefix + id1),
			ref(feedbackScreenshotPathPrefix + id2),
		},
	}

	t.Run("bug report with attachments and a configured mediaBaseURL embeds screenshots", func(t *testing.T) {
		_, body, _, paths := buildIssue(bugReqWithAttachments, "report-token", "https://media.test")

		assert.Contains(t, body, "## Screenshots")
		assert.Contains(t, body, "![screenshot-1](https://media.test"+feedbackScreenshotPathPrefix+id1+")")
		assert.Contains(t, body, "![screenshot-2](https://media.test"+feedbackScreenshotPathPrefix+id2+")")
		assert.Equal(t, []string{feedbackScreenshotPathPrefix + id1, feedbackScreenshotPathPrefix + id2}, paths)
		// The description free text is still present alongside the screenshots.
		assert.Contains(t, body, "Steps to reproduce")
	})

	t.Run("empty mediaBaseURL skips the screenshots section entirely", func(t *testing.T) {
		_, body, _, paths := buildIssue(bugReqWithAttachments, "report-token", "")

		assert.NotContains(t, body, "## Screenshots")
		assert.NotContains(t, body, "![screenshot")
		assert.Empty(t, paths)
	})

	t.Run("feature requests never emit a screenshots section, even with attachments set", func(t *testing.T) {
		featureReq := &submitRequest{
			Type:        reportTypeFeature,
			Title:       "Add dark mode",
			Description: "Would help.",
			Attachments: []attachmentRef{ref(feedbackScreenshotPathPrefix + id1)},
		}
		_, body, _, paths := buildIssue(featureReq, "report-token", "https://media.test")

		assert.NotContains(t, body, "## Screenshots")
		assert.Empty(t, paths)
	})

	t.Run("truncation returns only complete screenshot embeds", func(t *testing.T) {
		longOrigin := strings.Repeat("o", maxBodyBytes/2)
		_, body, _, paths := buildIssue(bugReqWithAttachments, "report-token", longOrigin)
		require.Len(t, body, maxBodyBytes)
		require.Equal(t, []string{feedbackScreenshotPathPrefix + id1}, paths)
		require.Contains(t, body, "![screenshot-1]("+longOrigin+feedbackScreenshotPathPrefix+id1+")")
		require.NotContains(t, body, "![screenshot-2]("+longOrigin+feedbackScreenshotPathPrefix+id2+")")
	})
}

// Regression for #3584. A local combined driver can supply the actual public
// Compose origin; ordinary package runs keep both independent positive controls.
func TestSubmit_PublicSelfhostScreenshotOrigin(t *testing.T) {
	const screenshotPath = "/api/v1/media/feedback-screenshots/550e8400-e29b-41d4-a716-446655440000"
	cases := []struct{ name, origin, wantURL string }{
		{"public API origin", "https://api.feedback-host.example.invalid",
			"https://api.feedback-host.example.invalid" + screenshotPath},
		{"explicit screenshot origin", "https://screenshots.example.invalid",
			"https://screenshots.example.invalid" + screenshotPath},
	}
	if origin, supplied := os.LookupEnv("CONCORD_TEST_SELFHOST_MEDIA_ORIGIN"); supplied {
		wantURL := os.Getenv("CONCORD_TEST_SELFHOST_EXPECTED_SCREENSHOT_URL")
		require.NotEmpty(t, wantURL, "rendered Compose consumer fixture needs an independent expected screenshot URL")
		cases = append(cases, struct{ name, origin, wantURL string }{"rendered public Compose origin", origin, wantURL})
	}
	for _, cell := range cases {
		t.Run(cell.name, func(t *testing.T) {
			github := &fakeGitHub{}
			handler := NewHandler(logger.New("test"), github, testCorrKey, cell.origin)
			handler.SetScreenshotPinner(&fakeScreenshotPinner{})
			response := doPost(t, newTestEngine(t, handler, "owned-screenshot-reporter"), map[string]interface{}{
				"type": "bug", "title": "Screenshot report", "description": "Screenshot should reach the issue.",
				"attachments": []map[string]string{{"url": screenshotPath}},
			})
			require.Equal(t, http.StatusOK, response.Code, "screenshot fixture must pass report validation")
			require.Len(t, github.calls, 1, "enabled feedback must reach the issue creation boundary")
			assert.Contains(t, github.calls[0].Body, "![screenshot-1]("+cell.wantURL+")",
				"public self-host feedback must embed the absolute screenshot link using the consumed media origin")
		})
	}
}
