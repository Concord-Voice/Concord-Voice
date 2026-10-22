package managedredis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientSecretFile is the protected Universal Auth client-secret leaf.
const ClientSecretFile = "/run/secrets/redis-infisical-client-secret"

var (
	errProvider            = errors.New("managed Redis provider unavailable")
	errProviderTemporary   = errors.New("managed Redis provider unavailable")
	errProviderDenied      = errors.New("managed Redis provider denied")
	errProviderRateLimited = errors.New("managed Redis provider rate limited")
)

// InfisicalReader makes only named Universal Auth GET requests. The client
// secret is read from a protected leaf and never appears in URL or Env.
type InfisicalReader struct {
	baseURL    *url.URL
	clientID   string
	secretFile string
	client     *http.Client
	mu         sync.Mutex
	token      string
	renewAt    time.Time
	expiresAt  time.Time
}

// NewInfisicalReader validates the provider origin and disables redirects.
func NewInfisicalReader(baseURL, clientID, secretFile string, client *http.Client) (*InfisicalReader, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopback(parsed.Hostname()))) ||
		clientID == "" || secretFile == "" {
		return nil, errProvider
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return nil, errProvider
		}
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &InfisicalReader{baseURL: parsed, clientID: clientID, secretFile: secretFile, client: &copyClient}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ReadExact fetches one named secret without imports or reference expansion.
func (r *InfisicalReader) ReadExact(ctx context.Context, scope SecretScope) (value []byte, name string, retErr error) {
	if !validScope(scope) {
		return nil, "", errProvider
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureToken(ctx); err != nil {
		return nil, "", err
	}
	u := *r.baseURL
	u.Path = path.Join(strings.TrimRight(r.baseURL.Path, "/"), "/api/v4/secrets/", url.PathEscape(scope.Name))
	q := u.Query()
	q.Set("projectId", scope.Project)
	q.Set("environment", scope.Environment)
	q.Set("secretPath", scope.Path)
	q.Set("type", "shared")
	q.Set("includeImports", "false")
	q.Set("expandSecretReferences", "false")
	q.Set("viewSecretValue", "true")
	u.RawQuery = q.Encode()
	secretCtx, secretCancel := context.WithTimeout(ctx, exactReadTimeout)
	defer secretCancel()
	req, err := http.NewRequestWithContext(secretCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", errProvider
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", errProvider
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			value, name, retErr = nil, "", errProvider
		}
	}()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			r.token = ""
			r.renewAt = time.Time{}
			r.expiresAt = time.Time{}
		}
		return nil, "", providerStatus(resp.StatusCode)
	}
	var result struct {
		Secret struct {
			Key   string `json:"secretKey"`
			Value string `json:"secretValue"`
		} `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil ||
		result.Secret.Key != scope.Name || result.Secret.Value == "" {
		return nil, "", errProvider
	}
	return []byte(result.Secret.Value), result.Secret.Key, nil
}

func (r *InfisicalReader) ensureToken(ctx context.Context) error {
	if r.token != "" && time.Now().Before(r.renewAt) {
		return nil
	}
	renewBudget := providerLoginTimeout
	if r.token != "" && time.Now().Before(r.expiresAt) {
		renewBudget = exactReadTimeout / 2 // Preserve the cached bearer and leave time for the named GET.
	}
	loginCtx, cancel := context.WithTimeout(ctx, renewBudget)
	err := r.login(loginCtx)
	cancel()
	if err == nil {
		return nil
	}
	if errors.Is(err, errProviderDenied) {
		r.token, r.renewAt, r.expiresAt = "", time.Time{}, time.Time{}
	}
	if (errors.Is(err, errProviderTemporary) || errors.Is(err, errProviderRateLimited)) &&
		r.token != "" && time.Now().Before(r.expiresAt) && ctx.Err() == nil {
		return nil
	}
	return err
}

func (r *InfisicalReader) login(ctx context.Context) (retErr error) {
	info, err := os.Lstat(r.secretFile)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16*1024 || info.Mode().Perm()&0o007 != 0 || info.Mode().Perm()&0o020 != 0 {
		return errProvider
	}
	secret, err := os.ReadFile(r.secretFile)
	if err != nil || len(secret) == 0 || len(secret) > 16*1024 {
		return errProvider
	}
	login := map[string]string{"clientId": r.clientID, "clientSecret": strings.TrimSpace(string(secret))}
	body, err := json.Marshal(login)
	if err != nil {
		return errProvider
	}
	u := *r.baseURL
	u.Path = path.Join(strings.TrimRight(r.baseURL.Path, "/"), "/api/v1/auth/universal-auth/login")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errProvider
	}
	req.Header.Set("Content-Type", "application/json")
	issuedAt := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		return errProviderTemporary
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			r.token = ""
			r.renewAt = time.Time{}
			r.expiresAt = time.Time{}
			retErr = errProvider
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return providerStatus(resp.StatusCode)
	}
	var value struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int64  `json:"expiresIn"`
		TokenType   string `json:"tokenType"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&value); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return errProviderTemporary
		}
		return errProvider
	}
	if value.AccessToken == "" || value.ExpiresIn <= 0 || value.TokenType != "Bearer" {
		return errProvider
	}
	ttl := time.Duration(value.ExpiresIn) * time.Second
	if ttl <= 0 {
		return errProvider
	}
	r.token = value.AccessToken
	r.renewAt = issuedAt.Add(ttl / 2)
	r.expiresAt = issuedAt.Add(ttl)
	return nil
}

func providerStatus(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errProviderDenied
	case http.StatusTooManyRequests:
		return errProviderRateLimited
	default:
		if status == http.StatusRequestTimeout || status >= http.StatusInternalServerError {
			return errProviderTemporary
		}
		return errProvider
	}
}
