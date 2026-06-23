// Package media provides HTTP handlers for media file upload, download, and proxy access.
// It implements a two-tier access model:
//
//   - Tier 1 (Authenticated): Profile images, server icons/banners, emojis, sounds.
//     Server-readable, processed on upload (resize, re-encode). Served via proxy
//     endpoints that validate JWT auth and context membership.
//
//   - Tier 2 (E2EE): Chat attachments in encrypted channels/DMs. Client-side encrypted
//     before upload and stored as opaque blobs; downloads are served via an authenticated
//     proxy endpoint that enforces membership checks. Server never sees plaintext.
package media

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	invitecodes "github.com/markdrogersjr/Concord/services/control-plane/internal/invites"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgAccessDenied       = "Access denied"
	errMsgFailedVerifyAccess = "Failed to verify access"
	errMsgFailedVerifyPerms  = "Failed to verify permissions"
	errMsgInternalServer     = "Internal server error"
	errMsgStorageUnavailable = "Object storage unavailable"
	storageErrNotFound       = "not found"
	purposeDMIcon            = "dm-icon"
	purposeServerIcon        = "server-icon"
	purposeServerBanner      = "server-banner"
	mimeOctetStream          = "application/octet-stream"
	headerContentType        = "Content-Type"
	headerCacheControl       = "Cache-Control"
	cacheControlPublic       = "public, max-age=3600, must-revalidate"
	cacheControlPublicShort  = "public, max-age=60, must-revalidate"
	cacheControlPrivate      = "private, max-age=3600, must-revalidate"
)

// ObjectStore defines the storage operations required by the media handler.
// This interface is satisfied by *storage.Client and can be mocked for testing.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error)
	DeleteObject(ctx context.Context, key string) error
}

// Tier 1 profile image dimension limits (output size after processing).
// Exported for use by the migrate-media CLI tool.
const (
	AvatarMaxDim = 512  // Avatars resize to fit 512x512
	BannerMaxW   = 1500 // Banners resize to fit 1500x500
	BannerMaxH   = 500
	IconMaxDim   = 512 // Server icons resize to fit 512x512
)

// Tier 1 profile image raw upload limits (before processing).
// These are purpose-specific, not type-specific — kept small because
// the server resizes everything down anyway.
// avatarMaxUpload and bannerMaxUpload are replaced by entitlement-resolved limits
// in UploadAvatar/UploadBanner (#1298); bannerMaxUpload is kept for server assets.
const (
	bannerMaxUpload = 10 * 1024 * 1024 // 10 MB — server-banner/dm-icon (server assets, not user-tier-gated)
	iconMaxUpload   = 5 * 1024 * 1024  // 5 MB
)

// Allowed MIME types for Tier 1 image uploads
var allowedImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// FileType classifies uploaded files for client-side rendering hints.
type FileType string

// File type constants for media classification.
const (
	FileTypePhoto    FileType = "photo"
	FileTypeAnimated FileType = "animated"
	FileTypeVideo    FileType = "video"
	FileTypeAudio    FileType = "audio"
	FileTypeFile     FileType = "file"
)

// MediaTier distinguishes access control models.
const (
	MediaTierAuthenticated = 1 // Tier 1: auth-gated, server-readable
	MediaTierE2EE          = 2 // Tier 2: E2EE, control-plane proxied access
)

// Handler provides HTTP handlers for media operations.
type Handler struct {
	db       *sql.DB
	store    ObjectStore
	log      *logger.Logger
	cfg      *config.Config
	resolver *rbac.Resolver
	tiers    entitlements.TierResolver
}

// NewHandler creates a new media handler.
func NewHandler(db *sql.DB, store ObjectStore, log *logger.Logger, cfg *config.Config, resolver *rbac.Resolver, tiers entitlements.TierResolver) *Handler {
	return &Handler{
		db:       db,
		store:    store,
		log:      log,
		cfg:      cfg,
		resolver: resolver,
		tiers:    tiers,
	}
}

