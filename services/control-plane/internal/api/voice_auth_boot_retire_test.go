package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func bootRetirePayload(boot string) protocol.BootRetire {
	return protocol.BootRetire{Schema: "concord.auth-boot-retirement", Version: 1, NodeBootID: boot}
}
func bootRetireInstall(r *voiceAuthRig, db *sql.DB) {
	h := newVoiceEnforcementSessionHandler(db, r.secret)
	r.router.POST("/api/v1/internal/voice/auth-connections/retire-boot", h.RetireBoot)
}
func TestVoiceAuthBootRetireExactLiveEvidence(t *testing.T) {
	for _, variant := range []string{"live_connection", "legacy_room", "bound_room", "idle_empty"} {
		t.Run(variant, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			// Root authorized removal of these prior disposable authored-fixture orphans.
			result, err := r.db.Exec(`DELETE FROM voice_enforcement_sessions s WHERE socket_id='registry-test-socket' AND NOT EXISTS(SELECT 1 FROM voice_auth_boot_fences f WHERE f.node_boot_id=s.node_boot_id)`)
			require.NoError(t, err)
			removed, err := result.RowsAffected()
			require.NoError(t, err)
			t.Logf("removed prior authored orphan room fixtures: %d", removed)
			var binding protocol.Binding
			if variant == "live_connection" || variant == "bound_room" {
				binding = r.register(t)
			}
			if variant == "legacy_room" || variant == "bound_room" {
				tx, err := r.db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer voiceAuthRollback(t, tx)
				generation := uuid.NewString()
				roomID := uuid.NewString()
				t.Cleanup(func() {
					if variant == "bound_room" {
						payload := protocol.RoomRelease{Schema: "concord.auth-room-binding", Version: 1, Binding: binding, Room: protocol.Room{SessionGeneration: generation, RoomID: roomID, RoomKind: "channel"}}
						w, request := r.request(t, protocol.EndpointRoomRelease, payload, "")
						require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
						r.verify(t, protocol.EndpointRoomRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
					} else {
						_, err := r.db.Exec(`DELETE FROM voice_enforcement_sessions WHERE session_generation=$1`, generation)
						require.NoError(t, err)
					}
				})
				_, err = tx.Exec(`INSERT INTO voice_enforcement_sessions(session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id) VALUES($1,$2,$3,'channel',$4,$5,$6)`, generation, r.target.NodeBootID, roomID, r.userID, r.epoch, r.target.SocketID)
				require.NoError(t, err)
				if variant == "bound_room" {
					_, err = tx.Exec(`INSERT INTO voice_auth_room_bindings(session_generation,connection_generation) VALUES($1,$2)`, generation, r.target.ConnectionGeneration)
					require.NoError(t, err)
				}
				require.NoError(t, tx.Commit())
			}
			bootRetireInstall(r, r.db)
			w, request := r.request(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
			if variant == "idle_empty" {
				require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
				r.verify(t, protocol.EndpointBootRetire, w, request, protocol.ExpectedResponse{})
			} else {
				closureFailure(t, w, http.StatusConflict)
			}
			var retired bool
			require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, r.target.NodeBootID).Scan(&retired))
			require.Equal(t, variant == "idle_empty", retired)
		})
	}
}
func TestVoiceAuthBootRetireUnknownCommitNeverSignsReceipt(t *testing.T) {
	r := newVoiceAuthRig(t)
	connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	db := sql.OpenDB(closureUnknownConnector{connector})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	bootRetireInstall(r, db)
	w, _ := r.request(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Empty(t, w.Header().Get("X-Concord-Auth-Connection-Response-Proof"))
	var retired bool
	require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, r.target.NodeBootID).Scan(&retired))
	require.True(t, retired, "real SQL commit precedes injected lost result")
}
func TestVoiceAuthBootRetireReceiptUsesPrecommitCopy(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	closed, closeRequest := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	require.Equal(t, http.StatusNoContent, closed.Code)
	r.verify(t, protocol.EndpointConnectionRelease, closed, closeRequest, protocol.ExpectedResponse{Binding: &binding})
	state := &closurePausedCommit{ready: make(chan struct{}), resume: make(chan struct{})}
	connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	db := sql.OpenDB(closurePausedConnector{Connector: connector, state: state})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var resume sync.Once
	defer resume.Do(func() { close(state.resume) })
	bootRetireInstall(r, db)
	wire, request := r.signed(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.do(wire) }()
	select {
	case <-state.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not commit")
	}
	select {
	case <-done:
		t.Fatal("receipt escaped before definite commit returned")
	default:
	}
	var retired bool
	require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, r.target.NodeBootID).Scan(&retired))
	require.True(t, retired)
	gc, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, gc.DeletedConnectionClosures)
	_, retained := closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Zero(t, retained)
	resume.Do(func() { close(state.resume) })
	select {
	case w := <-done:
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		r.verify(t, protocol.EndpointBootRetire, w, request, protocol.ExpectedResponse{})
	case <-time.After(5 * time.Second):
		t.Fatal("receipt did not finish")
	}
	require.Zero(t, state.subsequentSQL.Load(), "signing must not reread SQL after commit")
}

