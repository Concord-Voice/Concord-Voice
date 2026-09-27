package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	rawnats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func admissionNATSTestURL() string {
	if url := os.Getenv("NATS_URL"); url != "" {
		return url
	}
	return "nats://localhost:4222"
}

func TestActivatedAdmissionReleasesInitialFenceBeforePrepareAndRetainsReservation(t *testing.T) {
	bus, err := rawnats.Connect(admissionNATSTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); runs in CI", err)
	}
	t.Cleanup(func() { bus.Close() })
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	owner, member := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	serverID, channelID, roleID := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, insertPendingAdmissionJoinFixture(db, owner, member, serverID, channelID, roleID))
	admissionID, socketID := uuid.NewString(), "a2-socket"
	require.NoError(t, db.QueryRow(`INSERT INTO voice_pending_admissions (channel_id,user_id,admission_id,socket_id,expires_at) VALUES ($1,$2,$3,$4,clock_timestamp()+INTERVAL '30 seconds') RETURNING admission_id`, channelID, member, admissionID, socketID).Scan(&admissionID))
	control, err := natsclient.Connect(admissionNATSTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = control.Close() })
	prepared := make(chan *rawnats.Msg, 1)
	sub, err := bus.Subscribe(voiceAdmissionActivationSubject, func(msg *rawnats.Msg) { prepared <- msg })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, bus.Flush())
	h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test"), Cfg: &config.Config{JWTSecret: "test-secret"}, NATS: control, Resolver: rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), logger.New("test")), EntCache: entitlements.NewCache(redisClient, db)})
	r := gin.New()
	r.POST("/channels/:id/voice/join", func(c *gin.Context) {
		c.Set("user_id", member.String())
		c.Set("concord_service_hop", true)
		h.AuthorizeJoin(c)
	})
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/channels/"+channelID.String()+"/voice/join", bytes.NewBufferString(fmt.Sprintf(`{"admission_id":%q,"socket_id":%q,"activate":true}`, admissionID, socketID))))
		close(done)
	}()
	msg := <-prepared
	// This lock acquisition would time out if PREPARE still ran inside the first
	// server-lock transaction.
	lockCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	lockTx, err := db.BeginTx(lockCtx, nil)
	require.NoError(t, err)
	require.NoError(t, lockTx.QueryRowContext(lockCtx, `SELECT id FROM servers WHERE id=$1 FOR UPDATE`, serverID).Scan(new(uuid.UUID)))
	require.NoError(t, lockTx.Commit())
	var request voiceAdmissionActivationResponse
	require.NoError(t, json.Unmarshal(msg.Data, &request))
	request.Result = "ok"
	request.Proof = mediaproof.Sign(mediaproof.DeriveKey("test-secret", voiceAdmissionActivationResponsePurpose), voiceAdmissionActivationVersion, request.Timestamp, "activate", request.ChannelID, request.UserID, request.AdmissionID, request.SocketID, request.Revision, request.Nonce, request.Result)
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, msg.Respond(payload))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("voice admission did not complete after PREPARE acknowledgement")
	}
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id=$1 AND user_id=$2 AND admission_id=$3 AND socket_id=$4 AND expires_at>clock_timestamp()`, channelID, member, admissionID, socketID).Scan(&remaining))
	assert.Equal(t, 1, remaining, "post-A2 enforcement must still target the exact pending admission")
}

func TestActivatedAdmissionRechecksRevokedPermissionAfterPrepare(t *testing.T) {
	code, body := exerciseActivatedAdmissionMutation(t, func(db *sql.DB, _ uuid.UUID, _ uuid.UUID, roleID uuid.UUID, _ string, _ string) error {
		_, err := db.Exec(`UPDATE roles SET permissions = 0 WHERE id = $1`, roleID)
		return err
	})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.NotContains(t, body, `"allowed":true`, "a revoked A2 must not receive a stale success response")
}

func TestActivatedAdmissionRejectsReplacedExactPendingAdmissionAfterPrepare(t *testing.T) {
	code, body := exerciseActivatedAdmissionMutation(t, func(db *sql.DB, channelID, memberID, _ uuid.UUID, admissionID, socketID string) error {
		_, err := db.Exec(`
			UPDATE voice_pending_admissions
			SET admission_id = $3, socket_id = $4, expires_at = clock_timestamp() + INTERVAL '30 seconds'
			WHERE channel_id = $1 AND user_id = $2 AND admission_id = $5 AND socket_id = $6
		`, channelID, memberID, uuid.NewString(), "replacement-socket", admissionID, socketID)
		return err
	})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.NotContains(t, body, `"allowed":true`, "a replaced A2 must not receive a stale success response")
}

func exerciseActivatedAdmissionMutation(
	t *testing.T,
	mutate func(*sql.DB, uuid.UUID, uuid.UUID, uuid.UUID, string, string) error,
) (int, string) {
	t.Helper()
	bus, err := rawnats.Connect(admissionNATSTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); exercised in CI", err)
	}
	t.Cleanup(func() { bus.Close() })
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	owner, member := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	serverID, channelID, roleID := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, insertPendingAdmissionJoinFixture(db, owner, member, serverID, channelID, roleID))
	admissionID, socketID := uuid.NewString(), "mutation-socket"
	_, err = db.Exec(`INSERT INTO voice_pending_admissions (channel_id,user_id,admission_id,socket_id,expires_at) VALUES ($1,$2,$3,$4,clock_timestamp()+INTERVAL '30 seconds')`, channelID, member, admissionID, socketID)
	require.NoError(t, err)
	control, err := natsclient.Connect(admissionNATSTestURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = control.Close() })
	prepared := make(chan *rawnats.Msg, 1)
	sub, err := bus.Subscribe(voiceAdmissionActivationSubject, func(msg *rawnats.Msg) { prepared <- msg })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, bus.Flush())
	h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test"), Cfg: &config.Config{JWTSecret: "test-secret"}, NATS: control, Resolver: rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), logger.New("test")), EntCache: entitlements.NewCache(redisClient, db)})
	h.afterVoiceAdmissionPrepareForTest = func() {
		if err := mutate(db, channelID, member, roleID, admissionID, socketID); err != nil {
			t.Errorf("post-PREPARE mutation failed: %v", err)
		}
	}
	r := gin.New()
	r.POST("/channels/:id/voice/join", func(c *gin.Context) {
		c.Set("user_id", member.String())
		c.Set("concord_service_hop", true)
		h.AuthorizeJoin(c)
	})
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/channels/"+channelID.String()+"/voice/join", bytes.NewBufferString(fmt.Sprintf(`{"admission_id":%q,"socket_id":%q,"activate":true}`, admissionID, socketID))))
		close(done)
	}()
	msg := <-prepared
	var request voiceAdmissionActivationResponse
	require.NoError(t, json.Unmarshal(msg.Data, &request))
	request.Result = "ok"
	request.Proof = mediaproof.Sign(mediaproof.DeriveKey("test-secret", voiceAdmissionActivationResponsePurpose), voiceAdmissionActivationVersion, request.Timestamp, "activate", request.ChannelID, request.UserID, request.AdmissionID, request.SocketID, request.Revision, request.Nonce, request.Result)
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, msg.Respond(payload))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("voice admission did not complete after PREPARE acknowledgement")
	}
	return w.Code, w.Body.String()
}

func TestRenewPendingVoiceAdmissionRefreshesExactLease(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	ownerID := dbtest.CreateUser(t, db)
	memberID := dbtest.CreateUser(t, db)
	serverID, channelID, roleID := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, insertPendingAdmissionJoinFixture(db, ownerID, memberID, serverID, channelID, roleID))
	admissionID, socketID := uuid.NewString(), "renew-socket"
	_, err := db.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, $4, clock_timestamp() + INTERVAL '1 second')
	`, channelID, memberID, admissionID, socketID)
	require.NoError(t, err)

	h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test"), Cfg: &config.Config{}})
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.True(t, h.renewPendingVoiceAdmission(ctx, tx, channelID.String(), memberID.String(), voiceJoinAdmission{AdmissionID: admissionID, SocketID: socketID}))
	require.NoError(t, tx.Commit())

	var secondsRemaining float64
	require.NoError(t, db.QueryRow(`SELECT EXTRACT(EPOCH FROM (expires_at - clock_timestamp())) FROM voice_pending_admissions WHERE channel_id=$1 AND user_id=$2`, channelID, memberID).Scan(&secondsRemaining))
	assert.Greater(t, secondsRemaining, float64(25), "a valid A2 promotion must refresh the exact reservation for 30 seconds")
}