func (h *Handler) requireObjectStore(c *gin.Context) (ObjectStore, bool) {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgStorageUnavailable})
		return nil, false
	}
	return h.store, true
}

// UploadAvatar handles avatar image uploads.
// POST /api/v1/media/upload/avatar
// Accepts multipart/form-data with a "file" field.
// Processes the image (resize to 512x512, re-encode) and stores in object storage.
// Returns the storage key for use in profile updates.
func (h *Handler) UploadAvatar(c *gin.Context) {
	userID := c.GetString("user_id")
	ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), userID))
	h.handleTier1Upload(c, userID, "avatar", ent.MaxAvatarBytes, AvatarMaxDim, AvatarMaxDim)
}

// UploadBanner handles banner/header image uploads.
// POST /api/v1/media/upload/banner
func (h *Handler) UploadBanner(c *gin.Context) {
	userID := c.GetString("user_id")
	ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), userID))
	h.handleTier1Upload(c, userID, "banner", ent.MaxBannerBytes, BannerMaxW, BannerMaxH)
}

// UploadServerIcon handles server icon uploads.
// POST /api/v1/media/upload/server-icon
func (h *Handler) UploadServerIcon(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, purposeServerIcon, iconMaxUpload, IconMaxDim, IconMaxDim)
}

// UploadServerBanner handles server banner uploads.
// POST /api/v1/media/upload/server-banner
func (h *Handler) UploadServerBanner(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, purposeServerBanner, bannerMaxUpload, BannerMaxW, BannerMaxH)
}

// UploadDMIcon handles group DM icon uploads.
// POST /api/v1/media/upload/dm-icon
func (h *Handler) UploadDMIcon(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, purposeDMIcon, iconMaxUpload, IconMaxDim, IconMaxDim)
}

