package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/gin-gonic/gin"
)

func voiceAuthInsertConnectionClosure(ctx context.Context, tx *sql.Tx, v voiceAuthConnection) error {
	b := v.binding
	var user, family any
	if v.variant == "bound" {
		user, family = b.UserID, b.FamilyID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO voice_auth_connection_closures(connection_generation,node_boot_id,socket_id,credential_epoch,variant,user_id,family_id,registration_token_sha256,registration_body_sha256,closed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp())`, b.Target.ConnectionGeneration, b.Target.NodeBootID, b.Target.SocketID, b.Target.CredentialEpoch, v.variant, user, family, v.tokenDigest, v.bodyDigest)
	return err
}
func voiceAuthDeleteConnection(ctx context.Context, tx *sql.Tx, generation string) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM voice_auth_connections WHERE connection_generation=$1`, generation)
	if err != nil {
		return err
	}
	return voiceAuthOneChanged(result)
}
func voiceAuthOneChanged(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return voiceAuthFail(http.StatusConflict)
	}
	return nil
}

type voiceAuthCloseState struct {
	live    voiceAuthConnection
	closure voiceAuthConnection
	present bool
	closed  bool
	retired bool
}

func voiceAuthConnectionForClose(ctx context.Context, tx *sql.Tx, target protocol.Target) (voiceAuthCloseState, error) {
	var state voiceAuthCloseState
	// Boot before connection: the closure trigger takes the boot key, so taking
	// the connection key first would invert attachment's boot-row -> connection
	// order and close a cycle through HealthBootstrap's boot key -> boot row.
	if err := voiceAuthGeneration(ctx, tx, "boot", target.NodeBootID); err != nil {
		return state, err
	}
	if err := voiceAuthGeneration(ctx, tx, "connection", target.ConnectionGeneration); err != nil {
		return state, err
	}
	var err error
	state.live, state.present, err = voiceAuthReadConnection(ctx, tx, target.ConnectionGeneration, true)
	if err != nil {
		return state, err
	}
	state.closure, state.closed, err = voiceAuthReadConnectionClosure(ctx, tx, target.ConnectionGeneration)
	if err != nil {
		return state, err
	}
	state.retired, err = voiceAuthRetiredBoot(ctx, tx, target.NodeBootID)
	if err != nil {
		return state, err
	}
	if state.present && (state.closed || state.retired) {
		return state, voiceAuthFail(http.StatusConflict)
	}
	return state, nil
}

// ReleaseConnection authenticates only the service and the exact retained
// binding. It deliberately never consults the principal, SID, or live family.
func (h *voiceEnforcementSessionHandler) ReleaseConnection(c *gin.Context) {
	if h == nil || h.db == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	request, _, err := h.readVoiceAuth(c, protocol.EndpointConnectionRelease)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	value, ok := voiceAuthPayload(c, request.ConnectionRelease)
	if !ok {
		return
	}
	binding := protocol.Binding{UserID: value.UserID, FamilyID: value.FamilyID, Target: value.Target}
	ctx := c.Request.Context()
	response, err := h.runVoiceAuth(ctx, request, func(tx *sql.Tx) (protocol.ResponsePayload, error) {
		state, err := voiceAuthConnectionForClose(ctx, tx, value.Target)
		if err != nil {
			return nil, err
		}
		if err = protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()); err != nil {
			return nil, err
		}
		if err = voiceAuthNoAssociations(ctx, tx, value.Target.ConnectionGeneration); err != nil {
			return nil, err
		}
		return nil, releaseVoiceAuthConnectionTx(ctx, tx, state, binding, value.Target.ConnectionGeneration)
	})
	writeVoiceAuth(c, response, err)
}

// releaseVoiceAuthConnectionTx closes a bound live connection; a replay of the
// same bound closure succeeds and any other state is refused.
func releaseVoiceAuthConnectionTx(ctx context.Context, tx *sql.Tx, state voiceAuthCloseState, binding protocol.Binding, generation string) error {
	if state.closed {
		if state.closure.variant != "bound" || state.closure.binding != binding {
			return voiceAuthFail(http.StatusConflict)
		}
		return nil
	}
	if !state.present {
		if state.retired {
			return voiceAuthFail(http.StatusGone)
		}
		return voiceAuthFail(http.StatusConflict)
	}
	if state.live.binding != binding {
		return voiceAuthFail(http.StatusConflict)
	}
	if err := voiceAuthInsertConnectionClosure(ctx, tx, state.live); err != nil {
		return err
	}
	return voiceAuthDeleteConnection(ctx, tx, generation)
}

