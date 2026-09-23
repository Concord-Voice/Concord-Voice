//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	voiceEnforcementRegistrationTimestampHeader = "X-Concord-Voice-Enforcement-Registration-Timestamp"
	voiceEnforcementRegistrationProofHeader     = "X-Concord-Voice-Enforcement-Registration-Proof"
	voiceEnforcementRegistrationProofContext    = "concord/voice-enforcement-session/register/v1"
	voiceEnforcementRegistrationProofVersion    = "v1"
	voiceEnforcementReleaseTimestampHeader      = "X-Concord-Voice-Enforcement-Timestamp"
	voiceEnforcementReleaseProofHeader          = "X-Concord-Voice-Enforcement-Proof"
	voiceEnforcementReleaseProofContext         = "concord/voice-enforcement-session/release/v1"
	voiceEnforcementReleaseProofVersion         = "v1"
	voiceEnforcementCapabilityTimestampHeader   = "X-Concord-Voice-Enforcement-Capability-Timestamp"
	voiceEnforcementCapabilityProofHeader       = "X-Concord-Voice-Enforcement-Capability-Proof"
	voiceEnforcementCapabilityProofContext      = "concord/voice-enforcement-capability/v1"
	voiceEnforcementCapabilityProofVersion      = "v1"
	voiceEnforcementCapabilityMaxBodyBytes      = 64 << 10
	voiceEnforcementReleaseMaxBodyBytes         = 4 << 10
	voiceEnforcementHealthProofContext          = "concord/voice-enforcement-session/health/bootstrap/v1"
	voiceEnforcementHealthProofVersion          = "v1"
)

var voiceEnforcementCredentialEpochPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var voiceEnforcementHealthNoncePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type voiceEnforcementSessionHandler struct {
	db               *sql.DB
	registerKey      []byte
	releaseKey       []byte
	capabilityKey    []byte
	healthKey        []byte
	healthRequestKey []byte
	healthACKKey     []byte
	requester        dmBlockVoiceEjectionRequester
}

type voiceEnforcementSessionRequest struct {
	SessionGeneration string `json:"session_generation"`
	NodeBootID        string `json:"node_boot_id"`
	RoomID            string `json:"room_id"`
	RoomKind          string `json:"room_kind"`
	UserID            string `json:"user_id"`
	CredentialEpoch   string `json:"credential_epoch"`
	SocketID          string `json:"socket_id"`
}

func newVoiceEnforcementSessionHandler(db *sql.DB, secret string, requesters ...dmBlockVoiceEjectionRequester) *voiceEnforcementSessionHandler {
	var requester dmBlockVoiceEjectionRequester
	if len(requesters) > 0 {
		requester = requesters[0]
	}
	return &voiceEnforcementSessionHandler{
		db:               db,
		registerKey:      mediaproof.DeriveKey(secret, voiceEnforcementRegistrationProofContext),
		releaseKey:       mediaproof.DeriveKey(secret, voiceEnforcementReleaseProofContext),
		capabilityKey:    mediaproof.DeriveKey(secret, voiceEnforcementCapabilityProofContext),
		healthKey:        mediaproof.DeriveKey(secret, voiceEnforcementHealthProofContext),
		healthRequestKey: mediaproof.DeriveKey(secret, "concord/voice-enforcement-session/health/request/v1"),
		healthACKKey:     mediaproof.DeriveKey(secret, "concord/voice-enforcement-session/health/ack/v1"),
		requester:        requester,
	}
}

type voiceEnforcementHealthRequest struct {
	NodeBootID string `json:"node_boot_id"`
	Nonce      string `json:"nonce"`
}

