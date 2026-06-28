package updates

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestHandler(releasesDir string) *Handler {
	cfg := &config.Config{ReleasesDir: releasesDir}
	return NewHandler(cfg, logger.New("test"))
}

func setupRouter(h *Handler) *gin.Engine {
	r := gin.New()
	r.GET("/api/v1/updates/*filename", h.ServeUpdateAsset)
	return r
}

// createTestReleasesDir creates a temp directory with test release files.
func createTestReleasesDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "latest-mac.yml"), []byte("version: 0.1.4\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ConcordVoice-0.1.4-macos-arm64.zip"), []byte("fake-zip-data"), 0600))
	return dir
}

func TestIsMetadataFile(t *testing.T) {
	assert.True(t, isMetadataFile("latest-mac.yml"))
	assert.True(t, isMetadataFile("latest.yml"))
	assert.True(t, isMetadataFile("latest-linux.yml"))
	assert.True(t, isMetadataFile("latest-linux-arm64.yml"))
	assert.False(t, isMetadataFile("ConcordVoice-0.1.4-macos-arm64.zip"))
	assert.False(t, isMetadataFile("latest-mac.zip"))
	assert.False(t, isMetadataFile("ConcordVoice-0.1.59-macos-arm64.zip.blockmap"))
	assert.False(t, isMetadataFile("ConcordVoice-0.1.4-linux-x64.AppImage.sig"))
	assert.False(t, isMetadataFile(""))
}

func TestContentTypeForFile(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"latest-mac.yml", "text/yaml; charset=utf-8"},
		{"ConcordVoice-0.1.4-macos-arm64.zip", "application/zip"},
		{"ConcordVoice-0.1.4-windows-x64-Setup.exe", "application/vnd.microsoft.portable-executable"},
		{"concord-voice_0.1.4_linux-x64.deb", "application/vnd.debian.binary-package"},
		{"concord-voice-0.1.4-linux-x64.rpm", "application/x-rpm"},
		{"ConcordVoice-0.1.4-linux-x64.AppImage", "application/x-executable"},
		{"ConcordVoice-0.1.4-windows-x64-full.nupkg", "application/octet-stream"},
		{"ConcordVoice-0.1.59-macos-arm64.zip.blockmap", "application/octet-stream"},
		{"ConcordVoice-0.1.4-linux-x64.AppImage.sig", "application/octet-stream"},
		{"unknown-file.bin", "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			assert.Equal(t, tt.expected, contentTypeForFile(tt.filename))
		})
	}
}

func TestServeUpdateAssetNoReleasesDir(t *testing.T) {
	h := newTestHandler("")
	r := setupRouter(h)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/api/v1/updates/latest-mac.yml", nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "Update proxy not configured")
}

func TestServeUpdateAssetInvalidFilename(t *testing.T) {
	dir := createTestReleasesDir(t)
	h := newTestHandler(dir)
	r := setupRouter(h)

	tests := []struct {
		path       string
		expectCode int
	}{
		// filepath.Base reduces "../../../etc/passwd" to "passwd" which doesn't exist → 404
		{"/api/v1/updates/../../../etc/passwd", http.StatusNotFound},
		// Gin splits "foo/bar" into param "foo/bar", filepath.Base → "bar" which doesn't exist → 404
		{"/api/v1/updates/foo/bar", http.StatusNotFound},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		req, err := http.NewRequest("GET", tt.path, nil)
		require.NoError(t, err)
		r.ServeHTTP(w, req)
		// Key assertion: traversal paths never return 200 (no file content leaked)
		assert.Equal(t, tt.expectCode, w.Code, "path: %s", tt.path)
		assert.NotEqual(t, http.StatusOK, w.Code, "path should never succeed: %s", tt.path)
	}
}

func TestServeUpdateAssetNotFound(t *testing.T) {
	dir := createTestReleasesDir(t)
	h := newTestHandler(dir)
	r := setupRouter(h)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/api/v1/updates/nonexistent-file.yml", nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "Asset not found")
}

