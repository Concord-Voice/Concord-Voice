// Package voice provides handlers for voice channel state management.
// It exposes REST endpoints for voice join authorization and participant listing,
// and processes NATS events from the media plane to keep the DB and WS hub in sync.
package voice

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	voiceAdmissionActivationSubject         = "voice.admission.activate"
	voiceAdmissionActivationVersion         = "v1"
	enforcementSnapshotSchemaVersion        = 1
	voiceAdmissionActivationRequestPurpose  = "concord/voice-admission-activate/request/v1"
	voiceAdmissionActivationResponsePurpose = "concord/voice-admission-activate/response/v1"
	voiceAdmissionActivationNotReadyPurpose = "concord/voice-admission-activate/response/not-ready/v1"
	errMsgInvalidVoiceJoinRequestBody       = "Invalid request body"
	errMsgVoiceChannelAccessDenied          = "Channel not found or access denied"
)

type voiceAdmissionActivationAcknowledgement uint8

const (
	voiceAdmissionActivationRejected voiceAdmissionActivationAcknowledgement = iota
	voiceAdmissionActivationNotReady
	voiceAdmissionActivationAccepted
)

type voiceAdmissionActivationExpectation struct {
	channelID, userID, admissionID, socketID, revision, nonce string
}

type voiceAdmissionActivationResponse struct {
	Version, Timestamp, ChannelID, UserID, AdmissionID, SocketID, Revision, Nonce, Result, Proof string
}

type voiceJoinAdmission struct {
	AdmissionID string `json:"admission_id"`
	SocketID    string `json:"socket_id"`
	Activate    bool   `json:"activate"`
}

func verifyVoiceAdmissionActivationResponse(
	responseRaw []byte,
	secret string,
	expected voiceAdmissionActivationExpectation,
) voiceAdmissionActivationAcknowledgement {
	var response voiceAdmissionActivationResponse
	if err := json.Unmarshal(responseRaw, &response); err != nil ||
		response.Version != voiceAdmissionActivationVersion ||
		response.ChannelID != expected.channelID || response.UserID != expected.userID ||
		response.AdmissionID != expected.admissionID || response.SocketID != expected.socketID ||
		response.Revision != expected.revision || response.Nonce != expected.nonce {
		return voiceAdmissionActivationRejected
	}
	fields := []string{"activate", expected.channelID, expected.userID, expected.admissionID, expected.socketID, expected.revision, expected.nonce, response.Result}
	switch response.Result {
	case "not_ready":
		if mediaproof.Verify(mediaproof.DeriveKey(secret, voiceAdmissionActivationNotReadyPurpose), response.Proof, voiceAdmissionActivationVersion, response.Timestamp, fields...) {
			return voiceAdmissionActivationNotReady
		}
	case "ok":
		if mediaproof.Verify(mediaproof.DeriveKey(secret, voiceAdmissionActivationResponsePurpose), response.Proof, voiceAdmissionActivationVersion, response.Timestamp, fields...) {
			return voiceAdmissionActivationAccepted
		}
	}
	return voiceAdmissionActivationRejected
}

func parseVoiceJoinAdmission(c *gin.Context, channelID string) (voiceJoinAdmission, bool) {
	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return voiceJoinAdmission{}, false
	}
	if !middleware.IsMediaPlaneServiceHop(c) {
		return voiceJoinAdmission{}, true
	}

	var admission voiceJoinAdmission
	if err := c.ShouldBindJSON(&admission); err != nil || admission.SocketID == "" || len(admission.SocketID) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidVoiceJoinRequestBody})
		return voiceJoinAdmission{}, false
	}
	parsedAdmissionID, err := uuid.Parse(admission.AdmissionID)
	if err != nil || parsedAdmissionID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidVoiceJoinRequestBody})
		return voiceJoinAdmission{}, false
	}
	return admission, true
}

