package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing immutable INSERT comparison would allow a changed retry to rewrite
// the original commitments; these assertions spend the signed request in SQL.
func TestVoiceAuthConnectionRegistrationImmutableIdempotence(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	assert.Equal(t, r.familyID.String(), binding.FamilyID)
	assert.Equal(t, r.target, binding.Target)
	var tokenDigest, bodyDigest string
	require.NoError(t, r.db.QueryRow(`SELECT registration_token_sha256,registration_body_sha256 FROM voice_auth_connections WHERE connection_generation=$1`, r.target.ConnectionGeneration).Scan(&tokenDigest, &bodyDigest))
	body, err := json.Marshal(r.registration())
	require.NoError(t, err)
	assert.Equal(t, mediaproof.TokenDigest(r.token), tokenDigest)
	assert.Equal(t, mediaproof.TokenDigest(string(body)), bodyDigest)
	r.register(t)
	changed := r.registration()
	changed.SocketID += "-other"
	w, _ := r.request(t, protocol.EndpointConnectionRegister, changed, r.token)
	assert.Equal(t, http.StatusConflict, w.Code)
	var n int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM voice_auth_connections WHERE connection_generation=$1 AND socket_id=$2 AND registration_token_sha256=$3 AND registration_body_sha256=$4`, r.target.ConnectionGeneration, r.target.SocketID, tokenDigest, bodyDigest).Scan(&n))
	assert.Equal(t, 1, n)
	// A different token in the same family cannot replace the original digest.
	claims, err := auth.ValidateAccessToken(r.token, r.secret)
	require.NoError(t, err)
	// Mint at the original instant so only the token ID differs: a second
	// boundary would otherwise change exp and fail identity (401), not the digest.
	sid, err := uuid.Parse(claims.SessionID)
	require.NoError(t, err)
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: r.userID, SID: sid, CredentialEpoch: r.epoch, EmailVerified: true}, claims.IssuedAt.Time)
	require.NoError(t, err)
	replacement, err := prepared.Sign(r.secret)
	require.NoError(t, err)
	require.NotEqual(t, r.token, replacement)
	w, _ = r.request(t, protocol.EndpointConnectionRegister, r.registration(), replacement)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestVoiceAuthConnectionRegistrationRequiresRealCredentialAndHop(t *testing.T) {
	for _, name := range []string{"missing bearer", "missing hop", "wrong SID", "wrong identity", "wrong epoch", "expired bearer", "missing boot", "retired boot"} {
		t.Run(name, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			token := r.token
			value := r.registration()
			want := http.StatusForbidden
			switch name {
			case "missing bearer":
				wire, _ := r.signed(t, protocol.EndpointConnectionRegister, value, token)
				headers := wire.Headers[:0]
				for _, h := range wire.Headers {
					if h.Name != "Authorization" {
						headers = append(headers, h)
					}
				}
				wire.Headers = headers
				assert.Equal(t, http.StatusUnauthorized, r.do(wire).Code)
				return
			case "missing hop":
				wire, _ := r.signed(t, protocol.EndpointConnectionRegister, value, token)
				headers := wire.Headers[:0]
				for _, h := range wire.Headers {
					if h.Name != "X-Concord-Service-Proof" {
						headers = append(headers, h)
					}
				}
				wire.Headers = headers
				assert.Equal(t, want, r.do(wire).Code)
				return
			case "wrong SID":
				var err error
				token, err = auth.GenerateAccessToken(r.userID.String(), r.secret, true, r.epoch, uuid.NewString())
				require.NoError(t, err)
				want = http.StatusUnauthorized
			case "wrong identity":
				claims, err := auth.ValidateAccessToken(token, r.secret)
				require.NoError(t, err)
				other := testdb.CreateUser(t, r.db)
				_, err = r.db.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, other, r.epoch)
				require.NoError(t, err)
				token, err = auth.GenerateAccessToken(other.String(), r.secret, true, r.epoch, claims.SessionID)
				require.NoError(t, err)
				want = http.StatusUnauthorized
			case "wrong epoch":
				value.CredentialEpoch = strings.ReplaceAll(uuid.NewString(), "-", "")
				want = http.StatusConflict
			case "expired bearer":
				claims, err := auth.ValidateAccessToken(token, r.secret)
				require.NoError(t, err)
				prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: r.userID, SID: uuid.MustParse(claims.SessionID), CredentialEpoch: r.epoch, EmailVerified: true}, time.Now().Add(-time.Hour))
				require.NoError(t, err)
				token, err = prepared.Sign(r.secret)
				require.NoError(t, err)
				want = http.StatusUnauthorized
			case "missing boot":
				value.NodeBootID = uuid.NewString()
			case "retired boot":
				_, err := r.db.Exec(`UPDATE voice_auth_boot_fences SET retired_at=NOW() WHERE node_boot_id=$1`, r.target.NodeBootID)
				require.NoError(t, err)
			}
			w, _ := r.request(t, protocol.EndpointConnectionRegister, value, token)
			assert.Equal(t, want, w.Code, w.Body.String())
			var n int
			require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM voice_auth_connections`).Scan(&n))
			assert.Zero(t, n)
		})
	}
}
func voiceAuthRollback(t *testing.T, tx *sql.Tx) {
	t.Helper()
	err := tx.Rollback()
	if !errors.Is(err, sql.ErrTxDone) {
		require.NoError(t, err)
	}
}

