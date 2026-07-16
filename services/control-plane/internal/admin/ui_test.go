package admin_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
)

const adminPortalCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func TestUIIndexServesPortalShell(t *testing.T) {
	engine := adminUITestEngine(admin.NewUI(adminUITestFS()))

	for _, target := range []string{"/admin/", "/admin/enroll"} {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, target, nil)
			engine.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
			assert.Equal(t, adminPortalCSP, rec.Header().Get("Content-Security-Policy"))
			assert.Equal(t, "<!doctype html><div id=\"root\"></div>", rec.Body.String())
		})
	}
}

func TestUIAssetServesHashedAssetWithImmutableCache(t *testing.T) {
	engine := adminUITestEngine(admin.NewUI(adminUITestFS()))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/assets/app.abc123.js", nil)
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "private, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "console.log('ok')", rec.Body.String())
}

func TestUIIndexMissingBuildReturnsFixed503(t *testing.T) {
	for name, dist := range map[string]fs.FS{
		"nil":           nil,
		"empty":         fstest.MapFS{},
		"missing-index": fstest.MapFS{"assets/app.js": {Data: []byte("x")}},
	} {
		t.Run(name, func(t *testing.T) {
			engine := adminUITestEngine(admin.NewUI(dist))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
			engine.ServeHTTP(rec, req)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, "admin portal unavailable\n", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "index.html")
		})
	}
}

func TestUIIndexRejectsSymlinkedIndex(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := []byte("external sentinel")
	require.NoError(t, os.WriteFile(filepath.Join(outside, "index.html"), sentinel, 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "index.html"), filepath.Join(root, "index.html")))

	engine := adminUITestEngine(admin.NewUI(os.DirFS(root)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "admin portal unavailable\n", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), string(sentinel))
}

func TestUIAssetRejectsSymlinkedAsset(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := []byte("external sentinel")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "assets"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.html"), []byte("ok"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.js"), sentinel, 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.js"), filepath.Join(root, "assets", "app.js")))

	engine := adminUITestEngine(admin.NewUI(os.DirFS(root)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil)
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "not found\n", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), string(sentinel))
}

func TestUIAssetRejectsSymlinkedAssetParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := []byte("external sentinel")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "assets"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.html"), []byte("ok"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "app.js"), sentinel, 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "assets", "linked")))

	engine := adminUITestEngine(admin.NewUI(os.DirFS(root)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/assets/linked/app.js", nil)
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "not found\n", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), string(sentinel))
}

func TestUIAssetRejectsBadPathsWithFixed404(t *testing.T) {
	ui := admin.NewUI(adminUITestFS())
	for _, param := range []string{
		"",
		"/missing.js",
		"/",
		"/nested",
		"/..%2fsecret.txt",
		"/..\\secret.txt",
		"/../secret.txt",
		"/nested/../../secret.txt",
		"/./app.abc123.js",
	} {
		t.Run(strings.ReplaceAll(param, "/", "_"), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Params = gin.Params{{Key: "filepath", Value: param}}

			ui.Asset(c)

			require.Equal(t, http.StatusNotFound, rec.Code)
			assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
			assert.Equal(t, "not found\n", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "secret")
			assert.NotContains(t, rec.Body.String(), "..")
		})
	}
}

func adminUITestEngine(ui *admin.UI) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/admin/", ui.Index)
	engine.GET("/admin/enroll", ui.Index)
	engine.GET("/admin/assets/*filepath", ui.Asset)
	return engine
}

func adminUITestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><div id=\"root\"></div>")},
		"assets/app.abc123.js": {Data: []byte("console.log('ok')")},
		"assets/style.css":     {Data: []byte("body{}")},
		"assets/nested":        {Mode: fs.ModeDir},
	}
}