func TestVoiceAuthBootRetireRejectsUntrustedProof(t *testing.T) {
	for _, variant := range []string{"missing", "forged", "stale", "wrong_boot"} {
		t.Run(variant, func(t *testing.T) {
			r := newVoiceAuthRigPrepared(t)
			bootRetireInstall(r, r.db)
			wire, _ := r.signed(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
			switch variant {
			case "missing":
				wire.Headers = nil
			case "forged":
				for i := range wire.Headers {
					if strings.HasSuffix(wire.Headers[i].Name, "-Proof") {
						wire.Headers[i].Value = strings.Repeat("0", 64)
					}
				}
			case "stale":
				var err error
				wire, _, err = protocol.SignHTTPRequest(protocol.EndpointBootRetire, wire.Body, "", protocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10), Nonce: strings.Repeat("a", 64)}, []byte(r.secret))
				require.NoError(t, err)
			case "wrong_boot":
				var err error
				wire.Body, err = json.Marshal(bootRetirePayload(uuid.NewString()))
				require.NoError(t, err)
			}
			closureFailure(t, r.do(wire), http.StatusForbidden)
			var retired bool
			require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, r.target.NodeBootID).Scan(&retired))
			require.False(t, retired)
		})
	}
}

func TestVoiceAuthBootRetireUnknownServiceAndPermanentOperatorFence(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	boot := uuid.New()
	bootRetireInstall(r, r.db)
	w, _ := r.request(t, protocol.EndpointBootRetire, bootRetirePayload(boot.String()), "")
	closureFailure(t, w, http.StatusConflict)
	var count int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, RetireStoppedVoiceAuthBoot(t.Context(), r.db, boot))
	var original time.Time
	require.NoError(t, r.db.QueryRow(`SELECT retired_at FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&original))
	for range 2 {
		w, request := r.request(t, protocol.EndpointBootRetire, bootRetirePayload(boot.String()), "")
		require.Equal(t, http.StatusNoContent, w.Code)
		r.verify(t, protocol.EndpointBootRetire, w, request, protocol.ExpectedResponse{})
	}
	var current time.Time
	require.NoError(t, r.db.QueryRow(`SELECT retired_at FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&current))
	require.Equal(t, original, current)
	requester := &bootstrapRequester{secret: r.secret, boot: boot.String()}
	bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
	wire, _ := bootstrapWire(t, r, boot.String())
	closureFailure(t, r.do(wire), http.StatusForbidden)
}

func TestVoiceAuthBootRetireRechecksProofAfterBootWait(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	bootRetireInstall(r, r.db)
	body, err := json.Marshal(bootRetirePayload(r.target.NodeBootID))
	require.NoError(t, err)
	wire, _, err := protocol.SignHTTPRequest(protocol.EndpointBootRetire, body, "", protocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Add(-28*time.Second).Unix(), 10), Nonce: strings.Repeat("b", 64)}, []byte(r.secret))
	require.NoError(t, err)
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer voiceAuthRollback(t, tx)
	_, err = tx.Exec(`SELECT login_family_generation_lock('boot',$1::uuid)`, r.target.NodeBootID)
	require.NoError(t, err)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.do(wire) }()
	closureAwaitWaiters(t, r.db, 1)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case w := <-done:
		t.Fatalf("retirement escaped boot lock: %d", w.Code)
	case <-timer.C:
	}
	require.NoError(t, tx.Commit())
	select {
	case w := <-done:
		closureFailure(t, w, http.StatusForbidden)
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not finish")
	}
	var retired bool
	require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, r.target.NodeBootID).Scan(&retired))
	require.False(t, retired)
}

