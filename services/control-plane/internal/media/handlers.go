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
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	invitecodes "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	errMsgAccessDenied              = "Access denied"
	logMsgChannelServerLookup       = "Failed to look up channel server"
	errMsgFailedVerifyAccess        = "Failed to verify access"
	errMsgFailedVerifyPerms         = "Failed to verify permissions"
	errMsgFailedStoreImage          = "Failed to store image"
	errMsgUserNotFound              = "User not found"
	errMsgFailedRecordMediaMetadata = "Failed to record media metadata"
	errMsgNotFound                  = "Not found"
	errMsgInternalServer            = "Internal server error"
	errMsgStorageUnavailable        = "Object storage unavailable"
	// errMsgAttachmentStorageAtCapacity is returned with 507 Insufficient
	// Storage when DiskWatermark.Check refuses a new attachment write
	// (#2759 unit A1). SaaS-only -- see disk_watermark.go.
	errMsgAttachmentStorageAtCapacity = "Attachment storage is temporarily full. Please try again shortly."
	purposeAvatar                     = "avatar"
	purposeBanner                     = "banner"
	purposeDMIcon                     = "dm-icon"
	purposeServerIcon                 = "server-icon"
	purposeServerBanner               = "server-banner"
	errMsgInvalidImage                = "Failed to process image. Ensure the file is a valid image."
	mimeOctetStream                   = "application/octet-stream"
	headerContentType                 = "Content-Type"
	headerCacheControl                = "Cache-Control"
	cacheControlPublic                = "public, max-age=3600, must-revalidate"
	cacheControlPublicShort           = "public, max-age=60, must-revalidate"
	cacheControlPrivate               = "private, max-age=3600, must-revalidate"
	cacheControlNoStore               = "no-store"
)

const profileTier1MediaAdmittedQuery = `
	SELECT EXISTS (
		SELECT 1 FROM media_files
		WHERE storage_key = $1 AND media_tier = 1 AND deleted_at IS NULL
		  AND ($2::uuid IS NULL OR uploader_id = $2)
	) AND NOT EXISTS (
		SELECT 1 FROM tier1_erasure_delete_obligations WHERE storage_key = $1
	)
`

const profileTier1SlotKeyQuery = `
	SELECT storage_key
	FROM media_files
	WHERE uploader_id = $1
	  AND media_tier = 1
	  AND deleted_at IS NULL
	  AND (
		profile_slot = $2
		OR ($2 = 'avatar' AND profile_slot IS NULL AND storage_key = 'avatars/' || uploader_id::text)
		OR ($2 = 'banner' AND profile_slot IS NULL AND storage_key = 'banners/' || uploader_id::text)
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM tier1_erasure_delete_obligations
		WHERE storage_key = media_files.storage_key
	  )`

// ObjectStore defines the storage operations required by the media handler.
// This interface is satisfied by *storage.Client and can be mocked for testing.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error)
	DeleteObject(ctx context.Context, key string) error

	// Multipart upload, for the chunked attachment format (#2157 PR 2).
	//
	// Fail-closed by construction: an incomplete multipart upload is NOT
	// readable via GetObject, so there is no window in which a partial
	// attachment can be downloaded. The final object appears atomically at
	// CompleteMultipartUpload, byte-identical to what the single-shot path
	// writes -- which is why DownloadAttachment, DeleteMedia and CleanupObject
	// need no changes at all.
	NewMultipartUpload(ctx context.Context, key, contentType string) (uploadID string, err error)
	PutObjectPart(ctx context.Context, key, uploadID string, partNumber int, r io.Reader, size int64) (storage.ObjectPartInfo, error)
	ListObjectParts(ctx context.Context, key, uploadID string) ([]storage.ObjectPartInfo, error)
	CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []storage.ObjectPartInfo) error
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
	ListIncompleteUploads(ctx context.Context, olderThan time.Time) ([]storage.IncompleteUpload, error)
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
// in UploadAvatar/UploadBanner (#1298). Server uploads parse at the absolute
// server-axis max, then apply the tier-specific cap after server_id validation.
const (
	serverImageMaxUpload  = 8 * 1024 * 1024 // 8 MiB — Mach cap, used only before multipart parsing
	iconMaxUpload         = 5 * 1024 * 1024 // 5 MiB — group-DM icons stay on the existing limit
	profileTier1TxTimeout = 10 * time.Second
	// Keep a compromised or repeatedly cancelled profile upload from accumulating
	// unbounded durable deletion evidence. A soft-deleted generation remains
	// unresolved until successful object deletion removes its obligation.
	maxUnresolvedProfileUploadEvidence = 20
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
	db          *sql.DB
	store       ObjectStore
	log         *logger.Logger
	cfg         *config.Config
	resolver    *rbac.Resolver
	tiers       entitlements.TierResolver
	serverTiers entitlements.ServerTierResolver
	opsCounter  interface{ Increment(opsmetrics.MetricKey) }
	// sessionRedis backs the chunked attachment upload sessions (#2157 PR 2).
	// Injected via SetSessionRedis rather than NewHandler so the constructor
	// signature stays put; every session route answers 503 when it is nil.
	sessionRedis *redis.Client
	// diskWatermark gates NEW attachment writes on shared-disk occupancy
	// (#2759 unit A1). Injected via SetDiskWatermark for the same reason as
	// sessionRedis; a nil value allows every write (Check is nil-safe), which
	// matches the other optional dependencies on this struct.
	diskWatermark *DiskWatermark
	// backends resolves a media_files.storage_backend value to the store that
	// holds that row's object (ADR-0038 / #2759). Injected via
	// SetStoreResolver; nil keeps legacy rows on the process-wide store and
	// fails closed for everything else — see storeForRow in backend_store.go.
	backends StoreResolver
	// writeRouter resolves the store for a NEW write by the CALLER'S PURPOSE
	// rather than by an existing row's column (ADR-0038 / #2759). Injected via
	// SetWriteRouter; nil keeps every write on the process-wide store, which is
	// exactly the pre-ADR-0038 behaviour — see write_routing.go.
	writeRouter WriteRouter
	// tier1UploadCommit is a package-test seam for definite versus ambiguous
	// profile-upload metadata commits. Production uses tx.Commit.
	tier1UploadCommit func(*sql.Tx) error
}

// SetOpsCounter enables aggregate successful-upload counting.
func (h *Handler) SetOpsCounter(counter interface{ Increment(opsmetrics.MetricKey) }) {
	h.opsCounter = counter
}

// SetDiskWatermark wires the shared-disk occupancy gate for attachment
// writes (#2759 unit A1). Mirrors SetOpsCounter/SetSessionRedis: an optional
// dependency the handler works without (Check on a nil *DiskWatermark always
// allows the write).
func (h *Handler) SetDiskWatermark(watermark *DiskWatermark) {
	h.diskWatermark = watermark
}

func (h *Handler) recordSuccessfulUpload() {
	if h.opsCounter != nil {
		h.opsCounter.Increment(opsmetrics.MetricMediaUploadsTotal)
	}
}

