package managedredis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const providerFixtureSecret = "synthetic-provider-client-secret" // pragma: allowlist secret -- test fixture

type providerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f providerRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func writeProviderSecret(t *testing.T, mode os.FileMode) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "client-secret")
	require.NoError(t, os.WriteFile(filename, []byte(providerFixtureSecret), mode))
	return filename
}

func newTestInfisicalReader(t *testing.T, server *httptest.Server, secretFile string) *InfisicalReader {
	t.Helper()
	reader, err := NewInfisicalReader(server.URL, "synthetic-client-id", secretFile, server.Client())
	require.NoError(t, err)
	return reader
}

func TestInfisicalReaderUsesExactCamelCaseNamedGet(t *testing.T) {
	var loginBody map[string]string
	var loginCalls, secretCalls int
	var secretQuery url.Values
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			loginCalls++
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(body, &loginBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
		case "/api/v4/secrets/REDIS_CONTROL_PLANE_CREDENTIAL":
			secretCalls++
			secretQuery = r.URL.Query()
			authorization = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
		default:
			t.Errorf("unexpected provider endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
	scope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	value, name, err := reader.ReadExact(context.Background(), scope)

	require.NoError(t, err)
	require.Equal(t, "synthetic-credential-json", string(value))
	require.Equal(t, scope.Name, name)
	require.Equal(t, "Bearer synthetic-access-token", authorization)
	require.Equal(t, "project-test", secretQuery.Get("projectId"))
	require.Equal(t, "prod", secretQuery.Get("environment"))
	require.Equal(t, "/redis", secretQuery.Get("secretPath"))
	require.Equal(t, "shared", secretQuery.Get("type"))
	require.Equal(t, "false", secretQuery.Get("includeImports"))
	require.Equal(t, "false", secretQuery.Get("expandSecretReferences"))
	require.Equal(t, "true", secretQuery.Get("viewSecretValue"))
	require.Empty(t, secretQuery.Get("include_imports"), "Infisical v4 uses camelCase includeImports")
	require.Len(t, secretQuery, 7, "the named GET may carry only the fixed scope and value selectors")
	require.Equal(t, map[string]string{"clientId": "synthetic-client-id", "clientSecret": providerFixtureSecret}, loginBody)
	require.Equal(t, 1, loginCalls)
	require.Equal(t, 1, secretCalls)
}

func TestInfisicalReaderColdLoginAndNamedReadHaveSeparateBudgets(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
	}))
	defer server.Close()
	client := server.Client()
	transport := client.Transport
	var loginBudget, namedBudget time.Duration
	client.Transport = providerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		if strings.HasSuffix(req.URL.Path, "/login") {
			loginBudget = time.Until(deadline)
		} else {
			namedBudget = time.Until(deadline)
		}
		return transport.RoundTrip(req)
	})
	reader, err := NewInfisicalReader(server.URL, "synthetic-client-id", writeProviderSecret(t, 0o600), client)
	require.NoError(t, err)
	scope := SecretScope{Project: "project-test", Environment: "test", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	ctx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
	defer cancel()
	value, name, err := reader.ReadExact(ctx, scope)
	require.NoError(t, err)
	require.Equal(t, "synthetic-credential-json", string(value))
	require.Equal(t, scope.Name, name)
	require.Greater(t, loginBudget, 5*time.Second, "cold login must not inherit the named read's shorter cap")
	require.LessOrEqual(t, loginBudget, providerLoginTimeout)
	require.Greater(t, namedBudget, time.Duration(0))
	require.LessOrEqual(t, namedBudget, exactReadTimeout)
}

func TestInfisicalReaderRelogsAfterNamedGetRejectsCachedToken(t *testing.T) {
	loginCalls, secretCalls := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			loginCalls++
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-token-`+strconv.Itoa(loginCalls)+`","expiresIn":3600,"tokenType":"Bearer"}`)
		case "/api/v4/secrets/REDIS_CONTROL_PLANE_CREDENTIAL":
			secretCalls++
			if secretCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			require.Equal(t, "Bearer synthetic-token-2", r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
	scope := SecretScope{Project: "project-test", Environment: "test", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	_, _, err := reader.ReadExact(context.Background(), scope)
	require.ErrorContains(t, err, "denied")
	value, name, err := reader.ReadExact(context.Background(), scope)
	require.NoError(t, err)
	require.Equal(t, "synthetic-credential-json", string(value))
	require.Equal(t, scope.Name, name)
	require.Equal(t, 2, loginCalls)
	require.Equal(t, 2, secretCalls)
}

func TestInfisicalReaderReusesUnexpiredBearerAfterTemporaryRenewalFailure(t *testing.T) {
	loginCalls, secretCalls := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			loginCalls++
			if loginCalls > 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
			return
		}
		secretCalls++
		require.Equal(t, "Bearer synthetic-access-token", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
	}))
	defer server.Close()
	reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
	scope := SecretScope{Project: "project-test", Environment: "test", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	_, _, err := reader.ReadExact(context.Background(), scope)
	require.NoError(t, err)
	reader.renewAt = time.Now().Add(-time.Second)

	value, name, err := reader.ReadExact(context.Background(), scope)
	require.NoError(t, err)
	require.Equal(t, "synthetic-credential-json", string(value))
	require.Equal(t, scope.Name, name)
	require.Equal(t, 2, loginCalls)
	require.Equal(t, 2, secretCalls)

	reader.expiresAt = time.Now().Add(-time.Second)
	_, _, err = reader.ReadExact(context.Background(), scope)
	require.ErrorContains(t, err, "rate limited")
	require.Equal(t, 3, loginCalls)
	require.Equal(t, 2, secretCalls, "expired bearers must not authorize named reads")
}

