package dm

// Handler-seam tests for the DM message delete's delete-rate soft-lock
// (#3455, design spec §2.4–§2.6 as amended by D-1). Postgres is real; Redis
// is an in-process miniredis for its frozen clock — TTLs advance only via
// FastForward, never a sleep — the same exception
// internal/api/router_klipy_ratelimit_test.go:30-38 records. The route
// limiter is not mounted, so the soft-lock is reachable directly (AC1); the
// composition through the limiter is T6's router test.
//
// Every test names the mutant it kills.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	softLockGoodCode     = "123456"
	softLockPlaintext    = "correct-horse-battery-staple" // pragma: allowlist secret
	softLockDeleteRoute  = "/dm/conversations/:id/messages/:message_id"
	softLockRetryAfter30 = "30"
)

// softLockVerifier is a scriptable stepup.MFAVerifier that records what the
// delete route asked it. A code equal to good verifies; err, when set, is a
// verifier fault. onVerify runs inside the delete transaction.
type softLockVerifier struct {
	good     string
	err      error
	purposes []stepup.Purpose
	onVerify func()
}

func (v *softLockVerifier) IsEnabled(context.Context, string) bool { return true }
func (v *softLockVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return nil, errors.New("the delete route must use the locked subject's methods")
}
func (v *softLockVerifier) VerifyCode(context.Context, string, stepup.Purpose, string) (bool, error) {
	return false, errors.New("the delete route must verify inside its transaction")
}
func (v *softLockVerifier) VerifyCodeTx(_ context.Context, _ *sql.Tx, _ string, purpose stepup.Purpose, code string) (bool, error) {
	v.purposes = append(v.purposes, purpose)
	if v.onVerify != nil {
		v.onVerify()
	}
	return code == v.good, v.err
}

type softLockHarness struct {
	db       *sql.DB
	mr       *miniredis.Miniredis
	rdb      *redis.Client
	verifier *softLockVerifier
	logs     *bytes.Buffer
	handler  *Handler
	convID   string
	actor    string
	peer     string
	epoch    string
	// sid, when set, is the access token's session claim, which a step-up
	// grace is keyed on (#3454).
	sid string
}

// newSoftLockHarness wires a DM handler to a real database and its own
// miniredis. The actor has no privacy_settings row, which is the own rule ON.
func newSoftLockHarness(t *testing.T) *softLockHarness {
	t.Helper()
	db := hiddenTestDB(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	hs := &softLockHarness{db: db, mr: mr, rdb: rdb, verifier: &softLockVerifier{good: softLockGoodCode}, logs: &bytes.Buffer{}}
	hs.convID, hs.actor, hs.peer = seedHiddenConv(t, db)
	hs.handler = hs.newHandler(db, rdb)
	return hs
}

func (hs *softLockHarness) newHandler(db *sql.DB, rdb *redis.Client) *Handler {
	log := logger.NewWithWriter(hs.logs)
	return NewHandler(HandlerDeps{
		DB:          db,
		Log:         log,
		Redis:       rdb,
		PurgeEngine: purge.NewEngine(db, log, purge.NewReaper(db, log, nil), 5000),
		MFAVerifier: hs.verifier,
	})
}

// deleteAs sends DELETE for messageID as userID with body (empty for none).
func (hs *softLockHarness) deleteAs(t *testing.T, userID, messageID, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE(softLockDeleteRoute, func(c *gin.Context) {
		c.Set("user_id", userID)
		claims := jwt.MapClaims{"cred_epoch": hs.epoch}
		if hs.sid != "" {
			claims["sid"] = hs.sid
		}
		c.Set(middleware.JWTClaimsContextKey, claims)
		hs.handler.DeleteMessage(c)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodDelete,
		"/dm/conversations/"+hs.convID+"/messages/"+messageID, strings.NewReader(body)))
	return w
}

func (hs *softLockHarness) delete(t *testing.T, messageID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return hs.deleteAs(t, hs.actor, messageID, body)
}

func (hs *softLockHarness) message(t *testing.T) string {
	t.Helper()
	return insertUpdateTestMessage(t, hs.db, hs.convID, hs.actor, "ciphertext")
}

func (hs *softLockHarness) messageExists(t *testing.T, messageID string) bool {
	t.Helper()
	var n int
	require.NoError(t, hs.db.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, messageID).Scan(&n))
	return n == 1
}

