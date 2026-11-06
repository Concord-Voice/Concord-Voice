package api

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// The route and the capability flag share this one predicate (#1612), so a
// client is never told to ask for a URL this deployment cannot mint.
func TestDirectAttachmentDownloadEnabled(t *testing.T) {
	on := &config.Config{AttachmentDirectDownload: true}
	off := &config.Config{}
	h := &media.Handler{}

	assert.True(t, directAttachmentDownloadEnabled(on, h))
	assert.False(t, directAttachmentDownloadEnabled(off, h), "off unless configured")
	assert.False(t, directAttachmentDownloadEnabled(on, nil), "no media handler means no route")
}

// The route is registered once, inside the predicate's guard, so turning the
// flag off removes it rather than leaving it reachable.
func TestDirectAttachmentDownloadRouteIsGuarded(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)
	route := `mediaRoutes.GET("/attachments/:file_id/url",`
	require.Equal(t, 1, strings.Count(source, route))
	guard := strings.Index(source, "if directAttachmentDownloadEnabled(cfg, mediaHandler) {")
	at := strings.Index(source, route)
	require.GreaterOrEqual(t, guard, 0)
	require.Greater(t, at, guard)
	require.NotContains(t, source[guard:at], "}", "the route must sit directly inside the guard")
}