// ProvisionalClose recovers the binding exclusively from persisted original
// registration commitments, including when the initial response was lost.
func (h *voiceEnforcementSessionHandler) ProvisionalClose(c *gin.Context) {
	if h == nil || h.db == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	request, _, err := h.readVoiceAuth(c, protocol.EndpointProvisionalClose)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	value, ok := voiceAuthPayload(c, request.ProvisionalClose)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	response, err := h.runVoiceAuth(ctx, request, func(tx *sql.Tx) (protocol.ResponsePayload, error) {
		state, err := voiceAuthConnectionForClose(ctx, tx, value.Target)
		if err != nil {
			return nil, err
		}
		if err = protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()); err != nil {
			return nil, err
		}
		if err = voiceAuthNoAssociations(ctx, tx, value.Target.ConnectionGeneration); err != nil {
			return nil, err
		}
		return voiceAuthProvisionalCloseTx(ctx, tx, value, state)
	})
	writeVoiceAuth(c, response, err)
}
func voiceAuthProvisionalCloseTx(ctx context.Context, tx *sql.Tx, value protocol.ProvisionalClose, state voiceAuthCloseState) (protocol.ResponsePayload, error) {
	result := protocol.ProvisionalCloseResult{Schema: "concord.auth-connection-provisional-close-result", Version: 1, Target: value.Target, RegistrationTokenDigest: value.RegistrationTokenDigest, RegistrationBodyDigest: value.RegistrationBodyDigest}
	evidence := voiceAuthConnection{binding: protocol.Binding{Target: value.Target}, tokenDigest: value.RegistrationTokenDigest, bodyDigest: value.RegistrationBodyDigest, variant: "unregistered"}
	if state.present {
		evidence = state.live
	} else if state.closed {
		evidence = state.closure
	}
	if evidence.binding.Target != value.Target || evidence.tokenDigest != value.RegistrationTokenDigest || evidence.bodyDigest != value.RegistrationBodyDigest {
		return nil, voiceAuthFail(http.StatusConflict)
	}
	if !state.present && !state.closed && state.retired {
		result.Disposition = "retired_boot"
		return result, nil
	}
	if !state.closed {
		if err := voiceAuthInsertConnectionClosure(ctx, tx, evidence); err != nil {
			return nil, err
		}
		if state.present {
			if err := voiceAuthDeleteConnection(ctx, tx, value.Target.ConnectionGeneration); err != nil {
				return nil, err
			}
		}
	}
	result.Disposition = "unregistered"
	if evidence.variant == "bound" {
		result.Disposition = "registered"
		binding := evidence.binding
		result.Binding = &binding
	}
	return result, nil
}

type voiceAuthRoomEvidence struct {
	binding protocol.Binding
	room    protocol.Room
}

func voiceAuthReadRoomClosure(ctx context.Context, tx *sql.Tx, generation string) (voiceAuthRoomEvidence, bool, error) {
	var v voiceAuthRoomEvidence
	b, r := &v.binding, &v.room
	err := tx.QueryRowContext(ctx, `SELECT user_id,family_id,connection_generation,node_boot_id,socket_id,credential_epoch,session_generation,room_id,room_kind FROM voice_auth_room_closures WHERE session_generation=$1`, generation).Scan(&b.UserID, &b.FamilyID, &b.Target.ConnectionGeneration, &b.Target.NodeBootID, &b.Target.SocketID, &b.Target.CredentialEpoch, &r.SessionGeneration, &r.RoomID, &r.RoomKind)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}
func voiceAuthInsertRoomClosure(ctx context.Context, tx *sql.Tx, v voiceAuthRoomEvidence) error {
	b, r := v.binding, v.room
	_, err := tx.ExecContext(ctx, `INSERT INTO voice_auth_room_closures(session_generation,connection_generation,node_boot_id,socket_id,credential_epoch,user_id,family_id,room_id,room_kind,closed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp())`, r.SessionGeneration, b.Target.ConnectionGeneration, b.Target.NodeBootID, b.Target.SocketID, b.Target.CredentialEpoch, b.UserID, b.FamilyID, r.RoomID, r.RoomKind)
	return err
}
func voiceAuthReadRoom(ctx context.Context, tx *sql.Tx, generation string) (voiceEnforcementSessionRequest, bool, error) {
	var v voiceEnforcementSessionRequest
	err := tx.QueryRowContext(ctx, `SELECT session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id FROM voice_enforcement_sessions WHERE session_generation=$1 FOR UPDATE`, generation).Scan(&v.SessionGeneration, &v.NodeBootID, &v.RoomID, &v.RoomKind, &v.UserID, &v.CredentialEpoch, &v.SocketID)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}