func (hs *softLockHarness) burstKey(userID string) string {
	return stepup.NewDeleteSoftLock(nil).Key(uuid.MustParse(userID), stepup.DMDeleteScope())
}

func (hs *softLockHarness) dayKey(userID string) string {
	return stepup.NewDeleteSoftLock(nil).DayKey(uuid.MustParse(userID))
}

func (hs *softLockHarness) budgetKey() string {
	return stepup.NewBudget(nil, stepup.MFASettingsBudgetPrefix).Key(hs.actor)
}

// overThreshold primes the actor's burst counter so the next delete is the
// 16th: over, with a 30s window starting at that hit.
func (hs *softLockHarness) overThreshold(t *testing.T) {
	t.Helper()
	require.NoError(t, hs.mr.Set(hs.burstKey(hs.actor), "15"))
}

func (hs *softLockHarness) setOwnRule(t *testing.T, on bool) {
	t.Helper()
	_, err := hs.db.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = EXCLUDED.require_auth_before_purge`, hs.actor, on)
	require.NoError(t, err)
}

// enrollMFA gives the actor an inline factor (policy P1), so the own rule
// asks for MFA rather than the password.
func (hs *softLockHarness) enrollMFA(t *testing.T) {
	t.Helper()
	_, err := hs.db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, NOW())`, uuid.NewString(), hs.actor, []byte("cred-"+hs.actor))
	require.NoError(t, err)
}

// setPassword gives the actor a real Argon2id hash, so the own rule's
// password leg can verify.
func (hs *softLockHarness) setPassword(t *testing.T) {
	t.Helper()
	hash, err := auth.HashPassword(softLockPlaintext)
	require.NoError(t, err)
	_, err = hs.db.Exec(`UPDATE users SET password_hash = $1 WHERE id = $2`, hash, hs.actor)
	require.NoError(t, err)
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}

func codeBody(code string) string   { return `{"mfa_code":"` + code + `"}` }
func passwordBody(pw string) string { return `{"current_password":"` + pw + `"}` }
func tokenBody(token string) string { return `{"step_up_token":"` + token + `"}` }

// mintToken mints a password step-up token for the actor, as the mint
// endpoint would after verifying the password (#3509).
func (hs *softLockHarness) mintToken(t *testing.T, purpose stepup.Purpose) string {
	t.Helper()
	token, e := stepup.MintToken(context.Background(), hs.db, hs.actor, stepup.FactorPassword, purpose, hs.epoch)
	require.Nil(t, e)
	return token
}
func assertNoKey(t *testing.T, mr *miniredis.Miniredis, key, why string) {
	t.Helper()
	assert.False(t, mr.Exists(key), why)
}

// assertSoftLockRefusal checks the §2.6 decoration every soft-lock 403
// carries.
func assertSoftLockRefusal(t *testing.T, w *httptest.ResponseRecorder, wantError, retryAfter string) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	body := decodeBody(t, w)
	assert.Equal(t, wantError, body["error"])
	assert.Equal(t, true, body["delete_rate_limited"])
	assert.Equal(t, retryAfter, w.Header().Get("Retry-After"))
	return body
}