// UploadAttachment handles E2EE file uploads for chat attachments (Tier 2).
// POST /api/v1/media/upload/attachment
// The file body is pre-encrypted ciphertext — the server stores it as-is.
func (h *Handler) UploadAttachment(c *gin.Context) {
	userID := c.GetString("user_id")
	ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), userID))

	// Cap request body size before multipart parsing to prevent memory/disk DoS
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, ent.MaxAttachmentBytes+4096) // +4KB for multipart headers

	file, header, err := parseAttachmentFile(c, ent.MaxAttachmentBytes)
	if err != nil {
		return // response already sent
	}
	defer func() { _ = file.Close() }()

	fileType, mimeType, keyVersion, ok := validateAttachmentRequest(c)
	if !ok {
		return
	}

	channelID, conversationID, ok := validateAttachmentContext(c, h, userID)
	if !ok {
		return
	}

	fileID := uuid.New().String()
	storageKey := fmt.Sprintf("attachments/%s", fileID)

	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}

	if err := store.PutObject(c.Request.Context(), storageKey, file, header.Size, mimeOctetStream); err != nil {
		h.log.Error("Failed to store attachment", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store file"})
		return
	}

	if err := createAttachmentRecord(h, c, attachmentParams{
		fileID: fileID, userID: userID, fileType: fileType, mimeType: mimeType,
		storageKey: storageKey, fileSize: header.Size, keyVersion: keyVersion,
		channelID: channelID, conversationID: conversationID,
	}); err != nil {
		if delErr := store.DeleteObject(c.Request.Context(), storageKey); delErr != nil {
			h.log.Error("Failed to delete orphaned attachment object", "error", delErr, "storage_key", storageKey)
		}
		h.log.Error("Failed to record attachment metadata", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record file metadata"})
		return
	}

	h.log.Info("Attachment uploaded", "file_id", fileID, "user_id", userID, "size", header.Size, "type", fileType)

	c.JSON(http.StatusCreated, gin.H{
		"file_id":     fileID,
		"storage_key": storageKey,
		"file_type":   fileType,
		"file_size":   header.Size,
	})
}

// DownloadAttachment proxies an E2EE attachment download through the control plane.
// GET /api/v1/media/attachments/:file_id
// Validates that the requesting user has access to the channel/conversation,
// then streams the encrypted blob from MinIO to the client.
//
// This is a proxy (not a presigned URL redirect) because MinIO is only reachable
// within the Docker network — clients cannot reach minio:9000 directly.
func (h *Handler) DownloadAttachment(c *gin.Context) {
	userID := c.GetString("user_id")
	fileID := c.Param("file_id")

	if _, err := uuid.Parse(fileID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid file ID"})
		return
	}

	// Fetch file metadata and verify access
	var storageKey, mimeType string
	var fileSize int64
	var channelID, conversationID *string

	query := `SELECT storage_key, mime_type, file_size, channel_id, conversation_id FROM media_files
	          WHERE id = $1 AND deleted_at IS NULL AND media_tier = 2`
	err := h.db.QueryRow(query, fileID).Scan(&storageKey, &mimeType, &fileSize, &channelID, &conversationID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch file metadata", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch file"})
		return
	}

	if !h.userCanDownloadAttachment(c, userID, channelID, conversationID) {
		return
	}

	// Stream the encrypted blob from MinIO to the client
	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}
	obj, contentType, err := store.GetObject(c.Request.Context(), storageKey)
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), storageErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "File not found in storage"})
			return
		}
		h.log.Error("Failed to fetch attachment from storage", "error", err, "file_id", fileID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download file"})
		return
	}
	defer func() { _ = obj.Close() }()

	// E2EE attachments are opaque ciphertext — use stored MIME type hint for the
	// client but serve as octet-stream since the content is encrypted
	_ = contentType // from storage (application/octet-stream)
	c.Header(headerContentType, mimeOctetStream)
	c.Header("Content-Length", fmt.Sprintf("%d", fileSize))
	c.Header("X-File-Mime-Type", mimeType) // original MIME type hint for client-side decryption
	c.Header(headerCacheControl, "private, no-store")
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, obj); err != nil {
		h.log.Warn("Failed to stream attachment to client", "error", err, "file_id", fileID)
	}
}

func (h *Handler) userCanDownloadAttachment(c *gin.Context, userID string, channelID, conversationID *string) bool {
	switch {
	case channelID != nil:
		return h.userHasChannelAccess(c, userID, *channelID)
	case conversationID != nil:
		return h.userHasDMAccess(c, userID, *conversationID)
	default:
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
}

// ProxyAvatar serves a user's avatar image through the control plane.
// GET /api/v1/media/avatars/:user_id
//
// PUBLIC: registered without auth middleware so plain <img> tags can render
// avatars without an Authorization header. Do not add JWT/membership
// assumptions to this handler — see router.go for the registration.
// Response is publicly cacheable (Cloudflare-friendly).
func (h *Handler) ProxyAvatar(c *gin.Context) {
	targetUserID := c.Param("user_id")
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("avatars/%s", targetUserID), true)
}

// ProxyBanner serves a user's banner/header image through the control plane.
// GET /api/v1/media/banners/:user_id
//
// PUBLIC: registered without auth middleware (same as ProxyAvatar). Do not
// add JWT/membership assumptions. Response is publicly cacheable.
func (h *Handler) ProxyBanner(c *gin.Context) {
	targetUserID := c.Param("user_id")
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("banners/%s", targetUserID), true)
}

// ProxyServerIcon serves a server's icon through the control plane.
// GET /api/v1/media/server-icons/:server_id
// Public: server icons are surfaced via invite links and member lists,
// and the unguessable UUID gates discovery. Membership check removed
// so plain <img> tags can render without an Authorization header.
func (h *Handler) ProxyServerIcon(c *gin.Context) {
	serverID := c.Param("server_id")
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("server-icons/%s", serverID), true)
}

