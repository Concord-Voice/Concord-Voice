package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Only the broker request/reply boundary is controlled. Both command and ACK
// authentication run through the real strict codec; SQL authority is real.
type bootstrapRequester struct {
	secret  string
	boot    string
	calls   atomic.Int32
	respond func(context.Context, protocol.Command) ([]byte, error)
}

func (s *bootstrapRequester) RequestWithContext(ctx context.Context, subject string, data interface{}) ([]byte, error) {
	s.calls.Add(1)
	if subject != "voice.enforce.session."+s.boot {
		return nil, errors.New("wrong exact boot subject")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	command, err := protocol.DecodeCommand(raw)
	if err != nil {
		return nil, err
	}
	if err = protocol.VerifyCommand(command, []byte(s.secret), time.Now()); err != nil {
		return nil, err
	}
	health, ok := command.Health()
	if !ok || health.NodeBootID != s.boot {
		return nil, errors.New("wrong health command")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("network proof must be bounded")
	}
	if s.respond != nil {
		return s.respond(ctx, command)
	}
	return protocol.SignHealthACK(command, false, []byte(s.secret))
}
func bootstrapInstall(r *voiceAuthRig, h *voiceEnforcementSessionHandler) {
	r.router.POST("/api/v1/internal/voice/enforcement/health", h.HealthBootstrap)
}
func bootstrapWire(t *testing.T, r *voiceAuthRig, boot string) (protocol.HTTPRequest, protocol.Request) {
	t.Helper()
	// The bootstrap nonce equals the authenticated attempt nonce. Select it from
	// an ordinary signed request, then resign the matching closed payload.
	wire, _ := r.signed(t, protocol.EndpointBootRetire, protocol.BootRetire{Schema: "concord.auth-boot-retirement", Version: 1, NodeBootID: boot}, "")
	var stamp, nonce string
	for _, h := range wire.Headers {
		if strings.HasSuffix(h.Name, "-Timestamp") {
			stamp = h.Value
		}
		if strings.HasSuffix(h.Name, "-Nonce") {
			nonce = h.Value
		}
	}
	body, err := json.Marshal(protocol.HealthBootstrap{Version: 1, NodeBootID: boot, Nonce: nonce})
	require.NoError(t, err)
	wire, request, err := protocol.SignHTTPRequest(protocol.EndpointHealthBootstrap, body, "", protocol.AttemptMeta{Timestamp: stamp, Nonce: nonce}, []byte(r.secret))
	require.NoError(t, err)
	return wire, request
}
func TestVoiceAuthBootstrapPreparedStrictReceiptWithoutClients(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	boot := uuid.NewString()
	requester := &bootstrapRequester{secret: r.secret, boot: boot}
	bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
	wire, request := bootstrapWire(t, r, boot)
	w := r.do(wire)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	response := r.verify(t, protocol.EndpointHealthBootstrap, w, request, protocol.ExpectedResponse{})
	result, ok := response.HealthBootstrap()
	require.True(t, ok)
	require.Equal(t, protocol.HealthBootstrapResult{Version: 1, VoiceEnforcementProtocol: 4, NodeBootID: boot, Activation: "prepared", TransportOK: true, AdmissionGranted: false}, result)
	require.EqualValues(t, 1, requester.calls.Load())
	var known, live int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_boot_fences WHERE node_boot_id=$1 AND retired_at IS NULL`, boot).Scan(&known))
	require.Equal(t, 1, known)
	require.NoError(t, r.db.QueryRow(`SELECT (SELECT count(*) FROM voice_auth_connections)+(SELECT count(*) FROM voice_enforcement_sessions)+(SELECT count(*) FROM voice_auth_room_bindings)`).Scan(&live))
	require.Zero(t, live)
}
func TestVoiceAuthBootstrapRejectsUntrustedACKWithoutCreatingBoot(t *testing.T) {
	for _, variant := range []string{"missing", "forged", "wrong_boot", "wrong_challenge", "unknown_schema", "protocol3", "stale"} {
		t.Run(variant, func(t *testing.T) {
			r := newVoiceAuthRigPrepared(t)
			boot := uuid.NewString()
			requester := &bootstrapRequester{secret: r.secret, boot: boot, respond: func(_ context.Context, command protocol.Command) ([]byte, error) {
				if variant == "missing" {
					return nil, errors.New("no subscribers")
				}
				if variant == "wrong_boot" || variant == "wrong_challenge" || variant == "stale" {
					h, ok := command.Health()
					if !ok {
						return nil, errors.New("health expected")
					}
					if variant == "wrong_boot" {
						h.NodeBootID = uuid.NewString()
					}
					if variant == "wrong_challenge" {
						h.Challenge = strings.Repeat("b", 64)
					}
					if variant == "stale" {
						h.Timestamp = "1"
					}
					other, err := protocol.NewHealthCommand(h.NodeBootID, h.Challenge, h.Activation, h.AdmissionAllowed, h.Timestamp)
					if err != nil {
						return nil, err
					}
					return protocol.SignHealthACK(other, false, []byte(r.secret))
				}
				ack, err := protocol.SignHealthACK(command, false, []byte(r.secret))
				if err != nil {
					return nil, err
				}
				if variant == "forged" {
					return protocol.SignHealthACK(command, false, []byte("test-only-wrong-signing-key"))
				}
				if variant == "unknown_schema" {
					return []byte(strings.Replace(string(ack), "concord.voice-enforcement-health", "concord.unknown", 1)), nil
				}
				return []byte(strings.Replace(string(ack), `"protocol":4`, `"protocol":3`, 1)), nil
			}}
			bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
			wire, _ := bootstrapWire(t, r, boot)
			w := r.do(wire)
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			require.Empty(t, w.Header().Get("X-Concord-Auth-Connection-Response-Proof"))
			var known int
			require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&known))
			require.Zero(t, known)
		})
	}
}
func TestVoiceAuthBootstrapRetiredBootNeverReopens(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	boot := uuid.NewString()
	_, err := r.db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, boot)
	require.NoError(t, err)
	requester := &bootstrapRequester{secret: r.secret, boot: boot}
	bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
	wire, _ := bootstrapWire(t, r, boot)
	w := r.do(wire)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var retired bool
	require.NoError(t, r.db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&retired))
	require.True(t, retired)
}

func TestVoiceAuthBootstrapStrictRequestRefusals(t *testing.T) {
	for _, variant := range []string{"missing_proof", "duplicate_proof", "oversized_header", "oversized_body", "unknown_field", "second_document"} {
		t.Run(variant, func(t *testing.T) {
			r := newVoiceAuthRigPrepared(t)
			boot := uuid.NewString()
			requester := &bootstrapRequester{secret: r.secret, boot: boot}
			bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
			wire, _ := bootstrapWire(t, r, boot)
			want := http.StatusForbidden
			switch variant {
			case "missing_proof":
				for i, h := range wire.Headers {
					if strings.HasSuffix(h.Name, "-Proof") {
						wire.Headers = append(wire.Headers[:i], wire.Headers[i+1:]...)
						break
					}
				}
			case "duplicate_proof":
				for _, h := range wire.Headers {
					if strings.HasSuffix(h.Name, "-Proof") {
						wire.Headers = append(wire.Headers, h)
						break
					}
				}
			case "oversized_header":
				for i, h := range wire.Headers {
					if strings.HasSuffix(h.Name, "-Proof") {
						wire.Headers[i].Value = strings.Repeat("a", 5000)
					}
				}
			case "oversized_body":
				wire.Body = append(wire.Body, []byte(strings.Repeat(" ", 4096))...)
				want = http.StatusRequestEntityTooLarge
			case "unknown_field":
				wire.Body = []byte(strings.TrimSuffix(string(wire.Body), "}") + `,"extra":true}`)
				want = http.StatusBadRequest
			case "second_document":
				wire.Body = append(wire.Body, []byte(` {}`)...)
				want = http.StatusBadRequest
			}
			w := r.do(wire)
			require.Equal(t, want, w.Code, w.Body.String())
			require.Zero(t, requester.calls.Load())
		})
	}
}
func TestVoiceAuthBootstrapActiveRequiresCurrentProcessAndLocalGrant(t *testing.T) {
	for _, localGrant := range []bool{false, true} {
		t.Run(strconv.FormatBool(localGrant), func(t *testing.T) {
			r := newVoiceAuthRig(t)
			boot := r.target.NodeBootID
			reconciler := dmblock.New(r.db, nil)
			runtime := newVoiceAuthRuntime(r.db, nil, r.secret, nil, reconciler)
			requester := &bootstrapRequester{secret: r.secret, boot: boot, respond: func(_ context.Context, c protocol.Command) ([]byte, error) {
				h, ok := c.Health()
				if !ok {
					return nil, errors.New("health expected")
				}
				return protocol.SignHealthACK(c, localGrant && h.AdmissionAllowed, []byte(r.secret))
			}}
			handler := newVoiceEnforcementSessionHandler(r.db, r.secret, requester)
			handler.authoritySnapshot = runtime.AuthoritySnapshot
			bootstrapInstall(r, handler)
			check := func(want bool) {
				wire, request := bootstrapWire(t, r, boot)
				w := r.do(wire)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				response := r.verify(t, protocol.EndpointHealthBootstrap, w, request, protocol.ExpectedResponse{})
				result, ok := response.HealthBootstrap()
				require.True(t, ok)
				require.Equal(t, "active", result.Activation)
				require.Equal(t, want, result.AdmissionGranted)
			}
			check(false)
			_, err := reconciler.ReconcileDue(t.Context(), 100)
			require.NoError(t, err)
			check(localGrant)
			handler.authoritySnapshot = newVoiceAuthRuntime(r.db, nil, r.secret, nil, dmblock.New(r.db, nil)).AuthoritySnapshot
			check(false)
		})
	}
}

func TestVoiceAuthBootstrapDoesNotHoldSQLLocksAcrossNetwork(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	boot := r.target.NodeBootID
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()
	requester := &bootstrapRequester{secret: r.secret, boot: boot, respond: func(ctx context.Context, c protocol.Command) ([]byte, error) {
		close(entered)
		select {
		case <-resume:
			return protocol.SignHealthACK(c, false, []byte(r.secret))
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
	wire, _ := bootstrapWire(t, r, boot)
	done := make(chan int, 1)
	go func() { done <- r.do(wire).Code }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap did not reach broker")
	}
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	_, err = tx.Exec(`SET LOCAL lock_timeout='500ms'`)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT id FROM login_family_rollout WHERE id=true FOR UPDATE`)
	require.NoError(t, err, "rollout lock across network")
	_, err = tx.Exec(`SELECT login_family_generation_lock('boot',$1::uuid)`, boot)
	require.NoError(t, err, "boot identity lock across network")
	_, err = tx.Exec(`SELECT node_boot_id FROM voice_auth_boot_fences WHERE node_boot_id=$1 FOR UPDATE`, boot)
	require.NoError(t, err, "boot row lock across network")
	_, err = tx.Exec(`UPDATE voice_auth_boot_fences SET retired_at=clock_timestamp() WHERE node_boot_id=$1`, boot)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	release()
	select {
	case code := <-done:
		require.Equal(t, http.StatusForbidden, code, "retirement during remote proof must win final recheck")
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap failed to finish")
	}
}