// TestDMDeleteSoftLock_AC1 is AC1 at the handler seam: deletes 1–15
// succeed; the 16th is refused with every flag and a Retry-After read from the
// burst key's PTTL; the 16th with a valid confirmation succeeds and resets
// both tiers; the next 15 do not prompt; the 16th after the reset prompts
// again.
//
// Mutants killed: (1) drop the Hit call — the 16th succeeds; (2) drop the
// post-commit Reset — the burst key survives the confirmed delete and the
// next delete is refused; (3) hard-code Retry-After to the 30s window — the
// refusal reads "30", not "18"; (4) pass stepup.PurposeDMClear — the recorded
// purpose differs; (5) drop the delete_rate_limited decoration.
func TestDMDeleteSoftLock_AC1(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.enrollMFA(t)

	for i := 1; i <= stepup.DeleteSoftLockThreshold; i++ {
		w := hs.delete(t, hs.message(t), "")
		require.Equal(t, http.StatusOK, w.Code, "delete %d: %s", i, w.Body.String())
		if i == 1 {
			hs.mr.FastForward(12 * time.Second)
		}
	}

	sixteenth := hs.message(t)
	body := assertSoftLockRefusal(t, hs.delete(t, sixteenth, ""), "MFA verification required", "18")
	assert.Equal(t, true, body["mfa_required"])
	assert.Equal(t, []any{"webauthn"}, body["methods"])
	assert.True(t, hs.messageExists(t, sixteenth), "a refused delete must not delete")

	w := hs.delete(t, sixteenth, codeBody(softLockGoodCode))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, hs.messageExists(t, sixteenth))
	assert.Equal(t, []stepup.Purpose{stepup.PurposeDMMessageDelete}, hs.verifier.purposes)
	assertNoKey(t, hs.mr, hs.burstKey(hs.actor), "a confirmed delete resets the burst tier")
	assertNoKey(t, hs.mr, hs.dayKey(hs.actor), "a confirmed delete resets the day tier")
	assertNoKey(t, hs.mr, hs.budgetKey(), "a verified confirmation clears the budget")

	for i := 1; i <= stepup.DeleteSoftLockThreshold; i++ {
		w := hs.delete(t, hs.message(t), "")
		require.Equal(t, http.StatusOK, w.Code, "post-reset delete %d: %s", i, w.Body.String())
	}
	assertSoftLockRefusal(t, hs.delete(t, hs.message(t), ""), "MFA verification required", softLockRetryAfter30)
}

// TestDMDeleteSoftLock_OwnRuleOffIsNeverCountedOrRefused proves a member who
// turned require_auth_before_purge off is outside the population: a counter
// already far over the limit is neither read nor advanced, a factor in the
// body is ignored, and nothing is charged.
//
// Mutant killed: call Hit regardless of ownRule — the delete is refused and
// the counter reads 101.
func TestDMDeleteSoftLock_OwnRuleOffIsNeverCountedOrRefused(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.setOwnRule(t, false)
	require.NoError(t, hs.mr.Set(hs.burstKey(hs.actor), "100"))

	w := hs.delete(t, hs.message(t), codeBody("000000"))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "100", mustRedisGet(t, hs.mr, hs.burstKey(hs.actor)), "outside the population nothing is counted")
	assertNoKey(t, hs.mr, hs.dayKey(hs.actor), "outside the population the day tier is untouched")
	assertNoKey(t, hs.mr, hs.budgetKey(), "outside the population nothing is charged")
	assert.Empty(t, hs.verifier.purposes, "outside the population nothing is verified")
}

// TestDMDeleteSoftLock_MissingPrivacyRowMeansOn proves an absent
// privacy_settings row is the own rule ON (H2), exactly as Clear reads it.
//
// Mutant killed: COALESCE(…, FALSE) — the delete is not counted.
func TestDMDeleteSoftLock_MissingPrivacyRowMeansOn(t *testing.T) {
	hs := newSoftLockHarness(t)

	w := hs.delete(t, hs.message(t), "")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "1", mustRedisGet(t, hs.mr, hs.burstKey(hs.actor)))
	assert.Equal(t, "1", mustRedisGet(t, hs.mr, hs.dayKey(hs.actor)))
}

// TestDMDeleteSoftLock_AuthorAndRowChecksPrecedeTheCount proves nothing
// population-derived runs before the author 403 or the 404 (I7): neither
// leaves a key.
//
// Mutant killed: move the Hit above the row read / author check — the
// non-author's and the 404's keys appear.
func TestDMDeleteSoftLock_AuthorAndRowChecksPrecedeTheCount(t *testing.T) {
	hs := newSoftLockHarness(t)

	notAuthor := hs.deleteAs(t, hs.peer, hs.message(t), "")
	require.Equal(t, http.StatusForbidden, notAuthor.Code)
	assert.Equal(t, "You can only delete your own messages", decodeBody(t, notAuthor)["error"])
	assertNoKey(t, hs.mr, hs.burstKey(hs.peer), "a non-author 403 must not be counted")

	missing := hs.delete(t, uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, missing.Code)
	assertNoKey(t, hs.mr, hs.burstKey(hs.actor), "a 404 must not be counted")
}