func TestVoiceAuthBootOperatorAbsenceAgainstBootstrapBothOrders(t *testing.T) {
	for _, operatorFirst := range []bool{true, false} {
		t.Run(strconv.FormatBool(operatorFirst), func(t *testing.T) {
			r := newVoiceAuthRigPrepared(t)
			boot := uuid.New()
			requester := &bootstrapRequester{secret: r.secret, boot: boot.String()}
			bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
			wire, _ := bootstrapWire(t, r, boot.String())
			gate, err := r.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer voiceAuthRollback(t, gate)
			_, err = gate.Exec(`SELECT login_family_generation_lock('boot',$1::uuid)`, boot)
			require.NoError(t, err)
			operator := make(chan error, 1)
			bootstrap := make(chan int, 1)
			startOperator := func() { go func() { operator <- RetireStoppedVoiceAuthBoot(t.Context(), r.db, boot) }() }
			startBootstrap := func() { go func() { bootstrap <- r.do(wire).Code }() }
			if operatorFirst {
				startOperator()
			} else {
				startBootstrap()
			}
			closureAwaitWaiters(t, r.db, 1)
			if operatorFirst {
				startBootstrap()
			} else {
				startOperator()
			}
			closureAwaitWaiters(t, r.db, 2)
			require.NoError(t, gate.Commit())
			select {
			case err := <-operator:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("operator stuck")
			}
			select {
			case code := <-bootstrap:
				if operatorFirst {
					require.Equal(t, http.StatusForbidden, code)
				} else {
					require.Equal(t, http.StatusOK, code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("bootstrap stuck")
			}
			var retired bool
			require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&retired))
			require.True(t, retired)
			replay, _ := bootstrapWire(t, r, boot.String())
			closureFailure(t, r.do(replay), http.StatusForbidden)
		})
	}
}

// Only the database driver's read result is faulted; SQL and route/codec remain real.
type bootRetireQueryFailureConnector struct{ driver.Connector }

func (c bootRetireQueryFailureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return bootRetireQueryFailureConn{conn}, nil
}

type bootRetireQueryFailureConn struct{ driver.Conn }

func (c bootRetireQueryFailureConn) Prepare(query string) (driver.Stmt, error) {
	if strings.Contains(query, "SELECT NOT EXISTS") {
		return nil, errors.New("injected zero-live read failure")
	}
	return c.Conn.Prepare(query)
}
func (c bootRetireQueryFailureConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func TestVoiceAuthBootRetireZeroLiveQueryFailureNeverSigns(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	db := sql.OpenDB(bootRetireQueryFailureConnector{connector})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	bootRetireInstall(r, db)
	w, _ := r.request(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	closureFailure(t, w, http.StatusServiceUnavailable)
	var retired bool
	require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, r.target.NodeBootID).Scan(&retired))
	require.False(t, retired)
}

