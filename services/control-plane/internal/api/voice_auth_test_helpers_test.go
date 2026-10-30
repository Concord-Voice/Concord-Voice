package api

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type voiceAuthRig struct {
	db               *sql.DB
	secret           string
	router           *gin.Engine
	userID, familyID uuid.UUID
	epoch, token     string
	target           protocol.Target
}

type voiceAuthFixtureState uint8

const (
	voiceAuthFixtureActive voiceAuthFixtureState = iota
	voiceAuthFixturePrepared
)

func newVoiceAuthRig(t *testing.T) *voiceAuthRig {
	t.Helper()
	return newVoiceAuthRigAt(t, time.Now())
}

func newVoiceAuthRigAt(t *testing.T, mintTime time.Time) *voiceAuthRig {
	t.Helper()
	return newVoiceAuthRigState(t, mintTime, voiceAuthFixtureActive)
}

func newVoiceAuthRigPrepared(t *testing.T) *voiceAuthRig {
	t.Helper()
	return newVoiceAuthRigState(t, time.Now(), voiceAuthFixturePrepared)
}

func newVoiceAuthRigState(t *testing.T, mintTime time.Time, state voiceAuthFixtureState) *voiceAuthRig {
	t.Helper()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit isolated database required")
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	// The fixture owns the isolated rollout/boot state; this is not activation.
	_, err := testdb.ExecReplica(db, `TRUNCATE login_family_rollout,login_family_revocations,login_families,login_family_sids,voice_auth_connections,voice_enforcement_sessions,voice_auth_boot_fences CASCADE`)
	require.NoError(t, err)
	// Select the disposable singleton's initial state before the real mint.
	// An active fixture is never changed back into a prepared fixture.
	switch state {
	case voiceAuthFixtureActive:
		_, err = db.Exec(`INSERT INTO login_family_rollout(id,activated_at,receipt) VALUES(true,NOW(),'{}')`)
	case voiceAuthFixturePrepared:
		_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES(true)`)
	default:
		t.Fatalf("unknown voice auth fixture state: %d", state)
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := testdb.ExecReplica(db, `TRUNCATE login_family_rollout,login_family_revocations,login_families,login_family_sids,voice_auth_connections,voice_enforcement_sessions,voice_auth_boot_fences CASCADE`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES(true)`)
		require.NoError(t, err)
	})
	secret := make([]byte, 32)
	_, err = rand.Read(secret)
	require.NoError(t, err)
	r := &voiceAuthRig{db: db, secret: hex.EncodeToString(secret), userID: testdb.CreateUser(t, db)}
	r.epoch, err = credepoch.NewEpoch()
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, r.userID, r.epoch)
	require.NoError(t, err)
	sid := uuid.New()
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: r.userID, SID: sid, CredentialEpoch: r.epoch, EmailVerified: true}, mintTime)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	})
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(t.Context(), tx, user, r.epoch)
	require.NoError(t, err)
	r.familyID = family.Binding().FamilyID
	require.NoError(t, loginfamily.BindSIDTx(t.Context(), tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	r.token, err = prepared.Sign(r.secret)
	require.NoError(t, err)
	r.target = protocol.Target{ConnectionGeneration: uuid.NewString(), NodeBootID: uuid.NewString(), SocketID: "registry-test-socket", CredentialEpoch: r.epoch}
	_, err = db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, r.target.NodeBootID)
	require.NoError(t, err)
	rc := redistest.Client(t)
	h := newVoiceEnforcementSessionHandler(db, r.secret)
	r.router = gin.New()
	registerVoiceAuthAliases(r.router.Group("/api/v1"))
	productive := r.router.Group("", middleware.AuthRequired(r.secret, rc, db, credepoch.New(db, rc, nil)), middleware.MediaPlaneServiceHop(r.secret, nil), middleware.RequireVerifiedEmail())
	productive.POST("/api/v1/internal/voice/auth-connections", h.RegisterConnection)
	productive.POST("/api/v1/voice/enforcement-sessions", h.Register)
	r.router.POST("/api/v1/internal/voice/auth-connections/release", h.ReleaseConnection)
	r.router.POST("/api/v1/internal/voice/auth-connections/provisional-close", h.ProvisionalClose)
	r.router.POST("/api/v1/internal/voice/enforcement-sessions/release", h.Release)
	return r
}

