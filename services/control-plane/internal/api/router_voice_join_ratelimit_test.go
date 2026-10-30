package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
)

// initializeServiceJoinRollout selects the disposable fixture's initial active
// state before any users or families are minted; it is not rollout evidence.
func initializeServiceJoinRollout(t *testing.T, ts *testhelpers.TestServer) {
	t.Helper()
	_, err := testdb.ExecReplica(ts.DB, `TRUNCATE login_family_rollout`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO login_family_rollout(id,activated_at,receipt) VALUES(TRUE,clock_timestamp(),'{}')`)
	require.NoError(t, err)
}

// serviceJoinFixture installs real family-owned connection and room registry
// evidence through the production strict HTTP boundary before exercising joins.
func serviceJoinFixture(t *testing.T, ts *testhelpers.TestServer, owner testhelpers.TestUser, channelID, socket string) (map[string]interface{}, func() http.Header) {
	t.Helper()
	claims := &auth.Claims{}
	_, err := jwt.ParseWithClaims(owner.AccessToken, claims, func(_ *jwt.Token) (interface{}, error) { return []byte(testhelpers.TestJWTSecret), nil }, jwt.WithValidMethods([]string{"HS256"}))
	require.NoError(t, err)
	epoch := claims.CredentialEpoch
	target := protocol.Target{ConnectionGeneration: uuid.NewString(), NodeBootID: uuid.NewString(), SocketID: socket, CredentialEpoch: epoch}
	_, err = ts.DB.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, target.NodeBootID)
	require.NoError(t, err)
	hop := func(path string) http.Header {
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		h := testhelpers.AuthHeaders(owner.AccessToken)
		h.Set("X-Concord-Service-Timestamp", stamp)
		h.Set("X-Concord-Service-Proof", mediaproof.Sign(mediaproof.DeriveKey(testhelpers.TestJWTSecret, "concord/media-plane-service-hop/v1"), "v1", stamp, http.MethodPost, path, mediaproof.TokenDigest(owner.AccessToken)))
		h.Set("X-Concord-Client-Version", "0.2.44")
		return h
	}
	send := func(endpoint protocol.Endpoint, payload interface{}, expected protocol.ExpectedResponse) protocol.Response {
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		wire, request, err := protocol.SignHTTPRequest(endpoint, body, owner.AccessToken, protocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Unix(), 10), Nonce: mediaproof.TokenDigest(uuid.NewString())}, []byte(testhelpers.TestJWTSecret))
		require.NoError(t, err)
		req := httptest.NewRequest(wire.Method, wire.Path, bytes.NewReader(wire.Body))
		req.Header = hop(wire.Path)
		req.Header.Set("Content-Type", "application/json")
		for _, header := range wire.Headers {
			req.Header.Set(header.Name, header.Value)
		}
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		headers := []protocol.Header{}
		for name, values := range w.Header() {
			for _, value := range values {
				headers = append(headers, protocol.Header{Name: name, Value: value})
			}
		}
		ctx, err := protocol.NewRequestContext(request, expected)
		require.NoError(t, err)
		response, err := protocol.VerifyHTTPResponse(endpoint, protocol.HTTPResponse{Status: w.Code, Headers: headers, Body: w.Body.Bytes()}, ctx, []byte(testhelpers.TestJWTSecret), time.Now())
		require.NoError(t, err)
		return response
	}
	response := send(protocol.EndpointConnectionRegister, protocol.ConnectionRegister{Version: 1, ConnectionGeneration: target.ConnectionGeneration, NodeBootID: target.NodeBootID, SocketID: socket, CredentialEpoch: epoch}, protocol.ExpectedResponse{UserID: owner.ID})
	registered, ok := response.Registration()
	require.True(t, ok)
	binding := protocol.Binding{UserID: registered.UserID, FamilyID: registered.FamilyID, Target: registered.Target}
	room := protocol.RoomRegister{Schema: "concord.auth-room-binding", Version: 1, Mode: "attach", ConnectionGeneration: target.ConnectionGeneration, NodeBootID: target.NodeBootID, SocketID: socket, CredentialEpoch: epoch, SessionGeneration: uuid.NewString(), RoomID: channelID, RoomKind: "channel"}
	response = send(protocol.EndpointRoomRegister, room, protocol.ExpectedResponse{Binding: &binding})
	attached, ok := response.RoomRegistration()
	require.True(t, ok)
	require.Equal(t, binding, attached.Binding)
	var bound int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM voice_auth_room_bindings b JOIN voice_auth_connections c USING(connection_generation) JOIN login_families f USING(family_id) WHERE b.session_generation=$1 AND c.connection_generation=$2 AND f.user_id=$3`, room.SessionGeneration, target.ConnectionGeneration, owner.ID).Scan(&bound))
	require.Equal(t, 1, bound)
	admission := map[string]interface{}{"admission_id": uuid.NewString(), "socket_id": socket, "activate": false}
	path := "/api/v1/channels/" + channelID + "/voice/join"
	return admission, func() http.Header {
		h := hop(path)
		h.Del("X-Concord-Client-Version")
		body, err := json.Marshal(admission)
		require.NoError(t, err)
		digest := sha256.Sum256(body)
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		h.Set("X-Concord-Voice-Enforcement-Node-Boot-ID", target.NodeBootID)
		h.Set("X-Concord-Voice-Enforcement-Capability-Timestamp", stamp)
		h.Set("X-Concord-Voice-Enforcement-Capability-Proof", mediaproof.Sign(mediaproof.DeriveKey(testhelpers.TestJWTSecret, "concord/voice-enforcement-capability/v1"), "v1", stamp, http.MethodPost, path, mediaproof.TokenDigest(owner.AccessToken), target.NodeBootID, fmt.Sprintf("%x", digest)))
		return h
	}
}

