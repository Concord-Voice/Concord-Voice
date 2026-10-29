package auth_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/devicerecovery"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecoveryPolicyRace_ConsumersRecheckAfterPolicyWriter proves that every
// recovery-token consumer linearizes an already-issued email proof with the
// writer that enrolls a confirmed recovery-only factor.  The writer owns the
// users row first; a consumer must wait for that row and then reject the now
// insufficient proof before it can mutate or disclose recovery state.
func TestRecoveryPolicyRace_ConsumersRecheckAfterPolicyWriter(t *testing.T) {
	t.Run("password reset leaves credentials and keys untouched", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoverypolicypasswordrace")
		token := recoveryTokenFor(t, ts, user)
		var beforePassword string
		require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&beforePassword))
		var beforeKeys int
		require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM user_keys WHERE user_id = $1`, user.ID).Scan(&beforeKeys))

		_, wrappedKey, salt := testhelpers.E2EETestKeys()
		w := raceRecoveryPolicyWriter(t, ts, user.ID, func() *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodPost, pathRecoveryResetPwd, map[string]interface{}{
				"recovery_token": token, "new_password": testNewPassword,
				"wrapped_private_key": wrappedKey, "key_derivation_salt": salt, "key_derivation_alg": testKeyDerivationAlg,
			}, nil)
		})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var afterPassword string
		require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&afterPassword))
		assert.Equal(t, beforePassword, afterPassword)
		var afterKeys int
		require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM user_keys WHERE user_id = $1`, user.ID).Scan(&afterKeys))
		assert.Equal(t, beforeKeys, afterKeys)
	})

	t.Run("account reset leaves password and public key untouched", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoverypolicyaccountrace")
		token := recoveryTokenFor(t, ts, user)
		var beforePassword string
		var beforePublicKey []byte
		require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&beforePassword))
		require.NoError(t, ts.DB.QueryRow(`SELECT public_key FROM public_keys WHERE user_id = $1`, user.ID).Scan(&beforePublicKey))
		publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()

		w := raceRecoveryPolicyWriter(t, ts, user.ID, func() *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodPost, pathRecoveryResetAcct, map[string]interface{}{
				"recovery_token": token, "new_password": testNewPassword, "wrapped_private_key": wrappedKey,
				"key_derivation_salt": salt, "key_derivation_alg": testKeyDerivationAlg, "public_key": publicKey,
				"acknowledge_data_loss": true,
			}, nil)
		})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var afterPassword string
		var afterPublicKey []byte
		require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&afterPassword))
		require.NoError(t, ts.DB.QueryRow(`SELECT public_key FROM public_keys WHERE user_id = $1`, user.ID).Scan(&afterPublicKey))
		assert.Equal(t, beforePassword, afterPassword)
		assert.Equal(t, beforePublicKey, afterPublicKey)
	})

	t.Run("device create makes no request", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoverypolicydevicecreate")
		token := recoveryTokenFor(t, ts, user)
		require.NoError(t, insertTrustedRecoveryDevice(ts.DB, user.ID))
		w := raceRecoveryPolicyWriter(t, ts, user.ID, func() *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodPost, pathDeviceReqCreate, deviceCreateV2(t, user.ID, token), nil)
		})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assertNoDeviceRequest(t, ts, user.ID)
	})

	t.Run("device poll does not disclose approved payload", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoverypolicydevicepoll")
		token := recoveryTokenFor(t, ts, user)
		requestID, _ := approvedDeviceRecoveryRequest(t, ts, user, token)
		headers := http.Header{"Authorization": []string{"Bearer " + token}}
		w := raceRecoveryPolicyWriter(t, ts, user.ID, func() *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodGet, pathDeviceReqPoll+requestID, nil, headers)
		})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "encrypted_payload")
		assertDeviceApproved(t, ts, requestID)
	})

	t.Run("device complete does not transition or erase payload", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoverypolicydevicecomplete")
		token := recoveryTokenFor(t, ts, user)
		requestID, digest := approvedDeviceRecoveryRequest(t, ts, user, token)
		headers := http.Header{"Authorization": []string{"Bearer " + token}}
		w := raceRecoveryPolicyWriter(t, ts, user.ID, func() *httptest.ResponseRecorder {
			return ts.DoRequest(http.MethodPost, pathDeviceReqPoll+requestID+"/complete", devicerecovery.CompleteBody{ProtocolVersion: 2, TranscriptHash: digest}, headers)
		})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assertDeviceApproved(t, ts, requestID)
	})

	t.Run("social create inserts no request", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoverypolicysocialcreate")
		token := recoveryTokenFor(t, ts, user)
		circleID := insertRecoveryCircle(t, ts, user.ID)
		w := raceRecoveryPolicyWriter(t, ts, user.ID, func() *httptest.ResponseRecorder {
			return doDormantSocialRequest(t, ts, http.MethodPost, pathSocialReqCreate, map[string]string{"recovery_token": token, "ephemeral_public_key": "dGVzdA=="}, nil)
		})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var count int
		require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM recovery_circle_requests WHERE circle_id = $1`, circleID).Scan(&count))
		assert.Zero(t, count)
	})

	t.Run("social poll does not disclose guardian share", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoverypolicysocialpoll")
		token := recoveryTokenFor(t, ts, user)
		requestID := completedSocialRecoveryRequest(t, ts, user.ID, token)
		headers := http.Header{"Authorization": []string{"Bearer " + token}}
		w := raceRecoveryPolicyWriter(t, ts, user.ID, func() *httptest.ResponseRecorder {
			return doDormantSocialRequest(t, ts, http.MethodGet, pathSocialReqPoll+requestID, nil, headers)
		})
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "encrypted_share")
		var status string
		require.NoError(t, ts.DB.QueryRow(`SELECT status FROM recovery_circle_requests WHERE id = $1`, requestID).Scan(&status))
		assert.Equal(t, "complete", status)
	})
}

// TestRecoveryPolicyRace_FinalIssuanceRechecksPolicy keeps the writer's commit
// between a successful preflight and CompleteRecovery.  These are public HTTP
// flows; the small wrappers only create that otherwise unobservable boundary.
func TestRecoveryPolicyRace_FinalIssuanceRechecksPolicy(t *testing.T) {
	t.Run("email verification returns no final authority after policy tightens", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoveryissueemailrace")
		insertRecoveryKey(t, ts, user.ID)
		checker := &issuancePolicyChecker{Handler: newRaceMFA(ts), after: func() { tightenRecoveryPolicy(t, ts, user.ID) }}
		h := auth.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), testhelpers.TestJWTSecret, nil)
		h.SetMFAChecker(checker)
		router := gin.New()
		router.POST(pathRecoveryVerifyCode, h.RecoveryVerifyCode)
		seedRecoveryCode(t, ts, user.Email, testRecoveryCode, user.ID)

		w := serveRaceRequest(router, http.MethodPost, pathRecoveryVerifyCode, map[string]string{"email": user.Email, "code": testRecoveryCode})
		require.True(t, checker.committed.Load(), "policy writer must commit after preflight and before the response")
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assertNoRecoveryAuthority(t, w.Body.String())
	})

	t.Run("recovery challenge completion returns no final authority after policy tightens", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "recoveryissuancechallenge")
		insertRecoveryKey(t, ts, user.ID)
		keyring, err := mfa.ParseKeyring(strings.Repeat("0", 64), 1, "")
		require.NoError(t, err)
		mfaHandler := mfa.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), keyring, testhelpers.TestJWTSecret, nil, "test")
		secret := installConfirmedTOTP(t, ts, user.ID, keyring)
		_, err = ts.DB.Exec(`UPDATE users SET recovery_only_methods = '{totp}' WHERE id = $1`, user.ID)
		require.NoError(t, err)
		authHandler := auth.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), testhelpers.TestJWTSecret, nil)
		authHandler.SetMFAChecker(mfaHandler)
		completer := &issuancePolicyCompleter{delegate: authHandler, after: func() { tightenRecoveryPolicy(t, ts, user.ID) }}
		mfaHandler.SetRecoveryCompleter(completer)
		challenge, _, err := mfaHandler.GenerateRecoveryChallenge(context.Background(), user.ID)
		require.NoError(t, err)
		code, err := totp.GenerateCode(secret, time.Now())
		require.NoError(t, err)
		router := gin.New()
		router.POST("/api/v1/auth/mfa/verify", mfaHandler.Verify)

		w := serveRaceRequest(router, http.MethodPost, "/api/v1/auth/mfa/verify", map[string]string{"mfa_challenge_token": challenge, "method": "totp", "code": code})
		require.True(t, completer.committed.Load(), "policy writer must commit after factor verification and before final issuance")
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assertNoRecoveryAuthority(t, w.Body.String())
	})
}

type issuancePolicyChecker struct {
	*mfa.Handler
	once      sync.Once
	after     func()
	committed atomic.Bool
}

func (c *issuancePolicyChecker) GetRecoveryMethods(ctx context.Context, userID string) ([]string, error) {
	methods, err := c.Handler.GetRecoveryMethods(ctx, userID)
	if err == nil {
		c.once.Do(c.after)
		c.committed.Store(true)
	}
	return methods, err
}

type issuancePolicyCompleter struct {
	delegate  mfa.RecoveryCompleter
	once      sync.Once
	after     func()
	committed atomic.Bool
}

func (c *issuancePolicyCompleter) CompleteRecovery(ctx *gin.Context, userID, factor string) bool {
	c.once.Do(c.after)
	c.committed.Store(true)
	return c.delegate.CompleteRecovery(ctx, userID, factor)
}

func newRaceMFA(ts *testhelpers.TestServer) *mfa.Handler {
	return mfa.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), nil, testhelpers.TestJWTSecret, nil, "test")
}

func serveRaceRequest(router *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(string(encoded)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func tightenRecoveryPolicy(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("roll back recovery policy writer: %v", err)
		}
	}()
	var locked string
	require.NoError(t, tx.QueryRow(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&locked))
	credentialID := uuid.New()
	_, err = tx.Exec(`INSERT INTO user_mfa_webauthn(user_id, credential_id, public_key) VALUES ($1, $2, $3)`, userID, credentialID[:], []byte("race-webauthn-public-key"))
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE users SET recovery_only_methods = '{webauthn}' WHERE id = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func insertRecoveryKey(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`INSERT INTO user_recovery_keys(user_id, recovery_wrapped_private_key, recovery_key_salt) VALUES ($1, $2, $3)`, userID, []byte("race-wrapped-private-key"), []byte("race-salt"))
	require.NoError(t, err)
}

func installConfirmedTOTP(t *testing.T, ts *testhelpers.TestServer, userID string, keyring *mfa.Keyring) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Concord Test", AccountName: userID})
	require.NoError(t, err)
	sealed, nonce, version, err := keyring.Seal([]byte(key.Secret()))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO user_mfa_totp(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed) VALUES ($1, $2, $3, $4, TRUE, TRUE)`, userID, sealed, nonce, version)
	require.NoError(t, err)
	return key.Secret()
}