func TestVoiceAuthBootRetirementKeepsPendingClosureEvidence(t *testing.T) {
	for _, variant := range []string{"user_operation", "family_operation", "epoch", "dm", "other_boot_user_operation", "hub_delivery"} {
		t.Run(variant, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			binding := r.register(t)
			w, request := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
			require.Equal(t, http.StatusNoContent, w.Code)
			r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
			if variant == "other_boot_user_operation" {
				original := r.target
				r.target.NodeBootID = uuid.NewString()
				r.target.ConnectionGeneration = uuid.NewString()
				r.target.SocketID = "other-boot-native-socket"
				_, err := r.db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, r.target.NodeBootID)
				require.NoError(t, err)
				r.register(t)
				r.target = original
			}
			switch variant {
			case "user_operation", "family_operation", "other_boot_user_operation", "hub_delivery":
				tx, err := r.db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer voiceAuthRollback(t, tx)
				barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
				require.NoError(t, err)
				users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserUpdate)
				require.NoError(t, err)
				user, err := users.ForUser(r.userID)
				require.NoError(t, err)
				var targets loginfamily.Targets
				scope := loginfamily.ScopeUser
				if variant == "family_operation" {
					scope = loginfamily.ScopeSession
					var sid uuid.UUID
					require.NoError(t, tx.QueryRow(`SELECT sid FROM login_family_sids WHERE family_id=$1`, r.familyID).Scan(&sid))
					targets, err = loginfamily.ResolveTargetSIDTx(t.Context(), tx, user, sid)
				} else {
					targets, err = loginfamily.CaptureUserTargetsTx(t.Context(), tx, user, nil)
				}
				require.NoError(t, err)
				_, err = loginfamily.RevokeFamiliesTx(t.Context(), tx, targets, loginfamily.AuthCause{Scope: scope, Reason: loginfamily.ReasonExplicitRevoke})
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
			case "epoch":
				_, err := r.db.Exec(`INSERT INTO credential_epoch_voice_ejections(user_id,credential_epoch,superseded_credential_epoch,generation) VALUES($1,$2,$3,$4)`, r.userID, strings.Repeat("e", 32), r.epoch, uuid.New())
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := r.db.Exec(`DELETE FROM credential_epoch_voice_ejections WHERE user_id=$1`, r.userID)
					require.NoError(t, err)
				})
			case "dm":
				_, err := r.db.Exec(`INSERT INTO dm_block_voice_ejections(conversation_id,user_id) VALUES($1,$2)`, uuid.New(), r.userID)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := r.db.Exec(`DELETE FROM dm_block_voice_ejections WHERE user_id=$1`, r.userID)
					require.NoError(t, err)
				})
			}
			if variant == "hub_delivery" {
				hub := &bootRetireFailingHub{}
				worker := dmblock.New(r.db, nil)
				runtime := newVoiceAuthRuntime(r.db, nil, r.secret, hub, worker)
				worker.SetAuthRevocationEject(runtime.deliverAuth)
				_, err := worker.ReconcileDue(t.Context(), 100)
				require.NoError(t, err)
				require.Positive(t, hub.calls, "actual runtime must reach failed Hub receipt")
				var pending int
				require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id=$1 AND completed_at IS NULL`, r.userID).Scan(&pending))
				require.Equal(t, 1, pending)
			}
			bootRetireInstall(r, r.db)
			w, request = r.request(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			r.verify(t, protocol.EndpointBootRetire, w, request, protocol.ExpectedResponse{})
			gc, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 100)
			require.NoError(t, err)
			require.Zero(t, gc.DeletedConnectionClosures)
			_, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Equal(t, 1, closed, "retirement is not pending delivery completion")
		})
	}
}

func TestVoiceAuthBootRetirementWaitsForRealCreator(t *testing.T) {
	r := newVoiceAuthRig(t)
	bootRetireInstall(r, r.db)
	gate, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer voiceAuthRollback(t, gate)
	_, err = gate.Exec(`SELECT login_family_generation_lock('connection',$1::uuid)`, r.target.ConnectionGeneration)
	require.NoError(t, err)
	registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	created := make(chan *httptest.ResponseRecorder, 1)
	go func() { created <- r.do(registration) }()
	closureAwaitWaiters(t, r.db, 1)
	wire, _ := r.signed(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	retired := make(chan *httptest.ResponseRecorder, 1)
	go func() { retired <- r.do(wire) }()
	// Registration already owns boot SHARE when its connection-generation lock waits.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		require.NoError(t, r.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE%')`).Scan(&waiting))
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retirement did not wait for creator boot SHARE")
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, gate.Commit())
	select {
	case w := <-created:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("creator stuck")
	}
	select {
	case w := <-retired:
		closureFailure(t, w, http.StatusConflict)
	case <-time.After(5 * time.Second):
		t.Fatal("retirement stuck")
	}
}