func TestVoiceJoinRateLimitCountsVerifiedServiceHopsAsLogicalJoins(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	initializeServiceJoinRollout(t, ts)

	createVoiceChannel := func(username string) (testhelpers.TestUser, string) {
		t.Helper()
		owner := ts.CreateTestUser(t, username)
		serverID := ts.CreateTestServer(t, owner.ID, username+" server")
		channelID := ts.CreateTestChannel(t, serverID, username+" voice")
		_, err := ts.DB.Exec(`UPDATE channels SET type = 'voice' WHERE id = $1`, channelID)
		require.NoError(t, err)
		return owner, "/api/v1/channels/" + channelID + "/voice/join"
	}

	hopHeaders := func(token, path string) http.Header {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		proof := mediaproof.Sign(
			mediaproof.DeriveKey(testhelpers.TestJWTSecret, "concord/media-plane-service-hop/v1"),
			"v1", timestamp, http.MethodPost, path, mediaproof.TokenDigest(token),
		)
		require.NotEmpty(t, proof)
		headers := testhelpers.AuthHeaders(token)
		headers.Set("X-Concord-Service-Timestamp", timestamp)
		headers.Set("X-Concord-Service-Proof", proof)
		return headers
	}

	t.Run("twenty verified service hops fit and the twenty-first is rejected", func(t *testing.T) {
		owner, path := createVoiceChannel("voice_join_service_limit")
		admission, headers := serviceJoinFixture(t, ts, owner, strings.Split(path, "/")[4], "rate-limit-socket")
		for attempt := 0; attempt < 10; attempt++ {
			first := ts.DoRequest(http.MethodPost, path, admission, headers())
			require.Equal(t, http.StatusOK, first.Code, "verified service hop %d", 2*attempt+1)
			assert.Equal(t, "20", first.Header().Get("X-RateLimit-Limit"))

			second := ts.DoRequest(http.MethodPost, path, admission, headers())
			require.Equal(t, http.StatusOK, second.Code, "verified service hop %d", 2*attempt+2)
		}

		twentyFirst := ts.DoRequest(http.MethodPost, path, admission, headers())
		assert.Equal(t, http.StatusTooManyRequests, twentyFirst.Code)
	})

	t.Run("invalid service proof remains in the direct-client budget", func(t *testing.T) {
		owner, path := createVoiceChannel("voice_join_invalid_hop_limit")
		for request := 0; request < 9; request++ {
			response := ts.DoRequest(http.MethodPost, path, nil, testhelpers.AuthHeaders(owner.AccessToken))
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, "10", response.Header().Get("X-RateLimit-Limit"))
		}

		forgedHeaders := hopHeaders(owner.AccessToken, path)
		proof := forgedHeaders.Get("X-Concord-Service-Proof")
		require.NotEmpty(t, proof)
		prefix := "0"
		if proof[0] == '0' {
			prefix = "1"
		}
		forgedHeaders.Set("X-Concord-Service-Proof", prefix+proof[1:])

		tenth := ts.DoRequest(http.MethodPost, path, nil, forgedHeaders)
		assert.Equal(t, http.StatusOK, tenth.Code)
		assert.Equal(t, "10", tenth.Header().Get("X-RateLimit-Limit"))

		eleventh := ts.DoRequest(http.MethodPost, path, nil, forgedHeaders)
		assert.Equal(t, http.StatusTooManyRequests, eleventh.Code)
	})
}
