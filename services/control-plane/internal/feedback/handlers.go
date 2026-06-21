package feedback

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	// Per-field length caps mirror the client-side validation in
	// FeedbackModal / panels. The server still enforces them as defense
	// against a compromised client. Bytes, not runes — we cap below
	// MaxRequestBytes to keep the budget predictable.
	maxTitleBytes       = 200
	maxDescriptionBytes = 8000
	maxCategoryBytes    = 200
	// maxLogsBytes is the per-field cap on the diagnostic log dump that
	// the client may submit. Lowered from 80_000 to 40_000 (Gitar
	// finding, PR #1547) so the assembled GitHub issue body stays under
	// maxBodyBytes for typical max-payload bug reports — GitHub's REST
	// API rejects issue creation when the body exceeds ~65,536 chars.
	maxLogsBytes = 40_000
	// maxBodyBytes is a hard cap on the assembled GitHub issue body.
	// Stays comfortably under GitHub's 65,536-char limit to leave room
	// for unexpected expansion (markdown rendering, escape sequences).
	// `buildIssue` truncates the logs tail when this cap would be
	// exceeded — never silently submits an oversize body that GitHub
	// would reject with 422.
	maxBodyBytes = 60_000

	// MaxRequestBytes is the total POST body size cap enforced at decode
	// time. Anything larger triggers 413 before we touch JSON parsing.
	MaxRequestBytes = 128 * 1024
)

// reportType is the discriminator the client sets. The handler dispatches
// the GitHub issue title / body / labels based on this value.
type reportType string

const (
	reportTypeBug     reportType = "bug"
	reportTypeFeature reportType = "feature"
)

