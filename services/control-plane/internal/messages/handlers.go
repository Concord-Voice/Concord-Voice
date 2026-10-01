// Package messages provides handlers for managing chat messages.
package messages

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/klipy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgInvalidRequestBody     = "Invalid request body"
	errMsgInvalidMessageID       = "Invalid message ID"
	errMsgInsufficientPerms      = "Insufficient permissions"
	errMsgMessageNotFound        = "Message not found"
	errMsgFailedCheckPerms       = "Failed to check permissions"
	errMsgFailedFetchMessages    = "Failed to fetch messages"
	errMsgFailedSendMessage      = "Failed to send message"
	errMsgFailedUpdateMessage    = "Failed to update message"
	errMsgFailedDeleteMessage    = "Failed to delete message"
	errMsgFailedSuppressEmbeds   = "Failed to suppress embeds"
	errMsgInsufficientPermsLower = "insufficient permissions"
	errMsgNotMember              = "Not a member of this channel's server"
	errMsgFailedCheckMembership  = "Failed to check membership"
	errMsgInvalidCiphertext      = "Invalid ciphertext format for E2EE channel"
	errMsgInvalidKeyVersion      = "key_version is required and must be a positive integer"
	errMsgMemberTimedOut         = "Member is timed out"
	errMsgFailedVerifyKeyEpoch   = "Failed to verify key epoch"
	messageWriteTimeout          = 3 * time.Second
)

var errMessageDeleteGuardRejected = errors.New("message delete guard rejected")

// respondGuardTxError maps a credepoch.GuardTx failure onto the wire (#2201
// review), mirroring channels.respondKeyDistributionError: an epoch-fence
// rejection is the middleware-identical generic 401; any other failure (a
// store/lock read error, e.g. "credepoch: guard read") is a logged 500 —
// otherwise a transient DB error looks like "re-authenticate" to the client
// and leaves no log trail.
func (h *Handler) respondGuardTxError(c *gin.Context, err error, genericMsg string) {
	if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	h.log.Error("credential-epoch guard read failed", "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": genericMsg})
}

// minCiphertextSize is the minimum base64-decoded size for a valid AES-GCM ciphertext:
// 12 bytes IV + 16 bytes auth tag = 28 bytes minimum (empty plaintext).
const minCiphertextSize = 28

// isValidCiphertext checks that content is valid base64 and meets the minimum
// size for an AES-256-GCM ciphertext (12-byte IV + 16-byte auth tag).
func isValidCiphertext(content string) bool {
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return false
	}
	return len(decoded) >= minCiphertextSize
}

// Handler handles message-related requests
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	resolver    *rbac.Resolver
	tiers       entitlements.TierResolver // user-axis tier resolution (#1555 search-depth gate)
	purgeEngine *purge.Engine             // bulk message purge (#1352)
	ops         OpsCounter
	redis       *redis.Client            // delete-rate soft-lock counter + its MFA confirmation's attempt budget (#3455)
	mfaVerifier stepup.MFATxCodeVerifier // step-up auth for the delete-rate soft-lock's MFA confirmation (#3455)

	// beforeSoftLockConfirmHook is a test-only ordering seam (the dm
	// afterCandidateReadHook precedent): it runs after the soft-lock's
	// unlocked population read and before the transaction that re-reads it
	// under lock, so a test can flip the server's enforcement in flight. Nil
	// in production: NewHandler never sets it and only export_test.go can.
	beforeSoftLockConfirmHook func()
}

// OpsCounter is the optional aggregate counter sink used after committed writes.
type OpsCounter interface {
	Increment(opsmetrics.MetricKey)
}

type epochQueryRower interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

type messageChannelAuthorization struct {
	serverID   string
	channelID  string
	userID     string
	required   rbac.Permission
	genericMsg string
}

// NewHandler creates a new message handler. purgeEngine backs the bulk-purge
// endpoints (#1352); opsCounters stays variadic (main's #1689 shape) so it
// remains optional for callers that do not report aggregate metrics.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, tiers entitlements.TierResolver, purgeEngine *purge.Engine, opsCounters ...OpsCounter) *Handler {
	handler := &Handler{
		db:          db,
		log:         log,
		hub:         hub,
		resolver:    resolver,
		tiers:       tiers,
		purgeEngine: purgeEngine,
	}
	if len(opsCounters) > 0 {
		handler.ops = opsCounters[0]
	}
	return handler
}

// SetMFAVerifier wires the verifier that checks the delete-rate soft-lock's
// MFA confirmation (#3455). An unwired verifier fails closed (500 on every
// over-threshold delete), which is why the router boot guard asks
// HasMFAVerifier. Pattern mirrors servers.Handler's own SetMFAVerifier.
func (h *Handler) SetMFAVerifier(v stepup.MFATxCodeVerifier) { h.mfaVerifier = v }

// HasMFAVerifier reports whether SetMFAVerifier was called with a non-nil
// verifier. The router's boot guard interrogates the HANDLER through this.
func (h *Handler) HasMFAVerifier() bool { return h.mfaVerifier != nil }

// SetRedis wires the client behind the delete-rate soft-lock counter and its
// MFA confirmation's attempt budget (#3455). A nil client fails the soft-lock
// closed (503), which is safe but silent, so the boot guard asks HasRedis.
func (h *Handler) SetRedis(client *redis.Client) { h.redis = client }

// HasRedis reports whether SetRedis was called with a non-nil client.
func (h *Handler) HasRedis() bool { return h.redis != nil }

// SendMessageRequest represents a request to send a message.
// Max content length is 65536 bytes (64 KiB) of ciphertext — sized for a future
// 10,240-char paid-tier message under worst-case CJK UTF-8 (3 bytes/char) plus
// AES-GCM + base64 envelope, with ~60% headroom for envelope evolution.
type SendMessageRequest struct {
	ChannelID   string  `json:"channel_id" binding:"required,uuid"`
	Content     string  `json:"content" binding:"required,min=1,max=65536"`
	KeyVersion  int     `json:"key_version" binding:"required,min=1"`
	ReplyToID   *string `json:"reply_to_id,omitempty"`  // Optional: UUID of message being replied to (must be same channel)
	MentionMeta string  `json:"mention_meta,omitempty"` // Accepted but unused — mention routing is WebSocket-only. Field exists so REST clients don't get a 400 for including it.
	GifSlug     *string `json:"gif_slug,omitempty"`     // Optional: KLIPY GIF slug to embed in the message
}