func assertNoRecoveryAuthority(t *testing.T, body string) {
	t.Helper()
	assert.NotContains(t, body, "recovery_token")
	assert.NotContains(t, body, "recovery_wrapped_private_key")
	assert.NotContains(t, body, "recovery_key_salt")
}

// raceRecoveryPolicyWriter stages a valid recovery-only enrollment while its
// users-row lock remains held.  A protected consumer must become a row-lock
// waiter before the writer commits, so this is an ordering proof rather than a
// timing-sensitive best effort.
func raceRecoveryPolicyWriter(t *testing.T, ts *testhelpers.TestServer, userID string, request func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("roll back recovery policy writer: %v", err)
		}
	})
	var txID int64
	require.NoError(t, tx.QueryRow(`SELECT txid_current()`).Scan(&txID))
	var locked string
	require.NoError(t, tx.QueryRow(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&locked))
	_, err = tx.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed) VALUES ($1, $2, $3, TRUE, TRUE)`, userID, []byte("race-secret"), []byte("race-nonce"))
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE users SET recovery_only_methods = '{totp}' WHERE id = $1`, userID)
	require.NoError(t, err)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request() }()
	dbtest.WaitForRowLockWaiter(t, ts.DB, txID)
	require.NoError(t, tx.Commit())
	return <-done
}

