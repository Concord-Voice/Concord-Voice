package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func closureRequest(r *voiceAuthRig, wire protocol.HTTPRequest) protocol.ProvisionalClose {
	return protocol.ProvisionalClose{Schema: "concord.auth-connection-provisional-close", Version: 1, Target: r.target, RegistrationTokenDigest: mediaproof.TokenDigest(r.token), RegistrationBodyDigest: mediaproof.TokenDigest(string(wire.Body))}
}
func closureRelease(b protocol.Binding) protocol.ConnectionRelease {
	return protocol.ConnectionRelease{Version: 1, UserID: b.UserID, FamilyID: b.FamilyID, Target: b.Target}
}
func closureCount(t *testing.T, db *sql.DB, generation string) (live, closed int) {
	t.Helper()
	require.NoError(t, db.QueryRow(`SELECT (SELECT count(*) FROM voice_auth_connections WHERE connection_generation=$1),(SELECT count(*) FROM voice_auth_connection_closures WHERE connection_generation=$1)`, generation).Scan(&live, &closed))
	return
}
func closureFailure(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	require.Equal(t, status, w.Code, w.Body.String())
	require.Empty(t, w.Body.Bytes())
	for key := range w.Header() {
		require.False(t, strings.Contains(strings.ToLower(key), "-response-"), "failure carried success proof")
	}
}
func TestVoiceAuthClosureNormalSignedIdempotentAndUnknown(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	payload := closureRelease(binding)
	for range 2 {
		w, request := r.request(t, protocol.EndpointConnectionRelease, payload, "")
		response := r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
		require.True(t, response.Empty())
		live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
		require.Zero(t, live)
		require.Equal(t, 1, closed)
	}
	wrong := payload
	wrong.FamilyID = uuid.NewString()
	w, _ := r.request(t, protocol.EndpointConnectionRelease, wrong, "")
	closureFailure(t, w, http.StatusConflict)
	payload.Target.ConnectionGeneration = uuid.NewString()
	w, _ = r.request(t, protocol.EndpointConnectionRelease, payload, "")
	closureFailure(t, w, http.StatusConflict)
}

func TestVoiceAuthClosureLostRegistrationResponseUsesOriginalBytes(t *testing.T) {
	r := newVoiceAuthRig(t)
	wire, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	// Deliberately discard the original signed RegistrationResult. No family is
	// copied from that response into the provisional-close request.
	require.Equal(t, http.StatusOK, r.do(wire).Code)
	payload := closureRequest(r, wire)
	for _, field := range []string{"token", "body", "target"} {
		bad := payload
		switch field {
		case "token":
			bad.RegistrationTokenDigest = strings.Repeat("a", 64)
		case "body":
			bad.RegistrationBodyDigest = strings.Repeat("b", 64)
		case "target":
			bad.Target.SocketID += "-foreign"
		}
		w, _ := r.request(t, protocol.EndpointProvisionalClose, bad, "")
		closureFailure(t, w, http.StatusConflict)
		live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
		require.Equal(t, 1, live)
		require.Zero(t, closed)
	}
	for range 2 {
		w, request := r.request(t, protocol.EndpointProvisionalClose, payload, "")
		response := r.verify(t, protocol.EndpointProvisionalClose, w, request, protocol.ExpectedResponse{})
		result, ok := response.ProvisionalClose()
		require.True(t, ok)
		require.Equal(t, "registered", result.Disposition)
		require.NotNil(t, result.Binding)
		require.Equal(t, r.userID.String(), result.Binding.UserID)
		require.Equal(t, r.familyID.String(), result.Binding.FamilyID)
	}
	retry, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	closureFailure(t, r.do(retry), http.StatusConflict)
	live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Zero(t, live)
	require.Equal(t, 1, closed)
}

