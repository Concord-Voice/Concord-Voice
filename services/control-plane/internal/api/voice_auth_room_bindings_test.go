package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func roomBindingFixture(t *testing.T, r *voiceAuthRig, kind string) protocol.RoomRegister {
	t.Helper()
	resource := uuid.New()
	if kind == "channel" {
		server, role := uuid.New(), uuid.New()
		_, err := r.db.Exec(`INSERT INTO servers(id,name,owner_id) VALUES($1,'room registry',$2)`, server, r.userID)
		require.NoError(t, err)
		_, err = r.db.Exec(`INSERT INTO server_members(server_id,user_id) VALUES($1,$2)`, server, r.userID)
		require.NoError(t, err)
		_, err = r.db.Exec(`INSERT INTO roles(id,server_id,name,position,permissions,is_default,is_managed) VALUES($1,$2,'@all',0,$3,true,true)`, role, server, int64(rbac.BasePermissions))
		require.NoError(t, err)
		_, err = r.db.Exec(`INSERT INTO member_roles(server_id,user_id,role_id) VALUES($1,$2,$3)`, server, r.userID, role)
		require.NoError(t, err)
		_, err = r.db.Exec(`INSERT INTO channels(id,server_id,name,type) VALUES($1,$2,'voice','voice')`, resource, server)
		require.NoError(t, err)
	} else {
		peer := testdb.CreateUser(t, r.db)
		_, err := r.db.Exec(`INSERT INTO friendships(requester_id,addressee_id,status) VALUES($1,$2,'accepted')`, r.userID, peer)
		require.NoError(t, err)
		_, err = r.db.Exec(`INSERT INTO dm_conversations(id,is_group,is_personal,created_by) VALUES($1,false,true,$2)`, resource, r.userID)
		require.NoError(t, err)
		_, err = r.db.Exec(`INSERT INTO dm_participants(conversation_id,user_id) VALUES($1,$2),($1,$3)`, resource, r.userID, peer)
		require.NoError(t, err)
	}
	return protocol.RoomRegister{Schema: "concord.auth-room-binding", Version: 1, Mode: "attach", ConnectionGeneration: r.target.ConnectionGeneration, NodeBootID: r.target.NodeBootID, SocketID: r.target.SocketID, CredentialEpoch: r.epoch, SessionGeneration: uuid.NewString(), RoomID: resource.String(), RoomKind: kind}
}
func roomBindingTuple(p protocol.RoomRegister) protocol.Room {
	return protocol.Room{SessionGeneration: p.SessionGeneration, RoomID: p.RoomID, RoomKind: p.RoomKind}
}
func roomBindingRelease(b protocol.Binding, p protocol.RoomRegister) protocol.RoomRelease {
	return protocol.RoomRelease{Schema: "concord.auth-room-binding", Version: 1, Binding: b, Room: roomBindingTuple(p)}
}
func roomBindingCount(t *testing.T, r *voiceAuthRig, table, generation string) int {
	t.Helper()
	var n int
	// Table is chosen exclusively by this test's fixed callers.
	require.Contains(t, []string{"voice_auth_room_bindings", "voice_enforcement_sessions", "voice_auth_room_closures"}, table)
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE session_generation=$1`, generation).Scan(&n))
	return n
}
func roomBindingAttach(t *testing.T, r *voiceAuthRig, b protocol.Binding, p protocol.RoomRegister) {
	t.Helper()
	w, req := r.request(t, protocol.EndpointRoomRegister, p, r.token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	response := r.verify(t, protocol.EndpointRoomRegister, w, req, protocol.ExpectedResponse{Binding: &b})
	value, ok := response.RoomRegistration()
	require.True(t, ok)
	require.Equal(t, b, value.Binding)
	require.Equal(t, roomBindingTuple(p), value.Room)
	require.Equal(t, 1, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
	require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
	var parent string
	require.NoError(t, r.db.QueryRow(`SELECT connection_generation FROM voice_auth_room_bindings WHERE session_generation=$1`, p.SessionGeneration).Scan(&parent))
	require.Equal(t, b.Target.ConnectionGeneration, parent)
}

func TestVoiceAuthRoomBindingChannelAndDM(t *testing.T) {
	for _, kind := range []string{"channel", "dm"} {
		t.Run(kind, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			b := r.register(t)
			p := roomBindingFixture(t, r, kind)
			roomBindingAttach(t, r, b, p)
			p.Mode = "renew"
			roomBindingAttach(t, r, b, p)
			w, _ := r.request(t, protocol.EndpointConnectionRelease, protocol.ConnectionRelease{Version: 1, UserID: b.UserID, FamilyID: b.FamilyID, Target: b.Target}, "")
			require.Equal(t, http.StatusConflict, w.Code)
			require.Empty(t, w.Body.String())
			release := roomBindingRelease(b, p)
			w, req := r.request(t, protocol.EndpointRoomRelease, release, "")
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			r.verify(t, protocol.EndpointRoomRelease, w, req, protocol.ExpectedResponse{Binding: &b})
			require.Zero(t, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
			require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
			require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_closures", p.SessionGeneration))
			w, req = r.request(t, protocol.EndpointRoomRelease, release, "")
			require.Equal(t, http.StatusNoContent, w.Code)
			r.verify(t, protocol.EndpointRoomRelease, w, req, protocol.ExpectedResponse{Binding: &b})
			for _, mode := range []string{"attach", "renew"} {
				p.Mode = mode
				w, _ = r.request(t, protocol.EndpointRoomRegister, p, r.token)
				require.Equal(t, http.StatusConflict, w.Code)
				require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
			}
		})
	}
}

func TestVoiceAuthRoomRenewMissingNeverInserts(t *testing.T) {
	r := newVoiceAuthRig(t)
	r.register(t)
	p := roomBindingFixture(t, r, "channel")
	p.Mode = "renew"
	w, _ := r.request(t, protocol.EndpointRoomRegister, p, r.token)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Zero(t, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
	require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
}
func TestVoiceAuthRoomCloseBeforeAttach(t *testing.T) {
	r := newVoiceAuthRig(t)
	b := r.register(t)
	p := roomBindingFixture(t, r, "channel")
	delayed, _ := r.signed(t, protocol.EndpointRoomRegister, p, r.token)
	w, req := r.request(t, protocol.EndpointRoomRelease, roomBindingRelease(b, p), "")
	require.Equal(t, http.StatusNoContent, w.Code)
	r.verify(t, protocol.EndpointRoomRelease, w, req, protocol.ExpectedResponse{Binding: &b})
	w = r.do(delayed)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
}
func TestVoiceAuthRoomExactBindingConflicts(t *testing.T) {
	for _, field := range []string{"parent", "boot", "socket", "epoch", "user", "family", "room", "kind"} {
		t.Run(field, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			b := r.register(t)
			p := roomBindingFixture(t, r, "channel")
			roomBindingAttach(t, r, b, p)
			release := roomBindingRelease(b, p)
			switch field {
			case "parent":
				release.Binding.Target.ConnectionGeneration = uuid.NewString()
			case "boot":
				release.Binding.Target.NodeBootID = uuid.NewString()
			case "socket":
				release.Binding.Target.SocketID = "other"
			case "epoch":
				release.Binding.Target.CredentialEpoch = "11111111111111111111111111111111"
			case "user":
				release.Binding.UserID = uuid.NewString()
			case "family":
				release.Binding.FamilyID = uuid.NewString()
			case "room":
				release.Room.RoomID = uuid.NewString()
			case "kind":
				release.Room.RoomKind = "dm"
			}
			w, _ := r.request(t, protocol.EndpointRoomRelease, release, "")
			require.Equal(t, http.StatusConflict, w.Code)
			require.Empty(t, w.Body.String())
			require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
			require.Zero(t, roomBindingCount(t, r, "voice_auth_room_closures", p.SessionGeneration))
		})
	}
}
func TestVoiceAuthRoomFreshResourceAuthorityOnRenew(t *testing.T) {
	for _, kind := range []string{"channel", "dm"} {
		t.Run(kind, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			b := r.register(t)
			p := roomBindingFixture(t, r, kind)
			roomBindingAttach(t, r, b, p)
			var err error
			if kind == "channel" {
				_, err = r.db.Exec(`DELETE FROM server_members WHERE user_id=$1`, r.userID)
			} else {
				_, err = r.db.Exec(`DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, p.RoomID, r.userID)
			}
			require.NoError(t, err)
			p.Mode = "renew"
			w, _ := r.request(t, protocol.EndpointRoomRegister, p, r.token)
			require.Equal(t, http.StatusForbidden, w.Code)
			require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
		})
	}
}
func roomBindingTerminalize(t *testing.T, r *voiceAuthRig) {
	t.Helper()
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	targets, err := loginfamily.CaptureUserTargetsTx(t.Context(), tx, user, nil)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(t.Context(), tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonErasure})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}
func TestVoiceAuthRoomReleaseAfterTerminalAndErasure(t *testing.T) {
	for _, erase := range []bool{false, true} {
		t.Run(strconv.FormatBool(erase), func(t *testing.T) {
			r := newVoiceAuthRig(t)
			b := r.register(t)
			p := roomBindingFixture(t, r, "channel")
			roomBindingAttach(t, r, b, p)
			roomBindingTerminalize(t, r)
			if erase {
				_, err := r.db.Exec(`DELETE FROM users WHERE id=$1`, r.userID)
				require.NoError(t, err)
			}
			p.Mode = "renew"
			w, _ := r.request(t, protocol.EndpointRoomRegister, p, r.token)
			require.NotEqual(t, http.StatusOK, w.Code)
			w, req := r.request(t, protocol.EndpointRoomRelease, roomBindingRelease(b, p), "")
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			r.verify(t, protocol.EndpointRoomRelease, w, req, protocol.ExpectedResponse{Binding: &b})
			require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
			require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_closures", p.SessionGeneration))
		})
	}
}
func TestVoiceAuthRoomSignedResponseBindsAttemptAndBody(t *testing.T) {
	r := newVoiceAuthRig(t)
	b := r.register(t)
	p := roomBindingFixture(t, r, "channel")
	w, req := r.request(t, protocol.EndpointRoomRegister, p, r.token)
	require.Equal(t, http.StatusOK, w.Code)
	ctx, err := protocol.NewRequestContext(req, protocol.ExpectedResponse{Binding: &b})
	require.NoError(t, err)
	wire := voiceAuthResponse(w)
	var result map[string]any
	require.NoError(t, json.Unmarshal(wire.Body, &result))
	result["voiceEnforcementProtocol"] = 3
	wire.Body, err = json.Marshal(result)
	require.NoError(t, err)
	_, err = protocol.VerifyHTTPResponse(protocol.EndpointRoomRegister, wire, ctx, []byte(r.secret), time.Now())
	require.Error(t, err)
	_, other := r.signed(t, protocol.EndpointRoomRegister, p, r.token)
	otherCtx, err := protocol.NewRequestContext(other, protocol.ExpectedResponse{Binding: &b})
	require.NoError(t, err)
	_, err = protocol.VerifyHTTPResponse(protocol.EndpointRoomRegister, voiceAuthResponse(w), otherCtx, []byte(r.secret), time.Now())
	require.Error(t, err)
}