// submitRequest is the JSON shape the client POSTs. Fields not listed are
// ignored — we do NOT pass arbitrary keys through to GitHub.
type submitRequest struct {
	Type        reportType   `json:"type"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Category    string       `json:"category,omitempty"`
	Diagnostics *diagnostics `json:"diagnostics,omitempty"`
}

type diagnostics struct {
	AppVersion      string       `json:"appVersion"`
	Platform        string       `json:"platform"`
	MachineIDPrefix string       `json:"machineIdPrefix"`
	GPU             *gpuInfo     `json:"gpu,omitempty"`
	Display         *displayInfo `json:"display,omitempty"`
	ConnectionPhase string       `json:"connectionPhase"`
	Logs            string       `json:"logs"`
}

type gpuInfo struct {
	Vendor   string `json:"vendor"`
	Renderer string `json:"renderer"`
}

type displayInfo struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	RefreshRate float64 `json:"refreshRate,omitempty"`
	ScaleFactor float64 `json:"scaleFactor"`
}

// submitResponse is what the client gets back. `IssueURL` is empty on the
// dev-stub path so the client knows the submission was recorded but no
// GitHub issue was actually created. `Dev` is the explicit flag so the
// client can render a clarifying message in the success surface.
type submitResponse struct {
	IssueURL string `json:"issueUrl,omitempty"`
	Dev      bool   `json:"dev"`
}

// GitHubIssueCreator is the narrow interface handlers.Handler depends on so
// tests can swap in a fake. The production implementation is *Client in
// github.go.
type GitHubIssueCreator interface {
	CreateIssue(ctx context.Context, req IssueRequest) (*IssueResponse, error)
}

// Handler owns the POST /api/v1/feedback endpoint.
//
// Dev stub: when `github == nil` the handler logs the assembled issue body
// and returns `{dev: true}` without making any network call. This is the
// stub-in-dev / hard-fail-in-prod posture — config.go's production guard
// fatal-exits before we ever construct a nil-github Handler in production.
type Handler struct {
	log    *logger.Logger
	github GitHubIssueCreator
}

// NewHandler builds a Handler. Pass `nil` for `github` in dev to enable the
// log-and-skip stub.
func NewHandler(log *logger.Logger, github GitHubIssueCreator) *Handler {
	return &Handler{log: log, github: github}
}

// Submit handles POST /api/v1/feedback. Requires the AuthRequired
// middleware upstream so `user_id` is on the Gin context. Rate limit is
// enforced upstream via middleware.RateLimitByUser(redis, 10, 1*time.Hour) —
// raised from 3/hour per Gitar finding to reduce false lockouts from
// validation typos while still capping spam.
func (h *Handler) Submit(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Size cap before we touch JSON parsing — protects the parser and
	// prevents large-body resource burn from a misbehaving client. When the
	// cap is exceeded, ShouldBindJSON below surfaces an *http.MaxBytesError
	// which we translate to HTTP 413 (not the generic 400) so the client
	// can distinguish "your payload is too big" from "your payload is
	// malformed."
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBytes)

	var req submitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := validate(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Defense-in-depth PII re-sanitization. The threat model is a
	// compromised client that bypasses the renderer-side scrub at
	// services/logBufferService.ts — the server is the LAST line before
	// content reaches a public-ish GitHub issue, so every free-text
	// string the client sends gets scrubbed here, not just Logs.
	// (Gitar / @security-reviewer finding, PR #1547.)
	req.Title = Sanitize(req.Title)
	req.Description = Sanitize(req.Description)
	req.Category = Sanitize(req.Category)
	if req.Diagnostics != nil {
		req.Diagnostics.AppVersion = Sanitize(req.Diagnostics.AppVersion)
		req.Diagnostics.Platform = Sanitize(req.Diagnostics.Platform)
		req.Diagnostics.MachineIDPrefix = Sanitize(req.Diagnostics.MachineIDPrefix)
		req.Diagnostics.ConnectionPhase = Sanitize(req.Diagnostics.ConnectionPhase)
		if req.Diagnostics.GPU != nil {
			req.Diagnostics.GPU.Vendor = Sanitize(req.Diagnostics.GPU.Vendor)
			req.Diagnostics.GPU.Renderer = Sanitize(req.Diagnostics.GPU.Renderer)
		}
		req.Diagnostics.Logs = Sanitize(req.Diagnostics.Logs)
	}

	title, body, labels := buildIssue(&req, userID)

	// Dev stub: log the body and return success without calling GitHub.
	if h.github == nil {
		h.log.Info("feedback.submit (dev stub — no GitHub call)",
			"type", req.Type,
			"user_id", userID,
			"title", title,
			"label_count", len(labels),
			"body_bytes", len(body),
		)
		c.JSON(http.StatusOK, submitResponse{Dev: true})
		return
	}

	// Production path: post to GitHub. 12s budget (room under the client's
	// http.Client 15s above and the typical client UI timeout).
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()

	issue, err := h.github.CreateIssue(ctx, IssueRequest{
		Title:  title,
		Body:   body,
		Labels: labels,
	})
	if err != nil {
		h.log.Warn("feedback.submit: GitHub create failed", "error", err.Error(), "user_id", userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "feedback submission failed; please try again later"})
		return
	}

	c.JSON(http.StatusOK, submitResponse{IssueURL: issue.HTMLURL, Dev: false})
}

// validate runs cheap structural checks on the parsed request. Heavier
// content checks (sanitization, body assembly) happen after.
func validate(req *submitRequest) error {
	switch req.Type {
	case reportTypeBug, reportTypeFeature:
		// ok
	default:
		return fmt.Errorf("invalid report type")
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return fmt.Errorf("title is required")
	}
	if len(title) > maxTitleBytes {
		return fmt.Errorf("title exceeds %d bytes", maxTitleBytes)
	}
	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		return fmt.Errorf("description is required")
	}
	if len(desc) > maxDescriptionBytes {
		return fmt.Errorf("description exceeds %d bytes", maxDescriptionBytes)
	}
	// Category is optional, but when present must be bounded — the wire
	// schema (openapi.yaml /feedback) describes it as free-text for now;
	// #160 (Feature Request panel) will narrow it to a fixed enum.
	if len(req.Category) > maxCategoryBytes {
		return fmt.Errorf("category exceeds %d bytes", maxCategoryBytes)
	}
	if req.Diagnostics != nil && len(req.Diagnostics.Logs) > maxLogsBytes {
		return fmt.Errorf("logs exceed %d bytes", maxLogsBytes)
	}
	return nil
}

// neutralizeAutolinks inserts a zero-width space (U+200B) after Markdown
// control characters that the GitHub issue renderer would otherwise expand
// into autolinks or embedded content:
//
//   - `@username` / `@org/team` — notification spam (PII enumeration via
//     mentions, harassment via team-pings)
//   - `![alt](url)` — image embed; loads attacker-controlled URL with the
//     triager's browser when the issue is viewed (referrer leak + IP probe)
//   - `#123` — backlinks to unrelated issues/PRs in the feedback repo
//
// ZWSP is invisible in the rendered issue but breaks the autolink trigger
// pattern.
//
// SCOPE (post-#1547 review, Fixes 2/4/5): this is applied ONLY to the issue
// TITLE. GitHub does not render markdown links or raw HTML in issue titles, so
// masked links / `<a>` / `<img>` / bare URLs are inert there by GitHub's own
// title-context rules — but `@name` and `#123` autolinks DO still fire in
// titles, which is what this defuses. Every user-controlled BODY field is now
// rendered inert by code-fencing instead (fencedBlock for multi-line text,
// inlineCode for short metadata), which is strictly stronger than ZWSP
// insertion — see those helpers. Do NOT route body fields back through this:
// ZWSP does not inert a masked link `[t](u)` or raw HTML, fencing does.
//
// (Gitar / @security-reviewer finding, PR #1547.)
func neutralizeAutolinks(s string) string {
	const zwsp = "\u200b"
	if !strings.ContainsAny(s, "@!#") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		b.WriteByte(c)
		switch c {
		case '@':
			// Always neutralize — any `@`-prefixed token autolinks.
			b.WriteString(zwsp)
		case '!':
			// Only the `![` image syntax is dangerous; bare `!` is fine.
			if i+1 < len(s) && s[i+1] == '[' {
				b.WriteString(zwsp)
			}
		case '#':
			// `#123` / `#GH-123` / `#username/repo#123` all autolink.
			// Conservative: ZWSP when followed by an alphanumeric char.
			if i+1 < len(s) {
				next := s[i+1]
				if isAlphanumeric(next) {
					b.WriteString(zwsp)
				}
			}
		}
	}
	return b.String()
}