// ProxyInviteServerIcon serves a server icon through an invite-code-scoped URL.
// Invalid, expired, revoked, maxed-out, missing, and iconless invites all return
// the same fallback image so the route does not disclose server UUIDs.
func (h *Handler) ProxyInviteServerIcon(c *gin.Context) {
	code := c.Param("code")
	if !invitecodes.IsValidCode(code) {
		serveInviteIconFallback(c)
		return
	}

	var (
		serverID  string
		expiresAt *time.Time
		isRevoked bool
		maxUses   *int
		useCount  int
		iconURL   *string
	)
	err := h.db.QueryRow(`
		SELECT si.server_id, si.expires_at, si.is_revoked, si.max_uses, si.use_count,
		       s.icon_url
		FROM server_invites si
		INNER JOIN servers s ON si.server_id = s.id
		WHERE si.code = $1
	`, code).Scan(&serverID, &expiresAt, &isRevoked, &maxUses, &useCount, &iconURL)
	if err == sql.ErrNoRows {
		serveInviteIconFallback(c)
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch public invite icon", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalServer})
		return
	}
	valid := !isRevoked &&
		(expiresAt == nil || expiresAt.After(time.Now().UTC())) &&
		(maxUses == nil || *maxUses == 0 || useCount < *maxUses)
	if !valid || iconURL == nil {
		serveInviteIconFallback(c)
		return
	}

	h.proxyInviteIcon(c, fmt.Sprintf("server-icons/%s", serverID))
}

// ProxyServerBanner serves a server's banner through the control plane.
// GET /api/v1/media/server-banners/:server_id
// Public for the same reason as ProxyServerIcon.
func (h *Handler) ProxyServerBanner(c *gin.Context) {
	serverID := c.Param("server_id")
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server ID"})
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("server-banners/%s", serverID), true)
}

// ProxyDMIcon serves a group DM's icon through the control plane.
// GET /api/v1/media/dm-icons/:conversationId
// Public: the unguessable UUID is the only identifier. Members already
// know it; non-members can't enumerate it. Membership check removed so
// plain <img> tags can render without an Authorization header.
func (h *Handler) ProxyDMIcon(c *gin.Context) {
	conversationID := c.Param("conversationId")
	if _, err := uuid.Parse(conversationID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation ID"})
		return
	}

	h.proxyTier1Media(c, fmt.Sprintf("dm-icons/%s", conversationID), true)
}

