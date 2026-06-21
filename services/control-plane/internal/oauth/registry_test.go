package oauth_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
)

type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string                           { return f.name }
func (f *fakeProvider) AuthorizationURL(_, _, _ string) string { return "https://fake/auth" }

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := oauth.NewRegistry()
	r.Register(&fakeProvider{name: "google"})

	p, err := r.Get("google")
	require.NoError(t, err)
	assert.Equal(t, "google", p.Name())
}

func TestRegistry_GetUnknown(t *testing.T) {
	r := oauth.NewRegistry()
	_, err := r.Get("github")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown provider "github"`)
}

func TestRegistry_RegisterReplaces(t *testing.T) {
	r := oauth.NewRegistry()
	r.Register(&fakeProvider{name: "google"})
	r.Register(&fakeProvider{name: "google"}) // re-register same name
	p, err := r.Get("google")
	require.NoError(t, err)
	assert.Equal(t, "google", p.Name())
}