func TestVoiceAuthConnectionRegistrationSignedResponseTampering(t *testing.T) {
	r := newVoiceAuthRig(t)
	w, request := r.request(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	require.Equal(t, http.StatusOK, w.Code)
	expected := protocol.ExpectedResponse{UserID: r.userID.String()}
	r.verify(t, protocol.EndpointConnectionRegister, w, request, expected)
	ctx, err := protocol.NewRequestContext(request, expected)
	require.NoError(t, err)
	for _, name := range []string{"body", "proof", "nonce", "status"} {
		t.Run(name, func(t *testing.T) {
			wire := voiceAuthResponse(w)
			wire.Body = append([]byte(nil), wire.Body...)
			wire.Headers = append([]protocol.Header(nil), wire.Headers...)
			switch name {
			case "body":
				var payload map[string]any
				require.NoError(t, json.Unmarshal(wire.Body, &payload))
				payload["familyId"] = uuid.NewString()
				wire.Body, err = json.Marshal(payload)
				require.NoError(t, err)
			case "proof", "nonce":
				for i, h := range wire.Headers {
					if h.Name == "X-Concord-Auth-Connection-Response-"+map[string]string{"proof": "Proof", "nonce": "Nonce"}[name] {
						if h.Value[0] == '0' {
							wire.Headers[i].Value = "1" + h.Value[1:]
						} else {
							wire.Headers[i].Value = "0" + h.Value[1:]
						}
					}
				}
			case "status":
				wire.Status = http.StatusNoContent
			}
			_, err := protocol.VerifyHTTPResponse(protocol.EndpointConnectionRegister, wire, ctx, []byte(r.secret), time.Now())
			assert.Error(t, err)
		})
	}
}

func TestVoiceAuthConnectionRegistrationAfterRevocation(t *testing.T) {
	r := newVoiceAuthRig(t)
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { voiceAuthRollback(t, tx) })
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	targets, err := loginfamily.CaptureUserTargetsTx(t.Context(), tx, user, nil)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(t.Context(), tx, targets, loginfamily.AuthCause{Scope: "user", Reason: "explicit_revoke"})
	require.NoError(t, err)
	wire, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	done := make(chan int, 1)
	go func() { done <- r.do(wire).Code }()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	waiting := false
	for !waiting {
		select {
		case <-deadline:
			t.Fatal("authentication did not wait for revocation authority")
		case <-tick.C:
			require.NoError(t, r.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM users%')`).Scan(&waiting))
		}
	}
	require.NoError(t, tx.Commit())
	select {
	case status := <-done:
		// Frozen ABI productive-registration failure table: post-lock family denial is 403.
		assert.Equal(t, http.StatusForbidden, status)
	case <-time.After(3 * time.Second):
		t.Fatal("registration did not finish after revocation")
	}
	var n int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM voice_auth_connections`).Scan(&n))
	assert.Zero(t, n)
}

// A lock wait must not extend a token's validity. The request reaches the real
// middleware while valid, then blocks on the canonical generation key.
func TestVoiceAuthConnectionRegistrationRechecksExpiryAfterGenerationWait(t *testing.T) {
	r := newVoiceAuthRigAt(t, time.Now().Add(-auth.AccessTokenTTL+3*time.Second))
	claims, err := auth.ValidateAccessToken(r.token, r.secret)
	require.NoError(t, err)
	token := r.token
	expiresAt := claims.ExpiresAt.Time
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { voiceAuthRollback(t, tx) })
	_, err = tx.Exec(`SELECT login_family_generation_lock('connection',$1::uuid)`, r.target.ConnectionGeneration)
	require.NoError(t, err)
	wire, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), token)
	done := make(chan int, 1)
	go func() { done <- r.do(wire).Code }()
	// Observe the actual PostgreSQL waiter rather than guessing its scheduling.
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	waiting := false
	for !waiting {
		select {
		case <-deadline:
			t.Fatal("registration did not wait for generation lock")
		case <-tick.C:
			require.NoError(t, r.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%login_family_generation_lock%')`).Scan(&waiting))
		}
	}
	remaining := time.Until(expiresAt) + 100*time.Millisecond
	if remaining > 0 {
		timer := time.NewTimer(remaining)
		<-timer.C
	}
	require.NoError(t, tx.Commit())
	select {
	case status := <-done:
		// The initial bearer was valid; this is a final productive denial (frozen ABI).
		assert.Equal(t, http.StatusForbidden, status)
	case <-time.After(3 * time.Second):
		t.Fatal("registration failed to finish")
	}
	var n int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM voice_auth_connections`).Scan(&n))
	assert.Zero(t, n)
}

func TestVoiceAuthConnectionRegistrationRechecksProofAfterGenerationWait(t *testing.T) {
	r := newVoiceAuthRig(t)
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { voiceAuthRollback(t, tx) })
	_, err = tx.Exec(`SELECT login_family_generation_lock('connection',$1::uuid)`, r.target.ConnectionGeneration)
	require.NoError(t, err)
	body, err := json.Marshal(r.registration())
	require.NoError(t, err)
	staleAt := time.Now().Add(-29 * time.Second).Truncate(time.Second)
	wire, _, err := protocol.SignHTTPRequest(protocol.EndpointConnectionRegister, body, r.token, protocol.AttemptMeta{Timestamp: fmt.Sprint(staleAt.Unix()), Nonce: mediaproof.TokenDigest(uuid.NewString())}, []byte(r.secret))
	require.NoError(t, err)
	fresh := fmt.Sprint(time.Now().Unix())
	wire.Headers = append(wire.Headers, protocol.Header{Name: "X-Concord-Service-Timestamp", Value: fresh}, protocol.Header{Name: "X-Concord-Service-Proof", Value: mediaproof.Sign(mediaproof.DeriveKey(r.secret, "concord/media-plane-service-hop/v1"), "v1", fresh, wire.Method, wire.Path, mediaproof.TokenDigest(r.token))})
	done := make(chan int, 1)
	go func() { done <- r.do(wire).Code }()
	deadline := time.After(time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	waiting := false
	for !waiting {
		select {
		case <-deadline:
			t.Fatal("registration did not wait for generation lock")
		case <-tick.C:
			require.NoError(t, r.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%login_family_generation_lock%')`).Scan(&waiting))
		}
	}
	timer := time.NewTimer(time.Until(staleAt.Add(31 * time.Second)))
	<-timer.C
	require.NoError(t, tx.Commit())
	select {
	case status := <-done:
		assert.Equal(t, http.StatusForbidden, status)
	case <-time.After(3 * time.Second):
		t.Fatal("registration failed to finish")
	}
	var n int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM voice_auth_connections`).Scan(&n))
	assert.Zero(t, n)
}

func TestVoiceAuthConnectionRegistrationRejectsOnlySignedTrailingAliases(t *testing.T) {
	r := newVoiceAuthRig(t)
	wire, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
	wire.Path += "/"
	w := r.do(wire)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, w.Header().Get("Location"))
	r.router.POST("/ordinary", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w = r.do(protocol.HTTPRequest{Method: http.MethodPost, Path: "/ordinary/"})
	assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
	assert.Equal(t, "/ordinary", w.Header().Get("Location"))
}
