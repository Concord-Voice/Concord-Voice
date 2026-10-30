package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Every registry owner serializes absent as well as present generations using
// the database's canonical key. Receipt values are private copies made before
// commit; signing never consults collectible evidence after releasing the key.
type voiceAuthReceipt struct {
	request protocol.Request
	payload protocol.ResponsePayload
}

type voiceAuthFailure struct {
	status int
	cause  error
}

func (e *voiceAuthFailure) Error() string { return "voice registry request failed" }
func (e *voiceAuthFailure) Unwrap() error { return e.cause }
func voiceAuthFail(status int) error      { return &voiceAuthFailure{status: status} }
func voiceAuthStatus(err error) int {
	var failure *voiceAuthFailure
	if errors.As(err, &failure) {
		return failure.status
	}
	var wire *protocol.ProtocolError
	if errors.As(err, &wire) {
		switch wire.ProtocolReason() {
		case "payload_too_large":
			return http.StatusRequestEntityTooLarge
		case "invalid_headers", "invalid_proof", "proof_stale":
			return http.StatusForbidden
		default:
			return http.StatusBadRequest
		}
	}
	if errors.Is(err, loginfamily.ErrDenied) {
		return http.StatusForbidden
	}
	if errors.Is(err, loginfamily.ErrContract) {
		return http.StatusInternalServerError
	}
	if errors.Is(err, loginfamily.ErrConflict) {
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}

func voiceAuthBody(c *gin.Context) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var raw json.RawMessage
	if err := c.ShouldBindBodyWithJSON(&raw); err != nil {
		var over *http.MaxBytesError
		if errors.As(err, &over) {
			return nil, voiceAuthFail(http.StatusRequestEntityTooLarge)
		}
		return nil, voiceAuthFail(http.StatusBadRequest)
	}
	cached, present := c.Get(gin.BodyBytesKey)
	body, ok := cached.([]byte)
	if !present || !ok || !json.Valid(body) {
		return nil, voiceAuthFail(http.StatusBadRequest)
	}
	return body, nil
}
func (h *voiceEnforcementSessionHandler) decodeVoiceAuth(c *gin.Context, endpoint protocol.Endpoint, body []byte) (protocol.Request, error) {
	headers := make([]protocol.Header, 0, len(c.Request.Header))
	for name, values := range c.Request.Header {
		for _, value := range values {
			headers = append(headers, protocol.Header{Name: name, Value: value})
		}
	}
	request, err := protocol.DecodeHTTPRequest(endpoint, protocol.HTTPRequest{Method: c.Request.Method, Path: c.Request.URL.RequestURI(), Headers: headers, Body: body})
	if err != nil {
		return protocol.Request{}, err
	}
	if err = protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()); err != nil {
		return protocol.Request{}, err
	}
	return request, nil
}
func (h *voiceEnforcementSessionHandler) readVoiceAuth(c *gin.Context, endpoint protocol.Endpoint) (protocol.Request, []byte, error) {
	body, err := voiceAuthBody(c)
	if err != nil {
		return protocol.Request{}, nil, err
	}
	request, err := h.decodeVoiceAuth(c, endpoint, body)
	return request, body, err
}

// runVoiceAuth owns the entire transaction and discards its tentative receipt
// on every failed commit, including outcomes that may already be durable.
func (h *voiceEnforcementSessionHandler) runVoiceAuth(ctx context.Context, request protocol.Request, work func(*sql.Tx) (protocol.ResponsePayload, error)) (response protocol.HTTPResponse, retErr error) {
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return response, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, rollbackErr)
			response = protocol.HTTPResponse{}
		}
	}()
	payload, err := work(tx)
	if err != nil {
		return response, err
	}
	if err = protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()); err != nil {
		return response, err
	}
	receipt := voiceAuthReceipt{request: request, payload: payload}
	if err := tx.Commit(); err != nil {
		return response, err
	}
	return protocol.SignHTTPResponse(receipt.request, receipt.payload, strconv.FormatInt(time.Now().Unix(), 10), h.authSecret)
}
func writeVoiceAuth(c *gin.Context, response protocol.HTTPResponse, err error) {
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	for _, header := range response.Headers {
		c.Header(header.Name, header.Value)
	}
	if len(response.Body) == 0 {
		c.Status(response.Status)
		return
	}
	c.Data(response.Status, "application/json", response.Body)
}
func voiceAuthGeneration(ctx context.Context, tx *sql.Tx, kind, generation string) error {
	_, err := tx.ExecContext(ctx, `SELECT login_family_generation_lock($1,$2::uuid)`, kind, generation)
	return err
}
func voiceAuthActiveBoot(ctx context.Context, tx *sql.Tx, boot string) error {
	var retired sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT retired_at FROM voice_auth_boot_fences WHERE node_boot_id=$1 FOR SHARE`, boot).Scan(&retired)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && retired.Valid) {
		return voiceAuthFail(http.StatusForbidden)
	}
	return err
}
func voiceAuthRetiredBoot(ctx context.Context, tx *sql.Tx, boot string) (bool, error) {
	var retired bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM voice_auth_boot_fences WHERE node_boot_id=$1 AND retired_at IS NOT NULL)`, boot).Scan(&retired)
	return retired, err
}

