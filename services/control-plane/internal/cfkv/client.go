// Package cfkv is a minimal Cloudflare Workers KV write client used by the
// SSO Initiate path to publish state→loopback-port mappings for the
// apple-sso-bridge Worker (#973). Stdlib-only by policy (no Cloudflare SDK).
// The target host is a constant — not user-derived — so the outbound-request
// SSRF egress guard ([internal]rules/backend.md) is not triggered.
package cfkv

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.cloudflare.com"

// putTimeout bounds how long /initiate can be held hostage by a slow
// Cloudflare API; the human-visible budget at initiate is small.
const putTimeout = 3 * time.Second

// Client writes values to one Workers KV namespace. Safe for concurrent use.
type Client struct {
	baseURL     string
	accountID   string
	namespaceID string
	token       string
	http        *http.Client
}

// New constructs a production client against api.cloudflare.com.
func New(accountID, namespaceID, token string) *Client {
	return NewWithBaseURL(accountID, namespaceID, token, defaultBaseURL)
}

// NewWithBaseURL is the test seam (httptest servers).
func NewWithBaseURL(accountID, namespaceID, token, baseURL string) *Client {
	return &Client{
		baseURL:     baseURL,
		accountID:   accountID,
		namespaceID: namespaceID,
		token:       token,
		http:        &http.Client{Timeout: putTimeout},
	}
}

// Put writes key=value with the given TTL (seconds). The error string never
// contains the token or the response body (the body could echo the key).
func (c *Client) Put(ctx context.Context, key, value string, ttlSeconds int) error {
	endpoint := fmt.Sprintf(
		"%s/client/v4/accounts/%s/storage/kv/namespaces/%s/values/%s?expiration_ttl=%d",
		c.baseURL,
		url.PathEscape(c.accountID),
		url.PathEscape(c.namespaceID),
		url.PathEscape(key),
		ttlSeconds,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, strings.NewReader(value))
	if err != nil {
		return fmt.Errorf("cfkv: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.http.Do(req) // #nosec G704 -- False positive: host is c.baseURL (trusted Cloudflare API base from config); path segments are url.PathEscape'd. No user-controlled host.
	if err != nil {
		return fmt.Errorf("cfkv: put: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("cfkv: put status %d", resp.StatusCode)
	}
	return nil
}
