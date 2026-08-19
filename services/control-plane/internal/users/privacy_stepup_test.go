package users_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// keyRequireAuthBeforePurge is the toggle under test — the ONE privacy field
	// whose OFF transition demands a step-up (#2765).
	keyRequireAuthBeforePurge = "require_auth_before_purge"
	// keyMFACode is the step-up second factor's JSON key. Deliberately not named
	// for the credential it carries, matching keyCurrentPassword's precedent in
	// handlers_test.go.
	keyMFACode = "mfa_code"
	// keySearchableByEmail is an ordinary, ungated privacy column. It stands in
	// for "any of the other twelve fields" and materializes the settings row.
	keySearchableByEmail = "searchable_by_email"

	// noFactorsCopy is the actionable 400 body for an account with neither a
	// password nor MFA. Byte-identical to purgeFenceStepUpCopy.NoFactors in
	// handlers.go — the client discriminates on it, so a test that only checked
	// the status would not notice the copy drifting.
	noFactorsCopy = "Turning off purge verification requires proving your identity, but this account has no " +
		"password and no MFA method. Set a password or enable multi-factor authentication first."
)

// patchPrivacy issues the PATCH under test. Every case in this file goes through
// the full router — auth middleware included — because two of the behaviours
// being locked (the credential-epoch 401 and the in-transaction row lock) exist
// only on that path.
func patchPrivacy(
	t *testing.T, ts *testhelpers.TestServer, token string, body map[string]interface{},
) *httptest.ResponseRecorder {
	t.Helper()
	return ts.DoRequest(methodPatch, urlUsersMePrivacy, body, testhelpers.AuthHeaders(token))
}

// materializePrivacyRow forces the lazily-created privacy_settings row into
// existence via an ungated field, so a later assertion reads a stored value
// rather than the row-less fail-closed default.
func materializePrivacyRow(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) {
	t.Helper()
	require.Equal(t, http.StatusOK,
		patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{keySearchableByEmail: true}).Code)
	require.True(t, storedPurgeFence(t, ts, user.ID), "precondition: the fence starts up")
}

// makePasswordless puts the user in the SSO/passwordless state the step-up must
// handle: no password factor at all.
//
// It writes the EMPTY STRING, not NULL, and that is forced rather than chosen —
// users.password_hash is TEXT NOT NULL (migration 000001, never relaxed), so a
// genuine NULL cannot be stored. TestUsersPasswordHashCannotBeNull locks that
// premise: if a future migration drops the constraint, it fails and this helper
// must be re-pointed at a real NULL, because only then can the bare-Scan-into-
// string 500 this change exists to prevent actually occur.
func makePasswordless(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE users SET password_hash = '' WHERE id = $1`, userID)
	require.NoError(t, err)
}

// enableTOTP turns on a real, verifiable TOTP factor for the user and returns a
// generator for current codes. The secret is sealed with the all-zero key the
// test router is configured with (testhelpers.SetupTestServer's
// MFAEncryptionKey), so internal/mfa's real verifier — not a stub — validates
// the code inside the handler's transaction.
func enableTOTP(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) func() string {
	t.Helper()

	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Concord Voice", AccountName: user.Email})
	require.NoError(t, err)

	stubEncoding := make([]byte, 32) // matches the test config's all-zero keyring, version 1
	sealed, nonce, err := mfa.EncryptSecret([]byte(key.Secret()), stubEncoding)
	require.NoError(t, err)

	_, err = ts.DB.Exec(
		`UPDATE users SET mfa_enabled = TRUE, mfa_methods = ARRAY['totp']::TEXT[] WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
		 VALUES ($1, $2, $3, 1, TRUE, TRUE)`,
		user.ID, sealed, nonce)
	require.NoError(t, err)

	return func() string {
		code, genErr := totp.GenerateCode(key.Secret(), time.Now())
		require.NoError(t, genErr)
		return code
	}
}

// storedPurgeFence reads the durable column so a test asserts what the database
// holds, not what the handler echoed back.
func storedPurgeFence(t *testing.T, ts *testhelpers.TestServer, userID string) bool {
	t.Helper()
	var stored bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT require_auth_before_purge FROM privacy_settings WHERE user_id = $1`,
		userID).Scan(&stored))
	return stored
}