type voiceAuthConnection struct {
	binding     protocol.Binding
	tokenDigest string
	bodyDigest  string
	variant     string
}

func voiceAuthReadConnection(ctx context.Context, tx *sql.Tx, generation string, exclusive bool) (voiceAuthConnection, bool, error) {
	query := `SELECT user_id,family_id,connection_generation,node_boot_id,socket_id,credential_epoch,registration_token_sha256,registration_body_sha256 FROM voice_auth_connections WHERE connection_generation=$1 FOR KEY SHARE`
	if exclusive {
		query = `SELECT user_id,family_id,connection_generation,node_boot_id,socket_id,credential_epoch,registration_token_sha256,registration_body_sha256 FROM voice_auth_connections WHERE connection_generation=$1 FOR UPDATE`
	}
	var v voiceAuthConnection
	b := &v.binding
	err := tx.QueryRowContext(ctx, query, generation).Scan(&b.UserID, &b.FamilyID, &b.Target.ConnectionGeneration, &b.Target.NodeBootID, &b.Target.SocketID, &b.Target.CredentialEpoch, &v.tokenDigest, &v.bodyDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	v.variant = "bound"
	return v, err == nil, err
}
func voiceAuthReadConnectionClosure(ctx context.Context, tx *sql.Tx, generation string) (voiceAuthConnection, bool, error) {
	var v voiceAuthConnection
	b := &v.binding
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(user_id::text,''),COALESCE(family_id::text,''),connection_generation,node_boot_id,socket_id,credential_epoch,registration_token_sha256,registration_body_sha256,variant FROM voice_auth_connection_closures WHERE connection_generation=$1`, generation).Scan(&b.UserID, &b.FamilyID, &b.Target.ConnectionGeneration, &b.Target.NodeBootID, &b.Target.SocketID, &b.Target.CredentialEpoch, &v.tokenDigest, &v.bodyDigest, &v.variant)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}
func voiceAuthNoAssociations(ctx context.Context, tx *sql.Tx, generation string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM voice_auth_room_bindings WHERE connection_generation=$1)`, generation).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return voiceAuthFail(http.StatusConflict)
	}
	return nil
}
func voiceAuthUserGuard(ctx context.Context, tx *sql.Tx, identity loginfamily.AccessIdentity) (loginfamily.UserAuthority, error) {
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return loginfamily.UserAuthority{}, err
	}
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{identity.UserID}, loginfamily.UserShare)
	if err != nil {
		return loginfamily.UserAuthority{}, err
	}
	return users.ForUser(identity.UserID)
}
func voiceAuthFamily(ctx context.Context, tx *sql.Tx, user loginfamily.UserAuthority, identity loginfamily.AccessIdentity, target protocol.Target) (protocol.Binding, error) {
	family, err := loginfamily.GuardAfterUserLockTx(ctx, tx, user, identity, loginfamily.FamilyShare)
	if err != nil {
		return protocol.Binding{}, err
	}
	if target.CredentialEpoch != identity.CredentialEpoch {
		return protocol.Binding{}, voiceAuthFail(http.StatusConflict)
	}
	binding := family.Binding()
	return protocol.Binding{UserID: binding.UserID.String(), FamilyID: binding.FamilyID.String(), Target: target}, nil
}
func (h *voiceEnforcementSessionHandler) productiveVoiceAuth(c *gin.Context) (loginfamily.AccessIdentity, bool) {
	if h == nil || h.db == nil {
		c.Status(http.StatusServiceUnavailable)
		return loginfamily.AccessIdentity{}, false
	}
	if !middleware.IsMediaPlaneServiceHop(c) {
		c.Status(http.StatusForbidden)
		return loginfamily.AccessIdentity{}, false
	}
	identity, _, ok := middleware.VerifiedAccessFromContext(c)
	if !ok {
		c.Status(http.StatusUnauthorized)
	}
	return identity, ok
}