func isAlphanumeric(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// longestBacktickRun returns the length of the longest run of consecutive
// backticks in s. Used by writeDiagnostics to choose a code-fence length
// that exceeds any backtick sequence in the user-supplied logs — so the
// content cannot escape the fence and inject markdown / HTML into the
// surrounding issue body.
func longestBacktickRun(s string) int {
	maxRun := 0
	cur := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			if cur > maxRun {
				maxRun = cur
			}
		} else {
			cur = 0
		}
	}
	return maxRun
}

// fenceOf returns a backtick run of at least minLen and strictly longer than
// the longest backtick run inside s, so user-supplied content cannot escape
// the fence. minLen is the CommonMark floor for the context: 3 for fenced code
// BLOCKS, 1 for inline code SPANS.
func fenceOf(s string, minLen int) string {
	fenceLen := longestBacktickRun(s) + 1
	if fenceLen < minLen {
		fenceLen = minLen
	}
	return strings.Repeat("`", fenceLen)
}

// fencedBlock writes a fenced code block around multi-line user-controlled
// free text (Description, Logs). The fence length exceeds any backtick run in
// `content` (CommonMark floor 3 for blocks), so the content renders verbatim
// and cannot break out to inject markdown links, raw HTML (`<a>`/`<img>`), or
// autolinked bare URLs into the surrounding issue body. The fence is written
// on its own lines so a content payload cannot fuse with the opening/closing
// delimiters.
func fencedBlock(b *strings.Builder, content string) {
	fence := fenceOf(content, 3)
	b.WriteString(fence)
	b.WriteString("\n")
	b.WriteString(content)
	b.WriteString("\n")
	b.WriteString(fence)
	b.WriteString("\n")
}

