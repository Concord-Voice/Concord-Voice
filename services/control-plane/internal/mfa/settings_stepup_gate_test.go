package mfa_test

// This file pins the HTTP contract for the MFA-settings step-up gate BEFORE
// the gate exists ([internal]plans/2026-09-23-mfa-step-up-and-suppress-embeds.md
// Task 2 Steps 1, 2 (HTTP part) and 4 (empty-body part); contract at
// [internal]specs/2026-09-23-mfa-step-up-and-suppress-embeds-design.md
// §4.3). Every case here is expected to FAIL against the pre-fix handlers:
// none of the four routes (EmailSmsDisable, SetBackupEmail, StoreRecoveryKey
// overwrite, DeleteRecoveryKey) currently require a password for every write,
// return "password_required"/"mfa_required" bodies, or fence on
// credential_epoch. A case that passes today is reported as a finding rather
// than adjusted to fit.
//
// Event assertions (ReasonInvalidCredentials / ReasonChallengeInvalid /
// ReasonCredentialEpochMismatch) are SKIPPED in this file: TestServer wires a
// real event pipeline and exposes no recorder an external (`mfa_test`)
// package can observe. Handler-level event coverage lives in
// security_event_test.go (package mfa), covered by another batch.
//
// Scope note: this file covers Task 2 Steps 1, 2 (HTTP part) and 4
// (empty-body part) only. Step 3 (B1 concurrency/budget) and Step 5
// (concurrency/pool-safety) and the internal-package tests
// (settings_stepup_internal_test.go) are other batches.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var stepUpUserCounter int64

// nextStepUpUsername returns a short, unique username so each subtest gets
// its own user row without colliding with parallel package state.
func nextStepUpUsername() string {
	n := atomic.AddInt64(&stepUpUserCounter, 1)
	return fmt.Sprintf("stepup%d", n)
}