// TestDMDeleteSoftLock_BadBodyIsRefusedBeforeTheCount proves the body is read
// (and refused) before anything touches the counter.
//
// Mutant killed: read the body after the Hit — the refused request is counted.
func TestDMDeleteSoftLock_BadBodyIsRefusedBeforeTheCount(t *testing.T) {
	hs := newSoftLockHarness(t)
	messageID := hs.message(t)

	w := hs.delete(t, messageID, "null")

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, stepup.ErrMsgInvalidRequestBody, decodeBody(t, w)["error"])
	assertNoKey(t, hs.mr, hs.burstKey(hs.actor), "a 400 must not be counted")
	assert.True(t, hs.messageExists(t, messageID))
}

// TestDMDeleteSoftLock_UnderThresholdIgnoresTheFactor proves a factor sent
// below the threshold is neither verified nor charged (X4, H4).
//
// Mutant killed: charge the budget before the verdict — the budget key
// appears; verify below the threshold — the wrong code is refused.
func TestDMDeleteSoftLock_UnderThresholdIgnoresTheFactor(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.enrollMFA(t)

	w := hs.delete(t, hs.message(t), codeBody("000000"))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, hs.verifier.purposes)
	assertNoKey(t, hs.mr, hs.budgetKey(), "under the threshold nothing is charged")
}

// TestDMDeleteSoftLock_MFAAccount covers the own rule's MFA leg past the
// threshold, and the budget matrix on it.
//
// Mutants killed: charge the budget only when a code is absent / never — the
// wrong-code case reads no charge; clear the budget on a refusal — the
// wrong-code case reads no charge; charge a factor-less request — the
// missing-code case reads a charge; skip the Clear after commit — the valid
// case reads a charge.
func TestDMDeleteSoftLock_MFAAccount(t *testing.T) {
	t.Run("missing code is prompted and not charged", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.enrollMFA(t)
		hs.overThreshold(t)
		messageID := hs.message(t)

		body := assertSoftLockRefusal(t, hs.delete(t, messageID, ""), "MFA verification required", softLockRetryAfter30)

		assert.Equal(t, true, body["mfa_required"])
		assert.True(t, hs.messageExists(t, messageID))
		assertNoKey(t, hs.mr, hs.budgetKey(), "a factor-less request is not charged")
	})

	t.Run("wrong code is refused and charged, not cleared", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.enrollMFA(t)
		hs.overThreshold(t)
		messageID := hs.message(t)

		assertSoftLockRefusal(t, hs.delete(t, messageID, codeBody("000000")), stepup.ErrMsgInvalidMFACode, softLockRetryAfter30)

		assert.True(t, hs.messageExists(t, messageID))
		assert.Equal(t, "1", mustRedisGet(t, hs.mr, hs.budgetKey()))
		assert.Equal(t, "16", mustRedisGet(t, hs.mr, hs.burstKey(hs.actor)), "a refusal does not reset the soft-lock")
	})

	// Rewritten for #3509: the password no longer reaches the route, so the
	// substitute an MFA account must not get away with is a password token,
	// minted before the factor was enrolled.
	t.Run("password token does not substitute for a missing code", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		token := hs.mintToken(t, stepup.PurposeDMMessageDelete)
		hs.enrollMFA(t)
		hs.overThreshold(t)
		messageID := hs.message(t)

		body := assertSoftLockRefusal(t, hs.delete(t, messageID, tokenBody(token)),
			"MFA verification required", softLockRetryAfter30)

		assert.Equal(t, true, body["mfa_required"])
		assert.True(t, hs.messageExists(t, messageID))
		assert.Equal(t, "1", mustRedisGet(t, hs.mr, hs.budgetKey()), "either factor field is a charge (X4)")
	})

	t.Run("valid code deletes and clears the budget", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.enrollMFA(t)
		hs.overThreshold(t)
		require.NoError(t, hs.mr.Set(hs.budgetKey(), "2"))
		messageID := hs.message(t)

		w := hs.delete(t, messageID, codeBody(softLockGoodCode))

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.False(t, hs.messageExists(t, messageID))
		assertNoKey(t, hs.mr, hs.budgetKey(), "a verified, committed confirmation clears the budget")
		assertNoKey(t, hs.mr, hs.burstKey(hs.actor), "a verified, committed confirmation resets the soft-lock")
	})
}