func (h *Handler) preflightVoiceJoinServer(c *gin.Context, channelID string) (string, bool) {
	var serverID string
	err := h.db.QueryRowContext(c.Request.Context(), `SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgVoiceChannelAccessDenied})
		return "", false
	}
	if err != nil {
		h.log.Error("Failed to preflight voice channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return "", false
	}
	return serverID, true
}

func (h *Handler) prepareMediaVoiceAdmission(
	c *gin.Context,
	tx *sql.Tx,
	channelID, userID string,
	admission voiceJoinAdmission,
) (int64, bool) {
	var authorizationRevision int64
	if err := tx.QueryRowContext(c.Request.Context(), `SELECT nextval('voice_authorization_revision_seq')`).Scan(&authorizationRevision); err != nil {
		h.log.Error("Failed to allocate voice authorization revision", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return 0, false
	}
	if !admission.Activate {
		if _, err := tx.ExecContext(c.Request.Context(), `
			INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
			VALUES ($1, $2, $3, $4, clock_timestamp() + INTERVAL '30 seconds')
			ON CONFLICT (channel_id, user_id) DO UPDATE SET admission_id = EXCLUDED.admission_id, socket_id = EXCLUDED.socket_id, expires_at = EXCLUDED.expires_at
		`, channelID, userID, admission.AdmissionID, admission.SocketID); err != nil {
			h.log.Error("Failed to reserve pending voice admission", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
			return 0, false
		}
		return authorizationRevision, true
	}

	var matchedAdmissionID uuid.UUID
	if err := tx.QueryRowContext(c.Request.Context(), `SELECT admission_id FROM voice_pending_admissions WHERE channel_id=$1 AND user_id=$2 AND admission_id=$3 AND socket_id=$4 AND expires_at > clock_timestamp() FOR UPDATE`, channelID, userID, admission.AdmissionID, admission.SocketID).Scan(&matchedAdmissionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
			return 0, false
		}
		h.log.Error("Failed to validate pending voice admission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return 0, false
	}
	if !h.renewPendingVoiceAdmission(c, tx, channelID, userID, admission) {
		return 0, false
	}
	return authorizationRevision, true
}

func (h *Handler) authorizeMediaVoiceAdmission(
	c *gin.Context,
	tx *sql.Tx,
	channelID, userID string,
	admission voiceJoinAdmission,
) (int64, bool) {
	authorizationRevision, ok := h.prepareMediaVoiceAdmission(c, tx, channelID, userID, admission)
	if !ok || !admission.Activate {
		return authorizationRevision, ok
	}
	if !h.activateMediaVoiceAdmission(c, channelID, userID, admission, authorizationRevision) {
		return 0, false
	}
	return authorizationRevision, true
}

func (h *Handler) activateMediaVoiceAdmission(
	c *gin.Context,
	channelID, userID string,
	admission voiceJoinAdmission,
	authorizationRevision int64,
) bool {
	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		h.log.Error("Failed to generate voice admission nonce", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	revision := strconv.FormatInt(authorizationRevision, 10)
	nonce := hex.EncodeToString(nonceBytes[:])
	fields := []string{"activate", channelID, userID, admission.AdmissionID, admission.SocketID, revision, nonce}
	requestKey := mediaproof.DeriveKey(h.cfg.JWTSecret, voiceAdmissionActivationRequestPurpose)
	proof := mediaproof.Sign(requestKey, voiceAdmissionActivationVersion, timestamp, fields...)
	request := gin.H{"version": voiceAdmissionActivationVersion, "timestamp": timestamp, "channelId": channelID, "userId": userID, "admissionId": admission.AdmissionID, "socketId": admission.SocketID, "revision": revision, "nonce": nonce, "proof": proof}
	if h.nats == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	requestCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	responseRaw, requestErr := h.nats.RequestWithContext(requestCtx, voiceAdmissionActivationSubject, request)
	cancel()
	if requestErr != nil {
		h.log.Error("Voice admission activation failed", "error", requestErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	expectedResponse := voiceAdmissionActivationExpectation{channelID, userID, admission.AdmissionID, admission.SocketID, revision, nonce}
	acknowledgement := verifyVoiceAdmissionActivationResponse(responseRaw, h.cfg.JWTSecret, expectedResponse)
	if acknowledgement == voiceAdmissionActivationNotReady {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return false
	}
	if acknowledgement != voiceAdmissionActivationAccepted {
		h.log.Error("Voice admission activation acknowledgement rejected")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	return true
}

func (h *Handler) renewPendingVoiceAdmission(
	c *gin.Context,
	tx *sql.Tx,
	channelID, userID string,
	admission voiceJoinAdmission,
) bool {
	result, err := tx.ExecContext(c.Request.Context(), `
		UPDATE voice_pending_admissions
		SET expires_at = clock_timestamp() + INTERVAL '30 seconds'
		WHERE channel_id = $1 AND user_id = $2 AND admission_id = $3 AND socket_id = $4
	`, channelID, userID, admission.AdmissionID, admission.SocketID)
	if err != nil {
		h.log.Error("Failed to renew pending voice admission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil || rowsAffected != 1 {
		if err != nil {
			h.log.Error("Failed to read pending voice admission renewal result", "error", err)
		} else {
			h.log.Error("Unexpected pending voice admission renewal result", "rows_affected", rowsAffected)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	return true
}

func (h *Handler) maybeAuthorizeMediaVoiceAdmission(
	c *gin.Context,
	tx *sql.Tx,
	isMediaHop bool,
	channelID, userID string,
	admission voiceJoinAdmission,
) (int64, bool) {
	if !isMediaHop {
		return 0, true
	}
	return h.authorizeMediaVoiceAdmission(c, tx, channelID, userID, admission)
}

func resolveVoiceJoinMediaEntitlements(
	userTier, serverTier string,
	audioQualityTier *string,
) (entitlements.MediaEntitlements, string) {
	channelTier := ""
	if audioQualityTier != nil {
		channelTier = *audioQualityTier
	}
	return entitlements.MediaForChannel(userTier, serverTier, channelTier), entitlements.RoomCapTierForServer(serverTier)
}

func addVoiceAdmissionRevision(response gin.H, isMediaHop bool, authorizationRevision int64) {
	if isMediaHop {
		response["authorization_revision"] = strconv.FormatInt(authorizationRevision, 10)
	}
}

type voiceJoinAuthorization struct {
	channelID        string
	channelName      string
	serverID         string
	audioQualityTier *string
	permissions      rbac.Permission
	serverMuted      bool
	serverDeafened   bool
	username         string
	displayName      sql.NullString
	avatarURL        sql.NullString
}

// recheckActivatedVoiceJoin repeats every authority read after the out-of-tx
// PREPARE acknowledgement. A prepared candidate is not admitted until this
// transaction commits with the exact reservation still intact.
func (h *Handler) recheckActivatedVoiceJoin(c *gin.Context, tx *sql.Tx, preflightServerID, channelID, userID string) (voiceJoinAuthorization, bool) {
	if err := rbac.LockServerVisibilityCapture(c.Request.Context(), tx, preflightServerID); err != nil {
		h.log.Error("voice join: lock visibility", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinAuthorization{}, false
	}
	if err := credepoch.GuardTx(c.Request.Context(), tx, userID, middleware.TokenCredentialEpoch(c)); err != nil {
		h.respondVoiceGuardTxError(c, err, errMsgFailedAuthorize)
		return voiceJoinAuthorization{}, false
	}
	var lockedServerID string
	if err := tx.QueryRowContext(c.Request.Context(), `SELECT id FROM servers WHERE id = $1 FOR UPDATE`, preflightServerID).Scan(&lockedServerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgVoiceChannelAccessDenied})
		} else {
			h.log.Error("Failed to lock voice join server", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		}
		return voiceJoinAuthorization{}, false
	}
	var result voiceJoinAuthorization
	var channelType string
	var timedOutUntil sql.NullTime
	err := tx.QueryRowContext(c.Request.Context(), `
		SELECT c.id, c.name, c.type, c.server_id, c.audio_quality_tier, sm.timed_out_until
		FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1 AND c.server_id = $3
		FOR UPDATE OF c, sm
	`, channelID, userID, lockedServerID).Scan(&result.channelID, &result.channelName, &channelType, &result.serverID, &result.audioQualityTier, &timedOutUntil)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgVoiceChannelAccessDenied})
		return voiceJoinAuthorization{}, false
	}
	if err != nil {
		h.log.Error("Failed to fetch channel for voice join", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinAuthorization{}, false
	}
	if channelType != "voice" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Not a voice channel"})
		return voiceJoinAuthorization{}, false
	}
	result.permissions, err = h.resolver.ResolveChannelPermissionsTx(c.Request.Context(), tx, result.serverID, userID, result.channelID)
	if err != nil {
		h.log.Error("Failed to resolve effective voice permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinAuthorization{}, false
	}
	if !result.permissions.Has(rbac.PermViewVoiceChannels) || !result.permissions.Has(rbac.PermJoinVoice) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return voiceJoinAuthorization{}, false
	}
	if timedOutUntil.Valid && timedOutUntil.Time.After(time.Now().UTC()) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Member is timed out", "code": "member_timed_out", "timed_out_until": timedOutUntil.Time})
		return voiceJoinAuthorization{}, false
	}
	if err := tx.QueryRowContext(c.Request.Context(), `
		SELECT sm.server_muted, sm.server_deafened, u.username, u.display_name, u.avatar_url
		FROM server_members sm JOIN users u ON u.id = sm.user_id
		WHERE sm.server_id = $1 AND sm.user_id = $2
	`, result.serverID, userID).Scan(&result.serverMuted, &result.serverDeafened, &result.username, &result.displayName, &result.avatarURL); err != nil {
		h.log.Error("Failed to query server enforcement flags", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinAuthorization{}, false
	}
	return result, true
}

func (h *Handler) completeActivatedVoiceAdmission(
	c *gin.Context,
	tx *sql.Tx,
	preflightServerID, channelID, userID string,
	admission voiceJoinAdmission,
) (voiceJoinAuthorization, int64, bool) {
	provisionalRevision, authorized := h.prepareMediaVoiceAdmission(c, tx, channelID, userID, admission)
	if !authorized {
		return voiceJoinAuthorization{}, 0, false
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("voice join: commit pre-activation transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinAuthorization{}, 0, false
	}
	if !h.activateMediaVoiceAdmission(c, channelID, userID, admission, provisionalRevision) {
		return voiceJoinAuthorization{}, 0, false
	}
	if h.afterVoiceAdmissionPrepareForTest != nil {
		h.afterVoiceAdmissionPrepareForTest()
	}

	finalTx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error("voice join: begin final credential transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinAuthorization{}, 0, false
	}
	defer func() {
		if rbErr := finalTx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("voice join: final credential transaction rollback", "error", rbErr)
		}
	}()
	final, authorized := h.recheckActivatedVoiceJoin(c, finalTx, preflightServerID, channelID, userID)
	if !authorized {
		return voiceJoinAuthorization{}, 0, false
	}
	authorizationRevision, authorized := h.prepareMediaVoiceAdmission(c, finalTx, final.channelID, userID, admission)
	if !authorized {
		return voiceJoinAuthorization{}, 0, false
	}
	if err := finalTx.Commit(); err != nil {
		h.log.Error("voice join: commit final credential transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinAuthorization{}, 0, false
	}
	return final, authorizationRevision, true
}

// Handler handles voice-related requests.
type Handler struct {
	db                                *sql.DB
	log                               *logger.Logger
	hub                               *websocket.Hub
	cfg                               *config.Config
	resolver                          *rbac.Resolver
	nats                              *natsclient.Client
	audit                             *rbac.AuditWriter
	entCache                          *entitlements.Cache
	serverTiers                       entitlements.ServerTierResolver
	tempGrant                         *tempGrantManager
	afterVoiceAdmissionPrepareForTest func()
	commitVoiceEffectTxForTest        func(*sql.Tx) error
	beforeTemporaryGrantForTest       func()
	beforeVoiceEffectForTest          func()
}

// HandlerDeps groups the dependencies required to construct a Handler.
//
// Audit may be nil (the move audit entry is then skipped — used by lightweight
// test constructions); production wiring passes the shared rbac.AuditWriter so
// hierarchy-crossing moves are recorded (#487 §6.1). EntCache is the shared
// entitlement-tier cache (#1296) used to resolve the joining user's media
// entitlements (#1300); production wiring passes the same instance the auth
// handler receives.
type HandlerDeps struct {
	DB          *sql.DB
	Log         *logger.Logger
	Hub         *websocket.Hub
	Cfg         *config.Config
	Resolver    *rbac.Resolver
	NATS        *natsclient.Client
	Audit       *rbac.AuditWriter
	EntCache    *entitlements.Cache
	ServerTiers entitlements.ServerTierResolver
}

// NewHandler creates a new voice handler.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		db:          deps.DB,
		log:         deps.Log,
		hub:         deps.Hub,
		cfg:         deps.Cfg,
		resolver:    deps.Resolver,
		nats:        deps.NATS,
		audit:       deps.Audit,
		entCache:    deps.EntCache,
		serverTiers: deps.ServerTiers,
		tempGrant:   newTempGrantManager(deps.DB, deps.Log, deps.Hub, deps.Resolver, deps.NATS),
	}
}

// SetPresenceRecheck forwards the #2445 Rich Presence capture to this handler's
// tempGrantManager, so a REST-triggered temporary-SBAC revoke captures its
// pre-mutation Server Voice audience under the same per-server advisory lock the
// RBAC authority writes use. newTempGrantManager is called at three sites and
// each owns an independent manager, so every owner must forward the executor or
// its revoke path silently keeps the pre-#2445 no-capture behavior.
func (h *Handler) SetPresenceRecheck(p rbac.PresenceRecheck) {
	if h.tempGrant == nil {
		return
	}
	h.tempGrant.SetPresenceRecheck(p)
}

func (h *Handler) serverTier(ctx context.Context, serverID string) string {
	if h.serverTiers != nil {
		return h.serverTiers.GetServerTier(ctx, serverID)
	}
	return entitlements.ResolveServerTier(ctx, h.db, serverID)
}

func (h *Handler) respondVoiceGuardTxError(c *gin.Context, guardErr error, genericMsg string) {
	if errors.Is(guardErr, credepoch.ErrEpochMismatch) || errors.Is(guardErr, credepoch.ErrBlocked) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	h.log.Error("credential-epoch guard read failed", "error", guardErr)
	c.JSON(http.StatusInternalServerError, gin.H{"error": genericMsg})
}

// Participant represents a user currently in a voice channel.
type Participant struct {
	UserID          string `json:"user_id"`
	Username        string `json:"username"`
	DisplayName     string `json:"display_name,omitempty"`
	AvatarURL       string `json:"avatar_url,omitempty"`
	IsMuted         bool   `json:"is_muted"`
	IsDeafened      bool   `json:"is_deafened"`
	IsVideoOn       bool   `json:"is_video_on"`
	IsScreenSharing bool   `json:"is_screen_sharing"`
	JoinedAt        string `json:"joined_at"`
	ServerMuted     bool   `json:"server_muted"`
	ServerDeafened  bool   `json:"server_deafened"`
}

// GetParticipants returns all users currently in a voice channel.
// GET /channels/:id/voice/participants
func (h *Handler) GetParticipants(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	// Verify the user has access to this channel's server
	var serverID string
	err := h.db.QueryRow(`
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	} else if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}

	// CV-CAN-008: listing voice participants requires ViewVoice — otherwise any
	// server member could enumerate hidden voice room occupancy (usernames,
	// mute/deafen/video/screen-share state) by channel UUID. Server membership
	// alone is insufficient; the WS subscribe path already enforces ViewVoice.
	effectivePerms, permErr := h.resolver.ResolveEffectivePermissionsUncached(c.Request.Context(), serverID, userID, channelID)
	if permErr != nil {
		h.log.Error("Failed to check voice view permission", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}
	if !effectivePerms.Has(rbac.PermViewVoiceChannels) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	}

	// Fetch voice participants with user details and server enforcement flags
	rows, err := h.db.Query(`
		SELECT vp.user_id, u.username, COALESCE(u.display_name, ''), COALESCE(u.avatar_url, ''),
		       vp.is_muted, vp.is_deafened, vp.is_video_on, vp.is_screen_sharing, vp.joined_at,
		       sm.server_muted, sm.server_deafened
		FROM voice_participants vp
		INNER JOIN users u ON u.id = vp.user_id
		INNER JOIN channels c ON c.id = vp.channel_id
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = vp.user_id
		WHERE vp.channel_id = $1
		ORDER BY vp.joined_at ASC
	`, channelID)
	if err != nil {
		h.log.Error("Failed to query voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}
	defer func() { _ = rows.Close() }()

	participants := []Participant{}
	for rows.Next() {
		var p Participant
		if err := rows.Scan(
			&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL,
			&p.IsMuted, &p.IsDeafened, &p.IsVideoOn, &p.IsScreenSharing, &p.JoinedAt,
			&p.ServerMuted, &p.ServerDeafened,
		); err != nil {
			h.log.Error("Failed to scan voice participant", "error", err)
			continue
		}
		participants = append(participants, p)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFetchParticipants})
		return
	}

	c.JSON(http.StatusOK, gin.H{"participants": participants})
}