func TestVoiceAuthRoomProofExpiresWhileWaitingForGeneration(t *testing.T) {
	r := newVoiceAuthRig(t)
	r.register(t)
	p := roomBindingFixture(t, r, "channel")
	body, err := json.Marshal(p)
	require.NoError(t, err)
	// At receipt this proof is valid, but only for two further seconds.
	wire, _, err := protocol.SignHTTPRequest(protocol.EndpointRoomRegister, body, r.token, protocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Add(-28*time.Second).Unix(), 10), Nonce: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, []byte(r.secret))
	require.NoError(t, err)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	wire.Headers = append(wire.Headers, protocol.Header{Name: "X-Concord-Service-Timestamp", Value: now}, protocol.Header{Name: "X-Concord-Service-Proof", Value: mediaproof.Sign(mediaproof.DeriveKey(r.secret, "concord/media-plane-service-hop/v1"), "v1", now, wire.Method, wire.Path, mediaproof.TokenDigest(r.token))})
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`SELECT login_family_generation_lock('connection',$1::uuid)`, r.target.ConnectionGeneration)
	require.NoError(t, err)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.do(wire) }()
	require.Eventually(t, func() bool {
		var n int
		e := r.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%login_family_generation_lock%'`).Scan(&n)
		return e == nil && n > 0
	}, time.Second, 10*time.Millisecond)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case early := <-done:
		t.Fatalf("request escaped generation lock: %d", early.Code)
	case <-timer.C:
	}
	require.NoError(t, tx.Commit())
	select {
	case w := <-done:
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
	case <-time.After(3 * time.Second):
		t.Fatal("request remained blocked after generation unlock")
	}
}