// RegisterConnection records an idle or room-bound socket using SQL-derived
// family identity and immutable original registration byte commitments.
func (h *voiceEnforcementSessionHandler) RegisterConnection(c *gin.Context) {
	identity, ok := h.productiveVoiceAuth(c)
	if !ok {
		return
	}
	request, body, err := h.readVoiceAuth(c, protocol.EndpointConnectionRegister)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	value, ok := voiceAuthPayload(c, request.ConnectionRegister)
	if !ok {
		return
	}
	target := protocol.Target{ConnectionGeneration: value.ConnectionGeneration, NodeBootID: value.NodeBootID, SocketID: value.SocketID, CredentialEpoch: value.CredentialEpoch}
	original := voiceAuthConnection{tokenDigest: mediaproof.TokenDigest(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")), bodyDigest: mediaproof.TokenDigest(string(body)), variant: "bound"}
	ctx := c.Request.Context()
	response, err := h.runVoiceAuth(ctx, request, func(tx *sql.Tx) (protocol.ResponsePayload, error) {
		user, err := voiceAuthUserGuard(ctx, tx, identity)
		if err != nil {
			return nil, err
		}
		original.binding, err = voiceAuthFamily(ctx, tx, user, identity, target)
		if err != nil {
			return nil, err
		}
		if err = voiceAuthActiveBoot(ctx, tx, target.NodeBootID); err != nil {
			return nil, err
		}
		if err = voiceAuthGeneration(ctx, tx, "connection", target.ConnectionGeneration); err != nil {
			return nil, err
		}
		if err = h.freshVoiceAuth(ctx, tx, request, user, identity, target); err != nil {
			return nil, err
		}
		if err = voiceAuthInsertConnection(ctx, tx, original); err != nil {
			return nil, err
		}
		if err = h.freshVoiceAuth(ctx, tx, request, user, identity, target); err != nil {
			return nil, err
		}
		return protocol.RegistrationResult{Version: 1, VoiceEnforcementProtocol: 4, UserID: original.binding.UserID, FamilyID: original.binding.FamilyID, Target: target}, nil
	})
	writeVoiceAuth(c, response, err)
}
func (h *voiceEnforcementSessionHandler) freshVoiceAuth(ctx context.Context, tx *sql.Tx, request protocol.Request, user loginfamily.UserAuthority, identity loginfamily.AccessIdentity, target protocol.Target) error {
	if err := protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()); err != nil {
		return err
	}
	_, err := voiceAuthFamily(ctx, tx, user, identity, target)
	return err
}
func voiceAuthInsertConnection(ctx context.Context, tx *sql.Tx, v voiceAuthConnection) error {
	target := v.binding.Target
	if _, closed, err := voiceAuthReadConnectionClosure(ctx, tx, target.ConnectionGeneration); err != nil {
		return err
	} else if closed {
		return voiceAuthFail(http.StatusConflict)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO voice_auth_connections(connection_generation,family_id,user_id,credential_epoch,node_boot_id,socket_id,registration_token_sha256,registration_body_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, target.ConnectionGeneration, v.binding.FamilyID, v.binding.UserID, target.CredentialEpoch, target.NodeBootID, target.SocketID, v.tokenDigest, v.bodyDigest)
	if err != nil {
		return err
	}
	stored, present, err := voiceAuthReadConnection(ctx, tx, target.ConnectionGeneration, false)
	if err != nil {
		return err
	}
	if !present {
		return voiceAuthConnectionSocketConflict(ctx, tx, target)
	}
	if stored != v {
		return voiceAuthFail(http.StatusConflict)
	}
	return nil
}

// A uniqueness wait is followed by a fresh read of the actual competing tuple;
// neither an index conflict nor an advisory hash is an identity witness.
func voiceAuthConnectionSocketConflict(ctx context.Context, tx *sql.Tx, target protocol.Target) error {
	var actual protocol.Target
	err := tx.QueryRowContext(ctx, `SELECT connection_generation,node_boot_id,socket_id,credential_epoch FROM voice_auth_connections WHERE node_boot_id=$1 AND socket_id=$2`, target.NodeBootID, target.SocketID).Scan(&actual.ConnectionGeneration, &actual.NodeBootID, &actual.SocketID, &actual.CredentialEpoch)
	if err != nil {
		return err
	}
	return voiceAuthFail(http.StatusConflict)
}

// Explicit alias refusal prevents Gin's trailing-slash redirect from changing
// a signed target without changing redirect behavior for unrelated endpoints.
func registerVoiceAuthAliases(group *gin.RouterGroup) {
	for _, path := range []string{
		"/internal/voice/auth-connections",
		"/internal/voice/auth-connections/release",
		"/internal/voice/auth-connections/provisional-close",
		"/voice/enforcement-sessions",
		"/internal/voice/enforcement-sessions/release",
		"/internal/voice/enforcement/health",
		"/internal/voice/auth-connections/retire-boot",
	} {
		group.Any(path+"/", func(c *gin.Context) { c.Status(http.StatusNotFound) })
	}
}

// voiceAuthPayload reads the decoded request variant for an endpoint. The
// decoder selects the variant from the endpoint, so a mismatch is a server
// fault: answer 500 instead of handling a zero-value payload.
func voiceAuthPayload[T any](c *gin.Context, accessor func() (T, bool)) (T, bool) {
	value, ok := accessor()
	if !ok {
		c.Status(http.StatusInternalServerError)
	}
	return value, ok
}