// TestDMDeleteSoftLock_PasswordAccount covers the own rule's password leg
// (D-1): an account with no inline MFA confirms with its password, exactly as
// it does for Clear, instead of H2's hard cap. Since #3509 the password goes
// only to the mint endpoint, and the route spends the token it returned.
//
// Mutant killed: confirm through stepup.VerifyMFAFactorTx only (the MFA-only
// §2.5 path D-1 replaced) — the password account is never admitted, and the
// verifier is called.
func TestDMDeleteSoftLock_PasswordAccount(t *testing.T) {
	t.Run("missing token is prompted with the flag", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.setOwnRule(t, true)
		hs.setPassword(t)
		hs.overThreshold(t)
		messageID := hs.message(t)

		body := assertSoftLockRefusal(t, hs.delete(t, messageID, ""),
			dmMessageDeleteStepUpCopy.CredentialRequired, softLockRetryAfter30)

		assert.Equal(t, true, body["password_required"])
		assert.NotContains(t, body, "step_up_token_invalid")
		assert.True(t, hs.messageExists(t, messageID))
	})

	t.Run("token that matches nothing is refused, flagged and charged", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.setOwnRule(t, true)
		hs.setPassword(t)
		hs.overThreshold(t)
		messageID := hs.message(t)

		body := assertSoftLockRefusal(t, hs.delete(t, messageID, tokenBody("never-minted-but-token-shaped-0000000000")),
			stepup.ErrMsgStepUpTokenInvalid, softLockRetryAfter30)

		assert.Equal(t, true, body["password_required"])
		assert.Equal(t, true, body["step_up_token_invalid"])
		assert.True(t, hs.messageExists(t, messageID))
		assert.Equal(t, "1", mustRedisGet(t, hs.mr, hs.budgetKey()))
	})

	t.Run("minted token deletes and resets", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.setOwnRule(t, true)
		hs.setPassword(t)
		hs.overThreshold(t)
		messageID := hs.message(t)

		w := hs.delete(t, messageID, tokenBody(hs.mintToken(t, stepup.PurposeDMMessageDelete)))

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.False(t, hs.messageExists(t, messageID))
		assert.Empty(t, hs.verifier.purposes, "a password account never reaches the MFA verifier")
		assertNoKey(t, hs.mr, hs.burstKey(hs.actor), "a confirmed delete resets the soft-lock")
		assertNoKey(t, hs.mr, hs.budgetKey(), "a confirmed delete clears the budget")
	})
}

// TestDMDeleteSoftLock_CopyNeverNamesTheSetting pins H3 on the route's own
// copy: neither string may point at the setting whose value the refusal
// would otherwise disclose.
//
// Mutant killed: reuse clearStepUpCopy, whose NoFactors names the setting.
func TestDMDeleteSoftLock_CopyNeverNamesTheSetting(t *testing.T) {
	for _, s := range []string{dmMessageDeleteStepUpCopy.NoFactors, dmMessageDeleteStepUpCopy.CredentialRequired} {
		assert.NotContains(t, strings.ToLower(s), "require authentication")
		assert.NotContains(t, strings.ToLower(s), "privacy")
		assert.NotEmpty(t, s)
	}
}

// TestDMDeleteSoftLock_ExhaustedBudgetNeverReachesTheVerifier proves the
// budget is charged before the transaction opens.
//
// Mutant killed: charge the budget after DeleteOne (or not at all) — the
// valid code verifies and the message is deleted.
func TestDMDeleteSoftLock_ExhaustedBudgetNeverReachesTheVerifier(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.enrollMFA(t)
	hs.overThreshold(t)
	require.NoError(t, hs.mr.Set(hs.budgetKey(), "5"))
	messageID := hs.message(t)

	w := hs.delete(t, messageID, codeBody(softLockGoodCode))

	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	body := decodeBody(t, w)
	assert.Equal(t, stepup.ErrMsgTooManyAttempts, body["error"])
	assert.Equal(t, true, body["step_up_budget_exhausted"])
	assert.Empty(t, hs.verifier.purposes)
	assert.True(t, hs.messageExists(t, messageID))
}