// inlineCode wraps a SHORT, single-value user-controlled metadata string
// (Category, GPU vendor/renderer, App Version, Platform, Machine ID prefix,
// Connection phase) in an inline code span. Inside a code span GitHub renders
// the bytes verbatim: a masked link `[t](u)`, raw `<a>`/`<img>` HTML, an
// `@mention`, an `#issue` ref, and a bare `https://` URL are all inert — none
// parse as active markdown/HTML/autolink. The span delimiter length exceeds
// any backtick run in `s` so the value cannot terminate its own span early.
// A leading/trailing space is added when the value itself starts/ends with a
// backtick (CommonMark inline-code padding rule) so the fence isn't absorbed.
//
// This SUPERSEDES neutralizeAutolinks for body metadata fields — fencing is
// strictly stronger (it inerts masked links / raw HTML / bare URLs that ZWSP
// insertion does not). neutralizeAutolinks is retained only for the issue
// TITLE, which is title-context (GitHub renders no markdown links/HTML there,
// but `#123` and `@name` autolinks still fire). (#158 review, PR #1547 —
// Fixes 2/4/5.)
func inlineCode(s string) string {
	// Collapse CR/LF to spaces first: an inline span lives on one logical line,
	// and a blank line (\n\n) would otherwise terminate the span's paragraph
	// and let the trailing delimiter render literally — re-exposing the content
	// to the parser. Short metadata values are single-line by nature; this also
	// hardens against newline-injection from a hostile client.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	fence := fenceOf(s, 1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	return fence + pad + s + pad + fence
}

// buildIssue assembles the GitHub issue title/body/labels from the
// (already-validated, already-sanitized) request.
//
// The body is rendered with explicit section headers so triagers reading
// the GitHub issue have a consistent layout regardless of which mode the
// user picked. The "Reported by" / "Requested by" line carries the user's
// internal ID for follow-up correlation — it never reaches the public
// issue body's user-visible content (GitHub issues are public-ish; the body
// IS visible to anyone with repo read access, hence the dedicated feedback
// repo per the architectural decision recorded in the PR body).
//
// User-supplied free text reaches a PUBLIC repo, so every user field is
// rendered INERT — no masked link `[t](u)`, raw HTML `<a>`/`<img>`, or bare
// URL is ever interpreted:
//   - The multi-line Description (and Logs, in writeDiagnostics) is emitted in
//     a dynamic-length fenced code block via fencedBlock.
//   - Short metadata values (App Version, Platform, Category, plus the
//     diagnostics fields) are wrapped in inline code spans via inlineCode.
//
// The issue TITLE is title-context — GitHub renders no markdown links/HTML in
// titles — so it keeps neutralizeAutolinks, which still defuses the `#123`
// issue-ref and `@name` autolinks that DO fire in titles.
//
// If the assembled body would exceed `maxBodyBytes`, truncateBody does a
// rune-boundary-aware head-keep (header + the start of the Description block),
// dropping the remainder. The truncation is marked inline so triagers know it
// happened.
func buildIssue(req *submitRequest, userID string) (title, body string, labels []string) {
	var b strings.Builder

	switch req.Type {
	case reportTypeBug:
		title = "[Bug Report] " + neutralizeAutolinks(req.Title)
		labels = []string{"type: bug"}
		fmt.Fprintf(&b, "**Reported by:** user `%s` (internal ID)\n", userID)
		if req.Diagnostics != nil {
			fmt.Fprintf(&b, "**App Version:** %s\n", inlineCode(req.Diagnostics.AppVersion))
			fmt.Fprintf(&b, "**Platform:** %s\n", inlineCode(req.Diagnostics.Platform))
		}
		b.WriteString("\n## Description\n\n")
		fencedBlock(&b, req.Description)
		if req.Diagnostics != nil {
			writeDiagnostics(&b, req.Diagnostics)
		}

	case reportTypeFeature:
		title = "[Feature Request] " + neutralizeAutolinks(req.Title)
		labels = []string{"type: feature"}
		fmt.Fprintf(&b, "**Requested by:** user `%s` (internal ID)\n", userID)
		if req.Category != "" {
			fmt.Fprintf(&b, "**Category:** %s\n", inlineCode(req.Category))
		}
		b.WriteString("\n## Description\n\n")
		fencedBlock(&b, req.Description)
	}

	body = b.String()
	if len(body) > maxBodyBytes {
		body = truncateBody(body)
	}
	return title, body, labels
}