// DeleteMedia soft-deletes a media file and removes it from object storage.
// DELETE /api/v1/media/:file_id
// Only the uploader can delete their own Tier 2 (attachment) files.
// Tier 1 assets (avatars, banners, server icons) are managed via profile/server
// update endpoints and cannot be deleted directly.
func (h *Handler) DeleteMedia(c *gin.Context) {
	userID := c.GetString("user_id")
	fileID := c.Param("file_id")

	if _, err := uuid.Parse(fileID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid file ID"})
		return
	}

	// Verify ownership, tier 2 only, and get storage key
	var storageKey string
	query := `SELECT storage_key FROM media_files WHERE id = $1 AND uploader_id = $2 AND media_tier = 2 AND deleted_at IS NULL`
	err := h.db.QueryRow(query, fileID, userID).Scan(&storageKey)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch file for deletion", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete file"})
		return
	}

	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}

	// Soft-delete in DB
	_, err = h.db.Exec(`UPDATE media_files SET deleted_at = NOW() WHERE id = $1`, fileID)
	if err != nil {
		h.log.Error("Failed to soft-delete file", "error", err, "file_id", fileID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete file"})
		return
	}

	// Remove from object storage
	if err := store.DeleteObject(c.Request.Context(), storageKey); err != nil {
		h.log.Warn("Failed to delete object from storage (orphaned)", "error", err, "key", storageKey)
	}

	h.log.Info("Media file deleted", "file_id", fileID, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// --- Internal helpers ---

// handleTier1Upload processes a Tier 1 (authenticated) image upload.
// It validates the file, processes (resize + re-encode), stores in MinIO,
// and records metadata in the database.
func (h *Handler) handleTier1Upload(c *gin.Context, userID, purpose string, maxSize int64, maxW, maxH int) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSize+4096)

	file, header, err := parseMultipartFile(c, maxSize)
	if err != nil {
		return // response already sent
	}
	defer func() { _ = file.Close() }()

	serverID, conversationID, ok := validateTier1Context(c, h, userID, purpose)
	if !ok {
		return
	}

	if !validateImageType(c, file, header) {
		return
	}

	processed, err := processImage(file, purpose, maxW, maxH)
	if err != nil {
		h.log.Error("Failed to process image", "error", err, "user_id", userID, "purpose", purpose)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to process image. Ensure the file is a valid image."})
		return
	}

	storageKey := tier1StorageKey(purpose, userID, serverID, conversationID)

	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}

	reader := bytes.NewReader(processed.Data)
	if err := store.PutObject(c.Request.Context(), storageKey, reader, int64(len(processed.Data)), processed.ContentType); err != nil {
		h.log.Error("Failed to store processed image", "error", err, "user_id", userID, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store image"})
		return
	}

	fileID, err := insertTier1Record(h, c, store, userID, storageKey, processed)
	if err != nil {
		return // response already sent
	}

	if purpose == purposeDMIcon {
		if err := updateDMIconURL(h, c, conversationID); err != nil {
			return
		}
	}

	h.log.Info("Tier 1 image uploaded", "purpose", purpose, "user_id", userID,
		"size_original", header.Size, "size_processed", len(processed.Data),
		"dimensions", fmt.Sprintf("%dx%d", processed.Width, processed.Height))

	c.JSON(http.StatusCreated, gin.H{
		"file_id":     fileID,
		"storage_key": storageKey,
		"url":         fmt.Sprintf("/api/v1/media/%s", storageKey),
		"file_size":   len(processed.Data),
		"width":       processed.Width,
		"height":      processed.Height,
	})
}

// --- Extracted helpers for handleTier1Upload ---

func parseMultipartFile(c *gin.Context, maxSize int64) (multipart.File, *multipart.FileHeader, error) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    fmt.Sprintf("File exceeds maximum size of %d bytes", maxSize),
				"max_size": maxSize,
			})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing file in request"})
		}
		return nil, nil, err
	}
	if header.Size > maxSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("File exceeds maximum size of %d bytes", maxSize),
			"max_size": maxSize,
		})
		_ = file.Close()
		return nil, nil, fmt.Errorf("file too large")
	}
	return file, header, nil
}

func validateTier1Context(c *gin.Context, h *Handler, userID, purpose string) (serverID, conversationID string, ok bool) {
	if purpose == purposeServerIcon || purpose == purposeServerBanner {
		serverID = c.PostForm("server_id")
		if serverID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "server_id is required"})
			return "", "", false
		}
		if _, err := uuid.Parse(serverID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server_id"})
			return "", "", false
		}
		if !h.userCanManageServer(c, userID, serverID) {
			return "", "", false
		}
	}
	if purpose == purposeDMIcon {
		conversationID = c.PostForm("conversation_id")
		if conversationID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id is required"})
			return "", "", false
		}
		if _, err := uuid.Parse(conversationID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation_id"})
			return "", "", false
		}
		if !h.userIsDMAdmin(c, userID, conversationID) {
			return "", "", false
		}
	}
	return serverID, conversationID, true
}

func validateImageType(c *gin.Context, file multipart.File, header *multipart.FileHeader) bool {
	contentType := header.Header.Get(headerContentType)
	if contentType == "" || !allowedImageTypes[contentType] {
		buf := make([]byte, 512)
		n, readErr := file.Read(buf)
		if readErr != nil && readErr != io.EOF {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read file"})
			return false
		}
		if n == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Empty file"})
			return false
		}
		contentType = http.DetectContentType(buf[:n])
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process file"})
			return false
		}
	}
	if !allowedImageTypes[contentType] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "Invalid image type. Allowed: JPEG, PNG, GIF, WebP",
			"allowed_types": []string{"image/jpeg", "image/png", "image/gif", "image/webp"},
		})
		return false
	}
	return true
}