// TestUpdatePrivacy_DisableWithoutCredentialsIs403AndDoesNotWrite is the
// vulnerability proof for #2765: before the gate, this PATCH returned 200 and
// wrote false, so any request holding a live session could lower the fence and
// then purge unauthenticated (CWE-306).
func TestUpdatePrivacy_DisableWithoutCredentialsIs403AndDoesNotWrite(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencenocreds")

	// Materialize the row with an unrelated field so the assertion below reads a
	// stored value rather than the row-less fail-closed default.
	require.Equal(t, http.StatusOK, ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{"searchable_by_email": true},
		testhelpers.AuthHeaders(user.AccessToken)).Code)
	require.True(t, storedPurgeFence(t, ts, user.ID), "precondition: the fence starts up")

	w := ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{"require_auth_before_purge": false},
		testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, true, body["password_required"])

	require.True(t, storedPurgeFence(t, ts, user.ID),
		"the fence must be unchanged after a rejected request")
}

// TestPurgeFenceStepUp_RedisOutageDenies locks the fail-CLOSED half of the
// attempt budget. The route's own limiter is fail-open by design — it protects
// twelve innocuous fields — so without an in-handler counter a Redis outage
// would remove every bound from a path that verifies passwords.
func TestPurgeFenceStepUp_RedisOutageDenies(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefenceredisout")

	// Materialize the row so the assertion below reads a stored value rather
	// than the row-less fail-closed default.
	require.Equal(t, http.StatusOK, ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{"searchable_by_email": true},
		testhelpers.AuthHeaders(user.AccessToken)).Code)

	// A client pointed at a closed port: every INCR errors, so the budget must
	// deny rather than wave the request through.
	unreachable := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { require.NoError(t, unreachable.Close()) })
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	h.SetRedis(unreachable)

	// Correct credentials: only the unavailable budget may reject this.
	offRequestBody := `{"require_auth_before_purge": false, "current_password": "` +
		testhelpers.TestAuthPlaintext + `"}`

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", user.ID)
	c.Request = httptest.NewRequest(http.MethodPatch, urlUsersMePrivacy, strings.NewReader(offRequestBody))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdatePrivacySettings(c)

	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	require.True(t, storedPurgeFence(t, ts, user.ID),
		"a denied attempt must not lower the fence")
}

// TestUpdatePrivacy_DisableWithWrongPasswordIs403AndDoesNotWrite covers the
// ordinary rejected-credential case. The body string is asserted, not just the
// status: the desktop client discriminates the per-field error on it.
func TestUpdatePrivacy_DisableWithWrongPasswordIs403AndDoesNotWrite(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencewrongpass")
	materializePrivacyRow(t, ts, user)

	// Bound to a non-credential-shaped identifier: a quoted literal beside a
	// password-shaped name trips gosec G101 and the pre-commit secret scanner.
	unmatchedValue := "NotTheRightValue123!"

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        unmatchedValue,
	})

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, "Invalid password", body["error"])

	require.True(t, storedPurgeFence(t, ts, user.ID),
		"a wrong password must leave the fence up")
}

// TestUpdatePrivacy_DisableWithCorrectPasswordSucceeds is the happy path: the
// gate admits a verified actor and the column actually moves. Without this the
// suite would prove only that the handler rejects everything.
func TestUpdatePrivacy_DisableWithCorrectPasswordSucceeds(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencerightpass")
	materializePrivacyRow(t, ts, user)

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        testhelpers.TestAuthPlaintext,
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.False(t, storedPurgeFence(t, ts, user.ID),
		"a verified actor must be able to lower their own fence")
}