// truncateBody applies a blunt HEAD-keep truncation when the assembled body
// would exceed GitHub's issue-body cap: it keeps the first maxBodyBytes-N
// bytes (the header + the start of whatever section follows — Description and,
// for bug reports, the Diagnostics/Logs that trail it) and appends a marker
// noting that the body was truncated. Returns a body of at most maxBodyBytes
// bytes. This is a head-keep, NOT a within-logs tail-keep — the whole tail
// (including the closing log fence) is simply dropped.
//
// This is a belt-and-suspenders guard against the case where Description +
// metadata + a max-size Logs section overruns maxBodyBytes; in practice the
// per-field caps keep the body well under the limit. The marker is markdown
// that renders cleanly.
//
// The keep point is walked back to a UTF-8 rune boundary before slicing, so a
// multi-byte rune at the cut (e.g. the 3-byte ZWSP that neutralizeAutolinks
// inserts, or any non-ASCII user input) is never split into an invalid byte
// sequence that GitHub would render as U+FFFD. (#158 review, PR #1547 — Fix 8.)
func truncateBody(body string) string {
	const marker = "\n\n_... (body truncated to fit GitHub issue-body limit) ..._\n"
	if len(body) <= maxBodyBytes {
		return body
	}
	keep := maxBodyBytes - len(marker)
	if keep < 0 {
		keep = 0
	}
	// Walk `keep` back until it lands on a rune boundary so we never slice
	// through the middle of a multi-byte UTF-8 sequence.
	for keep > 0 && !utf8.RuneStart(body[keep]) {
		keep--
	}
	return body[:keep] + marker
}

// writeDiagnostics emits the optional "Diagnostics" section in the bug-report
// body. Only fields the client provided are rendered; missing optional fields
// (GPU, display) are silently omitted rather than rendered as "unknown".
//
// Every user-supplied free-text metadata value is wrapped in an inline code
// span via inlineCode so a hostile client cannot smuggle a masked link
// `[t](u)`, raw HTML `<a>`/`<img>`, an `@org` mention, a `#123` ref, or a bare
// URL through (e.g.) the GPU vendor string — inside a code span none of those
// parse. The span fence length exceeds any backtick run in the value, so the
// value cannot terminate its own span early.
//
// The logs block uses fencedBlock — a fence whose length exceeds any backtick
// run in the content — so the user-supplied logs cannot escape the fenced code
// block to inject markdown/HTML into the surrounding body.
func writeDiagnostics(b *strings.Builder, d *diagnostics) {
	b.WriteString("\n\n## Diagnostics\n\n")
	fmt.Fprintf(b, "- **Machine ID prefix:** %s\n", inlineCode(d.MachineIDPrefix))
	fmt.Fprintf(b, "- **Connection phase:** %s\n", inlineCode(d.ConnectionPhase))
	if d.GPU != nil {
		fmt.Fprintf(b, "- **GPU:** %s / %s\n",
			inlineCode(d.GPU.Vendor),
			inlineCode(d.GPU.Renderer))
	}
	if d.Display != nil {
		// Display dimensions are integer/float types — no markdown-injection
		// surface here, so they don't go through inlineCode.
		fmt.Fprintf(b, "- **Display:** %dx%d @ %.2fx", d.Display.Width, d.Display.Height, d.Display.ScaleFactor)
		if d.Display.RefreshRate > 0 {
			fmt.Fprintf(b, " (%.0fHz)", d.Display.RefreshRate)
		}
		b.WriteString("\n")
	}
	if d.Logs != "" {
		b.WriteString("\n### Recent logs (sanitized)\n\n")
		fencedBlock(b, d.Logs)
	}
}