func TestVoiceAuthClosureUnregisteredRetainsExactCommitments(t *testing.T) {
	r := newVoiceAuthRig(t)
	wire, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	payload := closureRequest(r, wire)
	w, request := r.request(t, protocol.EndpointProvisionalClose, payload, "")
	response := r.verify(t, protocol.EndpointProvisionalClose, w, request, protocol.ExpectedResponse{})
	result, ok := response.ProvisionalClose()
	require.True(t, ok)
	require.Equal(t, "unregistered", result.Disposition)
	require.Nil(t, result.Binding)
	closureFailure(t, r.do(wire), http.StatusConflict)
	var token, body, variant string
	require.NoError(t, r.db.QueryRow(`SELECT registration_token_sha256,registration_body_sha256,variant FROM voice_auth_connection_closures WHERE connection_generation=$1`, r.target.ConnectionGeneration).Scan(&token, &body, &variant))
	require.Equal(t, payload.RegistrationTokenDigest, token)
	require.Equal(t, payload.RegistrationBodyDigest, body)
	require.Equal(t, "unregistered", variant)
	bad := payload
	bad.RegistrationBodyDigest = strings.Repeat("c", 64)
	w, _ = r.request(t, protocol.EndpointProvisionalClose, bad, "")
	closureFailure(t, w, http.StatusConflict)
}

func TestVoiceAuthClosureUnexpectedAssociationRetainsLiability(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	wire, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	generation := uuid.NewString()
	tx, err := r.db.Begin()
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	_, err = tx.Exec(`INSERT INTO voice_enforcement_sessions(session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id) VALUES($1,$2,$3,'channel',$4,$5,$6)`, generation, r.target.NodeBootID, uuid.NewString(), r.userID, r.epoch, r.target.SocketID)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO voice_auth_room_bindings(session_generation,connection_generation) VALUES($1,$2)`, generation, r.target.ConnectionGeneration)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	for _, endpoint := range []protocol.Endpoint{protocol.EndpointConnectionRelease, protocol.EndpointProvisionalClose} {
		var payload any = closureRelease(binding)
		if endpoint == protocol.EndpointProvisionalClose {
			payload = closureRequest(r, wire)
		}
		w, _ := r.request(t, endpoint, payload, "")
		closureFailure(t, w, http.StatusConflict)
		live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
		require.Equal(t, 1, live)
		require.Zero(t, closed)
	}
	var count int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_room_bindings WHERE session_generation=$1`, generation).Scan(&count))
	require.Equal(t, 1, count)
}

func TestVoiceAuthClosureAfterFamilyTerminalAndUserErasure(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	targets, err := loginfamily.CaptureUserTargetsTx(ctx, tx, user, nil)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonErasure})
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM users WHERE id=$1`, r.userID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	w, request := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
	live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Zero(t, live)
	require.Equal(t, 1, closed)
}

// SQL wait evidence makes queue order explicit. This does not infer a winner
// from a canceled HTTP request or a timing sleep.
func closureAwaitWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event='advisory'`).Scan(&count))
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted %d SQL generation waiters, saw %d", want, count)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestVoiceAuthClosureGenerationRaces(t *testing.T) {
	for _, closeFirst := range []bool{true, false} {
		name := "create_before_close"
		if closeFirst {
			name = "close_before_late_create"
		}
		t.Run(name, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			registration, registrationRequest := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
			closeWire, closeRequest := r.signed(t, protocol.EndpointProvisionalClose, closureRequest(r, registration), "")
			gate, err := r.db.Begin()
			require.NoError(t, err)
			defer func() { err := gate.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
			_, err = gate.Exec(`SELECT login_family_generation_lock('connection',$1::uuid)`, r.target.ConnectionGeneration)
			require.NoError(t, err)
			created, closed := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
			startCreate := func() { go func() { created <- r.do(registration) }() }
			startClose := func() { go func() { closed <- r.do(closeWire) }() }
			if closeFirst {
				startClose()
			} else {
				startCreate()
			}
			closureAwaitWaiters(t, r.db, 1)
			if closeFirst {
				startCreate()
			} else {
				startClose()
			}
			closureAwaitWaiters(t, r.db, 2)
			require.NoError(t, gate.Commit())
			var cw, rw *httptest.ResponseRecorder
			select {
			case cw = <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("close failed to finish")
			}
			select {
			case rw = <-created:
			case <-time.After(5 * time.Second):
				t.Fatal("create failed to finish")
			}
			response := r.verify(t, protocol.EndpointProvisionalClose, cw, closeRequest, protocol.ExpectedResponse{})
			result, ok := response.ProvisionalClose()
			require.True(t, ok)
			if closeFirst {
				closureFailure(t, rw, http.StatusConflict)
				require.Equal(t, "unregistered", result.Disposition)
			} else {
				r.verify(t, protocol.EndpointConnectionRegister, rw, registrationRequest, protocol.ExpectedResponse{UserID: r.userID.String()})
				require.Equal(t, "registered", result.Disposition)
			}
			live, tombstones := closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Zero(t, live)
			require.Equal(t, 1, tombstones)
		})
	}
}

