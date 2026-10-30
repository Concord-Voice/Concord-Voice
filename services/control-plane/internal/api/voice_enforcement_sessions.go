//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
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
var voiceEnforcementRBACLog = logger.New("production")

type voiceEnforcementSessionHandler struct {
	db                *sql.DB
	authSecret        []byte
	registerKey       []byte
	releaseKey        []byte
	capabilityKey     []byte
	requester         dmBlockVoiceEjectionRequester
	authoritySnapshot func(context.Context, uuid.UUID) (VoiceAuthAuthoritySnapshot, error)
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
		db:            db,
		authSecret:    []byte(secret),
		registerKey:   mediaproof.DeriveKey(secret, voiceEnforcementRegistrationProofContext),
		releaseKey:    mediaproof.DeriveKey(secret, voiceEnforcementReleaseProofContext),
		capabilityKey: mediaproof.DeriveKey(secret, voiceEnforcementCapabilityProofContext),
		requester:     requester,
	}
}

// RequireMediaCapability closes service-hop admission while prepared, then
// requires a signed body/boot capability from a protocol-4 boot. Renderer-facing
// requests retain their existing path and final family transaction guards.
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
		`SELECT activated_at IS NOT NULL FROM login_family_rollout WHERE id = TRUE`).Scan(&activated); err != nil {
		c.Status(http.StatusServiceUnavailable)
		c.Abort()
		return
	}
	if !activated {
		c.Status(http.StatusForbidden)
		c.Abort()
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
	var capable bool
	if err := h.db.QueryRowContext(c.Request.Context(), `SELECT EXISTS(SELECT 1 FROM voice_auth_boot_fences WHERE node_boot_id=$1 AND retired_at IS NULL)`, nodeBootID).Scan(&capable); err != nil {
		c.Status(http.StatusServiceUnavailable)
		c.Abort()
		return
	}
	if !capable {
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

// Register attaches or renews an exact room under the same family authority as
// admission. Legacy epoch-only envelopes cannot create family registry evidence.
func (h *voiceEnforcementSessionHandler) Register(c *gin.Context) {
	identity, ok := h.productiveVoiceAuth(c)
	if !ok {
		return
	}
	request, _, err := h.readVoiceAuth(c, protocol.EndpointRoomRegister)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	value, ok := voiceAuthPayload(c, request.RoomRegister)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	serverID, err := h.voiceAuthRoomServer(ctx, value)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	var response protocol.HTTPResponse
	// A changed DM participant set requires a new transaction and complete user
	// prefix, never a late addition to the previous capability.
	for attempt := 0; attempt < 2; attempt++ {
		response, err = h.runVoiceAuth(ctx, request, func(tx *sql.Tx) (protocol.ResponsePayload, error) {
			return h.registerAuthRoomTx(ctx, tx, request, value, identity, serverID)
		})
		if !errors.Is(err, dmblock.ErrMembershipChanged) {
			break
		}
	}
	writeVoiceAuth(c, response, err)
}

// Release keeps schema-absent legacy draining available, but that proof can
// never delete family-aware room evidence. Unknown schemas never fall back.
func (h *voiceEnforcementSessionHandler) Release(c *gin.Context) {
	if h == nil || h.db == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	body, err := voiceAuthBody(c)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(body, &fields); err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	if _, present := fields["schema"]; present {
		h.releaseAuthRoom(c, body)
		return
	}
	h.releaseLegacyRoom(c, body)
}
func (h *voiceEnforcementSessionHandler) releaseLegacyRoom(c *gin.Context, body []byte) {
	var request voiceEnforcementSessionRequest
	if c.Request.Method != http.MethodPost || c.Request.URL.RequestURI() != "/api/v1/internal/voice/enforcement-sessions/release" || json.Unmarshal(body, &request) != nil || !request.valid() {
		c.Status(http.StatusBadRequest)
		return
	}
	verify := func() bool {
		return mediaproof.Verify(h.releaseKey, c.GetHeader(voiceEnforcementReleaseProofHeader), voiceEnforcementReleaseProofVersion, c.GetHeader(voiceEnforcementReleaseTimestampHeader), request.SessionGeneration, request.NodeBootID, request.RoomID, request.RoomKind, request.UserID, request.CredentialEpoch, request.SocketID)
	}
	if !verify() {
		c.Status(http.StatusForbidden)
		return
	}
	err := h.releaseLegacyRoomTx(c.Request.Context(), request, verify)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	c.Status(http.StatusNoContent)
}
func (h *voiceEnforcementSessionHandler) releaseLegacyRoomTx(ctx context.Context, request voiceEnforcementSessionRequest, verify func() bool) (retErr error) {
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()
	if err = voiceAuthGeneration(ctx, tx, "room", request.SessionGeneration); err != nil {
		return err
	}
	// An unbound legacy row has no parent connection to lock. The room key
	// prevents attachment while the following fresh association check holds.
	if _, bound, err := voiceAuthReadAssociation(ctx, tx, request.SessionGeneration); err != nil {
		return err
	} else if bound {
		return voiceAuthFail(http.StatusConflict)
	}
	if _, closed, err := voiceAuthReadRoomClosure(ctx, tx, request.SessionGeneration); err != nil {
		return err
	} else if closed {
		return voiceAuthFail(http.StatusConflict)
	}
	row, present, err := voiceAuthReadRoom(ctx, tx, request.SessionGeneration)
	if err != nil {
		return err
	}
	if !verify() {
		return voiceAuthFail(http.StatusForbidden)
	}
	if present && row != request {
		return voiceAuthFail(http.StatusConflict)
	}
	if present {
		result, err := tx.ExecContext(ctx, `DELETE FROM voice_enforcement_sessions WHERE session_generation=$1`, request.SessionGeneration)
		if err != nil {
			return err
		}
		if err = voiceAuthOneChanged(result); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (h *voiceEnforcementSessionHandler) registerAuthRoomTx(ctx context.Context, tx *sql.Tx, request protocol.Request, value protocol.RoomRegister, identity loginfamily.AccessIdentity, serverID string) (protocol.ResponsePayload, error) {
	user, err := voiceAuthRoomAuthority(ctx, tx, value, identity, serverID)
	if err != nil {
		return nil, err
	}
	target := protocol.Target{ConnectionGeneration: value.ConnectionGeneration, NodeBootID: value.NodeBootID, SocketID: value.SocketID, CredentialEpoch: value.CredentialEpoch}
	binding, err := voiceAuthFamily(ctx, tx, user, identity, target)
	if err != nil {
		return nil, err
	}
	if err = voiceAuthActiveBoot(ctx, tx, value.NodeBootID); err != nil {
		return nil, err
	}
	if err = voiceAuthGeneration(ctx, tx, "connection", value.ConnectionGeneration); err != nil {
		return nil, err
	}
	connection, present, err := voiceAuthReadConnection(ctx, tx, value.ConnectionGeneration, false)
	if err != nil {
		return nil, err
	}
	if !present || connection.binding != binding {
		return nil, voiceAuthFail(http.StatusConflict)
	}
	if _, closed, err := voiceAuthReadConnectionClosure(ctx, tx, value.ConnectionGeneration); err != nil {
		return nil, err
	} else if closed {
		return nil, voiceAuthFail(http.StatusConflict)
	}
	if err = voiceAuthGeneration(ctx, tx, "room", value.SessionGeneration); err != nil {
		return nil, err
	}
	room := protocol.Room{SessionGeneration: value.SessionGeneration, RoomID: value.RoomID, RoomKind: value.RoomKind}
	fresh := func() error { return h.freshVoiceAuth(ctx, tx, request, user, identity, target) }
	if err = voiceAuthAttachRoom(ctx, tx, value.Mode, binding, room, fresh); err != nil {
		return nil, err
	}
	if err = fresh(); err != nil {
		return nil, err
	}
	return protocol.RoomRegistrationResult{Schema: "concord.auth-room-binding", Version: 1, VoiceEnforcementProtocol: 4, Binding: binding, Room: room}, nil
}

// Channel discovery precedes the owned transaction; the authority helper's
// visibility lock remains its mandatory first statement and rechecks scope.
func (h *voiceEnforcementSessionHandler) voiceAuthRoomServer(ctx context.Context, value protocol.RoomRegister) (string, error) {
	if value.RoomKind == "dm" {
		return "", nil
	}
	var serverID string
	err := h.db.QueryRowContext(ctx, `SELECT server_id FROM channels WHERE id=$1`, value.RoomID).Scan(&serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", voiceAuthFail(http.StatusForbidden)
	}
	return serverID, err
}
func voiceAuthRoomAuthority(ctx context.Context, tx *sql.Tx, value protocol.RoomRegister, identity loginfamily.AccessIdentity, serverID string) (loginfamily.UserAuthority, error) {
	if value.RoomKind == "dm" {
		return voiceAuthDMRoomAuthority(ctx, tx, value.RoomID, identity)
	}
	user, status, err := voice.CheckChannelVoiceFamilyAuthorityTx(ctx, tx, rbac.NewResolver(nil, nil, voiceEnforcementRBACLog), serverID, value.RoomID, identity)
	if err != nil {
		// The service protocol distinguishes dependency failure from final
		// resource denial independently of ordinary voice admission statuses.
		switch {
		case errors.Is(err, loginfamily.ErrContract):
			status = http.StatusInternalServerError
		case status >= http.StatusInternalServerError:
			status = http.StatusServiceUnavailable
		default:
			status = http.StatusForbidden
		}
		return loginfamily.UserAuthority{}, &voiceAuthFailure{status: status, cause: err}
	}
	return user, nil
}
func voiceAuthDMRoomAuthority(ctx context.Context, tx *sql.Tx, roomID string, identity loginfamily.AccessIdentity) (loginfamily.UserAuthority, error) {
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return loginfamily.UserAuthority{}, err
	}
	subjects, users, err := dmblock.LockConversationAuthorityUsersTx(ctx, tx, barrier, roomID, []uuid.UUID{identity.UserID}, loginfamily.UserShare)
	if err != nil {
		return loginfamily.UserAuthority{}, voiceAuthDMError(err)
	}
	user, err := users.ForUser(identity.UserID)
	if err != nil {
		return user, err
	}
	if _, err = loginfamily.GuardAfterUserLockTx(ctx, tx, user, identity, loginfamily.FamilyShare); err != nil {
		return user, err
	}
	if err = dm.LockDMVoiceParticipantSetTx(ctx, tx, uuid.MustParse(roomID)); err != nil {
		return user, err
	}
	if _, err = dmblock.PrepareConversationAfterUserLocksTx(ctx, tx, roomID, subjects, dmblock.LockShare); err != nil {
		return user, voiceAuthDMError(err)
	}
	var member string
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM dm_participants WHERE conversation_id=$1 AND user_id=$2 FOR KEY SHARE`, roomID, identity.UserID).Scan(&member)
	if errors.Is(err, sql.ErrNoRows) {
		return user, voiceAuthFail(http.StatusForbidden)
	}
	return user, err
}
func voiceAuthDMError(err error) error {
	if errors.Is(err, dmblock.ErrUnavailable) || errors.Is(err, dmblock.ErrMembershipChanged) {
		return &voiceAuthFailure{status: http.StatusForbidden, cause: err}
	}
	return err
}
func voiceAuthAttachRoom(ctx context.Context, tx *sql.Tx, mode string, binding protocol.Binding, room protocol.Room, fresh func() error) error {
	if _, closed, err := voiceAuthReadRoomClosure(ctx, tx, room.SessionGeneration); err != nil {
		return err
	} else if closed {
		return voiceAuthFail(http.StatusConflict)
	}
	row, present, err := voiceAuthReadRoom(ctx, tx, room.SessionGeneration)
	if err != nil {
		return err
	}
	parent, bound, err := voiceAuthReadAssociation(ctx, tx, room.SessionGeneration)
	if err != nil {
		return err
	}
	if err = fresh(); err != nil {
		return err
	}
	if present {
		if row != voiceAuthRoomTuple(binding, room) || !bound || parent != binding.Target.ConnectionGeneration {
			return voiceAuthFail(http.StatusConflict)
		}
		return nil
	}
	if bound || mode == "renew" {
		return voiceAuthFail(http.StatusConflict)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO voice_enforcement_sessions(session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, room.SessionGeneration, binding.Target.NodeBootID, room.RoomID, room.RoomKind, binding.UserID, binding.Target.CredentialEpoch, binding.Target.SocketID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		var actual voiceEnforcementSessionRequest
		err = tx.QueryRowContext(ctx, `SELECT session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id FROM voice_enforcement_sessions WHERE node_boot_id=$1 AND socket_id=$2`, binding.Target.NodeBootID, binding.Target.SocketID).Scan(&actual.SessionGeneration, &actual.NodeBootID, &actual.RoomID, &actual.RoomKind, &actual.UserID, &actual.CredentialEpoch, &actual.SocketID)
		if err != nil {
			return err
		}
		return voiceAuthFail(http.StatusConflict)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_auth_room_bindings(session_generation,connection_generation) VALUES($1,$2)`, room.SessionGeneration, binding.Target.ConnectionGeneration)
	return err
}
