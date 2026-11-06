package media

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Direct attachment downloads (#1612, ADR-0038 § Delivery boundary).
//
// GET /api/v1/media/attachments/:file_id/url authorizes exactly as the proxy
// does, then answers with a short-lived presigned URL instead of the bytes, so
// a 1 GiB attachment is fetched from the vendor bucket rather than through the
// control plane's shared uplink.
//
// The shape is authorize-then-fetch, never a 302: S3 presigning cannot carry
// X-File-Key-Version, and dropping that header brings back the #2832 epoch
// regression. The epoch therefore rides in the JSON body.
//
// The proxy stays the default and the fallback. This route answers 409
// proxy_only, and the client uses the proxy, whenever:
//   - the row is on the legacy backend (MinIO is not reachable from outside
//     the Docker network, and profile media never reaches this route), or
//   - the object is below the presign floor, where a second round trip costs
//     more than the transfer.

// directDownloadTTL is the presigned URL lifetime. S3 checks the signature
// when the request STARTS, so this bounds how long the capability can be
// replayed, not how long a download may take. ADR-0038 asks for 60-120 s; a
// presigned URL is a transferable capability, so the short end matters.
const directDownloadTTL = 90 * time.Second

// errCodeProxyOnly tells the client to use GET /attachments/:file_id instead.
const errCodeProxyOnly = "proxy_only"

// attachmentDownloadURLResponse is the authorize-then-fetch body.
type attachmentDownloadURLResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	MimeType  string    `json:"mimeType"`
	// Nullable: rows predating the client-attested epoch (#2832) carry none,
	// and the client falls back to the current key, exactly as with the
	// proxy's absent header.
	KeyVersion *int  `json:"keyVersion"`
	Size       int64 `json:"size"`
}

// AttachmentDownloadURL mints a presigned download URL for an E2EE attachment.
// GET /api/v1/media/attachments/:file_id/url
func (h *Handler) AttachmentDownloadURL(c *gin.Context) {
	c.Header(headerCacheControl, cacheControlNoStore)
	row, ok := h.loadAuthorizedAttachment(c)
	if !ok {
		return
	}
	fileID, storageKey, mimeType, fileSize, keyVersion, storageBackend :=
		row.fileID, row.storageKey, row.mimeType, row.fileSize, row.keyVersion, row.storageBackend

	if isLegacyBackend(storageBackend) || fileSize < h.cfg.StoragePresignMinBytes {
		c.JSON(http.StatusConflict, gin.H{"error": "Use the attachment proxy", "code": errCodeProxyOnly})
		return
	}

	store, ok := h.requireObjectStoreForRow(c, storageBackend)
	if !ok {
		return
	}
	// Read the clock first so expiresAt is never later than the signature.
	signedAt := time.Now()
	url, err := store.PresignedGetURL(c.Request.Context(), storageKey, directDownloadTTL)
	if err != nil {
		h.log.Error("Failed to presign attachment download", "error", err, "file_id", fileID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare download"})
		return
	}
	// The bytes go straight from the bucket, so count the read here: a second
	// read of a cold file within 30 days promotes it back to Standard. This
	// counts a mint, not a completed fetch; a client that falls back to the
	// proxy after a failed direct fetch is counted twice (cold-tier runbook).
	h.recordAttachmentRead(store, fileID, storageKey)

	c.JSON(http.StatusOK, attachmentDownloadURLResponse{
		URL:        url,
		ExpiresAt:  signedAt.Add(directDownloadTTL).UTC(),
		MimeType:   mimeType,
		KeyVersion: keyVersion,
		Size:       fileSize,
	})
}

// attachmentRow is the tier-2 media_files row both attachment routes read.
type attachmentRow struct {
	fileID, storageKey, mimeType string
	fileSize                     int64
	// NULLable: rows predating the client-attested epoch (#2832) carry none, and
	// the client correctly falls back to the current key for those.
	keyVersion *int
	// NULLable, permanently: NULL means the legacy backend (migration 000114
	// is explicit that it is never backfilled), so this is not a gap to be
	// closed — it is the value every pre-cutover object carries forever.
	storageBackend *string
}

// loadAuthorizedAttachment parses :file_id, reads the row and authorizes the
// caller, writing the 400/404/403/500 itself. Authorization runs before the
// caller sees any placement detail, so an outsider gets the same 403 from the
// proxy and from the presign route.
func (h *Handler) loadAuthorizedAttachment(c *gin.Context) (attachmentRow, bool) {
	parsedID, err := uuid.Parse(c.Param("file_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid file ID"})
		return attachmentRow{}, false
	}
	// Canonical form, so every spelling of one UUID shares one cold-read counter.
	row := attachmentRow{fileID: parsedID.String()}
	var uploaderID string
	var channelID, conversationID *string
	err = h.db.QueryRow(
		`SELECT storage_key, mime_type, file_size, channel_id, conversation_id, key_version, storage_backend, uploader_id
		 FROM media_files
		 WHERE id = $1 AND deleted_at IS NULL AND media_tier = 2`, row.fileID).
		Scan(&row.storageKey, &row.mimeType, &row.fileSize, &channelID, &conversationID, &row.keyVersion, &row.storageBackend, &uploaderID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgFileNotFound})
		return row, false
	} else if err != nil {
		h.log.Error("Failed to fetch file metadata", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch file"})
		return row, false
	}
	if !h.userCanDownloadAttachment(c, c.GetString("user_id"), uploaderID, row.fileID, channelID, conversationID) {
		return row, false
	}
	return row, true
}