// UpdateMessageRequest represents a request to update a message.
// Max content length is 65536 bytes (64 KiB) — matches SendMessageRequest.
type UpdateMessageRequest struct {
	Content    string `json:"content" binding:"required,min=1,max=65536"`
	KeyVersion int    `json:"key_version" binding:"required,min=1"`
}

// isFKViolation returns true if the error is a PostgreSQL foreign key violation (23503).
func isFKViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23503"
}

// checkChannelAccess validates channel ID, checks membership, and verifies PermReadMessageHistory.
// Returns (serverID, ok). On failure, writes the JSON error to c.
func (h *Handler) checkChannelAccess(c *gin.Context, channelID, userID string) (string, bool) {
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel ID"})
		return "", false
	}

	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		WHERE c.id = $1 AND sm.user_id = $2
	`, channelID, userID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return "", false
	}
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return "", false
	}

	hasPerm, permErr := h.resolver.HasPermission(c.Request.Context(), serverID, userID, channelID, rbac.PermReadMessageHistory)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return "", false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return "", false
	}

	return serverID, true
}

// parsePagination extracts and validates limit + before cursor from query params.
func parsePagination(c *gin.Context) (limit int, before string, ok bool) {
	limit = 50
	if limitParam := c.Query("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}
	before = c.Query("before")
	if before != "" {
		if _, err := uuid.Parse(before); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid 'before' parameter"})
			return 0, "", false
		}
	}
	return limit, before, true
}

// parseBulkPagination is like parsePagination but with a higher default and max limit (200)
// for the search backfill bulk endpoint.
func parseBulkPagination(c *gin.Context) (limit int, before string, ok bool) {
	limit = 200
	if limitParam := c.Query("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil && l > 0 && l <= 200 {
			limit = l
		}
	}
	before = c.Query("before")
	if before != "" {
		if _, err := uuid.Parse(before); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid 'before' parameter"})
			return 0, "", false
		}
	}
	return limit, before, true
}

// GetMessagesBulk returns message history with a larger page size (200) for search backfill.
func (h *Handler) GetMessagesBulk(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, ok := h.checkChannelAccess(c, channelID, userID); !ok {
		return
	}

	limit, before, ok := parseBulkPagination(c)
	if !ok {
		return
	}

	// #1555 search-depth gate: bound the backfill window by the requesting
	// user's entitlement (free 90d / premium 180d; negative = unlimited).
	// This bounds ONLY the bulk backfill path — history ACCESS (GetMessages)
	// is never gated (privacy stance; E2EE search is client-side).
	ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), userID))
	var cutoff *time.Time
	if d := ent.MessageHistorySearchDays; d >= 0 {
		t := time.Now().UTC().AddDate(0, 0, -d)
		cutoff = &t
	}

	messages, err := h.queryMessagesBounded(channelID, before, limit, cutoff)
	if err != nil {
		h.log.Error("Failed to query messages (bulk)", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}

	h.enrichMessages(messages, userID)

	resp := gin.H{
		"messages": messages,
		"count":    len(messages),
	}
	if cutoff != nil {
		// Self-describing contract: tell the client how deep the backfill
		// reached so search UX can surface the bound. Omitted when unlimited.
		resp["search_depth_days"] = ent.MessageHistorySearchDays
	}
	c.JSON(http.StatusOK, resp)
}

// enrichMessages batch-loads reactions and replied-to summaries onto messages (non-fatal on failure).
func (h *Handler) enrichMessages(messages []models.MessageWithUser, userID string) {
	if len(messages) == 0 {
		return
	}
	h.attachReactions(messages, userID)
	h.attachRepliedTo(messages)
	h.attachAttachments(messages)
}

// attachAttachments batch-loads and attaches file attachment summaries to messages.
func (h *Handler) attachAttachments(messages []models.MessageWithUser) {
	messageIDs := make([]string, len(messages))
	for i, m := range messages {
		messageIDs[i] = m.ID
	}
	attachmentMap, err := loadAttachmentsForMessages(h.db, messageIDs)
	if err != nil {
		h.log.Error("Failed to load attachments", "error", err)
		return
	}
	for i := range messages {
		if attachments, ok := attachmentMap[messages[i].ID]; ok {
			messages[i].Attachments = attachments
		}
	}
}

// attachReactions batch-loads and attaches reaction summaries to messages.
func (h *Handler) attachReactions(messages []models.MessageWithUser, userID string) {
	messageIDs := make([]string, len(messages))
	for i, m := range messages {
		messageIDs[i] = m.ID
	}
	reactionMap, err := loadReactionsForMessages(h.db, messageIDs, userID)
	if err != nil {
		h.log.Error("Failed to load reactions", "error", err)
		return
	}
	for i := range messages {
		if reactions, ok := reactionMap[messages[i].ID]; ok {
			messages[i].Reactions = reactions
		}
	}
}

// attachRepliedTo batch-loads and attaches replied-to summaries to messages.
func (h *Handler) attachRepliedTo(messages []models.MessageWithUser) {
	replyMap, err := loadRepliedToForMessages(h.db, messages)
	if err != nil {
		h.log.Error("Failed to load replied-to summaries", "error", err)
		return
	}
	if replyMap == nil {
		return
	}
	for i := range messages {
		if summary, ok := replyMap[messages[i].ID]; ok {
			messages[i].RepliedTo = summary
		}
	}
}

// GetMessages returns message history for a channel
func (h *Handler) GetMessages(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, ok := h.checkChannelAccess(c, channelID, userID); !ok {
		return
	}

	limit, before, ok := parsePagination(c)
	if !ok {
		return
	}

	messages, err := h.queryMessages(channelID, before, limit)
	if err != nil {
		h.log.Error("Failed to query messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchMessages})
		return
	}

	h.enrichMessages(messages, userID)

	c.JSON(http.StatusOK, gin.H{
		"messages": messages,
		"count":    len(messages),
	})
}

// queryMessages fetches messages for a channel with optional cursor-based
// pagination. It delegates to queryMessagesBounded with no time bound, so the
// non-bulk history paths (GetMessages — never depth-gated, privacy stance) are
// provably unchanged by the #1555 search-depth gate.
func (h *Handler) queryMessages(channelID, before string, limit int) ([]models.MessageWithUser, error) {
	return h.queryMessagesBounded(channelID, before, limit, nil)
}

// queryMessagesBounded fetches messages for a channel with optional
// cursor-based pagination and an optional lower created_at bound (inclusive).
// A nil cutoff means unbounded. Placeholders are fixed literals ($1 channel,
// $2 limit, $3/$4 the optional arms in args order) — every operand is a
// constant string, every value is parameterized (no interpolation).
func (h *Handler) queryMessagesBounded(channelID, before string, limit int, cutoff *time.Time) ([]models.MessageWithUser, error) {
	query := `
		SELECT m.id, m.channel_id, m.user_id, m.content, COALESCE(m.key_version, 1),
		       m.embeds_suppressed, m.reply_to_id, m.pinned_at, m.pinned_by, m.edited_at, m.expires_at, m.created_at, m.updated_at,
		       m.type, m.expiration_event_payload,
		       u.username, u.display_name, u.avatar_url
		FROM messages m
		INNER JOIN users u ON m.user_id = u.id
		WHERE m.channel_id = $1`
	args := []interface{}{channelID, limit}

	switch {
	case before != "" && cutoff != nil:
		query += `
		  AND m.created_at < (SELECT created_at FROM messages WHERE id = $3)
		  AND m.created_at >= $4`
		args = append(args, before, *cutoff)
	case before != "":
		query += `
		  AND m.created_at < (SELECT created_at FROM messages WHERE id = $3)`
		args = append(args, before)
	case cutoff != nil:
		query += `
		  AND m.created_at >= $3`
		args = append(args, *cutoff)
	}
	query += `
		ORDER BY m.created_at DESC
		LIMIT $2`

	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	messages := []models.MessageWithUser{}
	for rows.Next() {
		var msg models.MessageWithUser
		// Scan through []byte so a NULL payload from ordinary messages remains
		// nil rather than failing conversion to json.RawMessage.
		var expirationEventRaw []byte
		scanErr := rows.Scan(
			&msg.ID,
			&msg.ChannelID,
			&msg.UserID,
			&msg.Content,
			&msg.KeyVersion,
			&msg.EmbedsSuppressed,
			&msg.ReplyToID,
			&msg.PinnedAt,
			&msg.PinnedBy,
			&msg.EditedAt,
			&msg.ExpiresAt,
			&msg.CreatedAt,
			&msg.UpdatedAt,
			&msg.Type,
			&expirationEventRaw,
			&msg.Username,
			&msg.DisplayName,
			&msg.AvatarURL,
		)
		if scanErr != nil {
			h.log.Error("Failed to scan message row", "error", scanErr)
			continue
		}
		if len(expirationEventRaw) > 0 {
			msg.ExpirationEventPayload = json.RawMessage(expirationEventRaw)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// validateReplyToID checks that a reply_to_id is a valid UUID referencing a message
// in the given channel. Returns the validated pointer and whether to continue.
// On validation failure, writes the appropriate JSON error to c.
func (h *Handler) validateReplyToID(c *gin.Context, replyToID *string, channelID string) (*string, bool) {
	if replyToID == nil || *replyToID == "" {
		return nil, true
	}
	if _, err := uuid.Parse(*replyToID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid reply_to_id"})
		return nil, false
	}
	var replyChannelUUID uuid.UUID
	err := h.db.QueryRow(`SELECT channel_id FROM messages WHERE id = $1`, *replyToID).Scan(&replyChannelUUID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Reply target message not found"})
		return nil, false
	}
	if err != nil {
		h.log.Error("Failed to validate reply_to_id", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return nil, false
	}
	parsedChannelID, _ := uuid.Parse(channelID)
	if replyChannelUUID != parsedChannelID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Reply target must be in the same channel"})
		return nil, false
	}
	return replyToID, true
}

// channelViewPermission maps a channel type to the view bit that gates it,
// mirroring the WebSocket send path (channelContext.viewPermission in
// internal/websocket/hub.go) so REST and WebSocket send refuse the same members.
// An unknown type returns ok=false, which callers treat as not viewable.
// ponytail: this mapping now lives in hub.go, channels, media and here; the shared
// home is internal/rbac, and consolidating the four is its own change.
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

// checkSendAccess validates membership, the channel's view bit, PermSendMessages, and fetches
// the embed policy. Returns (serverID, membershipIncarnation, allowEmbeds, ok). On failure,
// writes the JSON error to c.
func (h *Handler) checkSendAccess(c *gin.Context, channelID, userID string) (string, time.Time, bool, bool) {
	var serverID, channelType string
	var membershipIncarnation time.Time
	var serverAllowEmbeds bool
	var timedOutUntil sql.NullTime
	err := h.db.QueryRow(`
		SELECT c.server_id, c.type, sm.joined_at, s.allow_embedded_content, sm.timed_out_until
		FROM channels c
		INNER JOIN server_members sm ON c.server_id = sm.server_id
		INNER JOIN servers s ON c.server_id = s.id
		WHERE c.id = $1 AND sm.user_id = $2
	`, channelID, userID).Scan(&serverID, &channelType, &membershipIncarnation, &serverAllowEmbeds, &timedOutUntil)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return "", time.Time{}, false, false
	}
	if err != nil {
		h.log.Error(errMsgFailedCheckMembership, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return "", time.Time{}, false, false
	}

	effectivePerms, permErr := h.resolver.ResolveEffectivePermissionsUncached(c.Request.Context(), serverID, userID, channelID)
	if errors.Is(permErr, rbac.ErrNotMember) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return "", time.Time{}, false, false
	}
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return "", time.Time{}, false, false
	}
	// Send alone is not enough: a channel is made private by denying its view bit while
	// Send stays granted by the base role, so both are required, as on the WebSocket path.
	viewPerm, viewable := channelViewPermission(channelType)
	if !viewable || !effectivePerms.Has(viewPerm) || !effectivePerms.Has(rbac.PermSendMessages) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return "", time.Time{}, false, false
	}
	if timedOutUntil.Valid && timedOutUntil.Time.After(time.Now().UTC()) {
		c.JSON(http.StatusForbidden, gin.H{
			"error":           errMsgMemberTimedOut,
			"code":            "member_timed_out",
			"timed_out_until": timedOutUntil.Time,
		})
		return "", time.Time{}, false, false
	}

	return serverID, membershipIncarnation, serverAllowEmbeds, true
}

// enforceChannelEpoch rejects revoked key versions and reports the latest epoch
// from surviving keys or the authoritative revocation ledger.
func (h *Handler) enforceChannelEpoch(c *gin.Context, q epochQueryRower, channelID string, keyVersion int) bool {
	var epochRevoked bool
	if err := q.QueryRowContext(c.Request.Context(),
		`SELECT EXISTS(
			SELECT 1 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = $2
		)`,
		channelID, keyVersion,
	).Scan(&epochRevoked); err != nil {
		h.log.Error("Failed to check epoch revocation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyKeyEpoch})
		return false
	}
	if !epochRevoked {
		return h.enforceIssuedChannelEpoch(c, q, channelID, keyVersion)
	}

	var currentEpoch int
	if err := q.QueryRowContext(c.Request.Context(),
		`SELECT GREATEST(
			COALESCE(MAX(successor_epoch), 1),
			COALESCE((SELECT MAX(key_version) FROM channel_keys WHERE channel_id = $1), 1)
		)
		FROM key_revocations
		WHERE channel_id = $1`,
		channelID,
	).Scan(&currentEpoch); err != nil {
		h.log.Error("Failed to resolve current key epoch", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyKeyEpoch})
		return false
	}
	c.JSON(http.StatusConflict, gin.H{
		"error":         "Key epoch has been revoked — re-encrypt with current epoch",
		"code":          "epoch_revoked",
		"current_epoch": currentEpoch,
		"channel_id":    channelID,
	})
	return false
}

// enforceIssuedChannelEpoch rejects a label above the channel's newest issued
// epoch. Such a ciphertext names a key nobody holds, and every reader that
// sees the label fetches a key that does not exist (#2822).
func (h *Handler) enforceIssuedChannelEpoch(c *gin.Context, q epochQueryRower, channelID string, keyVersion int) bool {
	issued, err := keyrotation.IssuedChannelEpoch(c.Request.Context(), q, channelID)
	if err != nil {
		h.log.Error("Failed to resolve issued key epoch", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyKeyEpoch})
		return false
	}
	if keyVersion > issued {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "Key epoch has not been issued",
			"code":          "epoch_unissued",
			"current_epoch": issued,
		})
		return false
	}
	return true
}

// enforceE2EE validates ciphertext shape and epoch revocation for the channel.
// All channels are encrypted under E2EE-everywhere (#201).
// Returns (keyVersion, ok). On failure, writes the JSON error to c.
func (h *Handler) enforceE2EE(c *gin.Context, channelID string, content string, reqKeyVersion int) (int, bool) {
	if !isValidCiphertext(content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCiphertext})
		return 0, false
	}

	// #2832: the epoch is CLIENT-ATTESTED, never server-supplied. This previously read
	// `if keyVersion <= 0 { keyVersion = 1 }` — residue from the pre-#201 era, when an
	// unencrypted channel legitimately omitted the field. Left in place after #1042
	// removed the is_encrypted selector, it made enforceChannelEpoch below check
	// revocation against an epoch the sender never claimed.
	//
	// `binding:"required,min=1"` on SendMessageRequest.KeyVersion already rejects a
	// missing or non-positive value at the bind boundary. This guard is deliberate
	// defence-in-depth rather than redundancy: the defect being fixed here existed
	// precisely because one layer assumed another had enforced the invariant. Reject —
	// never substitute a default.
	keyVersion := reqKeyVersion
	if keyVersion < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidKeyVersion})
		return 0, false
	}

	if !h.enforceChannelEpoch(c, h.db, channelID, keyVersion) {
		return 0, false
	}

	return keyVersion, true
}

// lockMessageMembership serializes a message write with member removal and
// writes the matching HTTP failure response when the lock cannot be acquired.
func (h *Handler) lockMessageMembership(ctx context.Context, c *gin.Context, tx *sql.Tx, serverID, userID string, membershipIncarnation time.Time) bool {
	var timedOutUntil sql.NullTime
	var timedOut bool
	if err := tx.QueryRowContext(ctx,
		`SELECT timed_out_until, timed_out_until IS NOT NULL AND timed_out_until > clock_timestamp()
		 FROM server_members WHERE server_id = $1 AND user_id = $2 AND joined_at = $3 FOR SHARE`,
		serverID, userID, membershipIncarnation,
	).Scan(&timedOutUntil, &timedOut); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
			return false
		}
		h.log.Error("Failed to lock message membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return false
	}
	if timedOut {
		c.JSON(http.StatusForbidden, gin.H{
			"error":           errMsgMemberTimedOut,
			"code":            "member_timed_out",
			"timed_out_until": timedOutUntil.Time,
		})
		return false
	}
	return true
}

// lockMessageChannelTx takes the channel parent lock before membership. This
// is the authority-writer order; callers then lock the exact member row and
// call authorizeMessageChannelTx with the type captured by this lock under the
// same transaction.
func (h *Handler) lockMessageChannelTx(
	ctx context.Context, c *gin.Context, tx *sql.Tx, authorization messageChannelAuthorization,
) (string, bool) {
	var lockedChannelID, channelType string
	if err := tx.QueryRowContext(ctx,
		`SELECT id, type FROM channels WHERE id = $1 AND server_id = $2 FOR SHARE`, authorization.channelID, authorization.serverID,
	).Scan(&lockedChannelID, &channelType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
			return "", false
		}
		h.log.Error("Failed to lock message channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": authorization.genericMsg})
		return "", false
	}
	return channelType, true
}

func (h *Handler) authorizeMessageChannelTx(
	ctx context.Context, c *gin.Context, tx *sql.Tx, authorization messageChannelAuthorization, channelType string,
) bool {
	perms, err := h.resolver.ResolveChannelPermissionsTx(ctx, tx, authorization.serverID, authorization.userID, authorization.channelID)
	if err != nil {
		if errors.Is(err, rbac.ErrNotMember) {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
			return false
		}
		h.log.Error("Failed to resolve message permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": authorization.genericMsg})
		return false
	}
	hasRequired := perms.Has(authorization.required)
	// Moderators may delete their own messages through ManageAll even when a
	// custom role removed ManageOwn; the preflight has always allowed that.
	if authorization.required == rbac.PermManageOwnMessages {
		hasRequired = hasRequired || perms.Has(rbac.PermManageAllMessages)
	}
	viewPerm, viewable := channelViewPermission(channelType)
	if !viewable || !perms.Has(viewPerm) || !hasRequired {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermsLower})
		return false
	}
	return true
}

func (h *Handler) lockCurrentMessageMembership(ctx context.Context, c *gin.Context, tx *sql.Tx, serverID, userID, genericMsg string) bool {
	var timedOutUntil sql.NullTime
	var timedOut bool
	if err := tx.QueryRowContext(ctx,
		`SELECT timed_out_until, timed_out_until IS NOT NULL AND timed_out_until > clock_timestamp()
		 FROM server_members WHERE server_id = $1 AND user_id = $2 FOR SHARE`, serverID, userID,
	).Scan(&timedOutUntil, &timedOut); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
			return false
		}
		h.log.Error("Failed to lock message membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": genericMsg})
		return false
	}
	if timedOut {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgMemberTimedOut, "code": "member_timed_out", "timed_out_until": timedOutUntil.Time})
		return false
	}
	return true
}

// SendMessage sends a new message to a channel
func (h *Handler) SendMessage(c *gin.Context) {
	userID := c.GetString("user_id")

	var req SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	serverID, membershipIncarnation, serverAllowEmbeds, accessOK := h.checkSendAccess(c, req.ChannelID, userID)
	if !accessOK {
		return
	}

	keyVersion, e2eeOK := h.enforceE2EE(c, req.ChannelID, req.Content, req.KeyVersion)
	if !e2eeOK {
		return
	}

	// Validate reply_to_id if provided (must reference a message in the same channel)
	replyToID, replyOK := h.validateReplyToID(c, req.ReplyToID, req.ChannelID)
	if !replyOK {
		return
	}

	// Normalize empty/whitespace gif_slug to nil so an explicit `"gif_slug": ""`
	// or `"gif_slug": "   "` round-trips to a NULL column ("no GIF attached")
	// instead of being persisted as an empty string. The migration semantics
	// reserve NULL for "no GIF" — we never want a non-NULL empty value.
	gifSlug := klipy.NormalizeSlug(req.GifSlug)
	if !klipy.ValidateSlug(gifSlug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": klipy.SlugValidationError(gifSlug)})
		return
	}

	// Create message — stamp embeds_suppressed based on server policy.
	// The server is the ONLY entity trusted to set this flag to false (allow).
	// If server policy is OFF (default), embeds_suppressed = true.
	embedsSuppressed := !serverAllowEmbeds

	h.persistMessage(c, serverID, membershipIncarnation, models.Message{
		ID:               uuid.New().String(),
		ChannelID:        req.ChannelID,
		UserID:           userID,
		Content:          req.Content,
		KeyVersion:       keyVersion,
		EmbedsSuppressed: embedsSuppressed,
		ReplyToID:        replyToID,
		GifSlug:          gifSlug,
	})
}

// persistMessage inserts a validated message and writes the matching HTTP response.
func (h *Handler) persistMessage(c *gin.Context, serverID string, membershipIncarnation time.Time, message models.Message) {
	insertQuery := `
		WITH timestamps AS (SELECT clock_timestamp() AS created_at)
		INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, reply_to_id, gif_slug, created_at, updated_at, expires_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, timestamps.created_at, timestamps.created_at,
		       CASE WHEN $9::integer IS NULL THEN NULL ELSE timestamps.created_at + make_interval(secs => $9) END
		FROM timestamps
		RETURNING created_at, updated_at, expires_at
	`

	// #2201: an encrypted message write is key-material-coupled state — recheck
	// the sender's credential epoch inside the write transaction so a request
	// admitted before a destructive key reset cannot land ciphertext after it.
	ctx, cancel := context.WithTimeout(c.Request.Context(), messageWriteTimeout)
	defer cancel()
	tx, txErr := h.db.BeginTx(ctx, nil)
	if txErr != nil {
		h.log.Error("Failed to begin message tx", "error", txErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback message tx", "error", rbErr)
		}
	}()
	// Serialize the message write with member removal. The preflight's immutable
	// joined_at value prevents a kicked sender from writing through a same-key rejoin.
	if guardErr := credepoch.GuardTx(ctx, tx, message.UserID, middleware.TokenCredentialEpoch(c)); guardErr != nil {
		h.respondGuardTxError(c, guardErr, errMsgFailedSendMessage)
		return
	}
	authorization := messageChannelAuthorization{
		serverID: serverID, channelID: message.ChannelID, userID: message.UserID,
		required: rbac.PermSendMessages, genericMsg: errMsgFailedSendMessage,
	}
	channelType, locked := h.lockMessageChannelTx(ctx, c, tx, authorization)
	if !locked {
		return
	}
	var windowSeconds sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT expiration_window_seconds FROM channels WHERE id = $1 FOR SHARE`, message.ChannelID,
	).Scan(&windowSeconds); err != nil {
		h.log.Error("Failed to read message expiration policy", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}
	if !h.lockMessageMembership(ctx, c, tx, serverID, message.UserID, membershipIncarnation) {
		return
	}
	if !h.authorizeMessageChannelTx(ctx, c, tx, authorization, channelType) {
		return
	}
	// The initial epoch preflight can race a committed rotation while this
	// transaction waits for the channel parent. Re-read the ledger after that
	// lock, immediately before writing ciphertext.
	if !h.enforceChannelEpoch(c, tx, message.ChannelID, message.KeyVersion) {
		return
	}

	err := tx.QueryRowContext(ctx, insertQuery,
		message.ID, message.ChannelID, message.UserID, message.Content, message.KeyVersion,
		message.EmbedsSuppressed, message.ReplyToID, message.GifSlug, windowSeconds,
	).Scan(&message.CreatedAt, &message.UpdatedAt, &message.ExpiresAt)
	if err != nil {
		if isFKViolation(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Reply target message not found"})
			return
		}
		h.log.Error("Failed to create message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit message tx", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendMessage})
		return
	}
	if h.ops != nil {
		h.ops.Increment(opsmetrics.MetricChannelMessagesTotal)
	}

	h.log.Info("Message sent", "message_id", message.ID, "channel_id", message.ChannelID, "user_id", message.UserID)
	c.JSON(http.StatusCreated, gin.H{"message": message})
}