func TestVoiceAuthRigSetup_RemovesRetainedLegacyVoiceSessions(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit isolated database required")
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	generation := uuid.New()
	_, err := db.Exec(`INSERT INTO voice_enforcement_sessions
		(session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id)
		VALUES($1,$2,$3,'channel',$4,'','retained-fixture-socket')`, generation, uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation=$1`, generation).Scan(&count))
	require.Equal(t, 1, count)

	r := newVoiceAuthRigPrepared(t)

	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_enforcement_sessions WHERE session_generation=$1`, generation).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, r.db.QueryRow(`SELECT (SELECT count(*) FROM voice_auth_connections)+(SELECT count(*) FROM voice_enforcement_sessions)+(SELECT count(*) FROM voice_auth_room_bindings)`).Scan(&count))
	require.Zero(t, count)
	var prepared bool
	require.NoError(t, r.db.QueryRow(`SELECT activated_at IS NULL FROM login_family_rollout WHERE id=true`).Scan(&prepared))
	require.True(t, prepared)
}

func (r *voiceAuthRig) registration() protocol.ConnectionRegister {
	return protocol.ConnectionRegister{Version: 1, ConnectionGeneration: r.target.ConnectionGeneration, NodeBootID: r.target.NodeBootID, SocketID: r.target.SocketID, CredentialEpoch: r.target.CredentialEpoch}
}
func (r *voiceAuthRig) signed(t *testing.T, e protocol.Endpoint, payload any, token string) (protocol.HTTPRequest, protocol.Request) {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	nonce := make([]byte, 32)
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	wire, request, err := protocol.SignHTTPRequest(e, body, token, protocol.AttemptMeta{Timestamp: timestamp, Nonce: hex.EncodeToString(nonce)}, []byte(r.secret))
	require.NoError(t, err)
	if token != "" {
		wire.Headers = append(wire.Headers, protocol.Header{Name: "X-Concord-Service-Timestamp", Value: timestamp}, protocol.Header{Name: "X-Concord-Service-Proof", Value: mediaproof.Sign(mediaproof.DeriveKey(r.secret, "concord/media-plane-service-hop/v1"), "v1", timestamp, wire.Method, wire.Path, mediaproof.TokenDigest(token))})
	}
	return wire, request
}
func (r *voiceAuthRig) do(wire protocol.HTTPRequest) *httptest.ResponseRecorder {
	req := httptest.NewRequest(wire.Method, wire.Path, bytes.NewReader(wire.Body))
	for _, h := range wire.Headers {
		req.Header.Add(h.Name, h.Value)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.router.ServeHTTP(w, req)
	return w
}
func (r *voiceAuthRig) request(t *testing.T, e protocol.Endpoint, payload any, token string) (*httptest.ResponseRecorder, protocol.Request) {
	t.Helper()
	wire, request := r.signed(t, e, payload, token)
	return r.do(wire), request
}
func voiceAuthResponse(w *httptest.ResponseRecorder) protocol.HTTPResponse {
	headers := []protocol.Header{}
	for name, values := range w.Header() {
		for _, value := range values {
			headers = append(headers, protocol.Header{Name: name, Value: value})
		}
	}
	return protocol.HTTPResponse{Status: w.Code, Headers: headers, Body: w.Body.Bytes()}
}
func (r *voiceAuthRig) verify(t *testing.T, e protocol.Endpoint, w *httptest.ResponseRecorder, request protocol.Request, expected protocol.ExpectedResponse) protocol.Response {
	t.Helper()
	ctx, err := protocol.NewRequestContext(request, expected)
	require.NoError(t, err)
	response, err := protocol.VerifyHTTPResponse(e, voiceAuthResponse(w), ctx, []byte(r.secret), time.Now())
	require.NoError(t, err)
	return response
}
func (r *voiceAuthRig) register(t *testing.T) protocol.Binding {
	t.Helper()
	w, request := r.request(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	response := r.verify(t, protocol.EndpointConnectionRegister, w, request, protocol.ExpectedResponse{UserID: r.userID.String()})
	value, ok := response.Registration()
	require.True(t, ok)
	return protocol.Binding{UserID: value.UserID, FamilyID: value.FamilyID, Target: value.Target}
}