func TestAuthorizeMediaVoiceAdmissionPendingLookupErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		table      string
		wantStatus int
	}{
		{name: "missing reservation is forbidden", table: `CREATE TEMP TABLE voice_pending_admissions (channel_id UUID, user_id UUID, admission_id UUID, socket_id TEXT, expires_at TIMESTAMPTZ)`, wantStatus: http.StatusForbidden},
		{name: "database failure is server error", table: `CREATE TEMP TABLE voice_pending_admissions (admission_id UUID)`, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			conn, err := db.Conn(context.Background())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			_, err = conn.ExecContext(context.Background(), test.table)
			require.NoError(t, err)
			var logBuffer bytes.Buffer
			h := NewHandler(HandlerDeps{DB: db, Log: logger.NewWithWriter(&logBuffer), Cfg: &config.Config{JWTSecret: "test-secret"}})
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			tx, err := conn.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, tx.Rollback()) }()
			h.authorizeMediaVoiceAdmission(ctx, tx, uuid.Nil.String(), uuid.Nil.String(), voiceJoinAdmission{
				Activate: true, AdmissionID: uuid.NewString(), SocketID: "socket",
			})
			assert.Equal(t, test.wantStatus, ctx.Writer.Status())
			if test.wantStatus == http.StatusInternalServerError {
				assert.Contains(t, logBuffer.String(), "Failed to validate pending voice admission")
			}
		})
	}
}

func TestAuthorizeJoinLockedServerLookupErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		table      string
		wantStatus int
	}{
		{name: "missing server is forbidden", table: `CREATE TEMP TABLE servers (id UUID)`, wantStatus: http.StatusForbidden},
		{name: "database failure is server error", table: `CREATE TEMP TABLE servers (broken BOOL)`, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			seedDB, _ := dbtest.SetupTestDB(t)
			ownerID := dbtest.CreateUser(t, seedDB)
			memberID := dbtest.CreateUser(t, seedDB)
			serverID, channelID, roleID := uuid.New(), uuid.New(), uuid.New()
			require.NoError(t, insertPendingAdmissionJoinFixture(seedDB, ownerID, memberID, serverID, channelID, roleID))
			db, err := sql.Open("postgres", dbtest.DatabaseURL())
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			require.NoError(t, db.Ping())
			_, err = db.Exec(test.table)
			require.NoError(t, err)
			var logBuffer bytes.Buffer
			log := logger.NewWithWriter(&logBuffer)
			h := NewHandler(HandlerDeps{DB: db, Log: log, Cfg: &config.Config{JWTSecret: "test-secret"}, Resolver: rbac.NewResolver(db, nil, log)})
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("user_id", memberID.String()); c.Next() })
			router.POST("/channels/:id/voice/join", h.AuthorizeJoin)
			request := httptest.NewRequest(http.MethodPost, "/channels/"+channelID.String()+"/voice/join", nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, test.wantStatus, response.Code)
			if test.wantStatus == http.StatusInternalServerError {
				assert.Contains(t, logBuffer.String(), "Failed to lock voice join server")
			}
		})
	}
}