// AuthorizeJoin checks that a user can join a voice channel and returns
// media plane connection details.
// POST /channels/:id/voice/join
func (h *Handler) AuthorizeJoin(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")
	isMediaHop := middleware.IsMediaPlaneServiceHop(c)
	admission, ok := parseVoiceJoinAdmission(c, channelID)
	if !ok {
		return
	}
	// The visibility advisory lock needs a stable key before the transaction;
	// revalidation under that lock below remains authoritative.
	preflightServerID, ok := h.preflightVoiceJoinServer(c, channelID)
	if !ok {
		return
	}
	// Both resolvers may read through to Postgres. Do that before acquiring the
	// credential and visibility fences so a cold cache cannot lease a second
	// pool connection while the authorization transaction owns the first.
	userTier := entitlements.TierFree
	if h.entCache != nil {
		userTier = h.entCache.GetTier(c.Request.Context(), userID)
	}
	serverTier := h.serverTier(c.Request.Context(), preflightServerID)

	// The credential guard owns the users row until the authoritative channel,
	// membership, and permission decision has been made. Committing it before
	// those reads would let a concurrent credential reset linearize between the
	// guard and the successful media-admission response.
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error("voice join: begin credential transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("voice join: credential transaction rollback", "error", rbErr)
		}
	}()
	authorization, authorized := h.recheckActivatedVoiceJoin(c, tx, preflightServerID, channelID, userID)
	if !authorized {
		return
	}
	channelID = authorization.channelID

	// Resolve the server's Mach tier once and reuse it for both the media
	// entitlement and the room-cap tier below: same serverID and request
	// context, so the result is identical, and once #1556 makes serverTier a
	// real (Redis read-through) resolution this avoids a redundant round-trip
	// per channel join.
	// Room-scoped producer caps (#1542) follow the SERVER's Mach tier — the server
	// SUBSCRIPTION provisions the channel's egress capacity, so neither a premium
	// member nor a premium owner on a free server may raise a (large/public)
	// channel's cap (ADR-0029 amendment 2026-07-10, superseding the former
	// server-owner personal-tier resolution). serverTier is the #1556 seam
	// (Groundspeed today; real Mach when server subscriptions ship) and
	// RoomCapTierForServer collapses the ladder to the media-plane's binary
	// free/premium wire, fail-closed to free. The field name stays room_owner_tier
	// for wire stability. DMs do NOT carry this field (see dm/handlers.go) — there
	// the media-plane derives the cap from the max present-participant tier.
	mediaEnt, roomOwnerTier := resolveVoiceJoinMediaEntitlements(userTier, serverTier, authorization.audioQualityTier)
	var authorizationRevision int64
	if isMediaHop && admission.Activate {
		var completed bool
		authorization, authorizationRevision, completed = h.completeActivatedVoiceAdmission(c, tx, preflightServerID, channelID, userID, admission)
		if !completed {
			return
		}
		channelID = authorization.channelID
		mediaEnt, roomOwnerTier = resolveVoiceJoinMediaEntitlements(userTier, serverTier, authorization.audioQualityTier)
	} else {
		var authorized bool
		authorizationRevision, authorized = h.maybeAuthorizeMediaVoiceAdmission(c, tx, isMediaHop, channelID, userID, admission)
		if !authorized {
			return
		}
		if err := tx.Commit(); err != nil {
			h.log.Error("voice join: commit credential transaction", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
			return
		}
	}

	h.log.Info("Voice join authorized", "user_id", sanitizeLogValue(userID), "channel_id", sanitizeLogValue(channelID), "server_id", sanitizeLogValue(authorization.serverID), "media_tier", mediaEnt.Tier)

	response := gin.H{
		"allowed":            true,
		"media_server_url":   h.cfg.MediaPlaneURL,
		"ice_servers":        h.cfg.ICEServers(userID),
		"permissions":        strconv.FormatInt(int64(authorization.permissions), 10),
		"server_muted":       authorization.serverMuted,
		"server_deafened":    authorization.serverDeafened,
		"media_entitlements": mediaEnt,
		"room_owner_tier":    roomOwnerTier,
		// CV-CAN-017: server-authoritative display identity, resolved from the
		// authenticated user_id. The media-plane uses these in place of the
		// client-supplied handshake values so a member cannot spoof its display
		// identity to peers. display_name/avatar_url are empty strings when unset.
		"username":     authorization.username,
		"display_name": authorization.displayName.String,
		"avatar_url":   authorization.avatarURL.String,
		"channel": gin.H{
			"id":                 channelID,
			"name":               authorization.channelName,
			"server_id":          authorization.serverID,
			"audio_quality_tier": authorization.audioQualityTier,
		},
	}
	addVoiceAdmissionRevision(response, isMediaHop, authorizationRevision)
	c.JSON(http.StatusOK, response)
}