func processImage(file io.Reader, purpose string, maxW, maxH int) (*ProcessedImage, error) {
	if purpose == "banner" || purpose == purposeServerBanner {
		return ProcessImage(file, maxW, maxH)
	}
	return ProcessImagePNG(file, maxW, maxH)
}

func tier1StorageKey(purpose, userID, serverID, conversationID string) string {
	switch purpose {
	case "avatar":
		return fmt.Sprintf("avatars/%s", userID)
	case "banner":
		return fmt.Sprintf("banners/%s", userID)
	case purposeServerIcon:
		return fmt.Sprintf("server-icons/%s", serverID)
	case purposeServerBanner:
		return fmt.Sprintf("server-banners/%s", serverID)
	case purposeDMIcon:
		return fmt.Sprintf("dm-icons/%s", conversationID)
	}
	return fmt.Sprintf("media/%s/%s", purpose, userID)
}

func insertTier1Record(h *Handler, c *gin.Context, store ObjectStore, userID, storageKey string, processed *ProcessedImage) (string, error) {
	fileID := uuid.New().String()

	insertQuery := `
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (storage_key) WHERE deleted_at IS NULL
		DO UPDATE SET uploader_id = EXCLUDED.uploader_id, file_size = EXCLUDED.file_size, mime_type = EXCLUDED.mime_type, updated_at = NOW()
		RETURNING id
	`
	err := h.db.QueryRow(insertQuery, fileID, userID, string(FileTypePhoto), MediaTierAuthenticated,
		processed.ContentType, len(processed.Data), storageKey).Scan(&fileID)
	if err != nil {
		h.log.Error("Failed to record media metadata", "error", err, "user_id", userID)
		if delErr := store.DeleteObject(c.Request.Context(), storageKey); delErr != nil {
			h.log.Error("Failed to delete orphaned media object", "error", delErr, "storage_key", storageKey)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record media metadata"})
		return "", err
	}
	return fileID, nil
}

func updateDMIconURL(h *Handler, c *gin.Context, conversationID string) error {
	proxyURL := fmt.Sprintf("/api/v1/media/dm-icons/%s", conversationID)
	if _, dbErr := h.db.Exec(
		`UPDATE dm_conversations SET icon_url = $1, updated_at = NOW() WHERE id = $2`,
		proxyURL, conversationID,
	); dbErr != nil {
		h.log.Error("Failed to update DM icon URL", "error", dbErr, "conversation_id", conversationID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update group icon"})
		return dbErr
	}
	return nil
}

// --- Extracted helpers for UploadAttachment ---

func parseAttachmentFile(c *gin.Context, maxSize int64) (multipart.File, *multipart.FileHeader, error) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    fmt.Sprintf("File exceeds maximum upload size of %d bytes", maxSize),
				"max_size": maxSize,
			})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing file in request"})
		}
		return nil, nil, err
	}
	if header.Size > maxSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("File exceeds maximum upload size of %d bytes", maxSize),
			"max_size": maxSize,
		})
		_ = file.Close()
		return nil, nil, fmt.Errorf("file too large")
	}
	return file, header, nil
}

func validateAttachmentRequest(c *gin.Context) (fileType FileType, mimeType string, keyVersion int, ok bool) {
	fileType = FileType(c.PostForm("file_type"))
	if !isValidFileType(fileType) {
		fileType = FileTypeFile
	}

	mimeType = c.PostForm("mime_type")
	if mimeType == "" {
		mimeType = mimeOctetStream
	}

	keyVersion = 1
	keyVersionStr := c.PostForm("key_version")
	if keyVersionStr != "" {
		if v, err := fmt.Sscanf(keyVersionStr, "%d", &keyVersion); err != nil || v != 1 || keyVersion < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key_version must be a positive integer"})
			return "", "", 0, false
		}
	}

	return fileType, mimeType, keyVersion, true
}