func (h *Handler) commitTier1Upload(tx *sql.Tx) error {
	if h.tier1UploadCommit != nil {
		return h.tier1UploadCommit(tx)
	}
	return tx.Commit()
}

// NewHandler creates a new media handler.
func NewHandler(db *sql.DB, store ObjectStore, log *logger.Logger, cfg *config.Config, resolver *rbac.Resolver, tiers entitlements.TierResolver, serverTiers ...entitlements.ServerTierResolver) *Handler {
	var st entitlements.ServerTierResolver
	if len(serverTiers) > 0 {
		st = serverTiers[0]
	}
	return &Handler{
		db:          db,
		store:       store,
		log:         log,
		cfg:         cfg,
		resolver:    resolver,
		tiers:       tiers,
		serverTiers: st,
	}
}

func (h *Handler) serverTier(ctx context.Context, serverID string) string {
	if h.serverTiers != nil {
		return h.serverTiers.GetServerTier(ctx, serverID)
	}
	return entitlements.ResolveServerTier(ctx, h.db, serverID)
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
	h.handleTier1Upload(c, userID, purposeAvatar, ent.MaxAvatarBytes, AvatarMaxDim, AvatarMaxDim)
}

// UploadBanner handles banner/header image uploads.
// POST /api/v1/media/upload/banner
func (h *Handler) UploadBanner(c *gin.Context) {
	userID := c.GetString("user_id")
	ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), userID))
	h.handleTier1Upload(c, userID, purposeBanner, ent.MaxBannerBytes, BannerMaxW, BannerMaxH)
}

// UploadServerIcon handles server icon uploads.
// POST /api/v1/media/upload/server-icon
func (h *Handler) UploadServerIcon(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, purposeServerIcon, serverImageMaxUpload, IconMaxDim, IconMaxDim)
}

// UploadServerBanner handles server banner uploads.
// POST /api/v1/media/upload/server-banner
func (h *Handler) UploadServerBanner(c *gin.Context) {
	userID := c.GetString("user_id")
	h.handleTier1Upload(c, userID, purposeServerBanner, serverImageMaxUpload, BannerMaxW, BannerMaxH)
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

	// Cap request body size before multipart parsing to prevent memory/disk DoS.
	// User-axis cap only: the server-wide uplift (entitlements.EffectiveAttachmentBytes)
	// cannot apply here because channel_id arrives IN the multipart body — #1556
	// wires the composed limit alongside the query-param wire change (spec
	// 2026-07-03-1522 §S3).
	// Plaintext entitlement + the v1 envelope (IV + tag) + 4 KiB for multipart
	// headers. The envelope term is not slack: without it this cap sits BELOW the
	// size a fully-allowed file actually puts on the wire.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body,
		ent.MaxAttachmentBytes+LegacyEnvelopeOverheadBytes+4096)

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

	// Same gate as the chunked init. Both paths write media_files.key_version
	// and the download reflects it back to a viewer's key selection, so a check
	// on only one of them just moves the exploit to the other.
	if !h.validateAttestedEpoch(c, channelID, conversationID, keyVersion) {
		return
	}

	fileID := uuid.New().String()
	storageKey := attachmentStorageKey(fileID)

	store, backendID, ok := h.requireAttachmentWriteStore(c)
	if !ok {
		return
	}

	// Shared-disk occupancy gate (#2759 unit A1), legacy-only. Tier-1 profile
	// media (avatars/, server-icons/, dm-icons/) never calls this — only the
	// two attachment-write paths do.
	if !h.checkAttachmentDiskWatermark(c, backendID) {
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
		storageBackend: backendID,
	}); err != nil {
		// #2201 review: the metadata write can fail precisely BECAUSE the request
		// context was canceled (client disconnect). Deleting the just-uploaded
		// object with that same canceled context would also fail and strand an
		// orphaned blob the reaper can't find (no media_files row). Detach the
		// cleanup delete so it runs regardless.
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
		if delErr := store.DeleteObject(cleanupCtx, storageKey); delErr != nil {
			h.log.Error("Failed to delete orphaned attachment object", "error", delErr, "storage_key", storageKey)
		}
		cancelCleanup()
		if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		h.log.Error("Failed to record attachment metadata", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record file metadata"})
		return
	}

	h.log.Info("Attachment uploaded", "file_id", fileID, "user_id", userID, "size", header.Size, "type", fileType)

	h.recordSuccessfulUpload()
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
// then streams the encrypted blob from the backend THAT ROW names to the client.
//
// This is a proxy (not a presigned URL redirect) because MinIO is only reachable
// within the Docker network — clients cannot reach minio:9000 directly.
//
// Placement is per object (ADR-0038 / #2759): the store is resolved from
// media_files.storage_backend, not from a single process-wide client. A NULL
// column is the legacy backend — the permanent state of every pre-cutover
// object — and an unrecognized value fails closed with 503 rather than being
// read out of some other bucket.
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
	// NULLable: rows predating the client-attested epoch (#2832) carry none, and
	// the client correctly falls back to the current key for those.
	var keyVersion *int
	// NULLable, permanently: NULL means the legacy backend (migration 000114
	// is explicit that it is never backfilled), so this is not a gap to be
	// closed — it is the value every pre-cutover object carries forever.
	var storageBackend *string

	query := `SELECT storage_key, mime_type, file_size, channel_id, conversation_id, key_version, storage_backend
	          FROM media_files
	          WHERE id = $1 AND deleted_at IS NULL AND media_tier = 2`
	err := h.db.QueryRow(query, fileID).
		Scan(&storageKey, &mimeType, &fileSize, &channelID, &conversationID, &keyVersion, &storageBackend)
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

	// Stream the encrypted blob to the client from the backend this row names.
	store, ok := h.requireObjectStoreForRow(c, storageBackend)
	if !ok {
		return
	}
	obj, contentType, err := store.GetObject(c.Request.Context(), storageKey)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
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
	c.Header(middleware.FileMimeTypeHeader, mimeType) // original MIME type hint for client-side decryption
	// THE EPOCH THE FILE WAS ENCRYPTED UNDER.
	//
	// The upload path has attested this since #2832 and the column has always
	// held it -- it simply never reached the client, so both decrypt call sites
	// used getChannelKey (the LATEST epoch). Every revocation rotates the CSK, so
	// each rotation permanently orphaned every attachment uploaded before it: the
	// key still existed and was fetchable, the client just never asked for it.
	// Worse, the failure surfaced as "may be damaged or altered" -- a rotation
	// reported to the user as tampering.
	//
	// A header rather than a change to the message payload: the value is already
	// on this row, so existing attachments become decryptable again with no
	// backfill and no change to AttachmentSummary.
	if keyVersion != nil {
		c.Header(middleware.FileKeyVersionHeader, strconv.Itoa(*keyVersion))
	}
	c.Header(headerCacheControl, "private, no-store")
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, obj); err != nil {
		h.log.Warn("Failed to stream attachment to client", "error", err, "file_id", fileID)
	}
}