func TestInfisicalReaderLeavesNamedReadBudgetAfterRenewalTimeout(t *testing.T) {
	secretCalls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
			return
		}
		secretCalls++
		require.Equal(t, "Bearer synthetic-access-token", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
	}))
	defer server.Close()

	client := server.Client()
	transport := client.Transport
	loginCalls := 0
	var renewalDeadline time.Time
	client.Transport = providerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/login") {
			loginCalls++
			if loginCalls > 1 {
				var ok bool
				renewalDeadline, ok = req.Context().Deadline()
				require.True(t, ok)
				return nil, context.DeadlineExceeded
			}
		}
		return transport.RoundTrip(req)
	})
	reader, err := NewInfisicalReader(server.URL, "synthetic-client-id", writeProviderSecret(t, 0o600), client)
	require.NoError(t, err)
	scope := SecretScope{Project: "project-test", Environment: "test", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	_, _, err = reader.ReadExact(context.Background(), scope)
	require.NoError(t, err)
	reader.renewAt = time.Now().Add(-time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), exactReadTimeout)
	defer cancel()
	outerDeadline, ok := ctx.Deadline()
	require.True(t, ok)
	value, name, err := reader.ReadExact(ctx, scope)

	require.NoError(t, err)
	require.Equal(t, "synthetic-credential-json", string(value))
	require.Equal(t, scope.Name, name)
	require.Equal(t, 2, loginCalls)
	require.Equal(t, 2, secretCalls)
	require.Greater(t, outerDeadline.Sub(renewalDeadline), 2*time.Second)
}

func TestInfisicalReaderDoesNotReuseBearerAfterRenewalDenial(t *testing.T) {
	loginCalls, secretCalls := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			loginCalls++
			if loginCalls > 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
			return
		}
		secretCalls++
		_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
	}))
	defer server.Close()
	reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
	scope := SecretScope{Project: "project-test", Environment: "test", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	_, _, err := reader.ReadExact(context.Background(), scope)
	require.NoError(t, err)
	reader.renewAt = time.Now().Add(-time.Second)

	_, _, err = reader.ReadExact(context.Background(), scope)
	require.ErrorContains(t, err, "denied")
	require.Empty(t, reader.token)
	require.True(t, reader.expiresAt.IsZero())
	require.Equal(t, 2, loginCalls)
	require.Equal(t, 1, secretCalls, "denied renewal must not use the cached bearer")
}

func TestInfisicalReaderRejectsSecretNameMismatchAndInvalidScope(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"secret":{"secretKey":"OTHER_SECRET","secretValue":"synthetic-value"}}`)
	}))
	defer server.Close()
	reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
	scope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}

	value, name, err := reader.ReadExact(context.Background(), scope)
	require.Error(t, err)
	require.Empty(t, value)
	require.Empty(t, name)

	value, name, err = reader.ReadExact(context.Background(), SecretScope{Name: "../secrets"})
	require.Error(t, err)
	require.Empty(t, value)
	require.Empty(t, name)
}

func TestInfisicalReaderRedactsLoginAndSecretFetchFailures(t *testing.T) {
	for _, tc := range []struct {
		name         string
		loginStatus  int
		secretStatus int
		wantCode     string
	}{
		{name: "denied", loginStatus: http.StatusForbidden, wantCode: "denied"},
		{name: "rate limited", loginStatus: http.StatusTooManyRequests, wantCode: "rate limited"},
		{name: "secret fetch denied", loginStatus: http.StatusOK, secretStatus: http.StatusUnauthorized, wantCode: "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/login") {
					if tc.loginStatus != 0 && tc.loginStatus != http.StatusOK {
						w.WriteHeader(tc.loginStatus)
						_, _ = io.WriteString(w, "raw-secret-body "+providerFixtureSecret)
						return
					}
					_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
					return
				}
				w.WriteHeader(tc.secretStatus)
				_, _ = io.WriteString(w, "raw-secret-body synthetic-access-token")
			}))
			defer server.Close()
			reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
			scope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}

			_, _, err := reader.ReadExact(context.Background(), scope)

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantCode)
			require.NotContains(t, err.Error(), providerFixtureSecret)
			require.NotContains(t, err.Error(), "synthetic-access-token")
			require.NotContains(t, err.Error(), "raw-secret-body")
		})
	}
}

func TestInfisicalReaderRejectsMalformedUniversalAuthResponses(t *testing.T) {
	for _, response := range []string{
		`{"accessToken":"","expiresIn":3600,"tokenType":"Bearer"}`,
		`{"accessToken":"synthetic-access-token","expiresIn":0,"tokenType":"Bearer"}`,
		`{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Basic"}`,
		`not-json synthetic-access-token`,
	} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()
			reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
			err := reader.login(context.Background())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "synthetic-access-token")
		})
	}
}