// HealthBootstrap proves the media process can receive the SAME exact target
// subscription used for ejection. It grants no durable-row mutation authority.
func (h *voiceEnforcementSessionHandler) HealthBootstrap(c *gin.Context) {
	if h == nil || h.requester == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, voiceEnforcementReleaseMaxBodyBytes)
	var request voiceEnforcementHealthRequest
	if err := c.ShouldBindBodyWithJSON(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusBadRequest)
		return
	}
	rawBody, present := c.Get(gin.BodyBytesKey)
	body, ok := rawBody.([]byte)
	if !present || !ok || !json.Valid(body) {
		c.Status(http.StatusBadRequest)
		return
	}
	boot, err := uuid.Parse(request.NodeBootID)
	if err != nil || boot.String() != request.NodeBootID || !voiceEnforcementHealthNoncePattern.MatchString(request.Nonce) || !mediaproof.Verify(
		h.healthKey, c.GetHeader(voiceEnforcementReleaseProofHeader), voiceEnforcementHealthProofVersion,
		c.GetHeader(voiceEnforcementReleaseTimestampHeader), request.NodeBootID, request.Nonce, c.Request.Method, c.Request.URL.RequestURI(),
	) {
		c.Status(http.StatusForbidden)
		return
	}
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if err := publishVoiceEnforcementHealth(c.Request.Context(), h.requester, h.healthRequestKey, h.healthACKKey, boot, hex.EncodeToString(challengeBytes)); err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.Status(http.StatusNoContent)
}