func TestVoiceAuthRoomTokenExpiresWhileWaitingForGeneration(t *testing.T) {
	for _, mode := range []string{"attach", "renew"} {
		t.Run(mode, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			b := r.register(t)
			p := roomBindingFixture(t, r, "channel")
			if mode == "renew" {
				roomBindingAttach(t, r, b, p)
			}
			p.Mode = mode
			sid := uuid.New()
			prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: r.userID, SID: sid, CredentialEpoch: r.epoch, EmailVerified: true}, time.Now().Add(-15*time.Minute+5*time.Second))
			require.NoError(t, err)
			// Isolated fixture adds a same-family SID with the exact selected NumericDate.
			_, err = r.db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,$4)`, sid, r.familyID, r.userID, prepared.ExpiresAt())
			require.NoError(t, err)
			token, err := prepared.Sign(r.secret)
			require.NoError(t, err)
			tx, err := r.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.Exec(`SELECT login_family_generation_lock('connection',$1::uuid)`, r.target.ConnectionGeneration)
			require.NoError(t, err)
			wire, _ := r.signed(t, protocol.EndpointRoomRegister, p, token)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- r.do(wire) }()
			require.Eventually(t, func() bool {
				var n int
				e := r.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%login_family_generation_lock%'`).Scan(&n)
				return e == nil && n > 0
			}, 2*time.Second, 10*time.Millisecond)
			timer := time.NewTimer(time.Until(prepared.ExpiresAt()) + 100*time.Millisecond)
			defer timer.Stop()
			select {
			case w := <-done:
				t.Fatalf("request escaped generation lock: %d", w.Code)
			case <-timer.C:
			}
			require.NoError(t, tx.Commit())
			select {
			case w := <-done:
				require.Equal(t, http.StatusForbidden, w.Code)
			case <-time.After(3 * time.Second):
				t.Fatal("request remained blocked after unlock")
			}
			want := 0
			if mode == "renew" {
				want = 1
			}
			require.Equal(t, want, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
		})
	}
}