func (h *Handler) userCanDownloadAttachment(c *gin.Context, userID string, channelID, conversationID *string) bool {
	switch {
	case channelID != nil:
		// CV-CAN-003: downloading a channel attachment requires both the
		// type-appropriate VIEW bit (userHasChannelAccess) and the ability to
		// READ HISTORY in the channel, matching messages.checkChannelAccess. The
		// short-circuit ensures only one error response is written on denial.
		return h.userHasChannelAccess(c, userID, *channelID) &&
			h.checkReadHistoryPermission(c, userID, *channelID)
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
// Responses are never cached so erasure cannot be served from a cache.
func (h *Handler) ProxyAvatar(c *gin.Context) {
	c.Header(headerCacheControl, cacheControlNoStore)
	targetUserID := c.Param("user_id")
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	h.proxyProfileTier1Media(c, targetUserID, ProfileSlotAvatar)
}

// ProxyBanner serves a user's banner/header image through the control plane.
// GET /api/v1/media/banners/:user_id
//
// PUBLIC: registered without auth middleware (same as ProxyAvatar). Do not
// add JWT/membership assumptions. Responses are never cached.
func (h *Handler) ProxyBanner(c *gin.Context) {
	c.Header(headerCacheControl, cacheControlNoStore)
	targetUserID := c.Param("user_id")
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	h.proxyProfileTier1Media(c, targetUserID, ProfileSlotBanner)
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

// ProxyFriendCodeAvatar serves a friend-code owner's avatar keyed by CODE.
//
// The public avatar route is /api/v1/media/avatars/:user_id, so returning a raw
// avatar_url from the friend-code preview would re-leak the owner UUID that the
// preview body deliberately omits. This route closes that hole: the code is the
// only identifier in the URL, and the bytes are proxied - never redirected,
// because a 302 would put the UUID in the Location header.
//
// Every invalid class, every no-avatar case, and every storage failure serves
// the shared silhouette fallback with identical BYTES *and* identical headers,
// so an anonymous caller cannot tell them apart at all.
//
// Header parity is now real rather than aspirational: the success arm routes
// through proxyFriendCodeAvatarObject (not the shared proxyInviteIcon) and both
// success and fallback emit no-store. An earlier revision emitted different
// cache lifetimes, leaving the directive as an oracle after the bytes had been
// equalized.
// GET /api/v1/friends/codes/:code/avatar
func (h *Handler) ProxyFriendCodeAvatar(c *gin.Context) {
	c.Header(headerCacheControl, cacheControlNoStore)
	// The edge rate-limit rule matches on the RAW wire path, but gin routes on
	// the percent-DECODED path, so /…/CODE%2Favatar reaches this handler while
	// matching no WAF rule — no managed challenge, no edge bucket.
	// URL.RawPath is set only when net/url's own re-encoding of Path differs
	// from the wire string. That is NARROWER than "something was percent-
	// encoded": a 256-byte sweep found 181 single-byte encodings (%20, %25,
	// %3F, %7B, ...) that round-trip through escape(encodePath) and leave
	// RawPath EMPTY — so the guard does not, and cannot, mean "nothing was
	// encoded".
	//
	// It is still sufficient here, for a different reason than that: every byte
	// Go re-escapes is a byte that can never appear in a valid code, so such a
	// request fails IsValidCode on the next line regardless. The code charset is
	// entirely RFC-3986 unreserved, and encoding ANY unreserved character does
	// produce a Path/RawPath divergence. The sweep confirmed 0 bypasses, and the
	// property survives widening the charset to -_.~ (#1557).
	// Reject in the SAME uniform shape as every other invalid class, so closing
	// the rate-limit bypass introduces no enumeration oracle (#945, VULN-001).
	if c.Request.URL.RawPath != "" {
		serveFriendCodeAvatarFallback(c)
		return
	}

	code := c.Param("code")
	if !invitecodes.IsValidCode(code) {
		serveFriendCodeAvatarFallback(c)
		return
	}

	var (
		ownerID   string
		expiresAt *time.Time
		isRevoked bool
		maxUses   *int
		useCount  int
		avatarURL *string
	)
	err := h.db.QueryRow(`
		SELECT fc.user_id, fc.expires_at, fc.is_revoked, fc.max_uses, fc.use_count,
		       u.avatar_url
		FROM friend_codes fc
		INNER JOIN users u ON fc.user_id = u.id
		WHERE fc.code = $1
	`, code).Scan(&ownerID, &expiresAt, &isRevoked, &maxUses, &useCount, &avatarURL)
	if errors.Is(err, sql.ErrNoRows) {
		serveFriendCodeAvatarFallback(c)
		return
	}
	if err != nil {
		// The code is bearer material: log the error only, never the code.
		h.log.Error("Failed to fetch public friend code avatar", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalServer})
		return
	}
	valid := !isRevoked &&
		(expiresAt == nil || expiresAt.After(time.Now().UTC())) &&
		(maxUses == nil || *maxUses == 0 || useCount < *maxUses)
	if !valid || avatarURL == nil {
		serveFriendCodeAvatarFallback(c)
		return
	}

	key, found, err := ProfileTier1StorageKey(c.Request.Context(), h.db, ownerID, ProfileSlotAvatar)
	if err != nil || !found {
		serveFriendCodeAvatarFallback(c)
		return
	}
	h.proxyFriendCodeAvatarObject(c, key)
}

// friendAvatarMaxBytes bounds the in-memory buffer below. It matches the largest
// MaxAvatarBytes any entitlement tier permits at upload (8 MiB), so it cannot
// reject an object the service itself accepted; stored avatars are re-encoded to
// 512x512 and in practice sit far below it.
const friendAvatarMaxBytes int64 = 8 * 1024 * 1024

// proxyFriendCodeAvatarObject is proxyInviteIcon's fail-uniform twin, and exists
// because the two callers need opposite behaviour on a storage fault.
//
// proxyInviteIcon answers a non-not-found storage error with 500 and a nil store
// with 503. That is right for a server icon, where the caller already knows the
// server exists. It is wrong here: this arm is reachable ONLY by a code that is
// well-formed, exists, is live, and whose owner has an avatar, so a distinct
// status turns the route into a clean binary classifier for exactly the question
// the shared silhouette exists to refuse — and the nil-store branch makes that
// permanent in any deployment with no object store, not merely incident-scoped
// (CWE-203, #945 Md6).
//
// So every failure serves the same silhouette the invalid classes get. The error
// is logged rather than surfaced; an <img> consumer reads bytes, not status, so
// nothing downstream needs to tell these apart.
func (h *Handler) proxyFriendCodeAvatarObject(c *gin.Context, key string) {
	if h.store == nil {
		serveFriendCodeAvatarFallback(c)
		return
	}
	admitted, err := h.profileTier1MediaAdmitted(c.Request.Context(), key)
	if err != nil || !admitted {
		serveFriendCodeAvatarFallback(c)
		return
	}
	obj, contentType, err := h.store.GetObject(c.Request.Context(), key)
	if err != nil {
		if !errors.Is(err, storage.ErrObjectNotFound) {
			h.log.Error("Failed to fetch friend-code avatar from storage")
		}
		serveFriendCodeAvatarFallback(c)
		return
	}
	defer func() { _ = obj.Close() }()

	// Buffer BEFORE committing anything, rather than streaming with io.Copy.
	// A reader that yields bytes and then fails mid-stream would otherwise leave
	// a truncated 200 on the wire with the headers already sent and no way to
	// retract them — and a truncated body is distinguishable from the silhouette,
	// which is this route's oracle re-opened one step later in the flow. The
	// pre-read failure above and this mid-read failure are the same defect at
	// two different moments, so they get the same uniform answer.
	//
	// Bounded, because the reader's length is not trusted: read one byte past the
	// ceiling so an over-long object is detected rather than silently truncated,
	// and treat that overflow as a failure too.
	buf, err := io.ReadAll(io.LimitReader(obj, friendAvatarMaxBytes+1))
	if err != nil || int64(len(buf)) > friendAvatarMaxBytes {
		h.log.Error("Failed to read friend-code avatar from storage")
		serveFriendCodeAvatarFallback(c)
		return
	}

	c.Header(headerCacheControl, cacheControlNoStore)
	c.Data(http.StatusOK, contentType, buf)
}

func serveFriendCodeAvatarFallback(c *gin.Context) {
	c.Header(headerCacheControl, cacheControlNoStore)
	c.Data(http.StatusOK, "image/svg+xml; charset=utf-8", []byte(invitecodes.PublicInviteIconSVG))
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

	// Verify ownership, tier 2 only, and get the storage key AND its placement.
	// The backend rides along on the row we are already reading (ADR-0038 /
	// #2759 unit B2) — deleting from the wrong bucket would SUCCEED and erase
	// nothing. NULLable permanently: NULL is the legacy backend.
	var storageKey string
	var storageBackend *string
	query := `SELECT storage_key, storage_backend FROM media_files WHERE id = $1 AND uploader_id = $2 AND media_tier = 2 AND deleted_at IS NULL`
	err := h.db.QueryRow(query, fileID, userID).Scan(&storageKey, &storageBackend)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch file for deletion", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete file"})
		return
	}

	// Resolve BEFORE the soft-delete and 503 if the row's backend is unknown:
	// nothing is recorded, so the client retries against a state that never
	// claimed the file was gone. Identical body to the unconfigured-store 503.
	store, ok := h.requireObjectStoreForRow(c, storageBackend)
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

	// Remove from object storage. A failure here is a WARN rather than a 500
	// because this row is tier 2 and now soft-deleted with blob_reaped_at still
	// NULL, which makes it a straggler-sweep candidate — the sweep is the
	// durable retry, and it resolves the same backend off the same row. That
	// argument holds ONLY for tier 2: the sweep is bounded to media_tier = 2,
	// which is why media.CleanupObject (tier 1, no retry behind it) refuses
	// instead of warning.
	if err := store.DeleteObject(c.Request.Context(), storageKey); err != nil {
		h.log.Warn("Failed to delete object from storage (orphaned, left for the straggler sweep)",
			"error", err, "key", storageKey, "storage_backend", describeBackend(storageBackend))
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
	if !enforceTier1UploadLimit(c, h, purpose, serverID, header.Size, maxSize) {
		return
	}

	contentType, ok := validateImageType(c, file, header)
	if !ok {
		return
	}

	processed, ok := h.processTier1Image(c, file, tier1ImageJob{
		userID:      userID,
		purpose:     purpose,
		serverID:    serverID,
		contentType: contentType,
		maxW:        maxW,
		maxH:        maxH,
	})
	if !ok {
		return // response already sent
	}

	store, ok := h.requireTier1WriteStore(c)
	if !ok {
		return
	}

	var fileID string
	if purpose == purposeAvatar || purpose == purposeBanner {
		storageKey := profileTier1StorageKey(purpose, userID)
		if !h.createProfileTier1UploadIntent(c, userID, profileSlotForPurpose(purpose), storageKey) {
			return
		}
		fileID, ok = h.storeProfileTier1Image(c, store, userID, purpose, storageKey, processed)
		if !ok {
			return
		}
		h.respondTier1Upload(c, purpose, userID, storageKey, fileID, header.Size, processed)
		return
	}
	storageKey := tier1StorageKey(purpose, userID, serverID, conversationID)
	reader := bytes.NewReader(processed.Data)
	if err := store.PutObject(c.Request.Context(), storageKey, reader, int64(len(processed.Data)), processed.ContentType); err != nil {
		h.log.Error("Failed to store processed image", "error", err, "user_id", userID, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return
	}

	fileID, err = insertTier1Record(c.Request.Context(), h, c, nil, userID, storageKey, processed)
	if err != nil {
		if delErr := store.DeleteObject(c.Request.Context(), storageKey); delErr != nil {
			h.log.Error("Failed to delete orphaned media object", "error", delErr, "storage_key", storageKey)
		}
		return // response already sent
	}
	if purpose == purposeDMIcon {
		if err := updateDMIconURL(h, c, conversationID); err != nil {
			return
		}
	}

	h.respondTier1Upload(c, purpose, userID, storageKey, fileID, header.Size, processed)
}

func (h *Handler) respondTier1Upload(c *gin.Context, purpose, userID, storageKey, fileID string, originalSize int64, processed *ProcessedImage) {
	h.log.Info("Tier 1 image uploaded", "purpose", purpose, "user_id", userID,
		"size_original", originalSize, "size_processed", len(processed.Data),
		"dimensions", fmt.Sprintf("%dx%d", processed.Width, processed.Height))

	h.recordSuccessfulUpload()
	url := fmt.Sprintf("/api/v1/media/%s", storageKey)
	if purpose == purposeAvatar || purpose == purposeBanner {
		url = profileTier1CanonicalURL(purpose, userID)
	}
	c.JSON(http.StatusCreated, gin.H{
		"file_id":     fileID,
		"storage_key": storageKey,
		"url":         url,
		"file_size":   len(processed.Data),
		"width":       processed.Width,
		"height":      processed.Height,
	})
}

func enforceTier1UploadLimit(c *gin.Context, h *Handler, purpose, serverID string, fileSize, defaultMaxSize int64) bool {
	maxSize := defaultMaxSize
	if purpose == purposeServerIcon || purpose == purposeServerBanner {
		ent := entitlements.ForServer(h.serverTier(c.Request.Context(), serverID))
		if purpose == purposeServerIcon {
			maxSize = ent.MaxServerIconBytes
		} else {
			maxSize = ent.MaxServerBannerBytes
		}
	}
	if fileSize > maxSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("File exceeds maximum size of %d bytes", maxSize),
			"max_size": maxSize,
		})
		return false
	}
	return true
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