// AuthorizeVoiceAction checks whether a user has permission to perform a voice
// moderation action (mute, deafen, move) on a target user. The media plane
// should call this endpoint before executing moderation commands.
// POST /channels/:id/voice/authorize-action
func (h *Handler) AuthorizeVoiceAction(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")

	if _, err := uuid.Parse(channelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelID})
		return
	}

	var req struct {
		Action       string `json:"action" binding:"required,oneof=mute deafen move"`
		TargetUserID string `json:"target_user_id" binding:"required,uuid"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Keep the users-row epoch fence through every DB-backed authorization read
	// and the emitted authority response. A reset that acquires the users row
	// first fails this request; otherwise it necessarily commits after it.
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error("voice action: begin credential transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("voice action: credential transaction rollback", "error", rbErr)
		}
	}()
	if err := credepoch.GuardTx(c.Request.Context(), tx, userID, middleware.TokenCredentialEpoch(c)); err != nil {
		h.respondVoiceGuardTxError(c, err, errMsgFailedAuthAction)
		return
	}

	// Get server ID from channel
	var serverID string
	err = tx.QueryRowContext(c.Request.Context(), `
		SELECT c.server_id FROM channels c
		INNER JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $2
		WHERE c.id = $1
	`, channelID, userID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return
	}
	if err != nil {
		h.log.Error("Failed to check channel access", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}

	// Map action to required permission
	var perm rbac.Permission
	switch req.Action {
	case "mute":
		perm = rbac.PermMuteMembers
	case "deafen":
		perm = rbac.PermDeafenMembers
	case "move":
		perm = rbac.PermMoveMembers
	}

	// Check permission
	effectivePerms, permErr := h.resolver.ResolveChannelPermissionsTx(c.Request.Context(), tx, serverID, userID, channelID)
	if permErr != nil {
		h.log.Error("Failed to check voice moderation permission", "error", permErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}
	if !effectivePerms.Has(perm) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}

	// Verify target user is a member of this server
	var targetIsMember bool
	if err := tx.QueryRowContext(c.Request.Context(), `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, req.TargetUserID).Scan(&targetIsMember); err != nil {
		h.log.Error("Failed to check target membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}
	if !targetIsMember {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotMember})
		return
	}

	// Hierarchy check: cannot moderate members with equal or higher role
	if h.resolver.CheckHierarchyTx(c.Request.Context(), tx, serverID, userID, req.TargetUserID) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("voice action: commit credential transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthAction})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"allowed":    true,
		"action":     req.Action,
		"target_id":  req.TargetUserID,
		"channel_id": channelID,
	})
}

// --- Server-enforced voice moderation ---

const (
	errMsgInvalidChannelID       = "Invalid channel ID"
	errMsgInvalidServerID        = "Invalid server ID"
	errMsgInvalidUserID          = "Invalid user ID"
	errMsgNotMember              = "Not a member of this server"
	errMsgFetchParticipants      = "Failed to fetch participants"
	errMsgFailedAuthorize        = "Failed to authorize"
	errMsgFailedRevokeTempAccess = "Failed to revoke temporary access"
	errMsgFailedAuthAction       = "Failed to authorize action"
	errMsgInsufficientPerms      = "Insufficient permissions"
	errMsgFailedCheckPerms       = "Failed to check permissions"
	errMsgFailedCheckMember      = "Failed to check membership"
	errMsgFailedUnmuteMember     = "Failed to unmute member"
	errMsgFailedDisconnectMember = "Failed to disconnect member"
	errMsgTargetNotMember        = "Target user is not a member of this server"
	errMsgHierarchyViolation     = "Cannot moderate a member with equal or higher role position"
	errMsgTargetNotInVoice       = "Target user is not in a voice channel"
	errMsgCannotTargetSelf       = "Cannot target yourself"
)

// voiceModContext holds the validated state from authorizeVoiceMod.
type voiceModContext struct {
	serverID string
	targetID string
}

var (
	errVoiceModActorNotMember = errors.New("voice moderation actor is not a member")
	errVoiceModPermission     = errors.New("voice moderation permission denied")
	errVoiceModHierarchy      = errors.New("voice moderation hierarchy denied")
)

// revalidateVoiceModAuthorityTx repeats the RBAC decision inside the effect
// transaction. Credential epochs do not change for role or membership changes,
// so the preflight check alone cannot authorize a later mutation.
func revalidateVoiceModAuthorityTx(
	ctx context.Context, tx *sql.Tx, resolver *rbac.Resolver,
	mod *voiceModContext, actorID string, perm rbac.Permission, requireHierarchy bool,
) error {
	member, err := serverMemberExistsTx(ctx, tx, mod.serverID, actorID)
	if err != nil {
		return fmt.Errorf("revalidate voice moderation membership: %w", err)
	}
	if !member {
		return errVoiceModActorNotMember
	}
	perms, err := resolver.ResolveServerPermissionsTx(ctx, tx, mod.serverID, actorID)
	if err != nil {
		if errors.Is(err, rbac.ErrNotMember) {
			return errVoiceModActorNotMember
		}
		return fmt.Errorf("revalidate voice moderation permission: %w", err)
	}
	if !perms.Has(perm) {
		return errVoiceModPermission
	}
	if requireHierarchy && resolver.CheckHierarchyTx(ctx, tx, mod.serverID, actorID, mod.targetID) != nil {
		return errVoiceModHierarchy
	}
	return nil
}

func (h *Handler) respondVoiceModAuthorityTxError(c *gin.Context, err error, genericMsg string) {
	switch {
	case errors.Is(err, errVoiceModActorNotMember):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
	case errors.Is(err, errVoiceModPermission):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
	case errors.Is(err, errVoiceModHierarchy):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
	default:
		h.log.Error(genericMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": genericMsg})
	}
}

// guardVoiceModEffectTx establishes the server authority lock before the users
// lock, then verifies the actor against the same committed RBAC state as the
// mutation or external effect that follows.
func (h *Handler) guardVoiceModEffectTx(
	c *gin.Context, tx *sql.Tx, mod *voiceModContext, perm rbac.Permission, requireHierarchy bool, failMsg string,
) bool {
	if err := rbac.LockServerVisibilityCapture(c.Request.Context(), tx, mod.serverID); err != nil {
		h.log.Error(failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failMsg})
		return false
	}
	actorID := c.GetString("user_id")
	if err := credepoch.GuardTx(c.Request.Context(), tx, actorID, middleware.TokenCredentialEpoch(c)); err != nil {
		h.respondVoiceGuardTxError(c, err, failMsg)
		return false
	}
	if err := revalidateVoiceModAuthorityTx(c.Request.Context(), tx, h.resolver, mod, actorID, perm, requireHierarchy); err != nil {
		h.respondVoiceModAuthorityTxError(c, err, failMsg)
		return false
	}
	return true
}