// RequireMediaCapability rejects only service-hop admissions after the
// operator's post-drain activation receipt exists. Renderer-facing requests
// remain on their established path; an old media binary cannot create a new,
// unregistered session once durable enforcement is live.
func (h *voiceEnforcementSessionHandler) RequireMediaCapability(c *gin.Context) {
	if h == nil || h.db == nil {
		c.Status(http.StatusServiceUnavailable)
		c.Abort()
		return
	}
	if !middleware.IsMediaPlaneServiceHop(c) {
		c.Next()
		return
	}
	var activated bool
	if err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT activated_at IS NOT NULL FROM voice_enforcement_rollout WHERE id = TRUE`).Scan(&activated); err != nil {
		c.Status(http.StatusServiceUnavailable)
		c.Abort()
		return
	}
	if !activated {
		c.Next()
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, voiceEnforcementCapabilityMaxBodyBytes+1))
	if err != nil || len(body) > voiceEnforcementCapabilityMaxBodyBytes {
		c.Status(http.StatusBadRequest)
		c.Abort()
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	accessToken, found := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	nodeBootID := c.GetHeader("X-Concord-Voice-Enforcement-Node-Boot-ID")
	parsedBootID, parseErr := uuid.Parse(nodeBootID)
	bodyDigest := sha256.Sum256(body)
	if !found || accessToken == "" || parseErr != nil || parsedBootID.String() != nodeBootID || !mediaproof.Verify(
		h.capabilityKey, c.GetHeader(voiceEnforcementCapabilityProofHeader),
		voiceEnforcementCapabilityProofVersion,
		c.GetHeader(voiceEnforcementCapabilityTimestampHeader), c.Request.Method,
		c.Request.URL.RequestURI(), mediaproof.TokenDigest(accessToken),
		nodeBootID, fmt.Sprintf("%x", bodyDigest),
	) {
		// The node ID is deliberately a signed field, carried separately so the
		// proof's payload is inspectable and the target identity cannot be
		// replaced by a bearer-token holder.
		c.Status(http.StatusForbidden)
		c.Abort()
		return
	}
	c.Next()
}

func (r voiceEnforcementSessionRequest) valid() bool {
	if r.RoomKind != "dm" && r.RoomKind != "channel" {
		return false
	}
	if len(r.SocketID) == 0 || len([]byte(r.SocketID)) > 255 || strings.Contains(r.SocketID, "\n") {
		return false
	}
	if r.CredentialEpoch != "" && !voiceEnforcementCredentialEpochPattern.MatchString(r.CredentialEpoch) {
		return false
	}
	for _, value := range []string{r.SessionGeneration, r.NodeBootID, r.RoomID, r.UserID} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return false
		}
	}
	return true
}

func (r voiceEnforcementSessionRequest) same(other voiceEnforcementSessionRequest) bool {
	return r == other
}

// Register records an exact socket after A1 has made it provisional and before
// A2 promotion. It is restricted to authenticated media-plane hops: an end
// user has a bearer token but cannot mint the service-hop proof.
func (h *voiceEnforcementSessionHandler) Register(c *gin.Context) {
	if h == nil || h.db == nil || !middleware.IsMediaPlaneServiceHop(c) {
		c.Status(http.StatusForbidden)
		return
	}
	var request voiceEnforcementSessionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	request.UserID = c.GetString("user_id")
	if !request.valid() {
		c.Status(http.StatusBadRequest)
		return
	}
	accessToken, found := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !found || accessToken == "" || !mediaproof.Verify(
		h.registerKey, c.GetHeader(voiceEnforcementRegistrationProofHeader),
		voiceEnforcementRegistrationProofVersion,
		c.GetHeader(voiceEnforcementRegistrationTimestampHeader),
		mediaproof.TokenDigest(accessToken), request.SessionGeneration, request.NodeBootID,
		request.RoomID, request.RoomKind, request.UserID, request.CredentialEpoch, request.SocketID,
	) {
		c.Status(http.StatusForbidden)
		return
	}
	// This registration is A2's durable admission point. Its row is only
	// evidence of a session that passed the same credential and DM authority
	// fences as promotion; otherwise a block or rotation racing A2 could be
	// committed before a permanently unaddressable row is inserted.
	if request.CredentialEpoch != middleware.TokenCredentialEpoch(c) {
		c.Status(http.StatusForbidden)
		return
	}
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			// A failed rollback is logged without writing a second response.
			log.Printf("rollback voice enforcement registration: %v", rollbackErr)
		}
	}()
	if status, guardErr := guardVoiceEnforcementRegistration(c.Request.Context(), tx, request); guardErr != nil {
		c.Status(status)
		return
	}
	result, err := tx.ExecContext(c.Request.Context(), `
		INSERT INTO voice_enforcement_sessions
			(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`,
		request.SessionGeneration, request.NodeBootID, request.RoomID, request.RoomKind,
		request.UserID, request.CredentialEpoch, request.SocketID)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if changed == 1 {
		if err := tx.Commit(); err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		c.Status(http.StatusNoContent)
		return
	}
	var stored voiceEnforcementSessionRequest
	err = tx.QueryRowContext(c.Request.Context(), `
		SELECT session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id
		FROM voice_enforcement_sessions WHERE session_generation = $1`, request.SessionGeneration).Scan(
		&stored.SessionGeneration, &stored.NodeBootID, &stored.RoomID, &stored.RoomKind,
		&stored.UserID, &stored.CredentialEpoch, &stored.SocketID)
	if errors.Is(err, sql.ErrNoRows) {
		c.Status(http.StatusConflict)
		return
	}
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if !request.same(stored) {
		c.Status(http.StatusConflict)
		return
	}
	if err := tx.Commit(); err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.Status(http.StatusNoContent)
}

// guardVoiceEnforcementRegistration makes the registry row an extension of
// A2, rather than an independently admitted capability. DM uses the complete
// users -> participant-set advisory -> parent -> blocked-pair prefix so a
// membership/block mutation cannot pass between this guard and the insert.
func guardVoiceEnforcementRegistration(ctx context.Context, tx *sql.Tx, request voiceEnforcementSessionRequest) (int, error) {
	if request.RoomKind == "dm" {
		return guardDMVoiceEnforcementRegistration(ctx, tx, request)
	}
	if err := credepoch.GuardTx(ctx, tx, request.UserID, request.CredentialEpoch); err != nil {
		if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, sql.ErrNoRows) {
			return http.StatusForbidden, err
		}
		return http.StatusServiceUnavailable, fmt.Errorf("guard channel voice enforcement registration: %w", err)
	}
	return http.StatusNoContent, nil
}

func guardDMVoiceEnforcementRegistration(ctx context.Context, tx *sql.Tx, request voiceEnforcementSessionRequest) (int, error) {
	conversationID, err := uuid.Parse(request.RoomID)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("parse voice enforcement conversation ID: %w", err)
	}
	userID, err := uuid.Parse(request.UserID)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("parse voice enforcement user ID: %w", err)
	}
	subjects, err := dmblock.LockConversationUsersTx(ctx, tx, request.RoomID, []uuid.UUID{userID}, dmblock.LockShare)
	if err != nil {
		if errors.Is(err, dmblock.ErrUnavailable) || errors.Is(err, dmblock.ErrMembershipChanged) {
			return http.StatusForbidden, err
		}
		return http.StatusServiceUnavailable, fmt.Errorf("lock voice enforcement DM users: %w", err)
	}
	if err := credepoch.GuardTx(ctx, tx, request.UserID, request.CredentialEpoch); err != nil {
		if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, sql.ErrNoRows) {
			return http.StatusForbidden, err
		}
		return http.StatusServiceUnavailable, fmt.Errorf("guard DM voice enforcement registration: %w", err)
	}
	if err := dm.LockDMVoiceParticipantSetTx(ctx, tx, conversationID); err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("lock voice enforcement DM participant set: %w", err)
	}
	if _, err := dmblock.PrepareConversationAfterUserLocksTx(ctx, tx, request.RoomID, subjects, dmblock.LockShare); err != nil {
		if errors.Is(err, dmblock.ErrUnavailable) || errors.Is(err, dmblock.ErrMembershipChanged) {
			return http.StatusForbidden, err
		}
		return http.StatusServiceUnavailable, fmt.Errorf("prepare voice enforcement DM registration: %w", err)
	}
	var memberID string
	err = tx.QueryRowContext(ctx, `
		SELECT user_id FROM dm_participants
		WHERE conversation_id = $1 AND user_id = $2
		FOR KEY SHARE`, request.RoomID, request.UserID).Scan(&memberID)
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusForbidden, err
	}
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("recheck voice enforcement DM membership: %w", err)
	}
	return http.StatusNoContent, nil
}

// Release accepts only a body-bound, domain-separated proof from the media
// plane. It deliberately does not require the user's bearer token: credential
// rotation may revoke that token before the exact socket can be torn down.
func (h *voiceEnforcementSessionHandler) Release(c *gin.Context) {
	if h == nil || h.db == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	// This route is intentionally unauthenticated because the user JWT may be
	// invalidated before terminal teardown. Bound input before JSON decoding so
	// an unauthenticated peer cannot allocate an unbounded request body while
	// failing its proof later.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, voiceEnforcementReleaseMaxBodyBytes)
	var request voiceEnforcementSessionRequest
	if err := c.ShouldBindJSON(&request); err != nil || !request.valid() {
		c.Status(http.StatusBadRequest)
		return
	}
	timestamp := c.GetHeader(voiceEnforcementReleaseTimestampHeader)
	if !mediaproof.Verify(h.releaseKey, c.GetHeader(voiceEnforcementReleaseProofHeader),
		voiceEnforcementReleaseProofVersion, timestamp,
		request.SessionGeneration, request.NodeBootID, request.RoomID, request.RoomKind,
		request.UserID, request.CredentialEpoch, request.SocketID) {
		c.Status(http.StatusForbidden)
		return
	}
	result, err := h.db.ExecContext(c.Request.Context(), `
		DELETE FROM voice_enforcement_sessions
		WHERE session_generation = $1 AND node_boot_id = $2 AND room_id = $3 AND room_kind = $4
			AND user_id = $5 AND credential_epoch = $6 AND socket_id = $7`,
		request.SessionGeneration, request.NodeBootID, request.RoomID, request.RoomKind,
		request.UserID, request.CredentialEpoch, request.SocketID)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if changed == 1 {
		c.Status(http.StatusNoContent)
		return
	}
	var exists bool
	if err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT EXISTS(SELECT 1 FROM voice_enforcement_sessions WHERE session_generation = $1)`,
		request.SessionGeneration).Scan(&exists); err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if exists {
		c.Status(http.StatusConflict)
		return
	}
	c.Status(http.StatusNoContent)
}