func TestVoiceAuthClosureRetiredBootCollectedReplay(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	w, request := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
	_, err := r.db.Exec(`UPDATE voice_auth_boot_fences SET retired_at=clock_timestamp(),updated_at=clock_timestamp() WHERE node_boot_id=$1`, r.target.NodeBootID)
	require.NoError(t, err)
	_, err = loginfamily.SweepRetiredEvidence(context.Background(), r.db, 100)
	require.NoError(t, err)
	live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Zero(t, live)
	require.Zero(t, closed, "closure must actually be collected to test historical replay")
	w, _ = r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	closureFailure(t, w, http.StatusGone)
	w, request = r.request(t, protocol.EndpointProvisionalClose, closureRequest(r, registration), "")
	response := r.verify(t, protocol.EndpointProvisionalClose, w, request, protocol.ExpectedResponse{})
	result, ok := response.ProvisionalClose()
	require.True(t, ok)
	require.Equal(t, "retired_boot", result.Disposition)
	require.Nil(t, result.Binding)
	_, closed = closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Zero(t, closed, "replay cannot recreate collectible evidence")
	closureFailure(t, r.do(registration), http.StatusForbidden)
}

// Keep the real PostgreSQL transaction and commit effects. Only the transport's
// report of COMMIT is lost, so an unsafe receipt cannot hide behind rollback.
type closureUnknownConnector struct{ driver.Connector }

func (c closureUnknownConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return closureUnknownConn{conn}, nil
}

type closureUnknownConn struct{ driver.Conn }

func (c closureUnknownConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c closureUnknownConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return closureUnknownTx{tx}, nil
}

type closureUnknownTx struct{ driver.Tx }

func (tx closureUnknownTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return errors.New("test-only lost COMMIT result")
}
func TestVoiceAuthClosureUnknownCommitNeverSignsSuccess(t *testing.T) {
	for _, endpoint := range []protocol.Endpoint{protocol.EndpointConnectionRelease, protocol.EndpointProvisionalClose} {
		t.Run(string(endpoint), func(t *testing.T) {
			r := newVoiceAuthRig(t)
			binding := r.register(t)
			registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
			connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
			require.NoError(t, err)
			db := sql.OpenDB(closureUnknownConnector{connector})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h := newVoiceEnforcementSessionHandler(db, r.secret)
			route := gin.New()
			route.POST("/api/v1/internal/voice/auth-connections/release", h.ReleaseConnection)
			route.POST("/api/v1/internal/voice/auth-connections/provisional-close", h.ProvisionalClose)
			var payload any = closureRelease(binding)
			if endpoint == protocol.EndpointProvisionalClose {
				payload = closureRequest(r, registration)
			}
			wire, _ := r.signed(t, endpoint, payload, "")
			request := httptest.NewRequest(wire.Method, wire.Path, strings.NewReader(string(wire.Body)))
			for _, header := range wire.Headers {
				request.Header.Add(header.Name, header.Value)
			}
			w := httptest.NewRecorder()
			route.ServeHTTP(w, request)
			closureFailure(t, w, http.StatusServiceUnavailable)
			live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Zero(t, live)
			require.Equal(t, 1, closed, "real SQL commit must precede injected result loss")
			retry, retained := r.request(t, endpoint, payload, "")
			expected := protocol.ExpectedResponse{}
			if endpoint == protocol.EndpointConnectionRelease {
				expected.Binding = &binding
			}
			r.verify(t, endpoint, retry, retained, expected)
		})
	}
}