func TestNewInfisicalReaderRejectsInsecureRemoteAndCredentialsInBaseURL(t *testing.T) {
	secretFile := writeProviderSecret(t, 0o600)
	for _, baseURL := range []string{
		"://bad-url",
		"http://infisical.example.test",
		"https://user:password@infisical.example.test", // pragma: allowlist secret -- malformed test URL
		"https://infisical.example.test?token=synthetic-access-token",
		"https://infisical.example.test#fragment",
		"https://infisical.example.test:0",
		"https://infisical.example.test:65536",
		"https://infisical.example.test:99999",
		"ftp://infisical.example.test",
	} {
		_, err := NewInfisicalReader(baseURL, "synthetic-client-id", secretFile, nil)
		require.Error(t, err)
	}
	_, err := NewInfisicalReader("https://infisical.example.test", "", secretFile, nil)
	require.Error(t, err)
	_, err = NewInfisicalReader("https://infisical.example.test", "synthetic-client-id", "", nil)
	require.Error(t, err)
}

func TestInfisicalReaderAllowsLoopbackHTTPWithDefaultClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
	}))
	defer server.Close()
	reader, err := NewInfisicalReader(server.URL, "synthetic-client-id", writeProviderSecret(t, 0o600), nil)
	require.NoError(t, err)
	scope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}

	value, name, err := reader.ReadExact(context.Background(), scope)

	require.NoError(t, err)
	require.Equal(t, "synthetic-credential-json", string(value))
	require.Equal(t, scope.Name, name)
}

func TestIsLoopbackRecognizesLocalAddressesOnly(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{{"localhost", true}, {"LOCALHOST", true}, {"127.0.0.1", true}, {"::1", true}, {"example.test", false}} {
		t.Run(tc.host, func(t *testing.T) { require.Equal(t, tc.want, isLoopback(tc.host)) })
	}
}

func TestInfisicalReaderDoesNotFollowProviderRedirects(t *testing.T) {
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/unexpected-credential-sink", http.StatusFound)
	}))
	defer server.Close()
	reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))

	err := reader.login(context.Background())

	require.Error(t, err)
	require.Equal(t, 1, calls)
}

func TestInfisicalReaderRejectsGroupOrWorldReadableClientSecret(t *testing.T) {
	for _, mode := range []os.FileMode{0o604} {
		t.Run(mode.String(), func(t *testing.T) {
			var calls int
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			defer server.Close()
			reader := newTestInfisicalReader(t, server, writeProviderSecret(t, mode))

			err := reader.login(context.Background())
			require.Error(t, err)
			require.Zero(t, calls, "provider must not be called when the protected leaf has broad permissions")
		})
	}
}

func TestInfisicalReaderNamedGetDoesNotExposeSecretInURL(t *testing.T) {
	var requestURL *url.URL
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = io.WriteString(w, `{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)
			return
		}
		requestURL = r.URL
		_, _ = io.WriteString(w, `{"secret":{"secretKey":"REDIS_CONTROL_PLANE_CREDENTIAL","secretValue":"synthetic-credential-json"}}`)
	}))
	defer server.Close()
	reader := newTestInfisicalReader(t, server, writeProviderSecret(t, 0o600))
	scope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}

	_, _, err := reader.ReadExact(context.Background(), scope)

	require.NoError(t, err)
	require.NotContains(t, requestURL.String(), "synthetic-access-token")
	require.NotContains(t, requestURL.String(), providerFixtureSecret)
	require.NotContains(t, requestURL.String(), "redis://")
}