// TestUpdatePrivacy_EnableWithoutCredentialsSucceeds is the one-directional
// non-regression: tightening the control must never cost more than loosening
// it. The fence is lowered WITH credentials first so the ON request is a real
// transition rather than a no-op resend.
func TestUpdatePrivacy_EnableWithoutCredentialsSucceeds(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefenceraise")
	materializePrivacyRow(t, ts, user)

	require.Equal(t, http.StatusOK, patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        testhelpers.TestAuthPlaintext,
	}).Code)
	require.False(t, storedPurgeFence(t, ts, user.ID), "precondition: the fence is down")

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: true,
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, storedPurgeFence(t, ts, user.ID),
		"raising the fence must never require a step-up")
}

// TestUpdatePrivacy_UnrelatedFieldWithoutCredentialsSucceeds proves the gate is
// scoped to one field. The other twelve privacy columns are innocuous and must
// stay reachable without a password — a gate that spread to them would make
// routine settings edits fail for every SSO account.
func TestUpdatePrivacy_UnrelatedFieldWithoutCredentialsSucceeds(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefenceotherfield")

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keySearchableByEmail: true,
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	privacy, ok := body["privacy"].(map[string]interface{})
	require.True(t, ok, "expected a privacy object in the response")
	require.Equal(t, true, privacy[keySearchableByEmail])
	require.True(t, storedPurgeFence(t, ts, user.ID),
		"an unrelated field must not disturb the fence")
}

// TestUpdatePrivacy_MFAEnabledWithoutCodeIs403WithMethods covers the second
// factor's missing-input case. A correct password is supplied deliberately: the
// 403 must come from the ABSENT code, proving the MFA factor is required in
// addition to the password rather than as an alternative to it.
//
// The body must carry mfa_required plus a non-nil methods array — that is what
// tells the client which second-factor field to render, so a 403 with an empty
// body would leave an MFA user with no way to proceed.
func TestUpdatePrivacy_MFAEnabledWithoutCodeIs403WithMethods(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencemfanocode")
	materializePrivacyRow(t, ts, user)
	enableTOTP(t, ts, user)

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        testhelpers.TestAuthPlaintext,
	})

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, true, body["mfa_required"])
	methods, ok := body["methods"].([]interface{})
	require.True(t, ok, "methods must be a non-nil array so the client can render a factor")
	require.Contains(t, methods, "totp")

	require.True(t, storedPurgeFence(t, ts, user.ID),
		"a missing second factor must leave the fence up")
}

// TestUpdatePrivacy_SupersededCredentialEpochIs401 proves the session fence runs
// alongside the step-up and preempts it.
//
// SimulateStaleEpochWindow reproduces the #2201 race exactly: the DB holds the
// post-rotation epoch while the cache still serves the pre-rotation one, so the
// HTTP middleware ADMITS the request and only the in-transaction read can stop
// it. The password supplied is CORRECT — knowing a password is a different fact
// from holding a live session, and a revoked session must not lower the fence
// however good its credentials are.
func TestUpdatePrivacy_SupersededCredentialEpochIs401(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencestaleepoch")
	materializePrivacyRow(t, ts, user)

	staleToken := ts.SimulateStaleEpochWindow(t, user.ID)

	w := patchPrivacy(t, ts, staleToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        testhelpers.TestAuthPlaintext,
	})

	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.True(t, storedPurgeFence(t, ts, user.ID),
		"a superseded session must not lower the fence even with a correct password")
}

// TestUpdatePrivacy_RejectedDisableLeavesNoPrivacyRow locks the ordering claim
// that verification precedes the row-ensuring INSERT.
//
// privacy_settings rows are created lazily and internal/dm fail-closes to TRUE
// on a missing row. So a rejected request that still inserted a row would flip
// the durable answer from "absent, therefore fenced" to "present, defaulted" —
// materializing a weaker state out of a 403. Total rollback is what makes the
// fail-closed default hold, and only a fresh user with no row can observe it.
func TestUpdatePrivacy_RejectedDisableLeavesNoPrivacyRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencenorow")

	var before int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM privacy_settings WHERE user_id = $1`, user.ID).Scan(&before))
	require.Zero(t, before, "precondition: the settings row is created lazily")

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
	})

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	var after int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM privacy_settings WHERE user_id = $1`, user.ID).Scan(&after))
	require.Zero(t, after,
		"a rejected request must leave no durable trace; a row here would replace the "+
			"fail-closed no-row default with a defaulted one")
}