// fastDeadRedis is a client aimed at nothing, built with the C8 options so a
// dead case costs microseconds rather than DialerRetries' default ~1.7s.
func fastDeadRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// TestDMDeleteSoftLock_DeadRedis is AC4 at the handler seam, capped at the
// two dead cases C8 allows per route: a population member gets the 503 and
// nothing is deleted; a member outside it deletes normally. The one fixed
// log line carries the failure class and none of the C7 fields.
//
// Mutant killed: on a Hit error, fall through to the unconfirmed delete —
// the population case returns 200 and the message is gone.
func TestDMDeleteSoftLock_DeadRedis(t *testing.T) {
	t.Run("population member is refused and nothing is deleted", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.handler = hs.newHandler(hs.db, fastDeadRedis(t))
		messageID := hs.message(t)

		w := hs.delete(t, messageID, "")

		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.Equal(t, stepup.ErrMsgDeleteGuardUnavailable, decodeBody(t, w)["error"])
		assert.Equal(t, softLockRetryAfter30, w.Header().Get("Retry-After"))
		assert.True(t, hs.messageExists(t, messageID))
		logs := hs.logs.String()
		assert.Contains(t, logs, "failure_class=delete_softlock_unavailable")
		assertNoC7Fields(t, logs, hs.actor)
	})

	t.Run("member outside the population deletes normally", func(t *testing.T) {
		hs := newSoftLockHarness(t)
		hs.setOwnRule(t, false)
		hs.handler = hs.newHandler(hs.db, fastDeadRedis(t))
		messageID := hs.message(t)

		w := hs.delete(t, messageID, "")

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.False(t, hs.messageExists(t, messageID))
	})
}

// assertNoC7Fields checks the soft-lock-only log lines carry nothing that
// identifies population membership (C7).
func assertNoC7Fields(t *testing.T, logs, actorID string) {
	t.Helper()
	assert.NotContains(t, logs, actorID)
	for _, field := range []string{"user_id", "scope", "enforcing", "mfa_methods", "require_auth", "own_rule"} {
		assert.NotContains(t, logs, field+"=")
	}
}

// TestDMDeleteSoftLock_EpochMismatchIs401 proves both paths fence the
// session: the ordinary path through credepoch.GuardTx, the confirmation
// path through stepup.LockSubjectTx, which replaces it.
//
// Mutants killed: an ordinary-path fence that returns nil — 200; the
// confirmation path reading its subject through the unlocked, unfenced
// stepup.LoadSubject — 200.
func TestDMDeleteSoftLock_EpochMismatchIs401(t *testing.T) {
	for _, tc := range []struct {
		name string
		over bool
	}{
		{name: "ordinary path", over: false},
		{name: "confirmation path", over: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newSoftLockHarness(t)
			hs.enrollMFA(t)
			_, err := hs.db.Exec(`UPDATE users SET credential_epoch = 'epoch-rotated' WHERE id = $1`, hs.actor)
			require.NoError(t, err)
			hs.epoch = "epoch-stale"
			if tc.over {
				hs.overThreshold(t)
			}
			messageID := hs.message(t)

			w := hs.delete(t, messageID, codeBody(softLockGoodCode))

			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			assert.Equal(t, "Authentication required", decodeBody(t, w)["error"])
			assert.Empty(t, w.Header().Get("Retry-After"), "a 401 is not a soft-lock refusal")
			assert.True(t, hs.messageExists(t, messageID))
			assert.Empty(t, hs.verifier.purposes, "a fenced session never reaches the verifier")
		})
	}
}

// TestDMDeleteSoftLock_LockConflictIs503 proves a lock timeout on the users
// row — which arrives as the Cause of a 500 *stepup.Error — is answered as
// the busy 503 (§2.6 step 1), not as that 500.
//
// Mutant killed: classify *stepup.Error before IsLockConflict — the answer is
// 500 "Verification failed".
func TestDMDeleteSoftLock_LockConflictIs503(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.enrollMFA(t)
	hs.overThreshold(t)
	messageID := hs.message(t)

	dsn, err := url.Parse(testdb.DatabaseURL())
	require.NoError(t, err)
	q := dsn.Query()
	q.Set("lock_timeout", "100")
	dsn.RawQuery = q.Encode()
	timeoutDB, err := sql.Open("postgres", dsn.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = timeoutDB.Close() })
	hs.handler = hs.newHandler(timeoutDB, hs.rdb)

	holder, err := hs.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, hs.actor)
	require.NoError(t, err)

	w := hs.delete(t, messageID, codeBody(softLockGoodCode))
	require.NoError(t, holder.Rollback())

	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "1", w.Header().Get("Retry-After"))
	assert.Equal(t, true, decodeBody(t, w)["lock_conflict"])
	assert.True(t, hs.messageExists(t, messageID))
}