func validateAttachmentContext(c *gin.Context, h *Handler, userID string) (channelID, conversationID string, ok bool) {
	channelID = c.PostForm("channel_id")
	conversationID = c.PostForm("conversation_id")

	if channelID == "" && conversationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Either channel_id or conversation_id is required for attachments"})
		return "", "", false
	}
	if channelID != "" && conversationID != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Exactly one of channel_id or conversation_id must be provided"})
		return "", "", false
	}

	if channelID != "" {
		if !validateChannelAttachment(c, h, userID, channelID) {
			return "", "", false
		}
	}
	if conversationID != "" {
		if !validateDMAttachment(c, h, userID, conversationID) {
			return "", "", false
		}
	}

	return channelID, conversationID, true
}

func validateChannelAttachment(c *gin.Context, h *Handler, userID, channelID string) bool {
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel_id"})
		return false
	}
	if !h.userHasChannelAccess(c, userID, channelID) {
		return false
	}
	return h.checkAttachPermission(c, userID, channelID)
}

func validateDMAttachment(c *gin.Context, h *Handler, userID, conversationID string) bool {
	if _, err := uuid.Parse(conversationID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversation_id"})
		return false
	}
	return h.userHasDMAccess(c, userID, conversationID)
}

type attachmentParams struct {
	fileID         string
	userID         string
	fileType       FileType
	mimeType       string
	storageKey     string
	fileSize       int64
	keyVersion     int
	channelID      string
	conversationID string
}

func createAttachmentRecord(h *Handler, _ *gin.Context, p attachmentParams) error {
	var chID, convID interface{}
	if p.channelID != "" {
		chID = p.channelID
	}
	if p.conversationID != "" {
		convID = p.conversationID
	}

	insertQuery := `
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key,
		                         key_version, channel_id, conversation_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
	`
	_, err := h.db.Exec(insertQuery, p.fileID, p.userID, string(p.fileType), MediaTierE2EE,
		p.mimeType, p.fileSize, p.storageKey, p.keyVersion, chID, convID)
	return err
}

// proxyTier1Media fetches a Tier 1 media object from MinIO and streams it to
// the client with appropriate cache headers. Used for avatars, banners,
// server icons, server banners, and DM icons.
// If public is true, the response is marked publicly cacheable (Cloudflare /
// shared caches OK) — only safe for routes registered without auth middleware.
func (h *Handler) proxyTier1Media(c *gin.Context, key string, public bool) {
	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}
	obj, contentType, err := store.GetObject(c.Request.Context(), key)
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), storageErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
			return
		}
		h.log.Error("Failed to fetch media from storage", "error", err, "key", key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalServer})
		return
	}
	defer func() { _ = obj.Close() }()

	// Cache for 1 hour, allow revalidation. Public avatars/banners use a
	// shared-cacheable directive so Cloudflare can serve them; membership-gated
	// assets stay private.
	if public {
		c.Header(headerCacheControl, cacheControlPublic)
	} else {
		c.Header(headerCacheControl, cacheControlPrivate)
	}
	c.Header(headerContentType, contentType)
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, obj); err != nil {
		h.log.Warn("Failed to stream media to client", "error", err, "key", key)
	}
}

func (h *Handler) proxyInviteIcon(c *gin.Context, key string) {
	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}
	obj, contentType, err := store.GetObject(c.Request.Context(), key)
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), storageErrNotFound) {
			serveInviteIconFallback(c)
			return
		}
		h.log.Error("Failed to fetch media from storage", "error", err, "key", key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalServer})
		return
	}
	defer func() { _ = obj.Close() }()

	c.Header(headerCacheControl, cacheControlPublic)
	c.Header(headerContentType, contentType)
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, obj); err != nil {
		h.log.Warn("Failed to stream media to client", "error", err, "key", key)
	}
}