func TestVoiceAuthRoomLegacyDrainCannotDeleteBoundRoom(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(strconv.FormatBool(bound), func(t *testing.T) {
			r := newVoiceAuthRig(t)
			b := r.register(t)
			p := roomBindingFixture(t, r, "channel")
			if bound {
				roomBindingAttach(t, r, b, p)
			} else {
				_, err := r.db.Exec(`INSERT INTO voice_enforcement_sessions(session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.SessionGeneration, p.NodeBootID, p.RoomID, p.RoomKind, r.userID, p.CredentialEpoch, p.SocketID)
				require.NoError(t, err)
			}
			legacy := voiceEnforcementSessionRequest{SessionGeneration: p.SessionGeneration, NodeBootID: p.NodeBootID, RoomID: p.RoomID, RoomKind: p.RoomKind, UserID: r.userID.String(), CredentialEpoch: p.CredentialEpoch, SocketID: p.SocketID}
			raw, err := json.Marshal(legacy)
			require.NoError(t, err)
			stamp := strconv.FormatInt(time.Now().Unix(), 10)
			wire := protocol.HTTPRequest{Method: http.MethodPost, Path: "/api/v1/internal/voice/enforcement-sessions/release", Body: raw, Headers: []protocol.Header{
				{Name: voiceEnforcementReleaseTimestampHeader, Value: stamp},
				{Name: voiceEnforcementReleaseProofHeader, Value: mediaproof.Sign(mediaproof.DeriveKey(r.secret, voiceEnforcementReleaseProofContext), voiceEnforcementReleaseProofVersion, stamp, legacy.SessionGeneration, legacy.NodeBootID, legacy.RoomID, legacy.RoomKind, legacy.UserID, legacy.CredentialEpoch, legacy.SocketID)},
			}}
			w := r.do(wire)
			if bound {
				require.NotEqual(t, http.StatusNoContent, w.Code)
				require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
				require.Equal(t, 1, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
			} else {
				require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
				require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
				require.Zero(t, roomBindingCount(t, r, "voice_auth_room_closures", p.SessionGeneration))
			}
		})
	}
}

func TestVoiceAuthRoomFinalTextChannelDenialIsForbidden(t *testing.T) {
	r := newVoiceAuthRig(t)
	r.register(t)
	p := roomBindingFixture(t, r, "channel")
	_, err := r.db.Exec(`UPDATE channels SET type='text' WHERE id=$1`, p.RoomID)
	require.NoError(t, err)
	// The signed envelope, bearer, family, connection and membership are valid;
	// the final resource guard refuses a resource that exists but is not voice.
	w, _ := r.request(t, protocol.EndpointRoomRegister, p, r.token)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Empty(t, w.Body.String())
	require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
	require.Zero(t, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
}

func TestVoiceAuthRoomFinalResourceSQLFailureIsUnavailable(t *testing.T) {
	r := newVoiceAuthRig(t)
	r.register(t)
	p := roomBindingFixture(t, r, "channel")
	// The channel preflight only reads channels.server_id. Renaming this column
	// leaves that read and the family prefix intact, then deterministically fails
	// the final resource query SELECT id FROM servers. Restore before DB cleanup.
	_, err := r.db.Exec(`ALTER TABLE servers RENAME COLUMN id TO voice_auth_room_test_id`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, restoreErr := r.db.Exec(`ALTER TABLE servers RENAME COLUMN voice_auth_room_test_id TO id`)
		require.NoError(t, restoreErr)
	})
	var serverID string
	require.NoError(t, r.db.QueryRow(`SELECT server_id FROM channels WHERE id=$1`, p.RoomID).Scan(&serverID))
	require.NotEmpty(t, serverID)
	w, _ := r.request(t, protocol.EndpointRoomRegister, p, r.token)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Empty(t, w.Body.String())
	require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
	require.Zero(t, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
}