// TestDMDeleteSoftLock_FenceFollowsTheUsersFirstPrefix pins #3141's DM lock
// order on both paths: the session fence runs only after
// dmblock.PrepareConversationTx has taken every participant's users row, so
// while a peer's row is held the delete waits there and answers the busy 503.
// A stale session makes the fence observable: had it run first, the ordinary
// path would answer 401 and the confirmation path would reach the verifier.
//
// Mutant killed: move the fence ahead of PrepareConversationTx in
// deleteDMMessageTx — the ordinary path answers 401, the confirmation path
// calls the verifier.
func TestDMDeleteSoftLock_FenceFollowsTheUsersFirstPrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		over bool
	}{
		{name: "ordinary path", over: false},
		{name: "confirmation path", over: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newSoftLockHarness(t)
			hs.enrollMFA(t)
			_, err := hs.db.Exec(`UPDATE users SET credential_epoch = 'epoch-rotated' WHERE id = $1`, hs.actor)
			require.NoError(t, err)
			hs.epoch = "epoch-stale"
			if tc.over {
				hs.overThreshold(t)
			}
			messageID := hs.message(t)

			dsn, err := url.Parse(testdb.DatabaseURL())
			require.NoError(t, err)
			q := dsn.Query()
			q.Set("lock_timeout", "100")
			dsn.RawQuery = q.Encode()
			timeoutDB, err := sql.Open("postgres", dsn.String())
			require.NoError(t, err)
			t.Cleanup(func() { _ = timeoutDB.Close() })
			hs.handler = hs.newHandler(timeoutDB, hs.rdb)

			holder, err := hs.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = holder.Rollback() })
			_, err = holder.Exec(`SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, hs.peer)
			require.NoError(t, err)

			w := hs.delete(t, messageID, codeBody(softLockGoodCode))
			require.NoError(t, holder.Rollback())

			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			assert.Equal(t, true, decodeBody(t, w)["lock_conflict"])
			assert.Empty(t, hs.verifier.purposes, "the verifier runs only after the users-first prefix")
			assert.True(t, hs.messageExists(t, messageID))
		})
	}
}

// TestDMDeleteSoftLock_VerifierFaultIs500AndLogsNoC7Field proves a step-up
// 5xx is written as the opaque 500, undecorated, with its Cause logged under
// the C7 field rule.
//
// Mutant killed: decorate every *stepup.Error (not only 403s) — the 500
// carries delete_rate_limited; drop the fault log — the Cause is never logged.
func TestDMDeleteSoftLock_VerifierFaultIs500AndLogsNoC7Field(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.enrollMFA(t)
	hs.overThreshold(t)
	hs.verifier.err = errors.New("totp store unreachable")
	messageID := hs.message(t)

	w := hs.delete(t, messageID, codeBody(softLockGoodCode))

	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	body := decodeBody(t, w)
	assert.Equal(t, stepup.ErrMsgVerificationFailed, body["error"])
	assert.NotContains(t, body, "delete_rate_limited")
	assert.True(t, hs.messageExists(t, messageID))
	logs := hs.logs.String()
	assert.Contains(t, logs, "totp store unreachable")
	assertNoC7Fields(t, logs, hs.actor)
}

// TestDMDeleteSoftLock_ResetFailureDoesNotFailTheDelete proves the post-commit
// Reset and Clear are best-effort: Redis failing after the confirmation
// committed still answers 200, and each failure is one Warn line under C7.
//
// Mutant killed: answer 500 when Reset or Clear fails — the committed delete
// is reported as a failure.
func TestDMDeleteSoftLock_ResetFailureDoesNotFailTheDelete(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.enrollMFA(t)
	hs.overThreshold(t)
	hs.verifier.onVerify = func() { hs.mr.SetError("MISCONF simulated") }
	messageID := hs.message(t)

	w := hs.delete(t, messageID, codeBody(softLockGoodCode))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, hs.messageExists(t, messageID))
	logs := hs.logs.String()
	assert.Contains(t, logs, "failure_class=delete_softlock_reset_failed")
	assert.Contains(t, logs, "failure_class=delete_softlock_budget_clear_failed")
	assertNoC7Fields(t, logs, hs.actor)
}

func mustRedisGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	require.NoError(t, err, key)
	return v
}
