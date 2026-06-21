// Package updates serves desktop client update assets from a local directory.
// Release assets are downloaded by deploy.sh during the deploy cycle —
// the handler simply serves files from disk with appropriate caching headers.
// Cloudflare caches binary responses via Cache-Control.
package updates

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	// Binary downloads get a 24h Cache-Control so Cloudflare caches them.
	binaryCacheMaxAge = 86400

	// Metadata gets a 5m Cache-Control — balances freshness with caching.
	metadataCacheMaxAge = 300
)

// Handler serves update assets from a local directory.
type Handler struct {
	cfg *config.Config
	log *logger.Logger
}

// NewHandler creates a new update handler.
func NewHandler(cfg *config.Config, log *logger.Logger) *Handler {
	return &Handler{cfg: cfg, log: log}
}

// ServeUpdateAsset handles GET /api/v1/updates/*filename
// Serves release assets (yml metadata + binaries) from the local releases directory.
func (h *Handler) ServeUpdateAsset(c *gin.Context) {
	releasesDir := h.cfg.ReleasesDir
	if releasesDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Update proxy not configured"})
		return
	}

	rawParam := c.Param("filename")
	if rawParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing filename"})
		return
	}
	// Gin includes leading slash in wildcard param
	rawParam = strings.TrimPrefix(rawParam, "/")

	// Resolve the releases directory to an absolute path
	absReleasesDir, err := filepath.Abs(releasesDir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Configuration error"})
		return
	}

	// Sanitize: filepath.Base strips all directory components, preventing path traversal.
	// This breaks the taint chain — the resulting name is guaranteed to contain no separators.
	safeName := filepath.Base(filepath.Clean(rawParam))
	if safeName == "." || safeName == ".." || safeName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid filename"})
		return
	}

	// Reject filenames with path separators (defense in depth)
	if strings.ContainsAny(safeName, `/\`) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid filename"})
		return
	}

	// Build the safe path from the trusted directory and the sanitized base name
	safePath := filepath.Join(absReleasesDir, safeName)

	// Check file exists — Lstat to reject symlinks
	info, err := os.Lstat(safePath)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}

	// Set caching headers
	if isMetadataFile(safeName) {
		c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", metadataCacheMaxAge))
	} else {
		c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", binaryCacheMaxAge))
	}
	c.Header("Content-Type", contentTypeForFile(safeName))

	c.File(safePath)
}

// isMetadataFile returns true for electron-updater yml files.
func isMetadataFile(filename string) bool {
	return filename == "latest-mac.yml" ||
		filename == "latest.yml" ||
		filename == "latest-linux.yml" ||
		filename == "latest-linux-arm64.yml"
}

// contentTypeForFile returns the Content-Type for a release asset.
func contentTypeForFile(filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	switch ext {
	case ".yml", ".yaml":
		return "text/yaml; charset=utf-8"
	case ".zip":
		return "application/zip"
	case ".exe":
		return "application/vnd.microsoft.portable-executable"
	case ".deb":
		return "application/vnd.debian.binary-package"
	case ".rpm":
		return "application/x-rpm"
	case ".appimage":
		return "application/x-executable"
	case ".nupkg":
		return "application/octet-stream"
	default:
		return "application/octet-stream"
	}
}