// seedRecoveryKey inserts a recovery key directly, bypassing the handler, so
// tests can assert on step-up behaviour against a pre-existing key (B1's
// "existing key" fixture) without relying on the route under test.
func seedRecoveryKey(t *testing.T, ts *testhelpers.TestServer, userID string, key, salt []byte) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt)
		VALUES ($1, $2, $3)
	`, userID, key, salt)
	require.NoError(t, err)
}

var (
	stepUpSeedRecoveryKeyBytes = []byte("K1-seed-recovery-key-bytes-32by")
	stepUpSeedRecoverySalt     = []byte("K1-seed-salt-16b")
)

// readEmailSmsEnabledState returns the current value of the email MFA Redis
// flag, or "<absent>" if the key does not exist.
func readEmailSmsEnabledState(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	v, err := ts.Redis.Get(context.Background(), fmt.Sprintf("mfa_emailsms_enabled:%s:email", userID)).Result()
	if errors.Is(err, redis.Nil) {
		return "<absent>"
	}
	require.NoError(t, err)
	return v
}

// readBackupEmailState returns the current users.backup_email value, or
// "<null>" if unset.
func readBackupEmailState(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	var v sql.NullString
	err := ts.DB.QueryRow(`SELECT backup_email FROM users WHERE id = $1`, userID).Scan(&v)
	require.NoError(t, err)
	if !v.Valid {
		return "<null>"
	}
	return v.String
}

// readRecoveryKeyState returns the base64 of the stored wrapped private key,
// or "<absent>" if no row exists.
func readRecoveryKeyState(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	var b []byte
	err := ts.DB.QueryRow(`SELECT recovery_wrapped_private_key FROM user_recovery_keys WHERE user_id = $1`, userID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return "<absent>"
	}
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

// stepUpRoute describes one of the four gated routes for the per-route matrix.
type stepUpRoute struct {
	name      string
	method    string
	url       string
	extra     func() map[string]interface{}
	baseline  func(t *testing.T, ts *testhelpers.TestServer, userID string)
	readState func(t *testing.T, ts *testhelpers.TestServer, userID string) string
}

var stepUpRoutes = []stepUpRoute{
	{
		name:   "EmailSmsDisable",
		method: "POST",
		url:    urlEmailSmsDisable,
		extra:  func() map[string]interface{} { return map[string]interface{}{} },
		baseline: func(t *testing.T, ts *testhelpers.TestServer, userID string) {
			t.Helper()
			require.NoError(t, ts.Redis.Set(context.Background(), fmt.Sprintf("mfa_emailsms_enabled:%s:email", userID), "1", 0).Err())
		},
		readState: readEmailSmsEnabledState,
	},
	{
		name:   "SetBackupEmail",
		method: "PUT",
		url:    urlBackupEmail,
		extra: func() map[string]interface{} {
			return map[string]interface{}{"email": "new-backup@example.com"}
		},
		baseline: func(t *testing.T, ts *testhelpers.TestServer, userID string) {
			t.Helper()
			_, err := ts.DB.Exec(`UPDATE users SET backup_email = $1 WHERE id = $2`, "old-backup@example.com", userID)
			require.NoError(t, err)
		},
		readState: readBackupEmailState,
	},
	{
		name:   "StoreRecoveryKeyOverwrite",
		method: "PUT",
		url:    urlRecoveryKey,
		extra: func() map[string]interface{} {
			return map[string]interface{}{
				"recovery_wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("NEW-recovery-key-32-bytes-value")),
				"recovery_key_salt":            base64.StdEncoding.EncodeToString([]byte("NEW-salt-16bytes")),
			}
		},
		baseline: func(t *testing.T, ts *testhelpers.TestServer, userID string) {
			t.Helper()
			seedRecoveryKey(t, ts, userID, stepUpSeedRecoveryKeyBytes, stepUpSeedRecoverySalt)
		},
		readState: readRecoveryKeyState,
	},
	{
		name:   "DeleteRecoveryKey",
		method: "DELETE",
		url:    urlRecoveryKey,
		extra:  func() map[string]interface{} { return map[string]interface{}{} },
		baseline: func(t *testing.T, ts *testhelpers.TestServer, userID string) {
			t.Helper()
			seedRecoveryKey(t, ts, userID, stepUpSeedRecoveryKeyBytes, stepUpSeedRecoverySalt)
		},
		readState: readRecoveryKeyState,
	},
}

// stepUpCase describes one row of the per-route credential matrix.
type stepUpCase struct {
	name        string
	prep        func(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) (password, mfaCode string)
	rotateEpoch bool
	wantStatus  int
	wantBody    func(t *testing.T, body map[string]interface{})
	wantChanged bool
}

func wantPasswordRequiredBody(t *testing.T, body map[string]interface{}) {
	t.Helper()
	assert.True(t, testhelpers.JSONField[bool](t, body, "password_required"), "password_required")
}

func wantMFARequiredTOTPOnlyBody(t *testing.T, body map[string]interface{}) {
	t.Helper()
	assert.True(t, testhelpers.JSONField[bool](t, body, "mfa_required"), "mfa_required")
	methods := testhelpers.JSONField[[]interface{}](t, body, "methods")
	require.Len(t, methods, 1, "methods")
	assert.Equal(t, "totp", testhelpers.JSONElem[string](t, methods, 0), "methods[0]")
}

func wantExactErrorBody(msg string) func(t *testing.T, body map[string]interface{}) {
	return func(t *testing.T, body map[string]interface{}) {
		t.Helper()
		assert.Equal(t, map[string]interface{}{"error": msg}, body)
	}
}

func stepUpNoCredentials(_ *testing.T, _ *testhelpers.TestServer, _ testhelpers.TestUser) (string, string) {
	return "", ""
}

func stepUpPasswordOnly(_ *testing.T, _ *testhelpers.TestServer, _ testhelpers.TestUser) (string, string) {
	return testPassword, ""
}

func stepUpWrongPassword(_ *testing.T, _ *testhelpers.TestServer, _ testhelpers.TestUser) (string, string) {
	return testBadPassword, ""
}

func stepUpTOTPPasswordOnly(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) (string, string) {
	enrollTOTP(t, ts, user)
	return testPassword, ""
}

func stepUpTOTPValidCode(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) (string, string) {
	secret, _ := enrollTOTP(t, ts, user)
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return testPassword, code
}

func stepUpTOTPWrongCode(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) (string, string) {
	enrollTOTP(t, ts, user)
	return testPassword, "000000"
}

var stepUpCases = []stepUpCase{
	{
		name:        "no credentials",
		prep:        stepUpNoCredentials,
		wantStatus:  http.StatusForbidden,
		wantBody:    wantPasswordRequiredBody,
		wantChanged: false,
	},
	{
		name:        "password only, no inline factor",
		prep:        stepUpPasswordOnly,
		wantStatus:  http.StatusOK,
		wantChanged: true,
	},
	{
		name:        "TOTP enrolled, password only",
		prep:        stepUpTOTPPasswordOnly,
		wantStatus:  http.StatusForbidden,
		wantBody:    wantMFARequiredTOTPOnlyBody,
		wantChanged: false,
	},
	{
		name:        "TOTP enrolled, valid code",
		prep:        stepUpTOTPValidCode,
		wantStatus:  http.StatusOK,
		wantChanged: true,
	},
	{
		name:        "wrong password",
		prep:        stepUpWrongPassword,
		wantStatus:  http.StatusForbidden,
		wantBody:    wantExactErrorBody("Invalid password"),
		wantChanged: false,
	},
	{
		name:        "TOTP enrolled, wrong code",
		prep:        stepUpTOTPWrongCode,
		wantStatus:  http.StatusForbidden,
		wantBody:    wantExactErrorBody("Invalid MFA code"),
		wantChanged: false,
	},
	{
		name:        "rotated credential epoch",
		prep:        stepUpPasswordOnly,
		rotateEpoch: true,
		wantStatus:  http.StatusUnauthorized,
		wantBody:    wantExactErrorBody("Authentication required"),
		wantChanged: false,
	},
}

// TestStepUpSettings_RouteMatrix is the §6 per-route matrix (Task 2 Step 1):
// every one of the four gated routes, crossed with every credential
// presentation, asserting status, body and whether the protected state moved.
func TestStepUpSettings_RouteMatrix(t *testing.T) {
	for _, rt := range stepUpRoutes {
		rt := rt
		t.Run(rt.name, func(t *testing.T) {
			for _, cs := range stepUpCases {
				cs := cs
				t.Run(cs.name, func(t *testing.T) {
					ts := setupTS(t)
					user := ts.CreateTestUser(t, nextStepUpUsername())
					rt.baseline(t, ts, user.ID)

					password, mfaCode := cs.prep(t, ts, user)
					if cs.rotateEpoch {
						_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = gen_random_uuid()::text WHERE id = $1`, user.ID)
						require.NoError(t, err)
					}

					before := rt.readState(t, ts, user.ID)

					body := rt.extra()
					body["password"] = password // pragma: allowlist secret -- test credential variable
					body["mfa_code"] = mfaCode
					w := ts.DoRequest(rt.method, rt.url, body, testhelpers.AuthHeaders(user.AccessToken))

					assert.Equal(t, cs.wantStatus, w.Code, "%s / %s: status", rt.name, cs.name)

					var respBody map[string]interface{}
					testhelpers.ParseJSON(t, w, &respBody)
					if cs.wantBody != nil {
						cs.wantBody(t, respBody)
					}

					after := rt.readState(t, ts, user.ID)
					if cs.wantChanged {
						assert.NotEqual(t, before, after, "%s / %s: expected protected state to change", rt.name, cs.name)
					} else {
						assert.Equal(t, before, after, "%s / %s: expected protected state to stay unchanged", rt.name, cs.name)
					}
				})
			}
		})
	}
}

