package admin

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	adminPortalCSP         = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	adminPortalUnavailable = "admin portal unavailable\n"
	adminPortalNotFound    = "not found\n"
	cacheControlHeader     = "Cache-Control"
)

var adminAssetContentTypes = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
	".otf":   "font/otf",
	".svg":   "image/svg+xml",
	".png":   "image/png",
}

// UI serves the built admin SPA from an injected filesystem.
type UI struct {
	dist fs.FS
}

// NewUI returns an admin SPA handler rooted at dist.
func NewUI(dist fs.FS) *UI {
	return &UI{dist: dist}
}

// Index serves the admin SPA shell for browser-owned routes.
func (ui *UI) Index(c *gin.Context) {
	ui.setCommonHeaders(c)
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header(cacheControlHeader, "private, no-store")
	c.Header("Content-Security-Policy", adminPortalCSP)
	c.Header("Referrer-Policy", "no-referrer")

	if ui == nil || ui.dist == nil {
		c.String(http.StatusServiceUnavailable, adminPortalUnavailable)
		return
	}
	body, ok := ui.readRegularFile("index.html")
	if !ok {
		c.String(http.StatusServiceUnavailable, adminPortalUnavailable)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", body)
}

// Asset serves a validated regular file below the admin SPA assets directory.
func (ui *UI) Asset(c *gin.Context) {
	ui.setCommonHeaders(c)
	c.Header(cacheControlHeader, "private, no-store")

	name, ok := adminAssetName(c.Param("filepath"))
	if !ok || ui == nil || ui.dist == nil {
		c.String(http.StatusNotFound, adminPortalNotFound)
		return
	}
	body, ok := ui.readRegularFile(name)
	if !ok {
		c.String(http.StatusNotFound, adminPortalNotFound)
		return
	}
	c.Header(cacheControlHeader, "private, max-age=31536000, immutable")
	c.Data(http.StatusOK, adminAssetContentType(name), body)
}

func (ui *UI) setCommonHeaders(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
}

func (ui *UI) readRegularFile(name string) ([]byte, bool) {
	if !ui.hasRegularPath(name) {
		return nil, false
	}
	body, err := fs.ReadFile(ui.dist, name)
	if err != nil {
		return nil, false
	}
	return body, true
}

func (ui *UI) hasRegularPath(name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	dir := "."
	parts := strings.Split(name, "/")
	for i, part := range parts {
		entry, ok := ui.readDirEntry(dir, part)
		if !ok || entry.Type()&fs.ModeSymlink != 0 {
			return false
		}
		if i == len(parts)-1 {
			return entry.Type().IsRegular()
		}
		if !entry.IsDir() {
			return false
		}
		dir = path.Join(dir, part)
	}
	return false
}

func (ui *UI) readDirEntry(dir, name string) (fs.DirEntry, bool) {
	entries, err := fs.ReadDir(ui.dist, dir)
	if err != nil {
		return nil, false
	}
	for _, entry := range entries {
		if entry.Name() == name {
			return entry, true
		}
	}
	return nil, false
}

func adminAssetName(param string) (string, bool) {
	if param == "" || param == "/" {
		return "", false
	}
	lower := strings.ToLower(param)
	if strings.Contains(param, `\`) || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return "", false
	}
	name := "assets/" + strings.TrimPrefix(param, "/")
	if !fs.ValidPath(name) || name == "assets" {
		return "", false
	}
	return name, true
}

func adminAssetContentType(name string) string {
	ext := path.Ext(name)
	if contentType, ok := adminAssetContentTypes[ext]; ok {
		return contentType
	}
	if contentType := mime.TypeByExtension(ext); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}