func TestVoiceAuthBootstrapRechecksProofAfterBootLockWait(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	boot := r.target.NodeBootID
	requester := &bootstrapRequester{secret: r.secret, boot: boot}
	bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, requester))
	nonce := strings.Repeat("a", 64)
	body, err := json.Marshal(protocol.HealthBootstrap{Version: 1, NodeBootID: boot, Nonce: nonce})
	require.NoError(t, err)
	wire, _, err := protocol.SignHTTPRequest(protocol.EndpointHealthBootstrap, body, "", protocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Add(-28*time.Second).Unix(), 10), Nonce: nonce}, []byte(r.secret))
	require.NoError(t, err)
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	_, err = tx.Exec(`SELECT login_family_generation_lock('boot',$1::uuid)`, boot)
	require.NoError(t, err)
	done := make(chan int, 1)
	go func() { done <- r.do(wire).Code }()
	closureAwaitWaiters(t, r.db, 1)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case code := <-done:
		t.Fatalf("bootstrap escaped held identity lock: %d", code)
	case <-timer.C:
	}
	require.NoError(t, tx.Commit())
	select {
	case code := <-done:
		require.Equal(t, http.StatusForbidden, code)
	case <-time.After(5 * time.Second):
		t.Fatal("expired request did not finish")
	}
}