// authorizeVoiceMod validates params, membership, permission, and hierarchy.
// ServerMove owns the sole ADR-0023 hierarchy exception and deliberately does
// not call this helper.
// Returns nil and sends the HTTP error response if any check fails.
func (h *Handler) authorizeVoiceMod(c *gin.Context, perm rbac.Permission) *voiceModContext {
	actorID := c.GetString("user_id")
	serverID := c.Param("id")
	targetID := c.Param("userId")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return nil
	}
	if _, err := uuid.Parse(targetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return nil
	}

	// Always verify actor is a member of the server
	if !h.checkMembership(c, serverID, actorID) {
		return nil
	}

	// Check permission
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, actorID, "", perm)
	if err != nil {
		h.log.Error(errMsgFailedCheckPerms, "error", err, "permission", perm)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckPerms})
		return nil
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return nil
	}

	if h.resolver.CheckHierarchy(c.Request.Context(), serverID, actorID, targetID) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgHierarchyViolation})
		return nil
	}

	return &voiceModContext{serverID: serverID, targetID: targetID}
}

// checkMembership verifies that userID is a member of serverID.
// Returns false and sends the HTTP error response if the check fails.
func (h *Handler) checkMembership(c *gin.Context, serverID, userID string) bool {
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID).Scan(&exists); err != nil {
		h.log.Error(errMsgFailedCheckMember, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckMember})
		return false
	}
	if !exists {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotMember})
		return false
	}
	return true
}

func findVoiceChannelTx(ctx context.Context, tx *sql.Tx, serverID, targetID string) (string, error) {
	var channelID string
	err := tx.QueryRowContext(ctx, `
		SELECT vp.channel_id FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE c.server_id = $1 AND vp.user_id = $2
	`, serverID, targetID).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return channelID, nil
}

// publishEnforcement publishes a NATS enforcement message if the target is in voice.
func (h *Handler) publishEnforcement(subject, channelID, targetID, action string) {
	if channelID == "" {
		return
	}
	if h.beforeVoiceEffectForTest != nil {
		h.beforeVoiceEffectForTest()
	}
	if h.nats == nil {
		return
	}
	if pubErr := h.nats.Publish(subject, map[string]interface{}{
		"channelId": channelID, "userId": targetID, "action": action,
	}); pubErr != nil {
		h.log.Error("Failed to publish NATS enforcement", "error", pubErr, "subject", subject, "action", action)
	}
}

type serverEnforcementSnapshot struct {
	serverMuted     bool
	serverDeafened  bool
	authorizationID int64
	channelIDs      []string
}

