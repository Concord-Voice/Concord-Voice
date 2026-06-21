package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// githubAPIBase is the canonical REST root. Overridable in tests via the
// Client.baseURL field; production code never sets it.
const githubAPIBase = "https://api.github.com"

// IssueRequest is the minimal POST body we send to the GitHub issues API.
// We deliberately do not include `assignees`, `milestone`, or `projects` —
// the feedback bot lacks permission for those and including them is also
// noisy on the triage flow.
type IssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// IssueResponse is the subset of the GitHub issue response that the client
// needs. The full response is much larger; we only round-trip the fields a
// triager needs to follow up.
type IssueResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// Client is a minimal GitHub issues API client scoped to a single repo. The
// caller constructs one at startup with the feedback PAT and the
// `owner/repo` slug, then reuses it for every submission.
//
// The PAT is held in memory only. It is never logged. The String() shape on
// Config redacts it (see config.go).
type Client struct {
	httpClient *http.Client
	token      string
	repoSlug   string // "owner/repo"
	baseURL    string // overridable for tests
}

// NewClient constructs a Client. `repoSlug` is the `owner/repo` shape (e.g.
// "Concord-Voice/Concord-Voice-Feedback"). `token` is a fine-scoped PAT with
// `issues:write` on the target repo.
func NewClient(token, repoSlug string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		token:      token,
		repoSlug:   repoSlug,
		baseURL:    githubAPIBase,
	}
}

// CreateIssue POSTs an issue to the feedback repo and returns the response.
//
// Errors fall into two categories:
//  1. Transport errors (network unreachable, DNS fail, TLS error) — returned
//     as-is wrapped with `github: transport: %w`. The caller's handler
//     surfaces HTTP 502 to the client and logs the underlying.
//  2. GitHub non-2xx — returned as `github: API status %d: %s` with the
//     response body (truncated to 200 bytes). The caller surfaces HTTP 502;
//     the client never sees the GitHub error body directly (it could leak
//     repo-internal info on a misconfig).
func (c *Client) CreateIssue(ctx context.Context, req IssueRequest) (*IssueResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("github: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/issues", c.baseURL, c.repoSlug)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "concord-feedback-bot")

	resp, err := c.httpClient.Do(httpReq) // #nosec G704 -- False positive: host is c.baseURL (trusted GitHub API base from config) and repoSlug is operator-configured; user content is in the POST body, not the URL.
	if err != nil {
		return nil, fmt.Errorf("github: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 200 bytes of the error body for diagnostics. We don't
		// surface this to the end-user (handlers.go returns a generic 502);
		// it is logged for the operator.
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("github: API status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var issue IssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		return nil, fmt.Errorf("github: decode response: %w", err)
	}
	return &issue, nil
}