func TestServerEnforcementSnapshotIncludesPendingChannelsAndCompleteState(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	ownerID := dbtest.CreateUser(t, db)
	targetID := dbtest.CreateUser(t, db)
	serverID, activeChannelID, pendingChannelID, expiredChannelID, roleID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, insertPendingAdmissionJoinFixture(db, ownerID, targetID, serverID, activeChannelID, roleID))
	_, err := db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'pending', 'voice'), ($3, $2, 'expired', 'voice')`, pendingChannelID, serverID, expiredChannelID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, activeChannelID, targetID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, 'pending-socket', clock_timestamp() + INTERVAL '30 seconds'),
		       ($4, $2, $5, 'expired-socket', clock_timestamp() - INTERVAL '1 second')
	`, pendingChannelID, targetID, uuid.NewString(), expiredChannelID, uuid.NewString())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE server_members SET server_muted=TRUE, server_deafened=TRUE WHERE server_id=$1 AND user_id=$2`, serverID, targetID)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	snapshot, err := serverEnforcementSnapshotTx(context.Background(), tx, serverID.String(), targetID.String())
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	assert.True(t, snapshot.serverMuted)
	assert.True(t, snapshot.serverDeafened)
	assert.Positive(t, snapshot.authorizationID)
	assert.ElementsMatch(t, []string{activeChannelID.String(), pendingChannelID.String(), expiredChannelID.String()}, snapshot.channelIDs)
	assert.Equal(t, activeChannelID.String(), firstEnforcementChannel(snapshot))
}

func TestVerifyVoiceAdmissionActivationResponse_NotReadyFailsClosed(t *testing.T) {
	expected := voiceAdmissionActivationExpectation{
		channelID: uuid.NewString(), userID: uuid.NewString(), admissionID: uuid.NewString(),
		socketID: "socket-1", revision: "7", nonce: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	response := voiceAdmissionActivationResponse{
		Version: voiceAdmissionActivationVersion, Timestamp: timestamp,
		ChannelID: expected.channelID, UserID: expected.userID,
		AdmissionID: expected.admissionID, SocketID: expected.socketID,
		Revision: expected.revision, Nonce: expected.nonce, Result: "not_ready",
	}
	fields := []string{"activate", expected.channelID, expected.userID, expected.admissionID, expected.socketID, expected.revision, expected.nonce, response.Result}
	response.Proof = mediaproof.Sign(
		mediaproof.DeriveKey("test-secret", voiceAdmissionActivationNotReadyPurpose),
		voiceAdmissionActivationVersion, timestamp, fields...,
	)
	responseRaw, err := json.Marshal(response)
	require.NoError(t, err)
	require.Equal(t, voiceAdmissionActivationNotReady,
		verifyVoiceAdmissionActivationResponse(responseRaw, "test-secret", expected))

	response.Proof = mediaproof.Sign(
		mediaproof.DeriveKey("test-secret", voiceAdmissionActivationResponsePurpose),
		voiceAdmissionActivationVersion, timestamp, fields...,
	)
	responseRaw, err = json.Marshal(response)
	require.NoError(t, err)
	require.Equal(t, voiceAdmissionActivationRejected,
		verifyVoiceAdmissionActivationResponse(responseRaw, "test-secret", expected),
		"a positive-purpose proof must not authorize a negative acknowledgement")

	response.Proof = mediaproof.Sign(
		mediaproof.DeriveKey("test-secret", voiceAdmissionActivationNotReadyPurpose),
		voiceAdmissionActivationVersion, timestamp, fields...,
	)
	tamperedProof := []byte(response.Proof)
	tamperedProof[0] ^= 1
	response.Proof = string(tamperedProof)
	responseRaw, err = json.Marshal(response)
	require.NoError(t, err)
	require.Equal(t, voiceAdmissionActivationRejected,
		verifyVoiceAdmissionActivationResponse(responseRaw, "test-secret", expected))
}

func TestVerifyVoiceAdmissionActivationResponse_AcceptsOnlyMatchingSignedSuccess(t *testing.T) {
	expected := voiceAdmissionActivationExpectation{
		channelID: uuid.NewString(), userID: uuid.NewString(), admissionID: uuid.NewString(),
		socketID: "socket-2", revision: "8", nonce: strings.Repeat("b", 64),
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	response := voiceAdmissionActivationResponse{
		Version: voiceAdmissionActivationVersion, Timestamp: timestamp,
		ChannelID: expected.channelID, UserID: expected.userID,
		AdmissionID: expected.admissionID, SocketID: expected.socketID,
		Revision: expected.revision, Nonce: expected.nonce, Result: "ok",
	}
	fields := []string{"activate", expected.channelID, expected.userID, expected.admissionID, expected.socketID, expected.revision, expected.nonce, response.Result}
	response.Proof = mediaproof.Sign(
		mediaproof.DeriveKey("test-secret", voiceAdmissionActivationResponsePurpose),
		voiceAdmissionActivationVersion, timestamp, fields...,
	)
	responseRaw, err := json.Marshal(response)
	require.NoError(t, err)
	assert.Equal(t, voiceAdmissionActivationAccepted,
		verifyVoiceAdmissionActivationResponse(responseRaw, "test-secret", expected))

	assert.Equal(t, voiceAdmissionActivationRejected,
		verifyVoiceAdmissionActivationResponse([]byte("not-json"), "test-secret", expected))
	response.Revision = "9"
	responseRaw, err = json.Marshal(response)
	require.NoError(t, err)
	assert.Equal(t, voiceAdmissionActivationRejected,
		verifyVoiceAdmissionActivationResponse(responseRaw, "test-secret", expected),
		"a signed response for another revision must not authorize the admission")
	response.Revision = expected.revision
	response.Result = "unknown"
	responseRaw, err = json.Marshal(response)
	require.NoError(t, err)
	assert.Equal(t, voiceAdmissionActivationRejected,
		verifyVoiceAdmissionActivationResponse(responseRaw, "test-secret", expected))
}

func TestParseVoiceJoinAdmission_ValidatesMediaHopPayload(t *testing.T) {
	channelID := uuid.NewString()
	admissionID := uuid.NewString()
	for _, test := range []struct {
		name       string
		channelID  string
		body       string
		mediaHop   bool
		wantOK     bool
		wantStatus int
	}{
		{name: "direct request does not require admission body", channelID: channelID, mediaHop: false, wantOK: true, wantStatus: http.StatusOK},
		{name: "invalid channel id", channelID: "not-a-uuid", mediaHop: false, wantStatus: http.StatusBadRequest},
		{name: "malformed media payload", channelID: channelID, body: "{", mediaHop: true, wantStatus: http.StatusBadRequest},
		{name: "empty socket id", channelID: channelID, body: `{"admission_id":"` + admissionID + `","socket_id":""}`, mediaHop: true, wantStatus: http.StatusBadRequest},
		{name: "oversized socket id", channelID: channelID, body: `{"admission_id":"` + admissionID + `","socket_id":"` + strings.Repeat("x", 129) + `"}`, mediaHop: true, wantStatus: http.StatusBadRequest},
		{name: "invalid admission id", channelID: channelID, body: `{"admission_id":"not-a-uuid","socket_id":"socket"}`, mediaHop: true, wantStatus: http.StatusBadRequest},
		{name: "nil admission id", channelID: channelID, body: `{"admission_id":"00000000-0000-0000-0000-000000000000","socket_id":"socket"}`, mediaHop: true, wantStatus: http.StatusBadRequest},
		{name: "valid media payload", channelID: channelID, body: `{"admission_id":"` + admissionID + `","socket_id":"socket"}`, mediaHop: true, wantOK: true, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			if test.mediaHop {
				ctx.Set("concord_service_hop", true)
			}
			admission, ok := parseVoiceJoinAdmission(ctx, test.channelID)
			assert.Equal(t, test.wantOK, ok)
			assert.Equal(t, test.wantStatus, writer.Code)
			if test.wantOK && test.mediaHop {
				assert.Equal(t, admissionID, admission.AdmissionID)
				assert.Equal(t, "socket", admission.SocketID)
			}
		})
	}
}

func TestPreflightVoiceJoinServer_ReturnsForbiddenAndServerError(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test"), Cfg: &config.Config{}})

	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	_, ok := h.preflightVoiceJoinServer(ctx, uuid.NewString())
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, writer.Code)

	writer = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	_, ok = h.preflightVoiceJoinServer(ctx, "not-a-uuid")
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, writer.Code)
}

func TestPrepareMediaVoiceAdmission_FailsClosedOnRevisionAndReservationErrors(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test"), Cfg: &config.Config{}})

	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	canceled, cancel := context.WithCancel(request.Context())
	cancel()
	ctx.Request = request.WithContext(canceled)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, ok := h.prepareMediaVoiceAdmission(ctx, tx, uuid.NewString(), uuid.NewString(), voiceJoinAdmission{})
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, writer.Code)
	require.NoError(t, tx.Rollback())

	writer = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	tx, err = db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, ok = h.prepareMediaVoiceAdmission(ctx, tx, "not-a-uuid", "not-a-uuid", voiceJoinAdmission{})
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, writer.Code)
	require.NoError(t, tx.Rollback())
}

func TestRenewPendingVoiceAdmission_FailsClosedOnDatabaseAndRowCountErrors(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test"), Cfg: &config.Config{}})

	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	canceled, cancel := context.WithCancel(request.Context())
	cancel()
	ctx.Request = request.WithContext(canceled)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	ok := h.renewPendingVoiceAdmission(ctx, tx, uuid.NewString(), uuid.NewString(), voiceJoinAdmission{AdmissionID: uuid.NewString(), SocketID: "socket"})
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, writer.Code)
	require.NoError(t, tx.Rollback())

	writer = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	tx, err = db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	ok = h.renewPendingVoiceAdmission(ctx, tx, uuid.NewString(), uuid.NewString(), voiceJoinAdmission{AdmissionID: uuid.NewString(), SocketID: "socket"})
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, writer.Code)
	require.NoError(t, tx.Rollback())
}

func TestAuthorizeJoin_DirectPreflightDoesNotReserveAdmission(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))

	ownerID := dbtest.CreateUser(t, db)
	memberID := dbtest.CreateUser(t, db)
	serverID := uuid.New()
	channelID := uuid.New()
	everyoneRoleID := uuid.New()
	require.NoError(t, insertPendingAdmissionJoinFixture(
		db, ownerID, memberID, serverID, channelID, everyoneRoleID,
	))

	h := NewHandler(HandlerDeps{
		DB:       db,
		Log:      logger.New("test"),
		Cfg:      &config.Config{},
		Resolver: rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), logger.New("test")),
		EntCache: entitlements.NewCache(redisClient, db),
	})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", memberID.String())
		c.Next()
	})
	router.POST("/channels/:id/voice/join", h.AuthorizeJoin)

	req := httptest.NewRequest(http.MethodPost, "/channels/"+channelID.String()+"/voice/join", nil)
	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, req)
	require.Equal(t, http.StatusOK, writer.Code)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_pending_admissions WHERE channel_id=$1 AND user_id=$2`, channelID, memberID).Scan(&count))
	require.Zero(t, count, "unverified client preflight must not reserve or activate an admission")
}

func insertPendingAdmissionJoinFixture(
	db *sql.DB, ownerID, memberID, serverID, channelID, everyoneRoleID uuid.UUID,
) error {
	if _, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'voice', $2)`, serverID, ownerID); err != nil {
		return fmt.Errorf("insert server: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO server_members (server_id, user_id, role)
		VALUES ($1, $2, 'owner'), ($1, $3, 'member')
	`, serverID, ownerID, memberID); err != nil {
		return fmt.Errorf("insert server members: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		VALUES ($1, $2, '@all', 0, $3, TRUE, TRUE)
	`, everyoneRoleID, serverID, int64(rbac.BasePermissions)); err != nil {
		return fmt.Errorf("insert default role: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO member_roles (server_id, user_id, role_id)
		VALUES ($1, $2, $4), ($1, $3, $4)
	`, serverID, ownerID, memberID, everyoneRoleID); err != nil {
		return fmt.Errorf("assign default role: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO channels (id, server_id, name, type)
		VALUES ($1, $2, 'voice', 'voice')
	`, channelID, serverID); err != nil {
		return fmt.Errorf("insert voice channel: %w", err)
	}
	return nil
}
