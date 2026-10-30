//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceEnforcementSessionHandlersPersistAndReleaseExactRows(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	_, channelID := setupVoiceEnforcementChannel(t, r.db, r.userID)
	request := integrationVoiceSessionRequest(r, channelID, "channel")
	response, signedRequest := r.request(t, protocol.EndpointRoomRegister, request, r.token)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	r.verify(t, protocol.EndpointRoomRegister, response, signedRequest, protocol.ExpectedResponse{Binding: &binding})
	requireVoiceEnforcementRows(t, r, request, 1)
	retry, retryRequest := r.request(t, protocol.EndpointRoomRegister, request, r.token)
	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	r.verify(t, protocol.EndpointRoomRegister, retry, retryRequest, protocol.ExpectedResponse{Binding: &binding})
	conflicting := request
	conflicting.SocketID = "coverage-socket-conflict"
	conflict, _ := r.request(t, protocol.EndpointRoomRegister, conflicting, r.token)
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	requireVoiceEnforcementRows(t, r, request, 1)
	release := protocol.RoomRelease{Schema: "concord.auth-room-binding", Version: 1, Binding: binding, Room: protocol.Room{SessionGeneration: request.SessionGeneration, RoomID: request.RoomID, RoomKind: request.RoomKind}}
	wrongSocket := release
	wrongSocket.Binding.Target.SocketID = "coverage-socket-conflict"
	conflict, _ = r.request(t, protocol.EndpointRoomRelease, wrongSocket, "")
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	requireVoiceEnforcementRows(t, r, request, 1)
	for range 2 {
		released, releaseRequest := r.request(t, protocol.EndpointRoomRelease, release, "")
		require.Equal(t, http.StatusNoContent, released.Code, released.Body.String())
		r.verify(t, protocol.EndpointRoomRelease, released, releaseRequest, protocol.ExpectedResponse{Binding: &binding})
	}
	requireVoiceEnforcementRows(t, r, request, 0)
	var closures int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_room_closures WHERE session_generation=$1`, request.SessionGeneration).Scan(&closures))
	assert.Equal(t, 1, closures, "release retains immutable exact closure evidence")
}

// A valid bearer and service-hop proof must not make the old epoch-only body
// productive. The same caller's protocol-4 request succeeds as a control.
func TestVoiceEnforcementRegisterRejectsLegacyBodyWithProductiveIdentity(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	_, channelID := setupVoiceEnforcementChannel(t, r.db, r.userID)
	request := integrationVoiceSessionRequest(r, channelID, "channel")
	legacy := voiceEnforcementSessionRequest{SessionGeneration: request.SessionGeneration, NodeBootID: request.NodeBootID, RoomID: request.RoomID, RoomKind: request.RoomKind, UserID: r.userID.String(), CredentialEpoch: r.epoch, SocketID: request.SocketID}
	body, err := json.Marshal(legacy)
	require.NoError(t, err)
	wire, _ := r.signed(t, protocol.EndpointRoomRegister, request, r.token)
	wire.Body = body
	req := httptest.NewRequest(wire.Method, wire.Path, bytes.NewReader(wire.Body))
	for _, header := range wire.Headers {
		req.Header.Add(header.Name, header.Value)
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(voiceEnforcementRegistrationTimestampHeader, timestamp)
	req.Header.Set(voiceEnforcementRegistrationProofHeader, mediaproof.Sign(mediaproof.DeriveKey(r.secret, voiceEnforcementRegistrationProofContext), voiceEnforcementRegistrationProofVersion, timestamp, mediaproof.TokenDigest(r.token), legacy.SessionGeneration, legacy.NodeBootID, legacy.RoomID, legacy.RoomKind, legacy.UserID, legacy.CredentialEpoch, legacy.SocketID))
	response := httptest.NewRecorder()
	r.router.ServeHTTP(response, req)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	requireVoiceEnforcementRows(t, r, request, 0)
	control, signedRequest := r.request(t, protocol.EndpointRoomRegister, request, r.token)
	require.Equal(t, http.StatusOK, control.Code, control.Body.String())
	r.verify(t, protocol.EndpointRoomRegister, control, signedRequest, protocol.ExpectedResponse{Binding: &binding})
	requireVoiceEnforcementRows(t, r, request, 1)
}