// TestStepUpSettings_P1EmailOnlyAccountPasswordAlone is P1 (Task 2 Step 2):
// an email/SMS-only account has no inline-verifiable factor, so password
// alone must be sufficient on the two routes email/SMS-only accounts reach.
func TestStepUpSettings_P1EmailOnlyAccountPasswordAlone(t *testing.T) {
	ts := setupTS(t)

	cases := []struct {
		name   string
		method string
		url    string
		body   func() map[string]interface{}
	}{
		{"EmailSmsDisable", "POST", urlEmailSmsDisable, func() map[string]interface{} { return map[string]interface{}{} }},
		{"SetBackupEmail", "PUT", urlBackupEmail, func() map[string]interface{} {
			return map[string]interface{}{"email": "new-backup@example.com"}
		}},
	}

	for _, rt := range cases {
		rt := rt
		t.Run(rt.name, func(t *testing.T) {
			user := ts.CreateTestUser(t, nextStepUpUsername())
			require.NoError(t, ts.Redis.Set(context.Background(), fmt.Sprintf("mfa_emailsms_enabled:%s:email", user.ID), "1", 0).Err())
			_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{email}' WHERE id = $1`, user.ID)
			require.NoError(t, err)

			body := rt.body()
			body["password"] = testPassword // pragma: allowlist secret -- test credential constant
			w := ts.DoRequest(rt.method, rt.url, body, testhelpers.AuthHeaders(user.AccessToken))

			assert.Equal(t, http.StatusOK, w.Code, "email-only account must pass step-up with password alone")

			if w.Code == http.StatusForbidden {
				var body map[string]interface{}
				testhelpers.ParseJSON(t, w, &body)
				if methodsRaw, ok := body["methods"].([]interface{}); ok {
					for _, m := range methodsRaw {
						assert.NotEqual(t, "email", m, "methods must never offer email")
						assert.NotEqual(t, "sms", m, "methods must never offer sms")
					}
				}
			}
		})
	}
}