// validateImageType checks the upload against the Tier 1 image allowlist and
// returns the resolved content type (declared, or sniffed when the declared
// type is absent/unrecognized).
func validateImageType(c *gin.Context, file multipart.File, header *multipart.FileHeader) (string, bool) {
	contentType := header.Header.Get(headerContentType)
	if contentType == "" || !allowedImageTypes[contentType] {
		buf := make([]byte, 512)
		n, readErr := file.Read(buf)
		if readErr != nil && readErr != io.EOF {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read file"})
			return "", false
		}
		if n == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Empty file"})
			return "", false
		}
		contentType = http.DetectContentType(buf[:n])
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process file"})
			return "", false
		}
	}
	if !allowedImageTypes[contentType] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "Invalid image type. Allowed: JPEG, PNG, GIF, WebP",
			"allowed_types": []string{"image/jpeg", "image/png", mimeGIF, "image/webp"},
		})
		return "", false
	}
	return contentType, true
}

func processImage(file io.Reader, purpose string, maxW, maxH int) (*ProcessedImage, error) {
	if purpose == purposeBanner || purpose == purposeServerBanner {
		return ProcessImage(file, maxW, maxH)
	}
	return ProcessImagePNG(file, maxW, maxH)
}

// tier1ImageJob carries the routing and entitlement inputs for one validated
// Tier 1 upload through the processing helpers below (folded from positional
// parameters so the pipeline signatures stay small).
type tier1ImageJob struct {
	userID      string
	purpose     string
	serverID    string
	contentType string
	maxW        int
	maxH        int
}