func TestVoiceAuthBootRetiredReplayStillQueriesFreshLiveGate(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	require.NoError(t, RetireStoppedVoiceAuthBoot(t.Context(), r.db, uuid.MustParse(r.target.NodeBootID)))
	connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	db := sql.OpenDB(bootRetireQueryFailureConnector{connector})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	bootRetireInstall(r, db)
	w, _ := r.request(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	closureFailure(t, w, http.StatusServiceUnavailable)
}

func TestVoiceAuthBootRetirePrecopyCollectionCannotRemoveRequiredClosure(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	w, request := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	require.Equal(t, http.StatusNoContent, w.Code)
	r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
	gate, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer voiceAuthRollback(t, gate)
	_, err = gate.Exec(`SELECT login_family_generation_lock('boot',$1::uuid)`, r.target.NodeBootID)
	require.NoError(t, err)
	bootRetireInstall(r, r.db)
	wire, retireRequest := r.signed(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.do(wire) }()
	closureAwaitWaiters(t, r.db, 1)
	gc, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 100)
	require.NoError(t, err)
	require.Zero(t, gc.DeletedConnectionClosures)
	_, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Equal(t, 1, closed)
	require.NoError(t, gate.Commit())
	select {
	case w := <-done:
		require.Equal(t, http.StatusNoContent, w.Code)
		r.verify(t, protocol.EndpointBootRetire, w, retireRequest, protocol.ExpectedResponse{})
	case <-time.After(5 * time.Second):
		t.Fatal("retirement stuck")
	}
}

func TestVoiceAuthBootRetireNeedsNoBearerOrLivePrincipal(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	_, err := r.db.Exec(`DELETE FROM users WHERE id=$1`, r.userID)
	require.NoError(t, err)
	bootRetireInstall(r, r.db)
	w, request := r.request(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	r.verify(t, protocol.EndpointBootRetire, w, request, protocol.ExpectedResponse{})
}
func TestVoiceAuthBootOperatorUnknownCommitIsFailure(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	boot := uuid.New()
	connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	db := sql.OpenDB(closureUnknownConnector{connector})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.Error(t, RetireStoppedVoiceAuthBoot(t.Context(), db, boot))
	var retired bool
	require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&retired))
	require.True(t, retired, "lost commit acknowledgement must not become operator success")
}

type bootRetireFailingHub struct{ calls int }

func (h *bootRetireFailingHub) DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error {
	h.calls++
	return errors.New("injected missing Hub completion receipt")
}

type bootRetireBeforeCommitConnector struct {
	driver.Connector
	ready, resume chan struct{}
}

func (c bootRetireBeforeCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return bootRetireBeforeCommitConn{Conn: conn, ready: c.ready, resume: c.resume}, nil
}

type bootRetireBeforeCommitConn struct {
	driver.Conn
	ready, resume chan struct{}
}

func (c bootRetireBeforeCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return bootRetireBeforeCommitTx{Tx: tx, ready: c.ready, resume: c.resume}, nil
}

type bootRetireBeforeCommitTx struct {
	driver.Tx
	ready, resume chan struct{}
}

func (tx bootRetireBeforeCommitTx) Commit() error {
	close(tx.ready)
	<-tx.resume
	return tx.Tx.Commit()
}
func TestVoiceAuthBootRetirementBeforeRealCreator(t *testing.T) {
	r := newVoiceAuthRig(t)
	ready, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	db := sql.OpenDB(bootRetireBeforeCommitConnector{Connector: connector, ready: ready, resume: resume})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	bootRetireInstall(r, db)
	wire, request := r.signed(t, protocol.EndpointBootRetire, bootRetirePayload(r.target.NodeBootID), "")
	retired := make(chan *httptest.ResponseRecorder, 1)
	go func() { retired <- r.do(wire) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement never reached commit with boot lock")
	}
	registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	created := make(chan *httptest.ResponseRecorder, 1)
	go func() { created <- r.do(registration) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		require.NoError(t, r.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR SHARE%')`).Scan(&waiting))
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("creator did not wait on retirement's boot row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	once.Do(func() { close(resume) })
	select {
	case w := <-retired:
		require.Equal(t, http.StatusNoContent, w.Code)
		r.verify(t, protocol.EndpointBootRetire, w, request, protocol.ExpectedResponse{})
	case <-time.After(5 * time.Second):
		t.Fatal("retirement stuck")
	}
	select {
	case w := <-created:
		closureFailure(t, w, http.StatusForbidden)
	case <-time.After(5 * time.Second):
		t.Fatal("creator stuck")
	}
	live, _ := closureCount(t, r.db, r.target.ConnectionGeneration)
	require.Zero(t, live)
}
