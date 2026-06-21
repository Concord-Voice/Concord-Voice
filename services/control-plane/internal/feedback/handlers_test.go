package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// fakeGitHub captures CreateIssue calls so handler tests can assert what
// would have been sent to GitHub without making any network call.
type fakeGitHub struct {
	calls       []IssueRequest
	returnIssue *IssueResponse
	returnErr   error
}

func (f *fakeGitHub) CreateIssue(_ context.Context, req IssueRequest) (*IssueResponse, error) {
	f.calls = append(f.calls, req)
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	if f.returnIssue != nil {
		return f.returnIssue, nil
	}
	return &IssueResponse{Number: 1, HTMLURL: "https://github.com/test/repo/issues/1"}, nil
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

func newTestHandler(github GitHubIssueCreator) *Handler {
	return NewHandler(logger.New("test"), github)
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
	assert.Contains(t, call.Body, "user `user-42`")
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
	assert.Contains(t, call.Body, "user `user-9`")
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

// ─── helpers ──────────────────────────────────────────────────────────────

type fakeError string

func (e fakeError) Error() string { return string(e) }

func assertGitHubError(msg string) error { return fakeError("github: " + msg) }