// processTier1Image routes a validated upload through the static flatten
// pipeline or, for GIF uploads, the animated-aware path (#1302). On failure it
// writes the HTTP error response itself and returns ok=false.
func (h *Handler) processTier1Image(c *gin.Context, file multipart.File, job tier1ImageJob) (*ProcessedImage, bool) {
	if job.contentType != mimeGIF {
		return h.processStaticTier1Image(c, file, job)
	}
	return h.processGIFTier1Image(c, file, job)
}

func (h *Handler) processStaticTier1Image(c *gin.Context, file io.Reader, job tier1ImageJob) (*ProcessedImage, bool) {
	processed, err := processImage(file, job.purpose, job.maxW, job.maxH)
	if err != nil {
		h.log.Error("Failed to process image", "error", err, "user_id", job.userID, "purpose", job.purpose)
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidImage})
		return nil, false
	}
	return processed, true
}

// processGIFTier1Image is the #1302 animated-aware branch. The GIF is decoded
// ONCE through safeDecodeGIF — the decompression-bomb guards run during
// animation detection, before any entitlement gate — and the frame count
// selects the path:
//
//   - single frame → today's static flatten path, every tier (decision 5);
//   - multi frame  → entitlement gate per purpose, then animation-preserving
//     re-encode (uploads are never stored verbatim — decision 1).
//
// Routing keys on the declared/sniffed content type. Mislabeling cannot
// escalate: an animated GIF declared as another image type falls through to
// the static path and is flattened to its first frame, never preserved.
func (h *Handler) processGIFTier1Image(c *gin.Context, file multipart.File, job tier1ImageJob) (*ProcessedImage, bool) {
	raw, err := io.ReadAll(file)
	if err != nil {
		h.log.Error("Failed to read gif upload", "error", err, "user_id", job.userID, "purpose", job.purpose)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read file"})
		return nil, false
	}
	g, err := safeDecodeGIF(bytes.NewReader(raw))
	if err != nil {
		// Includes the bomb-guard rejections (frame count / total pixels /
		// screen dims) — the guard errors are PII-safe, bounds and counts only.
		h.log.Error("Failed to decode gif upload", "error", err, "user_id", job.userID, "purpose", job.purpose)
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidImage})
		return nil, false
	}
	if len(g.Image) <= 1 {
		// Static GIF keeps the flatten path for every tier (#1302 decision 5).
		return h.processStaticTier1Image(c, bytes.NewReader(raw), job)
	}
	if !h.authorizeAnimatedUpload(c, job.userID, job.purpose, job.serverID) {
		return nil, false
	}
	processed, err := processDecodedGIF(g, job.maxW, job.maxH)
	if err != nil {
		h.log.Error("Failed to process animated gif", "error", err, "user_id", job.userID, "purpose", job.purpose)
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidImage})
		return nil, false
	}
	return processed, true
}

// authorizeAnimatedUpload enforces the animated-media entitlements (#1302):
// user axis (AllowAnimatedProfile) for avatar/banner, server axis
// (AllowAnimatedServerBanner) for server banners, rejected for every other
// purpose. Typed codes let the client render the upsell affordance (#1522
// pattern). On rejection the 403 response is written and false returned.
func (h *Handler) authorizeAnimatedUpload(c *gin.Context, userID, purpose, serverID string) bool {
	switch purpose {
	case purposeAvatar, purposeBanner:
		ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), userID))
		if !ent.AllowAnimatedProfile {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Animated avatars and banners require a premium subscription",
				"code":  "animated_profile_premium",
			})
			return false
		}
	case purposeServerBanner:
		serverEnt := entitlements.ForServer(h.serverTier(c.Request.Context(), serverID))
		if !serverEnt.AllowAnimatedServerBanner {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Animated server banners require a Mach server boost",
				"code":  "animated_server_banner_mach",
			})
			return false
		}
	default:
		c.JSON(http.StatusForbidden, gin.H{
			"error": "Animated images are not supported for this upload type",
			"code":  "animated_not_supported",
		})
		return false
	}
	return true
}

