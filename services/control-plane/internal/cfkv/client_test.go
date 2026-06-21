package cfkv

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPut_SendsCorrectRequest(t *testing.T) {
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWithBaseURL("acct-1", "ns-1", "tok-1", srv.URL)
	require.NoError(t, c.Put(context.Background(), "state with space/slash", "51620", 600))

	assert.Equal(t, http.MethodPut, got.Method)
	assert.Equal(t,
		"/client/v4/accounts/acct-1/storage/kv/namespaces/ns-1/values/state%20with%20space%2Fslash",
		got.URL.RawPath)
	assert.Equal(t, "600", got.URL.Query().Get("expiration_ttl"))
	assert.Equal(t, "Bearer tok-1", got.Header.Get("Authorization"))
	assert.Equal(t, "51620", gotBody)
}

func TestPut_NonSuccessStatusIsErrorWithoutSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewWithBaseURL("acct-1", "ns-1", "super-secret-token", srv.URL)
	err := c.Put(context.Background(), "k", "v", 600)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.NotContains(t, err.Error(), "super-secret-token")
}

func TestPut_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewWithBaseURL("a", "n", "t", srv.URL)
	err := c.Put(ctx, "k", "v", 600)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "context canceled"))
}