func voiceAuthReadAssociation(ctx context.Context, tx *sql.Tx, generation string) (string, bool, error) {
	var parent string
	err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM voice_auth_room_bindings WHERE session_generation=$1`, generation).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return parent, err == nil, err
}
func voiceAuthRoomTuple(binding protocol.Binding, room protocol.Room) voiceEnforcementSessionRequest {
	return voiceEnforcementSessionRequest{SessionGeneration: room.SessionGeneration, NodeBootID: binding.Target.NodeBootID, RoomID: room.RoomID, RoomKind: room.RoomKind, UserID: binding.UserID, CredentialEpoch: binding.Target.CredentialEpoch, SocketID: binding.Target.SocketID}
}
func (h *voiceEnforcementSessionHandler) releaseAuthRoom(c *gin.Context, body []byte) {
	request, err := h.decodeVoiceAuth(c, protocol.EndpointRoomRelease, body)
	if err != nil {
		c.Status(voiceAuthStatus(err))
		return
	}
	value, ok := voiceAuthPayload(c, request.RoomRelease)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	response, err := h.runVoiceAuth(ctx, request, func(tx *sql.Tx) (protocol.ResponsePayload, error) {
		state, err := voiceAuthConnectionForClose(ctx, tx, value.Binding.Target)
		if err != nil {
			return nil, err
		}
		if err = voiceAuthGeneration(ctx, tx, "room", value.Room.SessionGeneration); err != nil {
			return nil, err
		}
		if err = protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()); err != nil {
			return nil, err
		}
		return nil, voiceAuthReleaseRoomTx(ctx, tx, value, state, func() error { return protocol.VerifyHTTPRequest(request, h.authSecret, time.Now()) })
	})
	writeVoiceAuth(c, response, err)
}
func voiceAuthReleaseRoomTx(ctx context.Context, tx *sql.Tx, value protocol.RoomRelease, state voiceAuthCloseState, fresh func() error) error {
	expected := voiceAuthRoomEvidence{binding: value.Binding, room: value.Room}
	closure, closed, err := voiceAuthReadRoomClosure(ctx, tx, value.Room.SessionGeneration)
	if err != nil {
		return err
	}
	if closed && closure != expected {
		return voiceAuthFail(http.StatusConflict)
	}
	row, present, err := voiceAuthReadRoom(ctx, tx, value.Room.SessionGeneration)
	if err != nil {
		return err
	}
	parent, bound, err := voiceAuthReadAssociation(ctx, tx, value.Room.SessionGeneration)
	if err != nil {
		return err
	}
	if err = fresh(); err != nil {
		return err
	}
	if (present && row != voiceAuthRoomTuple(value.Binding, value.Room)) || (bound && parent != value.Binding.Target.ConnectionGeneration) || present != bound || (closed && present) {
		return voiceAuthFail(http.StatusConflict)
	}
	if err = voiceAuthRoomReleaseParent(value.Binding, state, present); err != nil {
		return err
	}
	if closed {
		return nil
	}
	if state.retired {
		return voiceAuthFail(http.StatusGone)
	}
	if err = voiceAuthInsertRoomClosure(ctx, tx, expected); err != nil {
		return err
	}
	if !present {
		return nil
	}
	return voiceAuthDeleteRoom(ctx, tx, value)
}

// voiceAuthRoomReleaseParent requires the parent connection to match the release binding.
func voiceAuthRoomReleaseParent(binding protocol.Binding, state voiceAuthCloseState, roomPresent bool) error {
	if state.present && state.live.binding != binding {
		return voiceAuthFail(http.StatusConflict)
	}
	if state.closed && (state.closure.variant != "bound" || state.closure.binding != binding) {
		return voiceAuthFail(http.StatusConflict)
	}
	if !state.present && !state.closed {
		if state.retired && !roomPresent {
			return voiceAuthFail(http.StatusGone)
		}
		return voiceAuthFail(http.StatusConflict)
	}
	return nil
}

func voiceAuthDeleteRoom(ctx context.Context, tx *sql.Tx, value protocol.RoomRelease) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM voice_auth_room_bindings WHERE session_generation=$1 AND connection_generation=$2`, value.Room.SessionGeneration, value.Binding.Target.ConnectionGeneration)
	if err != nil {
		return err
	}
	if err = voiceAuthOneChanged(result); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM voice_enforcement_sessions WHERE session_generation=$1`, value.Room.SessionGeneration)
	if err != nil {
		return err
	}
	return voiceAuthOneChanged(result)
}