// TestPurgeFenceStepUp_WaitsOnResetLock is the only test that proves the design's
// central claim: the step-up runs INSIDE the write transaction, so a request
// authorized by credentials that were superseded while it waited cannot commit.
//
// Connection A stands in for a destructive reset — it takes the users row
// FOR NO KEY UPDATE, which is exactly the lock ChangePassword, ReplaceMyKeys and
// both recovery flows hold. Connection B fires the OFF request with a password
// that is correct AT THAT MOMENT. B must not complete: its FOR SHARE conflicts
// with A's lock, so it blocks rather than reading the pre-reset hash. When A
// commits a new password, B wakes, re-reads under its own lock, and rejects.
//
// Verifying before BeginTx would pass every other test in this file and fail
// this one: the read would land before A's lock existed, the stale hash would
// match, and the fence would come down on a credential the account no longer
// has. That is the window this ordering closes.
func TestPurgeFenceStepUp_WaitsOnResetLock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefenceresetlock")
	materializePrivacyRow(t, ts, user)
	ctx := context.Background()

	// Hashed before the lock is taken: Argon2id costs ~100ms and nothing is
	// gained by spending it while a row every reset flow needs is held.
	supersedingValue := "RecoveredByOwner456!"
	supersedingHash, err := auth.HashPassword(supersedingValue)
	require.NoError(t, err)

	resetTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resetTx.Rollback() }) // no-op once committed
	_, err = resetTx.ExecContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, user.ID)
	require.NoError(t, err)

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest(methodPatch, urlUsersMePrivacy, map[string]interface{}{
			keyRequireAuthBeforePurge: false,
			keyCurrentPassword:        testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(user.AccessToken))
	}()

	select {
	case early := <-response:
		t.Fatalf("the OFF request completed while the reset held the users row lock "+
			"(status %d, body %s): the step-up read outside the transaction",
			early.Code, early.Body.String())
	case <-time.After(500 * time.Millisecond):
	}

	_, err = resetTx.ExecContext(ctx,
		`UPDATE users SET password_hash = $1 WHERE id = $2`, supersedingHash, user.ID)
	require.NoError(t, err)
	require.NoError(t, resetTx.Commit())

	select {
	case w := <-response:
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		require.Equal(t, "Invalid password", body["error"],
			"the waiting request must be judged against the committed password, not the one it read")
	case <-time.After(10 * time.Second):
		t.Fatal("the OFF request did not complete after the reset committed")
	}

	require.True(t, storedPurgeFence(t, ts, user.ID),
		"a request authorized by a superseded password must not lower the fence")
}

// TestUsersPasswordHashCannotBeNull locks the schema premise the two
// passwordless cases below rest on.
//
// The #2765 design describes password_hash as nullable and COALESCEs it, and
// the acceptance criterion is phrased as "password_hash IS NULL". Against the
// live schema that state is UNREACHABLE: migration 000001 declares the column
// TEXT NOT NULL and nothing since relaxes it, so the reachable passwordless
// representation is the empty string — which is exactly what the COALESCE
// normalizes a hypothetical NULL to, and what makePasswordless writes.
//
// This test exists so that premise is checked rather than assumed. If a
// migration ever drops the constraint, this fails and whoever drops it must
// re-point makePasswordless at a genuine NULL — because only from that moment
// can a bare Scan into a Go string produce the 500 the change exists to
// prevent, and only then is the NULL path worth a test.
func TestUsersPasswordHashCannotBeNull(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencenullprobe")

	_, err := ts.DB.Exec(`UPDATE users SET password_hash = NULL WHERE id = $1`, user.ID)

	require.Error(t, err,
		"users.password_hash accepted NULL: the schema changed, so the passwordless "+
			"step-up cases must be re-pointed at a real NULL (see makePasswordless)")
	require.Contains(t, strings.ToLower(err.Error()), "null",
		"expected a not-null violation, got a different failure")
}