func tier1StorageKey(purpose, userID, serverID, conversationID string) string {
	switch purpose {
	case purposeAvatar:
		return fmt.Sprintf("avatars/%s", userID)
	case purposeBanner:
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

func profileSlotForPurpose(purpose string) string {
	if purpose == purposeBanner {
		return ProfileSlotBanner
	}
	return ProfileSlotAvatar
}

func profileTier1StorageKey(purpose, userID string) string {
	prefix := "avatars"
	if purpose == purposeBanner {
		prefix = "banners"
	}
	return fmt.Sprintf("%s/%s/%s", prefix, userID, uuid.NewString())
}

func profileTier1CanonicalURL(purpose, userID string) string {
	if purpose == purposeBanner {
		return fmt.Sprintf("/api/v1/media/banners/%s", userID)
	}
	return fmt.Sprintf("/api/v1/media/avatars/%s", userID)
}

func (h *Handler) createProfileTier1UploadIntent(c *gin.Context, userID, profileSlot, storageKey string) bool {
	ctx, cancel := context.WithTimeout(c.Request.Context(), profileTier1TxTimeout)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin profile upload intent transaction", "error", err, "profile_slot", profileSlot)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return false
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error("Failed to roll back profile upload intent transaction", "error", rollbackErr, "profile_slot", profileSlot)
		}
	}()

	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&lockedUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
			return false
		}
		h.log.Error("Failed to lock profile upload intent user", "error", err, "profile_slot", profileSlot)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return false
	}
	avatarPrefix := fmt.Sprintf("avatars/%s/", lockedUserID)
	bannerPrefix := fmt.Sprintf("banners/%s/", lockedUserID)
	// The range bounds use the byte immediately after '/' so the primary-key
	// lookup is limited to immutable generations for this user, rather than
	// scanning all permanent obligations. LIMIT keeps the count work bounded.
	var unresolvedEvidence int
	if err := tx.QueryRowContext(ctx, `
		WITH bounded_evidence AS (
			SELECT 1
			FROM (
				SELECT 1
				FROM tier1_profile_upload_intents
				WHERE user_id = $1
				LIMIT $2
			) AS active_intents
			UNION ALL
			SELECT 1
			FROM (
				SELECT 1
				FROM tier1_erasure_delete_obligations AS obligation
				WHERE (
					(obligation.storage_key >= $3 AND obligation.storage_key < $4)
					OR (obligation.storage_key >= $5 AND obligation.storage_key < $6)
				)
				LIMIT $2
			) AS unresolved_obligations
		)
		SELECT COUNT(*) FROM bounded_evidence`,
		lockedUserID,
		maxUnresolvedProfileUploadEvidence,
		avatarPrefix,
		fmt.Sprintf("avatars/%s0", lockedUserID),
		bannerPrefix,
		fmt.Sprintf("banners/%s0", lockedUserID),
	).Scan(&unresolvedEvidence); err != nil {
		h.log.Error("Failed to count unresolved profile upload evidence", "error", err, "profile_slot", profileSlot)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return false
	}
	if unresolvedEvidence >= maxUnresolvedProfileUploadEvidence {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many unresolved profile uploads"})
		return false
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tier1_profile_upload_intents (storage_key, user_id, profile_slot) VALUES ($1, $2, $3)`,
		storageKey, lockedUserID, profileSlot,
	); err != nil {
		h.log.Error("Failed to persist profile upload intent", "error", err, "profile_slot", profileSlot)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return false
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit profile upload intent", "error", err, "profile_slot", profileSlot)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return false
	}
	committed = true
	return true
}

func (h *Handler) storeProfileTier1Image(c *gin.Context, store ObjectStore, userID, purpose, storageKey string, processed *ProcessedImage) (fileID string, ok bool) {
	txCtx, cancel := context.WithTimeout(c.Request.Context(), profileTier1TxTimeout)
	defer cancel()

	tx, err := h.db.BeginTx(txCtx, nil)
	if err != nil {
		h.log.Error("Failed to begin profile image transaction", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error("Failed to roll back profile image transaction", "error", rollbackErr, "purpose", purpose)
		}
	}()

	var lockedUserID string
	if err := tx.QueryRowContext(txCtx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&lockedUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
			return "", false
		}
		h.log.Error("Failed to lock profile image user", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}

	profileSlot := profileSlotForPurpose(purpose)
	var intentKey string
	err = tx.QueryRowContext(txCtx, `
		SELECT storage_key
		FROM tier1_profile_upload_intents AS intents
		WHERE storage_key = $1 AND user_id = $2 AND profile_slot = $3
		  AND NOT EXISTS (
			SELECT 1 FROM tier1_erasure_delete_obligations
			WHERE storage_key = intents.storage_key
		  )
		FOR UPDATE`, storageKey, lockedUserID, profileSlot,
	).Scan(&intentKey)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
		return "", false
	}
	if err != nil {
		h.log.Error("Failed to lock profile upload intent", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}

	if _, err := tx.ExecContext(txCtx, `
		INSERT INTO tier1_erasure_delete_obligations (storage_key)
		SELECT storage_key FROM tier1_profile_upload_intents
		WHERE user_id = $1 AND profile_slot = $2 AND storage_key <> $3
		ON CONFLICT (storage_key) DO NOTHING`, lockedUserID, profileSlot, intentKey); err != nil {
		h.log.Error("Failed to terminalize superseded profile upload intents", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}
	if _, err := tx.ExecContext(txCtx,
		`DELETE FROM tier1_profile_upload_intents WHERE user_id = $1 AND profile_slot = $2 AND storage_key <> $3`,
		lockedUserID, profileSlot, intentKey,
	); err != nil {
		h.log.Error("Failed to delete superseded profile upload intents", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}
	legacyKey := tier1StorageKey(purpose, lockedUserID, "", "")
	if err := ensureProfileSlotUsesLegacyBackend(txCtx, tx, lockedUserID, profileSlot, legacyKey); err != nil {
		h.log.Error("Profile image replacement refused non-legacy backend", "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}
	if _, err := tx.ExecContext(txCtx, `
		WITH retired AS (
			UPDATE media_files SET deleted_at = NOW()
			WHERE uploader_id = $1 AND media_tier = 1
			  AND deleted_at IS NULL
			  AND (profile_slot = $2 OR storage_key = $3)
			RETURNING storage_key
		)
		INSERT INTO tier1_erasure_delete_obligations (storage_key)
		SELECT storage_key FROM retired
		UNION
		SELECT $3
		ON CONFLICT (storage_key) DO NOTHING`, lockedUserID, profileSlot, legacyKey); err != nil {
		h.log.Error("Failed to retire prior profile image", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}

	fileID, err = h.insertProfileTier1Record(txCtx, c, tx, lockedUserID, profileSlot, intentKey, processed)
	if err != nil {
		return "", false
	}

	reader := bytes.NewReader(processed.Data)
	if err := store.PutObject(txCtx, storageKey, reader, int64(len(processed.Data)), processed.ContentType); err != nil {
		h.log.Error("Failed to store profile image", "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}
	result, err := tx.ExecContext(txCtx,
		`DELETE FROM tier1_profile_upload_intents WHERE storage_key = $1 AND user_id = $2 AND profile_slot = $3`,
		intentKey, lockedUserID, profileSlot,
	)
	if err != nil {
		h.log.Error("Failed to finalize profile upload intent", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil || rowsAffected != 1 {
		h.log.Error("Profile upload intent finalization did not affect exactly one row", "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}

	if err := h.commitTier1Upload(tx); err != nil {
		h.log.Error("Failed to commit profile image metadata", "error", err, "purpose", purpose)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
		return "", false
	}
	committed = true
	return fileID, true
}

func (h *Handler) insertProfileTier1Record(ctx context.Context, c *gin.Context, tx *sql.Tx, userID, profileSlot, storageKey string, processed *ProcessedImage) (string, error) {
	fileID := uuid.NewString()
	err := tx.QueryRowContext(ctx, `
		INSERT INTO media_files (
			id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		RETURNING id`,
		fileID, userID, string(FileTypePhoto), MediaTierAuthenticated,
		processed.ContentType, len(processed.Data), storageKey, profileSlot,
	).Scan(&fileID)
	if err != nil {
		h.log.Error("Failed to record profile image metadata", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRecordMediaMetadata})
		return "", err
	}
	return fileID, nil
}

func insertTier1Record(ctx context.Context, h *Handler, c *gin.Context, tx *sql.Tx, userID, storageKey string, processed *ProcessedImage) (string, error) {
	fileID := uuid.New().String()

	insertQuery := `
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (storage_key) WHERE deleted_at IS NULL
		DO UPDATE SET uploader_id = EXCLUDED.uploader_id, file_size = EXCLUDED.file_size, mime_type = EXCLUDED.mime_type, updated_at = NOW()
		RETURNING id
	`
	var err error
	if tx != nil {
		err = tx.QueryRowContext(ctx, insertQuery, fileID, userID, string(FileTypePhoto), MediaTierAuthenticated,
			processed.ContentType, len(processed.Data), storageKey).Scan(&fileID)
	} else {
		err = h.db.QueryRowContext(ctx, insertQuery, fileID, userID, string(FileTypePhoto), MediaTierAuthenticated,
			processed.ContentType, len(processed.Data), storageKey).Scan(&fileID)
	}
	if err != nil {
		h.log.Error(errMsgFailedRecordMediaMetadata, "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRecordMediaMetadata})
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
	// header.Size is CIPHERTEXT; maxSize is the PLAINTEXT entitlement. Comparing
	// them directly is a live defect: a file in the top LegacyEnvelopeOverheadBytes
	// of a user's allowance passes the client's plaintext check and then 413s here.
	// Convert first so both sides are in the same unit.
	//
	// This route only ever carries the v1 single-shot envelope, whose overhead is
	// exactly IV + tag. The chunked format goes through the upload-session routes,
	// which do their own arithmetic — and the server never parses the envelope
	// header to find out, because that would re-create the trust inversion the
	// in-band AAD design exists to avoid.
	if header.Size-LegacyEnvelopeOverheadBytes > maxSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("File exceeds maximum upload size of %d bytes", maxSize),
			"max_size": maxSize,
		})
		_ = file.Close()
		return nil, nil, fmt.Errorf("file too large")
	}
	return file, header, nil
}

// errMsgKeyVersionRequired is returned when a tier-2 attachment upload omits or
// malforms key_version. The epoch must come from the sender (#2843).
const errMsgKeyVersionRequired = "key_version is required and must be a positive integer"

// errMsgKeyVersionUnknown answers an epoch that has never existed for the
// context. Distinct from errMsgKeyVersionRequired, which is about the shape of
// the value rather than whether the server has ever issued it.
const errMsgKeyVersionUnknown = "key_version names an epoch that does not exist for this context"

func validateAttachmentRequest(c *gin.Context) (fileType FileType, mimeType string, keyVersion int, ok bool) {
	fileType = FileType(c.PostForm("file_type"))
	if !isValidFileType(fileType) {
		fileType = FileTypeFile
	}

	mimeType = c.PostForm("mime_type")
	if mimeType == "" {
		mimeType = mimeOctetStream
	}

	// #2843: a tier-2 attachment's epoch is CLIENT-ATTESTED, never invented here.
	// This previously seeded `keyVersion = 1` and validated only when the field
	// was present, so an ABSENT key_version was silently supplied by the server —
	// and the media_files CHECK constraint (media_tier = 2 AND key_version IS NOT
	// NULL) could not detect it, because the value it checks for was the server's
	// own. A constraint on a server-suppliable field proves presence, not
	// authenticity. Same defect and same fix as #2832 on the message-send path.
	//
	// The default dates to #418 (2026-04-03), when an unencrypted upload
	// legitimately omitted the field; #1042 removed the is_encrypted selector
	// without removing the accommodation.
	keyVersionStr := c.PostForm("key_version")
	if keyVersionStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgKeyVersionRequired})
		return "", "", 0, false
	}
	if v, err := fmt.Sscanf(keyVersionStr, "%d", &keyVersion); err != nil || v != 1 || keyVersion < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgKeyVersionRequired})
		return "", "", 0, false
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
	// CV-CAN-004: uploading an attachment requires the ability to SEND in the
	// channel, not just ATTACH_FILES held independently of send/view.
	if !h.checkSendPermission(c, userID, channelID) {
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
	// storageBackend is the identifier of the backend the object was actually
	// written to. EMPTY MEANS LEGACY and is persisted as NULL -- never write
	// the literal "legacy" into the column (migration 000114's spelling).
	storageBackend string
}

func createAttachmentRecord(h *Handler, c *gin.Context, p attachmentParams) error {
	var chID, convID interface{}
	if p.channelID != "" {
		chID = p.channelID
	}
	if p.conversationID != "" {
		convID = p.conversationID
	}

	// #2201: a tier-2 attachment row is key-material-coupled (key_version +
	// E2EE ciphertext object) — recheck the uploader's credential epoch inside
	// the metadata-write transaction. The caller maps credepoch errors to 401.
	ctx := c.Request.Context()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin attachment tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback attachment tx", "error", rbErr)
		}
	}()
	if guardErr := credepoch.GuardTx(ctx, tx, p.userID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		return guardErr
	}

	insertQuery := `
		INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key,
		                         key_version, channel_id, conversation_id, storage_backend, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
	`
	var backend interface{}
	if p.storageBackend != "" {
		backend = p.storageBackend
	}
	if _, err := tx.ExecContext(ctx, insertQuery, p.fileID, p.userID, string(p.fileType), MediaTierE2EE,
		p.mimeType, p.fileSize, p.storageKey, p.keyVersion, chID, convID, backend); err != nil {
		return err
	}
	return tx.Commit()
}

// proxyTier1Media fetches a Tier 1 media object from MinIO and streams it to
// the client with appropriate cache headers. Used for avatars, banners,
// server icons, server banners, and DM icons. Profile keys are admitted only
// when live metadata exists and no durable erasure tombstone exists.
// If public is true, non-profile responses are marked publicly cacheable
// (Cloudflare / shared caches OK) — only safe for routes registered without
// auth middleware.
func (h *Handler) proxyTier1Media(c *gin.Context, key string, public bool) {
	profileMedia := isProfileTier1StorageKey(key)
	if profileMedia {
		admitted, err := h.profileTier1MediaAdmitted(c.Request.Context(), key)
		if err != nil || !admitted {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgNotFound})
			return
		}
	}
	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}
	obj, contentType, err := store.GetObject(c.Request.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgNotFound})
			return
		}
		if profileMedia {
			h.log.Error("Failed to fetch profile media from storage")
		} else {
			h.log.Error("Failed to fetch media from storage", "error", err, "key", key)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalServer})
		return
	}
	defer func() { _ = obj.Close() }()

	// Profile media remains uncached to make durable erasure visible immediately;
	// public server/DM assets retain their shared-cache policy.
	if profileMedia {
		c.Header(headerCacheControl, cacheControlNoStore)
	} else if public {
		c.Header(headerCacheControl, cacheControlPublic)
	} else {
		c.Header(headerCacheControl, cacheControlPrivate)
	}
	c.Header(headerContentType, contentType)
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, obj); err != nil {
		if profileMedia {
			h.log.Warn("Failed to stream profile media to client")
		} else {
			h.log.Warn("Failed to stream media to client", "error", err, "key", key)
		}
	}
}

func (h *Handler) proxyProfileTier1Media(c *gin.Context, userID, profileSlot string) {
	key, found, err := ProfileTier1StorageKey(c.Request.Context(), h.db, userID, profileSlot)
	if err != nil || !found {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgNotFound})
		return
	}
	h.proxyTier1Media(c, key, true)
}

func isProfileTier1StorageKey(key string) bool {
	return strings.HasPrefix(key, "avatars/") || strings.HasPrefix(key, "banners/")
}

func (h *Handler) profileTier1MediaAdmitted(ctx context.Context, key string) (bool, error) {
	return profileTier1MediaAdmitted(ctx, h.db, key, nil)
}

// ProfileTier1MediaOwnedAdmitted reports whether a user's exact profile-media
// key is live Tier 1 metadata and is not pending durable erasure.
func ProfileTier1MediaOwnedAdmitted(ctx context.Context, db RowQuerier, key, userID string) (bool, error) {
	return profileTier1MediaAdmitted(ctx, db, key, &userID)
}

// ProfileTier1MediaOwnedSlotAdmitted reports whether a canonical profile slot
// resolves to one live, non-tombstoned immutable physical object for its owner.
func ProfileTier1MediaOwnedSlotAdmitted(ctx context.Context, db RowQuerier, userID, profileSlot string) (bool, error) {
	_, found, err := ProfileTier1StorageKey(ctx, db, userID, profileSlot)
	return found, err
}

// ProfileTier1StorageKey resolves a canonical avatar/banner slot to its exact
// immutable object key. The durable tombstone check prevents a retired key from
// being served even if its metadata is still visible to a concurrent reader.
func ProfileTier1StorageKey(ctx context.Context, db RowQuerier, userID, profileSlot string) (string, bool, error) {
	if profileSlot != ProfileSlotAvatar && profileSlot != ProfileSlotBanner {
		return "", false, fmt.Errorf("invalid profile slot %q", profileSlot)
	}
	var storageKey string
	err := db.QueryRowContext(ctx, profileTier1SlotKeyQuery, userID, profileSlot).Scan(&storageKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return storageKey, true, nil
}

func profileTier1MediaAdmitted(ctx context.Context, db RowQuerier, key string, userID *string) (bool, error) {
	var admitted bool
	err := db.QueryRowContext(ctx, profileTier1MediaAdmittedQuery, key, userID).Scan(&admitted)
	return admitted, err
}

func (h *Handler) proxyInviteIcon(c *gin.Context, key string) {
	store, ok := h.requireObjectStore(c)
	if !ok {
		return
	}
	obj, contentType, err := store.GetObject(c.Request.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
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

// userHasChannelAccess checks that a user can view the channel that owns an
// attachment: server membership AND the channel's type-appropriate VIEW
// permission (CV-CAN-003/004). Falls back to membership-only when no RBAC
// resolver is configured (tests).
func (h *Handler) userHasChannelAccess(c *gin.Context, userID, channelID string) bool {
	// Membership check (resolves the channel's server + type; a non-member yields no row).
	var serverID, channelType string
	err := h.db.QueryRow(`
		SELECT ch.server_id, ch.type FROM channels ch
		JOIN server_members sm ON sm.server_id = ch.server_id AND sm.user_id = $2
		WHERE ch.id = $1
	`, channelID, userID).Scan(&serverID, &channelType)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}

	if h.resolver == nil {
		return true
	}
	// Require the channel's type-appropriate VIEW bit, matching the websocket
	// channel/message auth path (channelContext.viewPermission): text/bulletin
	// use PermViewTextChannels, voice uses PermViewVoiceChannels. A member denied
	// visibility must not read (download) or write (upload) a hidden channel's
	// attachments by UUID; a voice member without the text-view bit must not be
	// wrongly blocked from that voice channel's attachments.
	viewPerm, ok := channelViewPermission(channelType)
	if !ok {
		// Unknown / non-viewable channel type: deny, mirroring viewPermission().
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	canView, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, viewPerm)
	if permErr != nil {
		h.log.Error("Failed to check channel view permission", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyAccess})
		return false
	}
	if !canView {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	return true
}

// channelViewPermission maps a channel type to the RBAC "view" permission bit
// that gates its visibility, mirroring the websocket channel auth path
// (channelContext.viewPermission): text/bulletin use PermViewTextChannels,
// voice uses PermViewVoiceChannels. Returns ok=false for types with no view
// gate, which callers treat as not viewable.
func channelViewPermission(channelType string) (rbac.Permission, bool) {
	switch channelType {
	case "text", "bulletin":
		return rbac.PermViewTextChannels, true
	case "voice":
		return rbac.PermViewVoiceChannels, true
	default:
		return 0, false
	}
}

// checkSendPermission requires the SEND_MESSAGES RBAC permission for a channel
// and rejects members serving an active timeout. Used to gate attachment UPLOAD
// (CV-CAN-004): a member must be able to send in the channel, not merely hold
// ATTACH_FILES independently, and a timed-out member must not upload at all.
// Falls back to the SEND check being skipped (membership already verified
// upstream) when no RBAC resolver is configured, but the timeout gate always
// applies, mirroring messages.checkSendAccess.
func (h *Handler) checkSendPermission(c *gin.Context, userID, channelID string) bool {
	// Resolve the channel's server and the member's timeout state together. The
	// join to server_members yields timed_out_until; membership itself is already
	// verified upstream (userHasChannelAccess). A missing row (unknown channel or
	// non-member) is a client-facing condition, not a server fault.
	var serverID string
	var timedOutUntil sql.NullTime
	err := h.db.QueryRowContext(c.Request.Context(), `
		SELECT ch.server_id, sm.timed_out_until FROM channels ch
		JOIN server_members sm ON sm.server_id = ch.server_id AND sm.user_id = $2
		WHERE ch.id = $1
	`, channelID, userID).Scan(&serverID, &timedOutUntil)
	if err == sql.ErrNoRows {
		// Report 403 (matching userHasChannelAccess) and avoid leaking existence.
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
		return false
	}
	if err != nil {
		h.log.Error(logMsgChannelServerLookup, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}

	// A member serving an active timeout cannot upload, mirroring the send-message
	// path (messages.checkSendAccess). Enforced regardless of resolver config.
	if timedOutUntil.Valid && timedOutUntil.Time.After(time.Now().UTC()) {
		c.JSON(http.StatusForbidden, gin.H{
			"error":           "Member is timed out",
			"code":            "member_timed_out",
			"timed_out_until": timedOutUntil.Time,
		})
		return false
	}

	if h.resolver == nil {
		return true
	}
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermSendMessages)
	if err != nil {
		h.log.Error("Failed to check send permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have permission to send messages in this channel"})
		return false
	}
	return true
}

// checkReadHistoryPermission requires the READ_MESSAGE_HISTORY RBAC permission
// for a channel. Used to gate attachment DOWNLOAD (CV-CAN-003): a member who can
// see a channel but cannot read its history must not fetch that channel's
// attachments by file UUID, mirroring the message-read path
// (messages.checkChannelAccess). Applied only on the download path (not upload),
// after userHasChannelAccess has verified membership and the type-appropriate
// VIEW bit. Falls back to allow when no RBAC resolver is configured (tests).
func (h *Handler) checkReadHistoryPermission(c *gin.Context, userID, channelID string) bool {
	if h.resolver == nil {
		return true
	}
	var serverID string
	if err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT server_id FROM channels WHERE id = $1`, channelID,
	).Scan(&serverID); err != nil {
		// A missing channel is a client-facing condition, not a server fault;
		// report 403 (matching userHasChannelAccess) and avoid leaking existence.
		if err == sql.ErrNoRows {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgAccessDenied})
			return false
		}
		h.log.Error(logMsgChannelServerLookup, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermReadMessageHistory)
	if err != nil {
		h.log.Error("Failed to check read history permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
		return false
	}
	if !hasPerm {
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
		h.log.Error(logMsgChannelServerLookup, "error", err)
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