// authorizeMessageUpdate resolves the message's channel and verifies that the
// caller may edit their own message. On failure it writes the HTTP response.
func (h *Handler) authorizeMessageUpdate(c *gin.Context, messageID, userID string) (string, bool) {
	var authorID, channelID, serverID string
	authorQuery := `
		SELECT m.user_id, m.channel_id, c.server_id
		FROM messages m
		INNER JOIN channels c ON c.id = m.channel_id
		WHERE m.id = $1`

	err := h.db.QueryRow(authorQuery, messageID).Scan(&authorID, &channelID, &serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return "", false
	} else if err != nil {
		h.log.Error("Failed to check message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return "", false
	}

	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only edit your own messages"})
		return "", false
	}

	hasManageOwn, permErr := h.resolver.HasPermission(
		c.Request.Context(), serverID, userID, channelID, rbac.PermManageOwnMessages,
	)
	if permErr != nil {
		h.log.Error("Failed to check PermManageOwnMessages", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
		return "", false
	}
	if !hasManageOwn {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermsLower})
		return "", false
	}

	return channelID, true
}

// updateMessageCiphertext serializes the epoch check and ciphertext update.
// On failure it writes the HTTP response and returns false.
func (h *Handler) updateMessageCiphertext(
	c *gin.Context,
	messageID, _, userID string,
	req UpdateMessageRequest,
) (models.Message, bool) {
	// Serialize the ledger check and edit against every revocation insert.
	// READ COMMITTED gives the check below a fresh snapshot after a lock wait.
	tx, err := h.db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin message update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return models.Message{}, false
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback message update", "error", rbErr)
		}
	}()

	// The shared fence takes the users row (credential guard) before the channel
	// parent, matching membership removal and preventing the user↔channel
	// deadlock cycle.
	lockedChannelID, _, _, authorized := h.lockChannelMessageMutationTx(c, tx, messageID, userID, rbac.PermManageOwnMessages, errMsgFailedUpdateMessage)
	if !authorized {
		return models.Message{}, false
	}
	channelID := lockedChannelID
	var authorID string
	if err = tx.QueryRowContext(c.Request.Context(),
		`SELECT user_id FROM messages WHERE id = $1 AND channel_id = $2 FOR SHARE`, messageID, channelID,
	).Scan(&authorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		} else {
			h.log.Error("Failed to lock message update", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		}
		return models.Message{}, false
	}
	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only edit your own messages"})
		return models.Message{}, false
	}
	if !h.enforceChannelEpoch(c, tx, channelID, req.KeyVersion) {
		return models.Message{}, false
	}

	// Update message
	updateQuery := `
		UPDATE messages
		SET content = $1, key_version = $2, edited_at = NOW(), updated_at = NOW()
		WHERE id = $3 AND channel_id = $4
		  AND NOT EXISTS (
		      SELECT 1 FROM key_revocations
		      WHERE channel_id = $4 AND revoked_epoch = $2
		  )
		RETURNING channel_id, key_version, embeds_suppressed, edited_at, expires_at, created_at, updated_at
	`

	var message models.Message
	message.ID = messageID
	message.UserID = userID
	message.Content = req.Content

	err = tx.QueryRowContext(c.Request.Context(), updateQuery, req.Content, req.KeyVersion, messageID, channelID).Scan(
		&message.ChannelID,
		&message.KeyVersion,
		&message.EmbedsSuppressed,
		&message.EditedAt,
		&message.ExpiresAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		// Keep the conditional ledger guard as defense in depth. Re-read it to
		// return the recovery contract; otherwise the message disappeared
		// concurrently.
		if !h.enforceChannelEpoch(c, tx, channelID, req.KeyVersion) {
			return models.Message{}, false
		}
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return models.Message{}, false
	} else if err != nil {
		h.log.Error("Failed to update message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return models.Message{}, false
	}
	if err = tx.Commit(); err != nil {
		h.log.Error("Failed to commit message update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return models.Message{}, false
	}

	return message, true
}

// UpdateMessage updates a message's content
func (h *Handler) UpdateMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	var req UpdateMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	channelID, authorized := h.authorizeMessageUpdate(c, messageID, userID)
	if !authorized {
		return
	}
	if !isValidCiphertext(req.Content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCiphertext})
		return
	}

	message, updated := h.updateMessageCiphertext(c, messageID, channelID, userID, req)
	if !updated {
		return
	}

	h.log.Info(
		"Message updated",
		"message_id", sanitizeLogValue(messageID),
		"user_id", sanitizeLogValue(userID),
	)

	// Broadcast update to channel subscribers via WebSocket
	channelUUID, err := uuid.Parse(message.ChannelID)
	if err == nil {
		h.hub.BroadcastToChannelAuthorized(channelUUID, websocket.OutgoingMessage{
			Type: "message_update",
			Data: map[string]interface{}{
				"id":                messageID,
				"channel_id":        message.ChannelID,
				"content":           message.Content,
				"key_version":       message.KeyVersion,
				"embeds_suppressed": message.EmbedsSuppressed,
				"edited_at":         message.EditedAt,
				"expires_at":        message.ExpiresAt,
				"updated_at":        message.UpdatedAt,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": message})
}

// DeleteMessage deletes a message.
//
// Sequence (design spec §2.4): validate the id and the optional step-up body;
// preflight the row with the soft-lock's population inputs and the cached
// permission, refusing before anything population-derived (I7); then, for a
// population member only, count the attempt. The delete transaction then
// re-authorizes under #3142's locks (authorizeMessageDeleteTx). Under the
// threshold, or outside the population, that is all it does, and any step-up
// the body carried is ignored. Over it, the budget is charged when a factor
// is present, confirmSoftLockTx confirms as the transaction's first
// statement, and a verified, committed delete resets the counters and clears
// the budget.
func (h *Handler) DeleteMessage(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")
	ctx := c.Request.Context()

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}
	input, bodyErr := stepup.ReadOptionalStepUp(c)
	if bodyErr != nil {
		bodyErr.Write(c)
		return
	}

	preflight, authorized := h.preflightMessageDelete(c, messageID, userID)
	if !authorized {
		return
	}
	if h.purgeEngine == nil {
		h.log.Error(errMsgFailedDeleteMessage, "error", "purge engine unavailable")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
		return
	}

	gate := softLockGate{
		userID:    userID,
		serverID:  preflight.serverID,
		enforcing: preflight.enforcing,
		ownRule:   preflight.authorID == userID && preflight.actorOwnRule,
		purpose:   stepup.PurposeMessageDelete,
		input:     input,
		epoch:     middleware.TokenCredentialEpoch(c),
	}
	verdict, ok := h.chargeMessageDelete(ctx, c, gate)
	if !ok {
		return
	}
	h.runBeforeSoftLockConfirmHook()

	var confirmed, responseWritten bool
	locked := preflight
	err := h.purgeEngine.DeleteOne(ctx, messageID, purge.DeleteSpec{
		MessagesTable:    "messages",
		ScopeColumn:      "channel_id",
		ScopeID:          preflight.channelID,
		AttachmentsTable: "message_attachments",
		Guard: func(ctx context.Context, tx *sql.Tx) (err error) {
			if verdict.Over {
				// First, before #3142's channel and member locks: see
				// confirmSoftLockTx. Overwritten on every attempt, never
				// carried over: DeleteOne retries once on ErrMembershipChanged,
				// which no channel-delete guard returns today. On such a retry a
				// TOTP or backup code re-verifies after the rollback, while a
				// WebAuthn token was already spent, so the retry fails closed as
				// Invalid MFA code.
				if confirmed, err = h.confirmSoftLockTx(ctx, tx, gate); err != nil {
					return err
				}
			}
			if !h.authorizeMessageDeleteTx(c, tx, messageID, userID, &locked) {
				responseWritten = true
				return errMessageDeleteGuardRejected
			}
			return nil
		},
	})
	if responseWritten {
		return
	}
	if err != nil {
		h.respondSoftLockError(c, err, verdict.RetryAfter, errMsgMessageNotFound, errMsgFailedDeleteMessage)
		return
	}
	if confirmed {
		h.resetSoftLock(ctx, gate)
		h.clearSoftLockBudget(ctx, gate)
	}

	h.log.Info("Message deleted", "message_id", messageID, "deleted_by", userID, "author", locked.authorID)

	// Broadcast deletion to channel subscribers via WebSocket
	channelUUID, err := uuid.Parse(locked.channelID)
	if err == nil {
		h.hub.BroadcastToChannelAuthorized(channelUUID, websocket.OutgoingMessage{
			Type: "message_delete",
			Data: map[string]interface{}{
				"id":         messageID,
				"channel_id": locked.channelID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Message deleted successfully"})
}

// authorizeMessageDeleteTx re-authorizes a delete inside its transaction so
// role or member changes cannot turn the cache-backed preflight into a stale
// privileged delete (#3142): the shared mutation fence (credential epoch,
// channel, membership, ManageOwn-or-ManageAll, the message FOR UPDATE), then
// the author, then ManageAll for someone else's message. It records the
// locked placement and author in locked. On a refusal it has written the
// response and returns false.
func (h *Handler) authorizeMessageDeleteTx(c *gin.Context, tx *sql.Tx, messageID, userID string, locked *messageDeletePreflight) bool {
	ctx := c.Request.Context()
	channelID, serverID, channelType, allowed := h.lockChannelMessageMutationTx(c, tx, messageID, userID, rbac.PermManageOwnMessages, errMsgFailedDeleteMessage)
	if !allowed {
		return false
	}
	locked.channelID, locked.serverID = channelID, serverID
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM messages WHERE id = $1 AND channel_id = $2 FOR SHARE`, messageID, channelID).Scan(&locked.authorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		} else {
			h.log.Error("Failed to lock message delete", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
		}
		return false
	}
	return locked.authorID == userID || h.authorizeMessageChannelTx(ctx, c, tx, messageChannelAuthorization{
		serverID: serverID, channelID: channelID, userID: userID,
		required: rbac.PermManageAllMessages, genericMsg: errMsgFailedDeleteMessage,
	}, channelType)
}

// messageDeletePreflight is DeleteMessage's unlocked row read: the message's
// placement plus the two population inputs of the delete-rate soft-lock
// (#3455), read in the same statement so the population costs no round trip
// (design spec §2.2).
type messageDeletePreflight struct {
	authorID  string
	channelID string
	serverID  string
	// enforcing is servers.enforce_mfa_dangerous_actions, read unlocked.
	enforcing bool
	// actorOwnRule is the ACTOR's require_auth_before_purge, a missing row
	// reading TRUE. It governs only when the actor is also the author.
	actorOwnRule bool
}

// deleteTargetQuery joins servers for the enforcement flag and reads the
// actor's own-rule setting. Neither input is consulted before the permission
// check (I7), and neither is ever logged (C7).
const deleteTargetQuery = `
	SELECT m.user_id, m.channel_id, c.server_id, s.enforce_mfa_dangerous_actions,
	       COALESCE((SELECT ps.require_auth_before_purge FROM privacy_settings ps WHERE ps.user_id = $2), TRUE)
	FROM messages m
	INNER JOIN channels c ON m.channel_id = c.id
	INNER JOIN servers s ON s.id = c.server_id
	WHERE m.id = $1`

// preflightMessageDelete reads the message DeleteMessage acts on and checks
// the cached permission. On failure it has written the 404, 403 or 500.
func (h *Handler) preflightMessageDelete(c *gin.Context, messageID, userID string) (messageDeletePreflight, bool) {
	var preflight messageDeletePreflight
	err := h.db.QueryRow(deleteTargetQuery, messageID, userID).Scan(
		&preflight.authorID, &preflight.channelID, &preflight.serverID, &preflight.enforcing, &preflight.actorOwnRule)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return messageDeletePreflight{}, false
	} else if err != nil {
		h.log.Error("Failed to check message permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteMessage})
		return messageDeletePreflight{}, false
	}

	// Author can edit/delete own messages (if they have PermManageOwnMessages).
	// Others need PermManageAllMessages to delete other people's messages or suppress embeds.
	canDelete := false
	if preflight.authorID == userID {
		has, permErr := h.resolver.HasPermission(c.Request.Context(), preflight.serverID, userID, preflight.channelID, rbac.PermManageOwnMessages)
		if permErr != nil {
			h.log.Error("Failed to check PermManageOwnMessages", "error", permErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
			return messageDeletePreflight{}, false
		}
		canDelete = has
	}
	if !canDelete {
		has, permErr := h.resolver.HasPermission(c.Request.Context(), preflight.serverID, userID, preflight.channelID, rbac.PermManageAllMessages)
		if permErr != nil {
			h.log.Error("Failed to check PermManageAllMessages", "error", permErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
			return messageDeletePreflight{}, false
		}
		canDelete = has
	}

	if !canDelete {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermsLower})
		return messageDeletePreflight{}, false
	}
	return preflight, true
}

// SuppressEmbeds suppresses embedded content on a message (one-way ratchet).
// Requires PermManageAllMessages. Can only set embeds_suppressed = true, never false.
// Once suppressed, only the server policy can allow embeds on NEW messages.
func (h *Handler) SuppressEmbeds(c *gin.Context) {
	userID := c.GetString("user_id")
	messageID := c.Param("id")

	// Validate message ID
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidMessageID})
		return
	}

	ctx := c.Request.Context()

	// Resolve the message's scope ONLY. embeds_suppressed is deliberately not
	// read here: branching on it before the permission check told any caller
	// holding a message ID whether that message was suppressed.
	var channelID, serverID string
	err := h.db.QueryRowContext(ctx, `
		SELECT m.channel_id, c.server_id
		FROM messages m
		INNER JOIN channels c ON m.channel_id = c.id
		WHERE m.id = $1
	`, messageID).Scan(&channelID, &serverID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check message for embed suppression", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}

	// Authorization precedes any state-dependent branch. A non-member resolves
	// to (false, nil) and lands here with a 403 like any unprivileged member.
	hasPerm, permErr := h.resolver.HasPermission(ctx, serverID, userID, channelID, rbac.PermManageAllMessages)
	if permErr != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	tx, err := h.db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin embed suppression", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("Failed to rollback embed suppression", "error", rbErr)
		}
	}()
	lockedChannelID, _, _, authorized := h.lockChannelMessageMutationTx(c, tx, messageID, userID, rbac.PermManageAllMessages, errMsgFailedSuppressEmbeds)
	if !authorized {
		return
	}
	channelID = lockedChannelID
	// One-way ratchet: suppress only (false → true). The row count, not a
	// prior read, decides whether this request changed anything.
	res, err := tx.ExecContext(
		c.Request.Context(),
		`UPDATE messages SET embeds_suppressed = TRUE, updated_at = NOW() WHERE id = $1 AND embeds_suppressed = FALSE`,
		messageID,
	)
	if err != nil {
		h.log.Error("Failed to suppress embeds", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read suppress-embeds row count", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}
	if err = tx.Commit(); err != nil {
		h.log.Error("Failed to commit embed suppression", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSuppressEmbeds})
		return
	}
	if n == 0 {
		// Already suppressed by an authorized caller: success, and no second
		// broadcast for a change that did not happen.
		c.JSON(http.StatusOK, gin.H{"message": "Embeds already suppressed"})
		return
	}

	h.log.Info("Embeds suppressed", "message_id", messageID, "suppressed_by", userID)

	// Broadcast update to channel subscribers so clients hide the embeds
	channelUUID, parseErr := uuid.Parse(channelID)
	if parseErr == nil {
		h.hub.BroadcastToChannelAuthorized(channelUUID, websocket.OutgoingMessage{
			Type: "message_update",
			Data: map[string]interface{}{
				"id":                messageID,
				"channel_id":        channelID,
				"embeds_suppressed": true,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Embeds suppressed successfully"})
}