// TestStepUpSettings_P1StaleFlagsStillRequireCode is P1's second half (Task 2
// Step 2): inlineMFAMethods must read the live TOTP/WebAuthn tables, not the
// denormalized (and here deliberately stale) users.mfa_enabled/mfa_methods
// columns.
func TestStepUpSettings_P1StaleFlagsStillRequireCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, nextStepUpUsername())

	enrollTOTP(t, ts, user)
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = FALSE, mfa_methods = '{}' WHERE id = $1`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", urlEmailSmsDisable, map[string]interface{}{
		"password": testPassword,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code, "a confirmed TOTP row must still demand a code despite stale flags")
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.True(t, testhelpers.JSONField[bool](t, body, "mfa_required"), "mfa_required")
}

// rawStepUpRequest builds a request directly against ts.Router so a body can
// be omitted entirely, set to the literal string "null", or malformed —
// none of which ts.DoRequest (which always marshals valid JSON via
// json.Marshal) can produce. A nil rawBody sends no body at all, matching
// http.NoBody semantics for an empty request.
func rawStepUpRequest(t *testing.T, ts *testhelpers.TestServer, method, url string, rawBody *string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if rawBody != nil {
		body = strings.NewReader(*rawBody)
	}
	req := httptest.NewRequest(method, url, body)
	if headers != nil {
		req.Header = headers
	}
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

// TestStepUpSettings_EmptyBody is the empty-body half of Task 2 Step 4.
// EmailSmsDisable and DELETE recovery-key take credentials only, so an
// absent or null body is a legitimate (if credential-less) request and gets
// the actionable 403; malformed JSON is still a 400. SetBackupEmail and PUT
// recovery-key require shape beyond credentials, so an absent body is always
// 400.
func TestStepUpSettings_EmptyBody(t *testing.T) {
	ts := setupTS(t)
	nullBody := "null"
	malformedBody := "{"

	t.Run("EmailSmsDisable no body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		w := rawStepUpRequest(t, ts, "POST", urlEmailSmsDisable, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.True(t, testhelpers.JSONField[bool](t, body, "password_required"))
	})

	t.Run("EmailSmsDisable null body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		w := rawStepUpRequest(t, ts, "POST", urlEmailSmsDisable, &nullBody, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.True(t, testhelpers.JSONField[bool](t, body, "password_required"))
	})

	t.Run("EmailSmsDisable malformed body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		w := rawStepUpRequest(t, ts, "POST", urlEmailSmsDisable, &malformedBody, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("DeleteRecoveryKey no body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		seedRecoveryKey(t, ts, user.ID, stepUpSeedRecoveryKeyBytes, stepUpSeedRecoverySalt)
		w := rawStepUpRequest(t, ts, "DELETE", urlRecoveryKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.True(t, testhelpers.JSONField[bool](t, body, "password_required"))
	})

	t.Run("DeleteRecoveryKey null body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		seedRecoveryKey(t, ts, user.ID, stepUpSeedRecoveryKeyBytes, stepUpSeedRecoverySalt)
		w := rawStepUpRequest(t, ts, "DELETE", urlRecoveryKey, &nullBody, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("DeleteRecoveryKey malformed body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		seedRecoveryKey(t, ts, user.ID, stepUpSeedRecoveryKeyBytes, stepUpSeedRecoverySalt)
		w := rawStepUpRequest(t, ts, "DELETE", urlRecoveryKey, &malformedBody, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("SetBackupEmail no body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		w := rawStepUpRequest(t, ts, "PUT", urlBackupEmail, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("StoreRecoveryKey no body", func(t *testing.T) {
		user := ts.CreateTestUser(t, nextStepUpUsername())
		w := rawStepUpRequest(t, ts, "PUT", urlRecoveryKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// stepUpBudgetKey mirrors the gate's own key spelling (spec §4.2,
// "stepup:mfa_settings:<uid>") so pre-seeding and reading the counter cannot
// silently drift from the key the gate itself will use.
func stepUpBudgetKey(userID string) string { return "stepup:mfa_settings:" + userID }

// clearRouteRateLimit deletes the PRE-EXISTING, per-route RateLimitByUser
// counter (internal/middleware/ratelimit.go, key
// "ratelimit:user:<uid>:<method>:<path>") that sits in front of every one of
// these four routes independent of the step-up gate — e.g. EmailSmsDisable
// is capped at 3/min, StoreRecoveryKey and DeleteRecoveryKey at 3/min,
// SetBackupEmail at 5/min. A test that fires more than that many requests at
// one route in a tight sequence trips THAT limiter first, which is a
// different 429 (body {"error":"Rate limit exceeded", ...}) from the one the
// new gate's own budget produces. Isolating a budget test to the mechanism
// it names means clearing this key between iterations.
func clearRouteRateLimit(t *testing.T, ts *testhelpers.TestServer, userID, method, path string) {
	t.Helper()
	key := fmt.Sprintf("ratelimit:user:%s:%s:%s", userID, method, path)
	require.NoError(t, ts.Redis.Del(context.Background(), key).Err())
}

// readStepUpBudgetCounter returns the current budget counter as an int, or 0
// if the key is absent (AllowUserAction's own starting state).
func readStepUpBudgetCounter(t *testing.T, ts *testhelpers.TestServer, userID string) int {
	t.Helper()
	v, err := ts.Redis.Get(context.Background(), stepUpBudgetKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	n, err := strconv.Atoi(v)
	require.NoError(t, err)
	return n
}

// runConcurrentWithWatchdog fires n goroutines against fn, released together
// through a closed start channel (the "sync.WaitGroup barrier" the plan
// specifies), and fails the test if they have not all completed within
// timeout — a self-deadlocking gate must be caught in seconds, not by
// `go test`'s 10-minute panic.
func runConcurrentWithWatchdog(t *testing.T, timeout time.Duration, n int, fn func(i int) *httptest.ResponseRecorder) []*httptest.ResponseRecorder {
	t.Helper()
	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			results[i] = fn(i)
		}()
	}
	done := make(chan struct{})
	go func() {
		close(start)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("watchdog: concurrent requests did not complete within timeout")
	}
	return results
}

// TestStepUpSettings_B1RecoveryKey is Task 2 Step 3 (B1): the recovery-key
// PUT route's first-store-vs-overwrite split. A first-time store needs no
// credentials (TOTP enrollment's automatic upload); an overwrite is a
// superset of DeleteRecoveryKey's capability and must demand the same
// step-up.
func TestStepUpSettings_B1RecoveryKey(t *testing.T) {
	newKeyBody := func() map[string]interface{} {
		return map[string]interface{}{
			"recovery_wrapped_private_key": base64.StdEncoding.EncodeToString([]byte("B1-overwrite-key-32-bytes-value")),
			"recovery_key_salt":            base64.StdEncoding.EncodeToString([]byte("B1-overwrite-salt-16b")),
		}
	}

	t.Run("first store without credentials succeeds", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())

		before := readRecoveryKeyState(t, ts, user.ID)
		require.Equal(t, "<absent>", before)

		w := ts.DoRequest("PUT", urlRecoveryKey, newKeyBody(), testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		after := readRecoveryKeyState(t, ts, user.ID)
		assert.NotEqual(t, before, after, "first-time store must persist a key")
	})

	t.Run("existing key without credentials is refused, key unchanged", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())
		seedRecoveryKey(t, ts, user.ID, stepUpSeedRecoveryKeyBytes, stepUpSeedRecoverySalt)
		before := readRecoveryKeyState(t, ts, user.ID)

		w := ts.DoRequest("PUT", urlRecoveryKey, newKeyBody(), testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.True(t, testhelpers.JSONField[bool](t, body, "password_required"))

		after := readRecoveryKeyState(t, ts, user.ID)
		assert.Equal(t, before, after, "K1 must be unchanged")
	})

	t.Run("existing key with credentials is overwritten", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())
		seedRecoveryKey(t, ts, user.ID, stepUpSeedRecoveryKeyBytes, stepUpSeedRecoverySalt)
		before := readRecoveryKeyState(t, ts, user.ID)

		body := newKeyBody()
		body["password"] = testPassword // pragma: allowlist secret -- test credential constant
		w := ts.DoRequest("PUT", urlRecoveryKey, body, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		after := readRecoveryKeyState(t, ts, user.ID)
		assert.NotEqual(t, before, after, "credentialed overwrite must replace K1")
	})

	t.Run("concurrent first-time PUTs yield exactly one winner", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())

		keyA := base64.StdEncoding.EncodeToString([]byte("concurrent-key-A-32-bytes-long!"))
		saltA := base64.StdEncoding.EncodeToString([]byte("concurrent-salt-A-16b"))
		keyB := base64.StdEncoding.EncodeToString([]byte("concurrent-key-B-32-bytes-long!"))
		saltB := base64.StdEncoding.EncodeToString([]byte("concurrent-salt-B-16b"))
		bodies := []map[string]interface{}{
			{"recovery_wrapped_private_key": keyA, "recovery_key_salt": saltA},
			{"recovery_wrapped_private_key": keyB, "recovery_key_salt": saltB},
		}

		results := runConcurrentWithWatchdog(t, 10*time.Second, 2, func(i int) *httptest.ResponseRecorder {
			return ts.DoRequest("PUT", urlRecoveryKey, bodies[i], testhelpers.AuthHeaders(user.AccessToken))
		})

		statuses := []int{results[0].Code, results[1].Code}
		assert.ElementsMatch(t, []int{http.StatusOK, http.StatusForbidden}, statuses, "exactly one 200 and one 403")

		stored := readRecoveryKeyState(t, ts, user.ID)
		winnerKey := keyA
		if results[1].Code == http.StatusOK {
			winnerKey = keyB
		}
		assert.Equal(t, winnerKey, stored, "the stored key must be the winner's")
	})

	t.Run("first-time insert does not clear the budget", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())
		require.NoError(t, ts.Redis.Set(context.Background(), stepUpBudgetKey(user.ID), "4", 0).Err())

		body := newKeyBody()
		body["password"] = testPassword // pragma: allowlist secret -- test credential constant
		w := ts.DoRequest("PUT", urlRecoveryKey, body, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		assert.GreaterOrEqual(t, readStepUpBudgetCounter(t, ts, user.ID), 4, "a first-time insert must not clear the budget")
	})
}

// TestStepUpSettings_Budget is Task 2 Step 4 (budget half; the empty-body
// half lives in TestStepUpSettings_EmptyBody). Security-event assertions are
// deliberately absent here — they move to the internal (package mfa) test
// file, which can observe emitted events.
func TestStepUpSettings_Budget(t *testing.T) {
	t.Run("6th credentialed attempt returns 429", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())

		var last *httptest.ResponseRecorder
		for i := 0; i < 6; i++ {
			// Isolate this loop to the step-up budget alone: EmailSmsDisable's
			// own pre-existing route limiter is 3/min, well below 6.
			clearRouteRateLimit(t, ts, user.ID, "POST", urlEmailSmsDisable)
			last = ts.DoRequest("POST", urlEmailSmsDisable, map[string]interface{}{
				"password": testBadPassword, // pragma: allowlist secret -- test credential constant
			}, testhelpers.AuthHeaders(user.AccessToken))
		}

		assert.Equal(t, http.StatusTooManyRequests, last.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, last, &body)
		assert.Equal(t, map[string]interface{}{"error": "Too many verification attempts"}, body)
	})

	t.Run("credential-less requests do not consume budget", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())

		for i := 0; i < 10; i++ {
			// Same isolation as above: only the step-up budget is under test.
			clearRouteRateLimit(t, ts, user.ID, "POST", urlEmailSmsDisable)
			ts.DoRequest("POST", urlEmailSmsDisable, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
		}

		clearRouteRateLimit(t, ts, user.ID, "POST", urlEmailSmsDisable)
		w := ts.DoRequest("POST", urlEmailSmsDisable, map[string]interface{}{
			"password": testPassword, // pragma: allowlist secret -- test credential constant
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code, "ten credential-less requests must not exhaust the budget a correct one then needs")
	})

	t.Run("verified commit clears the budget", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())
		require.NoError(t, ts.Redis.Set(context.Background(), stepUpBudgetKey(user.ID), "3", 0).Err())

		w := ts.DoRequest("POST", urlEmailSmsDisable, map[string]interface{}{
			"password": testPassword, // pragma: allowlist secret -- test credential constant
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code)

		_, err := ts.Redis.Get(context.Background(), stepUpBudgetKey(user.ID)).Result()
		assert.True(t, errors.Is(err, redis.Nil), "a verified commit must delete the budget key")
	})

	// "dead redis fails closed with 429" is deliberately NOT implemented here.
	// The miniredis "dead client" pattern in security_event_test.go is
	// package mfa (internal) and not reachable from this mfa_test package.
	// A full-connection substitute (close ts.Redis outright, or point at an
	// unreachable address) does not observe the intended mechanism either:
	// AuthRequired's own disabled-account check runs BEFORE any route-level
	// logic and fails closed with its own 503 "Authentication temporarily
	// unavailable" (internal/middleware/auth.go:194) the moment Redis is
	// unreachable at all — measured directly, see this batch's report. A
	// fully-dead Redis therefore never reaches the MFA handler, let alone the
	// new gate's own budget check. Observing the gate's specific
	// ReasonRateLimitBackendUnavailable 429 needs a fault that fails ONLY the
	// budget INCR (a go-redis ProcessHook, matching security_event_test.go's
	// own "dead redis" pattern) while leaving auth's own Redis calls healthy
	// — that requires constructing a Handler directly, so it belongs in the
	// internal (package mfa) test file, per the task's own instruction.
}

// TestStepUpSettings_Step5NoDeadlock is Task 2 Step 5's first bullet: same-user
// concurrent requests on the two routes that write `users` (SetBackupEmail,
// EmailSmsDisable — both FOR NO KEY UPDATE post-fix) must never 40P01.
//
// This case is GREEN TODAY, and that is expected, not a finding: the pre-fix
// handlers take no row lock at all, so nothing can deadlock — a bare UPDATE's
// implicit row lock releases the instant the statement commits. Its value is
// prospective. It must turn RED under the Task 4 mutation
// lockForNoKeyUpdate -> lockForShare (a lock upgrade inside one transaction,
// which deadlocks two concurrent same-user requests with 40P01) — see the
// plan's Task 4 Step 8 mutation check. That mutation is out of scope for this
// batch; this comment records what proves the case is wired to the thing it
// names.
//
// Rounds, not one N=10 burst, and for two independent reasons:
//  1. A burst that size collides with the PRE-EXISTING per-route
//     RateLimitByUser (3/min on EmailSmsDisable, 5/min on SetBackupEmail),
//     unrelated to the new gate.
//  2. Once the new gate lands, every one of these requests is CREDENTIALED,
//     so it also charges the new step-up budget (5 per 15 min) — a single
//     burst of 10 credentialed requests could never go green post-fix either,
//     budget exhaustion would 429 the tail of the burst on its own.
//
// R=4 rounds of a 3-wide barrier-released concurrent burst (3 fits under
// both routes' tighter, 3/min cap) resolve both: between rounds — never
// mid-flight, where the counters are actively racing the burst itself — the
// route-limiter key and the step-up budget key are cleared, so each round
// starts both ceilings fresh. The whole case runs under one outer watchdog.
func TestStepUpSettings_Step5NoDeadlock(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, nextStepUpUsername())

	const roundSize = 3
	const rounds = 4

	type roundResult struct {
		round, i int
		w        *httptest.ResponseRecorder
	}

	runRoute := func(method, url string, seedRound func(), bodyFor func(round, i int) map[string]interface{}) []roundResult {
		var all []roundResult
		for round := 0; round < rounds; round++ {
			assert.NoError(t, ts.Redis.Del(context.Background(), fmt.Sprintf("ratelimit:user:%s:%s:%s", user.ID, method, url)).Err())
			assert.NoError(t, ts.Redis.Del(context.Background(), stepUpBudgetKey(user.ID)).Err())
			if seedRound != nil {
				seedRound()
			}

			results := make([]*httptest.ResponseRecorder, roundSize)
			var wg sync.WaitGroup
			wg.Add(roundSize)
			start := make(chan struct{})
			for i := 0; i < roundSize; i++ {
				i := i
				go func() {
					defer wg.Done()
					<-start
					results[i] = ts.DoRequest(method, url, bodyFor(round, i), testhelpers.AuthHeaders(user.AccessToken))
				}()
			}
			close(start)
			wg.Wait()

			for i, w := range results {
				all = append(all, roundResult{round: round, i: i, w: w})
			}
		}
		return all
	}

	done := make(chan struct{})
	var emailResults, backupResults []roundResult
	go func() {
		defer close(done)
		emailResults = runRoute("POST", urlEmailSmsDisable, func() {
			// Re-seed each round so the burst does real work rather than a
			// no-op on an already-disabled account.
			assert.NoError(t, ts.Redis.Set(context.Background(), fmt.Sprintf("mfa_emailsms_enabled:%s:email", user.ID), "1", 0).Err())
		}, func(_, _ int) map[string]interface{} {
			return map[string]interface{}{
				"password": testPassword, // pragma: allowlist secret -- test credential constant
			}
		})
		backupResults = runRoute("PUT", urlBackupEmail, nil, func(round, i int) map[string]interface{} {
			return map[string]interface{}{
				"email":    fmt.Sprintf("concurrent-%d-%d@example.com", round, i),
				"password": testPassword, // pragma: allowlist secret -- test credential constant
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		// 60s, not 10s: on a loaded machine (load average 26-29 measured),
		// Postgres connection opens intermittently stall for seconds with
		// zero 40P01 — that stall alone tripped the tighter watchdog. A
		// real lock-upgrade deadlock still surfaces as a 500 via Postgres's
		// 1s deadlock detection and fails the per-response 200 check below,
		// and a real pool self-deadlock never resolves, so 60s keeps both
		// signals live without chasing load-average flakes.
		t.Fatal("watchdog: Step 5 rounds did not complete within 60s")
	}

	for _, r := range append(emailResults, backupResults...) {
		require.NotNil(t, r.w, "round %d request %d did not complete", r.round, r.i)
		assert.Equal(t, http.StatusOK, r.w.Code, "round %d request %d", r.round, r.i)
		assert.NotContains(t, r.w.Body.String(), "40P01", "round %d request %d body", r.round, r.i)
	}
}

// ── The nine pool-side routes speak the step-up seam ─────────────────────
//
// TOTPSetup, WebAuthnRegisterBegin, SetRecoveryOnly, SetRecoveryHardened,
// EmailSmsSetup, DesignateTrustedDevice, RemoveTrustedDevice,
// UpsertRecoveryCircle and DeleteRecoveryCircle used to answer with their own
// legacy bodies ("Incorrect password", "MFA code is required" with no methods,
// a bind 400 for a missing password). They now answer with the internal/stepup
// bodies the client parses for every other gate, and charge the same budget.

type poolStepUpRoute struct {
	name, method, url string
	body              func() map[string]interface{}
	// credentialRequired is the route's own CredentialRequired copy (I6).
	credentialRequired string
}

var poolStepUpRoutes = []poolStepUpRoute{
	{"TOTPSetup", "POST", urlTOTPSetup, func() map[string]interface{} { return map[string]interface{}{} },
		"Enter your password to set up an authenticator app."},
	{"WebAuthnRegisterBegin", "POST", urlWebAuthnRegBegin, func() map[string]interface{} {
		return map[string]interface{}{"credential_name": testCredName}
	}, "Enter your password to add a security key."},
	{"SetRecoveryOnly", "PUT", urlRecoveryOnly, func() map[string]interface{} {
		return map[string]interface{}{"methods": []string{}}
	}, "Enter your password to change which methods are for recovery only."},
	{"SetRecoveryHardened", "PUT", urlRecoveryHardened, func() map[string]interface{} {
		return map[string]interface{}{"enabled": true}
	}, "Enter your password to change hardened recovery."},
	{"EmailSmsSetup", "POST", urlEmailSmsSetup, func() map[string]interface{} {
		return map[string]interface{}{"methods": []string{"email"}}
	}, "Enter your password to turn on email or text-message codes."},
	{"DesignateTrustedDevice", "POST", urlTrustedDevices, func() map[string]interface{} {
		return map[string]interface{}{"device_name": "Laptop"}
	}, "Enter your password to trust this device for account recovery."},
	{"RemoveTrustedDevice", "DELETE", urlTrustedDevices + testZeroUUID, func() map[string]interface{} {
		return map[string]interface{}{}
	}, "Enter your password to remove a trusted recovery device."},
	{"UpsertRecoveryCircle", "PUT", urlRecoveryCircle, func() map[string]interface{} {
		share := base64.StdEncoding.EncodeToString([]byte(testShareData))
		return map[string]interface{}{
			"threshold_k": 2, "total_shares_n": 2,
			"shares": []map[string]interface{}{
				{"contact_id": "c1", "share_index": 1, "encrypted_share": share},
				{"contact_id": "c2", "share_index": 2, "encrypted_share": share},
			},
		}
	}, "Enter your password to set up your recovery circle."},
	{"DeleteRecoveryCircle", "DELETE", urlRecoveryCircle, func() map[string]interface{} {
		return map[string]interface{}{}
	}, "Enter your password to delete your recovery circle."},
}

func TestStepUpSettings_PoolRoutesSpeakTheSeam(t *testing.T) {
	for _, rt := range poolStepUpRoutes {
		rt := rt
		t.Run(rt.name, func(t *testing.T) {
			t.Run("no credentials: route copy + password_required", func(t *testing.T) {
				ts := setupTS(t)
				user := ts.CreateTestUser(t, nextStepUpUsername())
				w := ts.DoRequest(rt.method, rt.url, rt.body(), testhelpers.AuthHeaders(user.AccessToken))
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				var body map[string]interface{}
				testhelpers.ParseJSON(t, w, &body)
				assert.Equal(t, map[string]interface{}{"error": rt.credentialRequired, "password_required": true}, body)
				assert.Equal(t, 0, readStepUpBudgetCounter(t, ts, user.ID), "a credential-less request must not be charged")
			})
			t.Run("wrong password: seam body, charged", func(t *testing.T) {
				ts := setupTS(t)
				user := ts.CreateTestUser(t, nextStepUpUsername())
				body := rt.body()
				body["password"] = testBadPassword // pragma: allowlist secret -- test credential constant
				w := ts.DoRequest(rt.method, rt.url, body, testhelpers.AuthHeaders(user.AccessToken))
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				var resp map[string]interface{}
				testhelpers.ParseJSON(t, w, &resp)
				assert.Equal(t, map[string]interface{}{"error": "Invalid password"}, resp)
				assert.Equal(t, 1, readStepUpBudgetCounter(t, ts, user.ID), "a credentialed attempt must be charged")
			})
			t.Run("TOTP enrolled, password only: mfa_required with the P1 methods", func(t *testing.T) {
				ts := setupTS(t)
				user := ts.CreateTestUser(t, nextStepUpUsername())
				enrollTOTP(t, ts, user)
				// Stale email listing must never reach the offered methods.
				_, err := ts.DB.Exec(`UPDATE users SET mfa_methods = '{totp,email}' WHERE id = $1`, user.ID)
				require.NoError(t, err)
				body := rt.body()
				body["password"] = testPassword // pragma: allowlist secret -- test credential constant
				w := ts.DoRequest(rt.method, rt.url, body, testhelpers.AuthHeaders(user.AccessToken))
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				var resp map[string]interface{}
				testhelpers.ParseJSON(t, w, &resp)
				assert.Equal(t, "MFA verification required", resp["error"])
				wantMFARequiredTOTPOnlyBody(t, resp)
			})
			// The old inline gate answered a verifier ERROR as 403 "Invalid MFA
			// code", telling a user whose secret the server cannot decrypt that
			// they mistyped. The seam answers 500 and never runs the action.
			t.Run("verifier error: 500 Verification failed, not Invalid MFA code", func(t *testing.T) {
				ts := setupTS(t)
				user := ts.CreateTestUser(t, nextStepUpUsername())
				enrollTOTP(t, ts, user)
				_, err := ts.DB.Exec(`UPDATE user_mfa_totp SET totp_secret_nonce = substring(totp_secret_nonce from 1 for 11) WHERE user_id = $1`, user.ID)
				require.NoError(t, err)
				body := rt.body()
				body["password"] = testPassword // pragma: allowlist secret -- test credential constant
				body["mfa_code"] = "123456"
				w := ts.DoRequest(rt.method, rt.url, body, testhelpers.AuthHeaders(user.AccessToken))
				assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
				var resp map[string]interface{}
				testhelpers.ParseJSON(t, w, &resp)
				assert.Equal(t, map[string]interface{}{"error": "Verification failed"}, resp)
			})
		})
	}
}

// TestStepUpSettings_InTransactionRoutesPinTheirCopy is I6 for the four
// in-transaction routes: the exact CredentialRequired text, not only the
// password_required flag the route matrix checks.
func TestStepUpSettings_InTransactionRoutesPinTheirCopy(t *testing.T) {
	want := map[string]string{
		"EmailSmsDisable":           "Enter your password to turn off email verification. If you aren't asked for it, update Concord Voice.",
		"SetBackupEmail":            "Enter your password to change your backup email. If you aren't asked for it, update Concord Voice.",
		"StoreRecoveryKeyOverwrite": "Enter your password to replace your recovery key.",
		"DeleteRecoveryKey":         "Enter your password to remove your recovery key.",
	}
	for _, rt := range stepUpRoutes {
		rt := rt
		t.Run(rt.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, nextStepUpUsername())
			rt.baseline(t, ts, user.ID)
			w := ts.DoRequest(rt.method, rt.url, rt.extra(), testhelpers.AuthHeaders(user.AccessToken))
			assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var body map[string]interface{}
			testhelpers.ParseJSON(t, w, &body)
			assert.Equal(t, map[string]interface{}{"error": want[rt.name], "password_required": true}, body)
		})
	}
}

// TestStepUpSettings_PoolRouteBudget: the nine pool routes share the fail-closed
// budget — the 6th credentialed refusal is the 429, and a verified success
// clears it so correct credentials never lock the owner out.
func TestStepUpSettings_PoolRouteBudget(t *testing.T) {
	t.Run("6th wrong-password attempt returns 429", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())
		var last *httptest.ResponseRecorder
		for i := 0; i < stepUpBudgetLimitForTests+1; i++ {
			clearRouteRateLimit(t, ts, user.ID, "POST", urlTOTPSetup)
			last = ts.DoRequest("POST", urlTOTPSetup, map[string]interface{}{
				"password": testBadPassword, // pragma: allowlist secret -- test credential constant
			}, testhelpers.AuthHeaders(user.AccessToken))
		}
		assert.Equal(t, http.StatusTooManyRequests, last.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, last, &body)
		assert.Equal(t, map[string]interface{}{"error": "Too many verification attempts"}, body)
	})

	t.Run("verified success clears the budget", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, nextStepUpUsername())
		require.NoError(t, ts.Redis.Set(context.Background(), stepUpBudgetKey(user.ID), "3", 0).Err())
		w := ts.DoRequest("POST", urlTOTPSetup, map[string]interface{}{
			"password": testPassword, // pragma: allowlist secret -- test credential constant
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		_, err := ts.Redis.Get(context.Background(), stepUpBudgetKey(user.ID)).Result()
		assert.True(t, errors.Is(err, redis.Nil), "a verified success must delete the budget key")
	})
}

// stepUpBudgetLimitForTests mirrors stepup.BudgetLimit; spelled here because
// this package tests the HTTP contract, where the bound is the observable.
const stepUpBudgetLimitForTests = 5