func insertTrustedRecoveryDevice(db *sql.DB, userID string) error {
	_, err := db.Exec(`INSERT INTO trusted_recovery_devices(user_id, device_name, machine_id) VALUES ($1, 'race device', $2)`, userID, uuid.NewString())
	return err
}

func approvedDeviceRecoveryRequest(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, token string) (string, string) {
	t.Helper()
	require.NoError(t, insertTrustedRecoveryDevice(ts.DB, user.ID))
	create := ts.DoRequest(http.MethodPost, pathDeviceReqCreate, deviceCreateV2(t, user.ID, token), nil)
	require.Equal(t, http.StatusOK, create.Code, create.Body.String())
	var pending struct {
		RequestID string `json:"request_id"`
	}
	testhelpers.ParseJSON(t, create, &pending)
	digest, payload := deviceOfferV2(t, ts, user.ID, pending.RequestID)
	approved := ts.DoRequest(http.MethodPost, "/api/v1/mfa/recovery-requests/"+pending.RequestID+"/respond", map[string]any{"action": "approve", "protocol_version": 2, "transcript_hash": digest, "encrypted_payload": payload}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, approved.Code, approved.Body.String())
	return pending.RequestID, digest
}

func assertNoDeviceRequest(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM recovery_requests WHERE user_id = $1`, userID).Scan(&count))
	assert.Zero(t, count)
}

func assertDeviceApproved(t *testing.T, ts *testhelpers.TestServer, requestID string) {
	t.Helper()
	var status string
	var payload []byte
	require.NoError(t, ts.DB.QueryRow(`SELECT status, encrypted_payload FROM recovery_requests WHERE id = $1`, requestID).Scan(&status, &payload))
	assert.Equal(t, "approved", status)
	assert.NotEmpty(t, payload)
}

func insertRecoveryCircle(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	circleID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO recovery_circles(id, user_id, threshold_k, total_shares_n) VALUES ($1, $2, 2, 2)`, circleID, userID)
	require.NoError(t, err)
	return circleID
}

func completedSocialRecoveryRequest(t *testing.T, ts *testhelpers.TestServer, userID, token string) string {
	t.Helper()
	circleID := insertRecoveryCircle(t, ts, userID)
	claims, err := mfa.ValidateChallengeToken(token, testhelpers.TestJWTSecret, mfa.PurposeRecovery)
	require.NoError(t, err)
	requestID := uuid.NewString()
	_, err = ts.DB.Exec(`INSERT INTO recovery_circle_requests(id, circle_id, user_id, recovery_token_jti, ephemeral_public_key, status, shares_received, expires_at) VALUES ($1, $2, $3, $4, $5, 'complete', 2, $6)`, requestID, circleID, userID, claims.ID, []byte("ephemeral"), time.Now().Add(time.Hour))
	require.NoError(t, err)
	guardian := ts.CreateTestUser(t, "recoverypolicyguardian")
	_, err = ts.DB.Exec(`INSERT INTO recovery_circle_responses(request_id, contact_id, encrypted_share) VALUES ($1, $2, $3)`, requestID, guardian.ID, []byte("never-disclose-this-share"))
	require.NoError(t, err)
	return requestID
}