func TestServeUpdateAssetServesMetadata(t *testing.T) {
	dir := createTestReleasesDir(t)
	h := newTestHandler(dir)
	r := setupRouter(h)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/api/v1/updates/latest-mac.yml", nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "version: 0.1.4")
	assert.Equal(t, "text/yaml; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=300", w.Header().Get("Cache-Control"))
}

func TestServeUpdateAssetServesBinary(t *testing.T) {
	dir := createTestReleasesDir(t)
	h := newTestHandler(dir)
	r := setupRouter(h)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/api/v1/updates/ConcordVoice-0.1.4-macos-arm64.zip", nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "fake-zip-data", w.Body.String())
	assert.Equal(t, "application/zip", w.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=86400", w.Header().Get("Cache-Control"))
}

// TestServeUpdateAssetServesBlockmap locks the #1292 AC wire contract end-to-end
// (HTTP 200, application/octet-stream, 24h binary cache, body round-trip) through
// the real ServeUpdateAsset handler — mirroring TestServeUpdateAssetServesBinary
// for the double-extension .zip.blockmap name. The pure-helper assertions in
// TestContentTypeForFile/TestIsMetadataFile cover the branch logic; this covers
// the serve path (filepath.Base/Clean + os.Lstat) for the double-extension name.
func TestServeUpdateAssetServesBlockmap(t *testing.T) {
	dir := createTestReleasesDir(t)
	const blockmapName = "ConcordVoice-0.1.4-macos-arm64.zip.blockmap"
	require.NoError(t, os.WriteFile(filepath.Join(dir, blockmapName), []byte("fake-blockmap-bytes"), 0600))
	h := newTestHandler(dir)
	r := setupRouter(h)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/api/v1/updates/"+blockmapName, nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "fake-blockmap-bytes", w.Body.String())
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=86400", w.Header().Get("Cache-Control"))
}

// TestServeUpdateAssetSignatureSidecar locks the #653 (Linux update signing)
// wire contract end-to-end (HTTP 200, application/octet-stream, 24h binary cache,
// body round-trip) through the real ServeUpdateAsset handler for the detached
// .sig update-signature sidecar. The .sig name is NOT in isMetadataFile's
// allowlist and falls through contentTypeForFile's default case, so it must serve
// as a binary with the 24h cache — NOT the 5-minute metadata cache. This test
// prevents a future refactor from regressing that (e.g. an over-broad metadata
// match or a new Content-Type branch).
func TestServeUpdateAssetSignatureSidecar(t *testing.T) {
	dir := createTestReleasesDir(t)
	const sigName = "ConcordVoice-0.1.4-linux-x64.AppImage.sig"
	sigBytes := make([]byte, 64)
	for i := range sigBytes {
		sigBytes[i] = byte(i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, sigName), sigBytes, 0600))
	h := newTestHandler(dir)
	r := setupRouter(h)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/api/v1/updates/"+sigName, nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, sigBytes, w.Body.Bytes())
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	// Binary 24h cache, NOT the 5-minute metadata cache.
	assert.Equal(t, "public, max-age=86400", w.Header().Get("Cache-Control"))
}

func TestServeUpdateAssetDirectoryTraversal(t *testing.T) {
	dir := createTestReleasesDir(t)
	// Create a file outside the releases dir to verify it can't be accessed
	parentFile := filepath.Join(filepath.Dir(dir), "secret.txt")
	require.NoError(t, os.WriteFile(parentFile, []byte("secret"), 0600))
	t.Cleanup(func() { _ = os.Remove(parentFile) })

	h := newTestHandler(dir)
	r := setupRouter(h)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/api/v1/updates/..%2Fsecret.txt", nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)

	// Should be blocked (either 400 or 404, never 200)
	assert.NotEqual(t, http.StatusOK, w.Code)
}