func serveInviteIconFallback(c *gin.Context) {
	c.Header(headerCacheControl, cacheControlPublicShort)
	c.Data(http.StatusOK, "image/svg+xml; charset=utf-8", []byte(invitecodes.PublicInviteIconSVG))
}

// userHasChannelAccess checks if a user is a member of the server that owns a channel.
func (h *Handler) userHasChannelAccess(c *gin.Context, userID, channelID string) bool {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM channels ch
			JOIN server_members sm ON sm.server_id = ch.server_id
			WHERE ch.id = $1 AND sm.user_id = $2
		)
	`
	var hasAccess bool
	if err := h.db.QueryRow(query, channelID, userID).Scan(&hasAccess); err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}
	if !hasAccess {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	return true
}

// userHasDMAccess checks if a user is a participant in a DM conversation.
func (h *Handler) userHasDMAccess(c *gin.Context, userID, conversationID string) bool {
	query := `SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`
	var hasAccess bool
	if err := h.db.QueryRow(query, conversationID, userID).Scan(&hasAccess); err != nil {
		h.log.Error("Failed to check DM access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}
	if !hasAccess {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	return true
}

// userIsDMAdmin checks if a user is an admin participant in a group DM conversation.
func (h *Handler) userIsDMAdmin(c *gin.Context, userID, conversationID string) bool {
	var role string
	err := h.db.QueryRow(
		`SELECT dp.role FROM dm_participants dp
		 JOIN dm_conversations dc ON dc.id = dp.conversation_id
		 WHERE dp.conversation_id = $1 AND dp.user_id = $2 AND dc.is_group = TRUE`,
		conversationID, userID,
	).Scan(&role)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusForbidden, gin.H{"error": "Not a group DM participant"})
		} else {
			h.log.Error("Failed to check DM admin", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		}
		return false
	}
	if role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only group admins can upload icons"})
		return false
	}
	return true
}

// userIsServerMember checks if a user is a member of a server.
func (h *Handler) userIsServerMember(c *gin.Context, userID, serverID string) bool {
	query := `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`
	var isMember bool
	if err := h.db.QueryRow(query, serverID, userID).Scan(&isMember); err != nil {
		h.log.Error("Failed to check server membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return false
	}
	return true
}

// checkAttachPermission checks if a user has the ATTACH_FILES RBAC permission for a channel.
func (h *Handler) checkAttachPermission(c *gin.Context, userID, channelID string) bool {
	// Look up server_id for this channel
	var serverID string
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT server_id FROM channels WHERE id = $1`, channelID,
	).Scan(&serverID)
	if err != nil {
		h.log.Error("Failed to look up channel server", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if h.resolver == nil {
		// Fallback to membership-only if no RBAC resolver configured (e.g. tests)
		return h.userIsServerMember(c, userID, serverID)
	}
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermAttachFiles)
	if err != nil {
		h.log.Error("Failed to check attach permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have permission to attach files in this channel"})
		return false
	}
	return true
}

// userCanManageServer checks if a user has the manage_server RBAC permission.
// Used for server icon/banner uploads to match the same gate as UpdateServer.
func (h *Handler) userCanManageServer(c *gin.Context, userID, serverID string) bool {
	if h.resolver == nil {
		// Fallback to membership-only if no RBAC resolver configured (e.g. tests)
		return h.userIsServerMember(c, userID, serverID)
	}
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageServer)
	if err != nil {
		h.log.Error("Failed to check server permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
		return false
	}
	return true
}

func isValidFileType(ft FileType) bool {
	switch ft {
	case FileTypePhoto, FileTypeAnimated, FileTypeVideo, FileTypeAudio, FileTypeFile:
		return true
	}
	return false
}