// TestUpdatePrivacy_PasswordlessWithMFACodeAloneSucceeds is the acceptance
// criterion for the SSO/passwordless account (#2765).
//
// An account with no password factor can still prove identity with MFA, so the
// step-up must accept the code ALONE — no password field, no 400, and above all
// no 500. It runs against a real Postgres and a real internal/mfa verifier
// because that is the only configuration in which the defect class this closes
// (reading an absent password factor out of the users row and then demanding a
// password anyway) can actually appear.
func TestUpdatePrivacy_PasswordlessWithMFACodeAloneSucceeds(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencessomfa")
	materializePrivacyRow(t, ts, user)

	code := enableTOTP(t, ts, user)
	makePasswordless(t, ts, user.ID)

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyMFACode:                code(),
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.False(t, storedPurgeFence(t, ts, user.ID),
		"an MFA-verified passwordless account must be able to lower its own fence")
}

// TestUpdatePrivacy_PasswordlessWithoutMFAIsActionable400 covers the account
// with NEITHER factor.
//
// The status is the point. A 500 tells the user nothing and reads as a server
// fault; a 400 carrying the NoFactors copy tells them precisely what to do
// (set a password or enable MFA). The explicit NotEqual on 500 is deliberate
// redundancy — it names the regression this guards against.
func TestUpdatePrivacy_PasswordlessWithoutMFAIsActionable400(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencenofactors")
	materializePrivacyRow(t, ts, user)
	makePasswordless(t, ts, user.ID)

	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
	})

	require.NotEqual(t, http.StatusInternalServerError, w.Code,
		"an account with no available factor must get an actionable answer, not a server error")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, noFactorsCopy, body["error"])

	require.True(t, storedPurgeFence(t, ts, user.ID),
		"the fence must be unchanged when no factor could be verified")
}

// TestUpdatePrivacy_SuccessfulDisableClearsTheAttemptBudget locks the fix for a
// Gitar review finding on #2792.
//
// middleware.AllowUserAction increments before the outcome is knowable and never
// decrements, so without a reset a legitimate actor who toggles the fence off and
// on — or retries past a transient failure — burns the budget meant to bound
// password GUESSING and is eventually refused while holding correct credentials.
// Counting every attempt and clearing on a committed success is the standard
// failed-attempts shape: a wrong password still accumulates, a right one costs
// nothing.
func TestUpdatePrivacy_SuccessfulDisableClearsTheAttemptBudget(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgefencebudgetreset")
	materializePrivacyRow(t, ts, user)

	budgetKey := "stepup:privacy_purge_fence:" + user.ID
	unmatchedValue := "NotTheRightValue123!"

	// Spend part of the budget on genuinely wrong credentials.
	for i := 0; i < 2; i++ {
		w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
			keyRequireAuthBeforePurge: false,
			keyCurrentPassword:        unmatchedValue,
		})
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	}
	spent, err := ts.Redis.Get(context.Background(), budgetKey).Result()
	require.NoError(t, err, "failed attempts must accumulate in the budget")
	require.NotEqual(t, "0", spent)

	// A correct disable commits, so the budget is returned.
	w := patchPrivacy(t, ts, user.AccessToken, map[string]interface{}{
		keyRequireAuthBeforePurge: false,
		keyCurrentPassword:        testhelpers.TestAuthPlaintext,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.False(t, storedPurgeFence(t, ts, user.ID), "the fence should now be down")

	_, err = ts.Redis.Get(context.Background(), budgetKey).Result()
	require.ErrorIs(t, err, redis.Nil,
		"a committed disable must clear the attempt budget; otherwise correct "+
			"credentials eventually lock the actor out of their own setting")
}