// The first receipt removes the live connection. Only then, after the exact
// signed expiry, may canonical retention collect SID/family evidence. The
// nonretired boot keeps the copied closure available for service-only retries.
func TestVoiceAuthClosureRetainedReplayAfterSIDAndFamilyPurge(t *testing.T) {
	r := newVoiceAuthRigAt(t, time.Now().Add(-auth.AccessTokenTTL+3*time.Second))
	claims, err := auth.ValidateAccessToken(r.token, r.secret)
	require.NoError(t, err)
	registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	binding := r.register(t)
	w, request := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
	if remaining := time.Until(claims.ExpiresAt.Time) + 100*time.Millisecond; remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		<-timer.C
	}
	gc, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, gc.DeletedSIDs)
	require.EqualValues(t, 1, gc.DeletedFamilies)
	var sids, families int
	require.NoError(t, r.db.QueryRow(`SELECT (SELECT count(*) FROM login_family_sids WHERE user_id=$1),(SELECT count(*) FROM login_families WHERE user_id=$1)`, r.userID).Scan(&sids, &families))
	require.Zero(t, sids)
	require.Zero(t, families)
	_, err = r.db.Exec(`DELETE FROM users WHERE id=$1`, r.userID)
	require.NoError(t, err)
	var users int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM users WHERE id=$1`, r.userID).Scan(&users))
	require.Zero(t, users)
	w, request = r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
	w, request = r.request(t, protocol.EndpointProvisionalClose, closureRequest(r, registration), "")
	response := r.verify(t, protocol.EndpointProvisionalClose, w, request, protocol.ExpectedResponse{})
	result, ok := response.ProvisionalClose()
	require.True(t, ok)
	require.Equal(t, "registered", result.Disposition)
	require.NotNil(t, result.Binding)
	require.Equal(t, binding, *result.Binding)
	live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Zero(t, live)
	require.Equal(t, 1, closed)
}

// Pause after the actual PostgreSQL COMMIT has released every SQL lock, but
// before database/sql can report definite success to the receipt owner.
type closurePausedCommit struct {
	ready         chan struct{}
	resume        chan struct{}
	committed     atomic.Bool
	subsequentSQL atomic.Int64
}
type closurePausedConnector struct {
	driver.Connector
	state *closurePausedCommit
}

func (c closurePausedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return closurePausedConn{Conn: conn, state: c.state}, nil
}

type closurePausedConn struct {
	driver.Conn
	state *closurePausedCommit
}

// With Queryer/Execer optional interfaces omitted, database/sql sends SQL
// through Prepare, including a reread on any newly opened wrapped connection.
func (c closurePausedConn) Prepare(query string) (driver.Stmt, error) {
	if c.state.committed.Load() {
		c.state.subsequentSQL.Add(1)
	}
	return c.Conn.Prepare(query)
}
func (c closurePausedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c closurePausedConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return closurePausedTx{Tx: tx, state: c.state}, nil
}

type closurePausedTx struct {
	driver.Tx
	state *closurePausedCommit
}

func (tx closurePausedTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.state.committed.Store(true)
	close(tx.state.ready)
	<-tx.state.resume
	return nil
}
func TestVoiceAuthClosureReceiptSurvivesPostcommitCollection(t *testing.T) {
	for _, endpoint := range []protocol.Endpoint{protocol.EndpointConnectionRelease, protocol.EndpointProvisionalClose} {
		t.Run(string(endpoint), func(t *testing.T) {
			r := newVoiceAuthRig(t)
			binding := r.register(t)
			registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
			state := &closurePausedCommit{ready: make(chan struct{}), resume: make(chan struct{})}
			connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
			require.NoError(t, err)
			db := sql.OpenDB(closurePausedConnector{Connector: connector, state: state})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h := newVoiceEnforcementSessionHandler(db, r.secret)
			route := gin.New()
			route.POST("/api/v1/internal/voice/auth-connections/release", h.ReleaseConnection)
			route.POST("/api/v1/internal/voice/auth-connections/provisional-close", h.ProvisionalClose)
			var payload any = closureRelease(binding)
			if endpoint == protocol.EndpointProvisionalClose {
				payload = closureRequest(r, registration)
			}
			wire, retained := r.signed(t, endpoint, payload, "")
			request := httptest.NewRequest(wire.Method, wire.Path, strings.NewReader(string(wire.Body)))
			for _, header := range wire.Headers {
				request.Header.Add(header.Name, header.Value)
			}
			result := make(chan *httptest.ResponseRecorder, 1)
			var resume sync.Once
			t.Cleanup(func() { resume.Do(func() { close(state.resume) }) })
			go func() { w := httptest.NewRecorder(); route.ServeHTTP(w, request); result <- w }()
			select {
			case <-state.ready:
			case <-time.After(5 * time.Second):
				t.Fatal("real close did not commit")
			}
			live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Zero(t, live)
			require.Equal(t, 1, closed)
			select {
			case <-result:
				t.Fatal("receipt escaped before definite commit return")
			default:
			}
			// Disposable fixture retirement passes the existing SQL zero-live guard.
			// Canonical retention supplies its own generation keys and GC authority.
			_, err = r.db.Exec(`UPDATE voice_auth_boot_fences SET retired_at=clock_timestamp(),updated_at=clock_timestamp() WHERE node_boot_id=$1`, r.target.NodeBootID)
			require.NoError(t, err)
			gc, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 100)
			require.NoError(t, err)
			require.EqualValues(t, 1, gc.DeletedConnectionClosures)
			live, closed = closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Zero(t, live)
			require.Zero(t, closed)
			resume.Do(func() { close(state.resume) })
			var w *httptest.ResponseRecorder
			select {
			case w = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("retained receipt did not complete")
			}
			expected := protocol.ExpectedResponse{}
			if endpoint == protocol.EndpointConnectionRelease {
				expected.Binding = &binding
			}
			response := r.verify(t, endpoint, w, retained, expected)
			if endpoint == protocol.EndpointConnectionRelease {
				require.True(t, response.Empty())
			} else {
				provisional, ok := response.ProvisionalClose()
				require.True(t, ok)
				require.Equal(t, "registered", provisional.Disposition)
				require.NotNil(t, provisional.Binding)
				require.Equal(t, binding, *provisional.Binding)
			}
			require.Zero(t, state.subsequentSQL.Load(), "receipt must sign from its precommit copy without a SQL reread")
			_, closed = closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Zero(t, closed)
		})
	}
}

// A release waiting on the boot key must hold no connection key yet; holding it
// would invert attachment's boot-row -> connection order (Codex #4135 P1).
func TestVoiceAuthClosureTakesBootKeyBeforeConnectionKey(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	gate, err := r.db.Begin()
	require.NoError(t, err)
	defer func() { err := gate.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	_, err = gate.Exec(`SELECT login_family_generation_lock('boot',$1::uuid)`, r.target.NodeBootID)
	require.NoError(t, err)
	wire, request := r.signed(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	released := make(chan *httptest.ResponseRecorder, 1)
	go func() { released <- r.do(wire) }()
	closureAwaitWaiters(t, r.db, 1)
	probe, err := r.db.Begin()
	require.NoError(t, err)
	var free bool
	require.NoError(t, probe.QueryRow(`SELECT pg_try_advisory_xact_lock(hashtextextended('concord.login-family.v1/connection/' || $1::uuid::text, 0))`, r.target.ConnectionGeneration).Scan(&free))
	require.NoError(t, probe.Rollback())
	require.True(t, free, "release held the connection key while waiting for the boot key")
	require.NoError(t, gate.Commit())
	select {
	case w := <-released:
		r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
	case <-time.After(5 * time.Second):
		t.Fatal("release failed to finish")
	}
}