// serverEnforcementSnapshotTx reads the post-mutation moderation state and
// gives it one monotonic control-plane revision before the surrounding
// visibility-locked transaction commits. The media plane rejects older
// snapshots, including one that races a staged A2 promotion.
func serverEnforcementSnapshotTx(
	ctx context.Context, tx *sql.Tx, serverID, targetID string,
) (snapshot serverEnforcementSnapshot, returnErr error) {
	if err := tx.QueryRowContext(ctx, `
		SELECT server_muted, server_deafened
		FROM server_members
		WHERE server_id = $1 AND user_id = $2
	`, serverID, targetID).Scan(&snapshot.serverMuted, &snapshot.serverDeafened); err != nil {
		return serverEnforcementSnapshot{}, fmt.Errorf("read server enforcement state: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT nextval('voice_authorization_revision_seq')`).Scan(&snapshot.authorizationID); err != nil || snapshot.authorizationID <= 0 {
		if err == nil {
			err = errors.New("invalid voice authorization revision")
		}
		return serverEnforcementSnapshot{}, fmt.Errorf("allocate server enforcement revision: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT channel_id
		FROM (
			SELECT participant.channel_id, 0 AS source_priority
			FROM voice_participants AS participant
			JOIN channels AS channel ON channel.id = participant.channel_id
			WHERE channel.server_id = $1 AND participant.user_id = $2
			UNION ALL
			SELECT pending.channel_id, 1 AS source_priority
			FROM voice_pending_admissions AS pending
			JOIN channels AS channel ON channel.id = pending.channel_id
			WHERE channel.server_id = $1 AND pending.user_id = $2
		) AS voice_channels
		GROUP BY channel_id
		ORDER BY MIN(source_priority), channel_id
	`, serverID, targetID)
	if err != nil {
		return serverEnforcementSnapshot{}, fmt.Errorf("discover server enforcement channels: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close server enforcement channel discovery: %w", closeErr))
		}
	}()
	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			return serverEnforcementSnapshot{}, fmt.Errorf("scan server enforcement channel: %w", err)
		}
		snapshot.channelIDs = append(snapshot.channelIDs, channelID)
	}
	if err := rows.Err(); err != nil {
		return serverEnforcementSnapshot{}, fmt.Errorf("iterate server enforcement channels: %w", err)
	}
	return snapshot, nil
}

// publishServerEnforcementSnapshot sends the complete moderator-owned state to
// each established or staged channel session. The subject remains action-scoped
// for compatibility; fields are a control-plane snapshot, never client input.
func (h *Handler) publishServerEnforcementSnapshot(
	subject, targetID, action string, snapshot serverEnforcementSnapshot,
) {
	if h.nats == nil {
		return
	}
	for _, channelID := range snapshot.channelIDs {
		if pubErr := h.nats.Publish(subject, map[string]interface{}{
			"channelId":             channelID,
			"userId":                targetID,
			"action":                action,
			"serverMuted":           snapshot.serverMuted,
			"serverDeafened":        snapshot.serverDeafened,
			"authorizationRevision": strconv.FormatInt(snapshot.authorizationID, 10),
			"version":               enforcementSnapshotSchemaVersion,
		}); pubErr != nil {
			h.log.Error("Failed to publish NATS enforcement", "error", pubErr, "subject", subject, "action", action)
		}
	}
}

func firstEnforcementChannel(snapshot serverEnforcementSnapshot) string {
	if len(snapshot.channelIDs) == 0 {
		return ""
	}
	return snapshot.channelIDs[0]
}

// broadcastVoiceStateUpdate sends a voice_state_update WS event to current server
// subscribers who retain permission to view the affected voice channel.
func (h *Handler) broadcastVoiceStateUpdate(serverID, targetID, channelID, action string) {
	serverUUID, serverErr := uuid.Parse(serverID)
	channelUUID, channelErr := uuid.Parse(channelID)
	targetUUID, targetErr := uuid.Parse(targetID)
	if serverErr != nil || channelErr != nil || targetErr != nil {
		return
	}
	h.hub.BroadcastToServerVoiceParticipant(serverUUID, channelUUID, targetUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"action":     action,
			"user_id":    targetID,
			"server_id":  serverID,
			"channel_id": channelID,
		},
	})
}

// enforcementParams groups the per-action strings for applyServerEnforcement.
type enforcementParams struct {
	query       string // SQL UPDATE statement
	permission  rbac.Permission
	natsSubject string // NATS subject to publish on
	natsAction  string // action field in NATS payload
	wsAction    string // action field in WS broadcast
	successMsg  string // HTTP 200 message
	failMsg     string // HTTP 500 / log message
}

// applyServerEnforcement executes the SQL update, finds voice channel, publishes NATS, broadcasts WS.
func (h *Handler) applyServerEnforcement(c *gin.Context, ctx *voiceModContext, p enforcementParams) {
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error(p.failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("voice enforcement rollback failed", "error", rbErr)
		}
	}()
	if !h.guardVoiceModEffectTx(c, tx, ctx, p.permission, true, p.failMsg) {
		return
	}
	result, err := tx.ExecContext(c.Request.Context(), p.query, ctx.serverID, ctx.targetID)
	if err != nil {
		h.log.Error(p.failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	rowsAffected, raErr := result.RowsAffected()
	if raErr != nil {
		h.log.Error("Failed to check rows affected", "error", raErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotMember})
		return
	}
	snapshot, err := serverEnforcementSnapshotTx(c.Request.Context(), tx, ctx.serverID, ctx.targetID)
	if err != nil {
		h.log.Error(p.failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error(p.failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": p.failMsg})
		return
	}
	h.publishServerEnforcementSnapshot(p.natsSubject, ctx.targetID, p.natsAction, snapshot)
	h.broadcastVoiceStateUpdate(ctx.serverID, ctx.targetID, firstEnforcementChannel(snapshot), p.wsAction)
	c.JSON(http.StatusOK, gin.H{"message": p.successMsg})
}

// userLevelAction sends a real-time user-level enforcement command via NATS.
func (h *Handler) userLevelAction(c *gin.Context, perm rbac.Permission, natsSubject, natsAction, successMsg, failMsg string) {
	if c.GetString("user_id") == c.Param("userId") {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCannotTargetSelf})
		return
	}
	ctx := h.authorizeVoiceMod(c, perm)
	if ctx == nil {
		return
	}
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error(failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failMsg})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("user voice action rollback failed", "error", rbErr)
		}
	}()
	if !h.guardVoiceModEffectTx(c, tx, ctx, perm, true, failMsg) {
		return
	}

	channelID, findErr := findVoiceChannelTx(c.Request.Context(), tx, ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("Failed to find voice channel", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
	}
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotInVoice})
		return
	}

	if h.nats != nil {
		if pubErr := h.nats.Publish(natsSubject, map[string]interface{}{
			"channelId": channelID, "userId": ctx.targetID, "action": natsAction,
		}); pubErr != nil {
			h.log.Error(failMsg, "error", pubErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": failMsg})
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.log.Error(failMsg, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failMsg})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": successMsg})
}

// ServerMute applies a persistent server-level mute to a member.
// POST /servers/:id/voice/:userId/mute
func (h *Handler) ServerMute(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMuteMembers)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query: `UPDATE server_members SET server_muted = true WHERE server_id = $1 AND user_id = $2`, permission: rbac.PermMuteMembers,
		natsSubject: "voice.enforce.mute", natsAction: "mute", wsAction: "server_muted",
		successMsg: "Member server-muted", failMsg: "Failed to mute member",
	})
}

// ServerUnmute removes a persistent server-level mute from a member.
// DELETE /servers/:id/voice/:userId/mute
func (h *Handler) ServerUnmute(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMuteMembers)
	if ctx == nil {
		return
	}
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error("Failed to begin server-unmute transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnmuteMember})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("server-unmute rollback failed", "error", rbErr)
		}
	}()
	if !h.guardVoiceModEffectTx(c, tx, ctx, rbac.PermMuteMembers, true, errMsgFailedUnmuteMember) {
		return
	}

	// Check if target is server_deafened — cannot unmute without undeafening first
	var serverDeafened bool
	if err := tx.QueryRowContext(c.Request.Context(), `SELECT server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2 FOR UPDATE`,
		ctx.serverID, ctx.targetID).Scan(&serverDeafened); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotMember})
			return
		}
		h.log.Error("Failed to check deafen state", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnmuteMember})
		return
	}
	if serverDeafened {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot unmute a server-deafened member; undeafen first"})
		return
	}

	// Remove server mute
	if _, err := tx.ExecContext(c.Request.Context(), `UPDATE server_members SET server_muted = false WHERE server_id = $1 AND user_id = $2`,
		ctx.serverID, ctx.targetID); err != nil {
		h.log.Error("Failed to server-unmute member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnmuteMember})
		return
	}

	snapshot, err := serverEnforcementSnapshotTx(c.Request.Context(), tx, ctx.serverID, ctx.targetID)
	if err != nil {
		h.log.Error(errMsgFailedUnmuteMember, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnmuteMember})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit server-unmute", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUnmuteMember})
		return
	}
	h.publishServerEnforcementSnapshot("voice.enforce.mute", ctx.targetID, "unmute", snapshot)
	h.broadcastVoiceStateUpdate(ctx.serverID, ctx.targetID, firstEnforcementChannel(snapshot), "server_unmuted")

	c.JSON(http.StatusOK, gin.H{"message": "Member server-unmuted"})
}

// ServerDeafen applies a persistent server-level deafen (implies mute) to a member.
// POST /servers/:id/voice/:userId/deafen
func (h *Handler) ServerDeafen(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermDeafenMembers)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query: `UPDATE server_members SET server_muted = true, server_deafened = true WHERE server_id = $1 AND user_id = $2`, permission: rbac.PermDeafenMembers,
		natsSubject: "voice.enforce.deafen", natsAction: "deafen", wsAction: "server_deafened",
		successMsg: "Member server-deafened", failMsg: "Failed to deafen member",
	})
}

// ServerUndeafen removes a persistent server-level deafen (and mute) from a member.
// DELETE /servers/:id/voice/:userId/deafen
func (h *Handler) ServerUndeafen(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermDeafenMembers)
	if ctx == nil {
		return
	}
	h.applyServerEnforcement(c, ctx, enforcementParams{
		query: `UPDATE server_members SET server_deafened = false, server_muted = false WHERE server_id = $1 AND user_id = $2`, permission: rbac.PermDeafenMembers,
		natsSubject: "voice.enforce.deafen", natsAction: "undeafen", wsAction: "server_undeafened",
		successMsg: "Member server-undeafened", failMsg: "Failed to undeafen member",
	})
}

// UserMute sends a real-time user-level mute command to the media plane.
// No persistent DB state — requires the target to be in voice.
// POST /servers/:id/voice/:userId/user-mute
func (h *Handler) UserMute(c *gin.Context) {
	h.userLevelAction(c, rbac.PermMuteMembers, "voice.user_mute", "mute", "User mute command sent", "Failed to mute user")
}

// UserDeafen sends a real-time user-level deafen command to the media plane.
// No persistent DB state — requires the target to be in voice.
// POST /servers/:id/voice/:userId/user-deafen
func (h *Handler) UserDeafen(c *gin.Context) {
	h.userLevelAction(c, rbac.PermDeafenMembers, "voice.user_deafen", "deafen", "User deafen command sent", "Failed to deafen user")
}

// --- Force-disconnect (#487 P3) ---

// ServerDisconnect force-disconnects a member from whatever voice channel they
// are currently in within the server. Unlike /move (the single ADR-0023
// hierarchy exception), disconnect RESPECTS hierarchy: a moderator cannot
// disconnect a member with an equal-or-higher role position. It requires the
// Move Members permission and the target must currently be in a voice channel
// in this server (else 409). The action publishes voice.enforce.disconnect so
// the media plane closes the peer's transports; the resulting voice.left NATS
// event drives the composite temp-grant cleanup automatically, so
// this handler does NOT duplicate that cleanup.
//
// POST /servers/:id/voice/:userId/disconnect
func (h *Handler) ServerDisconnect(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMoveMembers)
	if ctx == nil {
		return
	}
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error("disconnect: begin transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisconnectMember})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("disconnect: rollback", "error", rbErr)
		}
	}()
	if !h.guardVoiceModEffectTx(c, tx, ctx, rbac.PermMoveMembers, true, errMsgFailedDisconnectMember) {
		return
	}

	channelID, findErr := findVoiceChannelTx(c.Request.Context(), tx, ctx.serverID, ctx.targetID)
	if findErr != nil {
		h.log.Error("disconnect: find current voice channel", "error", findErr, "server_id", ctx.serverID, "target_id", ctx.targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisconnectMember})
		return
	}
	if channelID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgTargetNotInVoice})
		return
	}

	// The successful guard transaction commit is the authorization decision
	// point. Publish only afterwards: a failed or ambiguous commit must not close
	// transports before the guard decision is confirmed.
	if err := h.commitVoiceEffectTx(tx); err != nil {
		h.log.Error("disconnect: commit transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisconnectMember})
		return
	}
	// The media plane closes the peer's transports and emits voice.left, which
	// (via the NATSSubscriber) updates DB state and triggers the composite
	// temp-grant cleanup — no duplicate cleanup here.
	h.publishEnforcement(natsSubjectEnforceDisconnect, channelID, ctx.targetID, "disconnect")

	c.JSON(http.StatusOK, gin.H{"disconnected": true})
}

// --- Moderator temp-grant revoke (#487 Scope C) ---

const errMsgInvalidChannelIDBody = "Invalid channel_id"

// tempAccessRevokeRequest is the RevokeTempAccess request body.
type tempAccessRevokeRequest struct {
	ChannelID string `json:"channel_id" binding:"required,uuid"`
}

// RevokeTempAccess lets a moderator revoke a move-granted temporary SBAC grant
// while the target is still in the VC (#487 Scope C edge case). It converges on
// the single revokeTemporaryChannelAccess path (delete temp override, purge the
// user's channel_keys + pending requests, rotate the channel CSK,
// force-disconnect the live peer, broadcast channel_access_revoked). Authorize
// with Move Members + hierarchy (this is a moderation action, NOT the move
// exception). If no temporary grant exists for (user, channel) the revoke is a
// no-op and returns 200 {revoked:false}.
//
// DELETE /servers/:id/voice/:userId/temp-access  body {channel_id}
func (h *Handler) RevokeTempAccess(c *gin.Context) {
	ctx := h.authorizeVoiceMod(c, rbac.PermMoveMembers)
	if ctx == nil {
		return
	}
	var req tempAccessRevokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidChannelIDBody})
		return
	}

	reqCtx := c.Request.Context()

	// Scope the body channel_id to the path server: authorizeVoiceMod authorized the
	// actor for :id only, so a temp grant in a DIFFERENT server must not be revocable
	// here (cross-server IDOR guard — Gitar finding). Reuses the ServerMove scope helper;
	// temp grants are only ever issued on voice channels, so isVoiceChannelInServer is exact.
	inServer, scopeErr := h.isVoiceChannelInServer(reqCtx, req.ChannelID, ctx.serverID)
	if scopeErr != nil {
		h.log.Error("temp-access revoke: channel-scope check", "error", scopeErr, "channel_id", req.ChannelID, "server_id", ctx.serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeTempAccess})
		return
	}
	if !inServer {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotVoiceInSrv})
		return
	}

	// The shared transactional delete reports the actual result, avoiding a
	// preflight-to-delete race that could otherwise claim a revoke occurred.
	actorID := c.GetString("user_id")
	removed, err := h.tempGrant.revokeTemporaryChannelAccessWithCredential(
		reqCtx, ctx.serverID, req.ChannelID, ctx.targetID,
		temporaryGrantAuthorization{
			actorID:         actorID,
			credentialEpoch: middleware.TokenCredentialEpoch(c),
			guardCredential: true,
			authority: func(txCtx context.Context, tx *sql.Tx) error {
				return revalidateVoiceModAuthorityTx(txCtx, tx, h.resolver, ctx, actorID, rbac.PermMoveMembers, true)
			},
		},
	)
	if err != nil {
		if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
			h.respondVoiceGuardTxError(c, err, errMsgFailedRevokeTempAccess)
			return
		}
		if errors.Is(err, errVoiceModActorNotMember) || errors.Is(err, errVoiceModPermission) || errors.Is(err, errVoiceModHierarchy) {
			h.respondVoiceModAuthorityTxError(c, err, errMsgFailedRevokeTempAccess)
			return
		}
		h.log.Error("temp-access revoke", "error", err, "channel_id", req.ChannelID, "target_id", ctx.targetID, "server_id", ctx.serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeTempAccess})
		return
	}

	c.JSON(http.StatusOK, gin.H{"revoked": removed})
}

// --- Move (#487 Scope B) ---

const (
	errMsgInvalidTargetChannel = "Invalid target_channel_id"
	errMsgTargetNotVoiceInSrv  = "target is not a voice channel in this server"
	errMsgAlreadyInTarget      = "user is already in the target channel"
	errMsgMovePrep             = "Failed to prepare move"
	errMsgMoveTargetBlocked    = "target cannot access the destination channel due to a permanent permission override"
	auditActionVoiceMoved      = "voice_member_moved"
)

// moveRequest is the ServerMove request body.
type moveRequest struct {
	TargetChannelID string `json:"target_channel_id" binding:"required,uuid"`
}

// ServerMove relocates a user to another voice channel in the same server (#487
// Scope B). It is the SINGLE sanctioned requireHierarchy=false voice action:
//
//	DELIBERATE HIERARCHY EXCEPTION (ADR-0023): MOVE_MEMBERS bypasses CheckHierarchy
//	so a designated organizer can move anyone — including higher roles and the owner
//	— between same-server voice channels. Worst case is annoyance, not privilege
//	escalation (no data access, no kick/ban, server-scoped, target must already be
//	in voice). Hierarchy-crossing moves are audit-logged. The rbac-reviewer must
//	treat any OTHER requireHierarchy=false voice action as a finding. See #487.
//
// POST /servers/:id/voice/:userId/move  body {target_channel_id}
func (h *Handler) ServerMove(c *gin.Context) {
	actorID := c.GetString("user_id")
	serverID := c.Param("id")
	targetID := c.Param("userId")

	req, ok := h.parseMoveRequest(c, serverID, targetID)
	if !ok {
		return
	}

	selfMove := actorID == targetID
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error("move: begin credential transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("move: credential transaction rollback", "error", rbErr)
		}
	}()
	if err := rbac.LockServerVisibilityCapture(c.Request.Context(), tx, serverID); err != nil {
		h.log.Error("move: lock server authority", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return
	}
	// The visibility lock serializes RBAC writers before the users lock is held
	// through the no-grant move decision. That makes reset and the committed
	// decision a single linearization point: reset first yields 401; guard first
	// makes reset wait, then the committed decision may signal the move.
	if err := credepoch.GuardTx(c.Request.Context(), tx, actorID, middleware.TokenCredentialEpoch(c)); err != nil {
		h.respondVoiceGuardTxError(c, err, errMsgMovePrep)
		return
	}

	isVoice, err := isVoiceChannelInServerTx(c.Request.Context(), tx, req.TargetChannelID, serverID)
	if err != nil {
		h.log.Error("move: target channel lookup", "error", err, "target_channel_id", req.TargetChannelID, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return
	}
	if !isVoice {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotVoiceInSrv})
		return
	}
	fromChannelID, err := findVoiceChannelTx(c.Request.Context(), tx, serverID, targetID)
	if err != nil {
		h.log.Error("move: find current voice channel", "error", err, "server_id", serverID, "target_id", targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return
	}
	if fromChannelID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgTargetNotInVoice})
		return
	}
	if fromChannelID == req.TargetChannelID {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgAlreadyInTarget})
		return
	}

	canJoin := true
	if !selfMove {
		if authorityErr := revalidateVoiceModAuthorityTx(c.Request.Context(), tx, h.resolver, &voiceModContext{serverID: serverID, targetID: targetID}, actorID, rbac.PermMoveMembers, false); authorityErr != nil {
			h.respondVoiceModAuthorityTxError(c, authorityErr, errMsgMovePrep)
			return
		}
		targetPerms, permErr := h.resolver.ResolveChannelPermissionsTx(c.Request.Context(), tx, serverID, targetID, req.TargetChannelID)
		if permErr != nil {
			h.log.Error("move: target join-permission check", "error", permErr, "target_id", sanitizeLogValue(targetID), "target_channel_id", sanitizeLogValue(req.TargetChannelID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
			return
		}
		canJoin = targetPerms.Has(rbac.PermViewVoiceChannels) && targetPerms.Has(rbac.PermJoinVoice)
	}

	if canJoin {
		if err := h.commitVoiceEffectTx(tx); err != nil {
			h.log.Error("move: commit credential transaction", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
			return
		}
		h.signalMove(serverID, targetID, fromChannelID, req.TargetChannelID)
		if !selfMove {
			h.auditMoveIfCrossesHierarchy(c.Request.Context(), serverID, actorID, targetID, fromChannelID, req.TargetChannelID)
		}
		c.JSON(http.StatusOK, gin.H{"moved": true})
		return
	}

	// A temporary grant has its own visibility/lifecycle mutation transaction and
	// credential fence. Release this read/decision transaction before that nested
	// mutation (preserving users-before-child lock order). A successful grant
	// commit is the final move decision: reset before it is rejected by that
	// transaction; reset after it cannot retroactively cancel the move signal.
	if err := tx.Commit(); err != nil {
		h.log.Error("move: commit pre-grant credential transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return
	}
	if h.beforeTemporaryGrantForTest != nil {
		h.beforeTemporaryGrantForTest()
	}
	if !h.prepareModeratedMove(c, serverID, actorID, targetID, fromChannelID, req.TargetChannelID) {
		return
	}
	h.signalMove(serverID, targetID, fromChannelID, req.TargetChannelID)
	h.auditMoveIfCrossesHierarchy(c.Request.Context(), serverID, actorID, targetID, fromChannelID, req.TargetChannelID)
	c.JSON(http.StatusOK, gin.H{"moved": true})
}

func serverMemberExistsTx(ctx context.Context, tx *sql.Tx, serverID, userID string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`, serverID, userID).Scan(&exists)
	return exists, err
}

func isVoiceChannelInServerTx(ctx context.Context, tx *sql.Tx, channelID, serverID string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1 AND server_id = $2 AND type = 'voice')`,
		channelID, serverID).Scan(&exists)
	return exists, err
}

func (h *Handler) commitVoiceEffectTx(tx *sql.Tx) error {
	if h.commitVoiceEffectTxForTest != nil {
		return h.commitVoiceEffectTxForTest(tx)
	}
	return tx.Commit()
}

// parseMoveRequest validates the path params + JSON body of a move request. It
// writes the 400 response and returns ok=false on any malformed input.
func (h *Handler) parseMoveRequest(c *gin.Context, serverID, targetID string) (moveRequest, bool) {
	var req moveRequest
	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidServerID})
		return req, false
	}
	if _, err := uuid.Parse(targetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return req, false
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidTargetChannel})
		return req, false
	}
	return req, true
}

// prepareModeratedMove prepares any temporary destination access before signaling.
// It writes the error response and returns false on a
// failed permission check, a failed grant, or when a permanent override prevents
// the grant from conferring both required voice bits. Self-moves never reach here.
func (h *Handler) prepareModeratedMove(c *gin.Context, serverID, actorID, targetID, sourceChannelID, targetChannelID string) bool {
	reqCtx := c.Request.Context()

	// GRANT BEFORE SIGNAL (ordering load-bearing — the client's subsequent
	// AuthorizeJoin requires BOTH PermViewVoiceChannels and PermJoinVoice per the
	// CV-CAN-006 dual-bit gate). Only grant if the target cannot already join the
	// destination with BOTH bits (avoids polluting overrides for users who already
	// have role-based access). Checking JoinVoice alone would skip the grant for a
	// target who inherits JoinVoice but is denied ViewVoice, then AuthorizeJoin
	// would reject the moved client. grantTemporaryChannelAccess never downgrades a
	// permanent grant, and the temp mask (tempGrantAllow) supplies both required
	// bits (VIEW|CONNECT|SPEAK).
	perms, permErr := h.resolver.ResolveEffectivePermissionsUncached(reqCtx, serverID, targetID, targetChannelID)
	if permErr != nil {
		h.log.Error("move: target join-permission check", "error", permErr, "target_id", sanitizeLogValue(targetID), "target_channel_id", sanitizeLogValue(targetChannelID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
		return false
	}
	canJoin := perms.Has(rbac.PermViewVoiceChannels) && perms.Has(rbac.PermJoinVoice)
	if !canJoin {
		granted, grantErr := h.tempGrant.grantTemporaryChannelAccessWithCredential(
			reqCtx, serverID, targetChannelID, targetID,
			temporaryGrantAuthorization{
				actorID:                 actorID,
				credentialEpoch:         middleware.TokenCredentialEpoch(c),
				expectedSourceChannelID: sourceChannelID,
				guardCredential:         true,
				authority: func(txCtx context.Context, tx *sql.Tx) error {
					return revalidateVoiceModAuthorityTx(txCtx, tx, h.resolver, &voiceModContext{serverID: serverID, targetID: targetID}, actorID, rbac.PermMoveMembers, false)
				},
			},
		)
		if grantErr != nil {
			if errors.Is(grantErr, credepoch.ErrEpochMismatch) || errors.Is(grantErr, credepoch.ErrBlocked) {
				h.respondVoiceGuardTxError(c, grantErr, errMsgMovePrep)
				return false
			}
			if errors.Is(grantErr, errTemporaryGrantSourceChanged) {
				c.JSON(http.StatusConflict, gin.H{"error": errMsgTargetNotInVoice})
				return false
			}
			if errors.Is(grantErr, errVoiceModActorNotMember) || errors.Is(grantErr, errVoiceModPermission) || errors.Is(grantErr, errVoiceModHierarchy) {
				h.respondVoiceModAuthorityTxError(c, grantErr, errMsgMovePrep)
				return false
			}
			h.log.Error("move: temp grant", "error", grantErr, "target_id", sanitizeLogValue(targetID), "target_channel_id", sanitizeLogValue(targetChannelID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMovePrep})
			return false
		}
		// A permanent user override makes the locked upsert a no-op. The preflight
		// already established that the target lacks at least one required voice bit,
		// so fail closed rather than signal a move that AuthorizeJoin will refuse.
		if !granted {
			c.JSON(http.StatusConflict, gin.H{"error": errMsgMoveTargetBlocked})
			return false
		}
	}

	return true
}

// signalMove directs the moved client to leave+rejoin (D2 client-cooperative).
// The voice.left/voice.joined NATS events then refresh every sidebar.
func (h *Handler) signalMove(serverID, targetID, fromChannelID, targetChannelID string) {
	targetUUID, parseErr := uuid.Parse(targetID)
	if parseErr != nil {
		h.log.Error("move: invalid target UUID for directed broadcast", "error", parseErr, "target_id", targetID)
		return
	}
	if h.beforeVoiceEffectForTest != nil {
		h.beforeVoiceEffectForTest()
	}
	h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
		Type: "voice_move",
		Data: map[string]interface{}{
			"user_id":         targetID,
			"from_channel_id": fromChannelID,
			"to_channel_id":   targetChannelID,
			"server_id":       serverID,
		},
	})
}

// isVoiceChannelInServer reports whether channelID is a voice channel belonging to
// serverID. channelID is already UUID-validated by the request binding
// (target_channel_id binding:"required,uuid"), so no parse guard is needed here.
func (h *Handler) isVoiceChannelInServer(ctx context.Context, channelID, serverID string) (bool, error) {
	var exists bool
	err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1 AND server_id = $2 AND type = 'voice')`,
		channelID, serverID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// auditMoveIfCrossesHierarchy writes an audit_log entry when a move crosses role
// hierarchy — i.e., the target outranks or equals the actor (CheckHierarchy returns
// non-nil). Best-effort: a failed audit write is logged but does NOT block the move
// (the move is already authorized by PermMoveMembers). Self-moves never reach here.
func (h *Handler) auditMoveIfCrossesHierarchy(ctx context.Context, serverID, actorID, targetID, fromChannelID, toChannelID string) {
	if h.resolver.CheckHierarchy(ctx, serverID, actorID, targetID) == nil {
		return // actor outranks target → ordinary move, no audit needed
	}
	if h.audit == nil {
		return
	}
	actor := actorID
	target := targetID
	if err := h.audit.Log(ctx, serverID, &actor, auditActionVoiceMoved, "member", &target, map[string]interface{}{
		"from_channel_id":   fromChannelID,
		"to_channel_id":     toChannelID,
		"hierarchy_crossed": true,
	}); err != nil {
		h.log.Error("move: audit log", "error", err, "server_id", serverID, "target_id", targetID)
	}
}
