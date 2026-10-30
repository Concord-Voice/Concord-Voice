package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests deliberately fail, rather than silently skip or use testdb's
// developer default, unless the runner provides an isolated named database.
func setupFamilyRolloutDB(t *testing.T) *sql.DB {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	require.NotEmpty(t, raw, "set DATABASE_URL to a disposable *_test database")
	require.True(t, strings.HasSuffix(parsed.Path, "_test"), "DATABASE_URL must name a disposable *_test database")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ROOM_PLACEMENT_ENABLED", "false")
	db, _ := dbtest.SetupTestDB(t)
	// The generic fixture intentionally preserves global authority evidence.
	// Reset it only inside this explicitly disposable test database.
	_, err = dbtest.ExecReplica(db, `TRUNCATE login_family_rollout, voice_auth_room_closures,
  voice_auth_connection_closures, voice_auth_boot_fences, login_family_revocations,
  login_families, voice_enforcement_sessions CASCADE`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES(TRUE)`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE voice_enforcement_rollout SET activated_at=NULL WHERE id`)
	require.NoError(t, err)
	return db
}

func assertFamilyRolloutPrepared(t *testing.T, db *sql.DB) {
	t.Helper()
	var prepared bool
	require.NoError(t, db.QueryRow(`SELECT activated_at IS NULL AND receipt IS NULL FROM login_family_rollout WHERE id`).Scan(&prepared))
	require.True(t, prepared, "a refused CLI operation must leave both floor fields untouched")
}

func familyRolloutStatus(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	code, stdout, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout([]string{"status", "--json"}) })
	require.Equal(t, 0, code, stderr)
	require.Empty(t, stderr)
	var status map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stdout), &status))
	return status
}

func TestLoginFamilyRolloutPreparedStatusClosedSchema(t *testing.T) {
	setupFamilyRolloutDB(t)
	status := familyRolloutStatus(t)
	expected := []string{"schema", "version", "protocol", "familyActivation", "familyActivationId", "legacyUsableSIDs", "familyRows", "familySIDRows", "authConnectionRows", "authRoomBindingRows", "authParentRows", "authConnectionClosureRows", "authRoomClosureRows", "retiredBootRows", "registryRows", "dmParentRows", "credentialParentRows", "dmReconciliationRows", "oldestUnsettledSeconds", "securityRoundFresh", "productiveGrantEligible"}
	actual := make([]string, 0, len(status))
	for key := range status {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	require.Equal(t, expected, actual)
	assert.JSONEq(t, `"concord.voice-enforcement-status"`, string(status["schema"]))
	assert.Equal(t, "1", string(status["version"]))
	assert.Equal(t, "4", string(status["protocol"]))
	assert.JSONEq(t, `"prepared"`, string(status["familyActivation"]))
	assert.Equal(t, "null", string(status["familyActivationId"]))
	assert.Equal(t, "null", string(status["oldestUnsettledSeconds"]))
	assert.Equal(t, "false", string(status["securityRoundFresh"]))
	assert.Equal(t, "false", string(status["productiveGrantEligible"]))
	for _, key := range expected {
		if strings.HasSuffix(key, "Rows") || key == "legacyUsableSIDs" {
			var count int64
			require.NoError(t, json.Unmarshal(status[key], &count), key)
			assert.Zero(t, count, key)
			assert.Equal(t, "0", string(status[key]), key)
		}
	}
}

func rolloutReceipt(t *testing.T, db *sql.DB) map[string]any {
	t.Helper()
	boot := uuid.New().String()
	_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
	require.NoError(t, err)
	return map[string]any{
		"schema": "concord.login-family-activation", "version": 1, "activationId": uuid.New().String(), "protocol": 4,
		"controlPlaneArtifactSHA256": strings.Repeat("a", 64),
		"mediaSlots":                 []any{map[string]any{"slot": "media-plane", "artifactSHA256": strings.Repeat("b", 64), "nodeBootId": boot}},
		"legacyUsableSIDs":           0, "registryRows": 0, "authConnectionRows": 0, "authRoomBindingRows": 0,
		"dmParentRows": 0, "credentialParentRows": 0, "authParentRows": 0, "dmReconciliationRows": 0,
		"authConnectionClosureRows": 0, "authRoomClosureRows": 0, "retiredBootRows": 0,
	}
}

func receiptFile(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "activation.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	return path
}

func activateReceipt(t *testing.T, receipt map[string]any) (int, string, string) {
	t.Helper()
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	return activateReceiptBytes(t, raw)
}

func activateReceiptBytes(t *testing.T, raw []byte) (int, string, string) {
	t.Helper()
	path := receiptFile(t, raw)
	return captureRolloutOutput(t, func() int {
		return runVoiceEnforcementRollout([]string{"activate", "--confirm-drained", "--receipt-file", path})
	})
}

func TestLoginFamilyRolloutRejectsInvalidReceiptWithoutMutation(t *testing.T) {
	for _, name := range []string{"malformed", "duplicate", "extra", "partial", "null-count", "null-slots", "wrong-protocol", "fractional-count", "negative-retained", "incomplete-slots", "duplicate-slot", "unknown-boot", "retired-boot"} {
		t.Run(name, func(t *testing.T) {
			db := setupFamilyRolloutDB(t)
			receipt := rolloutReceipt(t, db)
			switch name {
			case "null-count":
				receipt["authRoomClosureRows"] = nil
			case "null-slots":
				receipt["mediaSlots"] = nil
			case "extra":
				receipt["unexpected"] = true
			case "partial":
				delete(receipt, "authRoomClosureRows")
			case "wrong-protocol":
				receipt["protocol"] = 3
			case "fractional-count":
				receipt["registryRows"] = 0.5
			case "negative-retained":
				receipt["retiredBootRows"] = -1
			case "incomplete-slots":
				receipt["mediaSlots"] = []any{}
			case "duplicate-slot":
				slots := receiptSlots(t, receipt)
				receipt["mediaSlots"] = append(slots, slots[0])
			case "unknown-boot":
				firstReceiptSlot(t, receipt)["nodeBootId"] = uuid.New().String()
			case "retired-boot":
				boot := firstReceiptSlot(t, receipt)["nodeBootId"]
				_, err := db.Exec(`UPDATE voice_auth_boot_fences SET retired_at=clock_timestamp() WHERE node_boot_id=$1`, boot)
				require.NoError(t, err)
				receipt["retiredBootRows"] = 1
			}
			raw, err := json.Marshal(receipt)
			require.NoError(t, err)
			if name == "malformed" {
				raw = []byte(`{"schema":`)
			}
			if name == "duplicate" {
				raw = []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1))
			}
			code, _, stderr := activateReceiptBytes(t, raw)
			require.NotEqual(t, 0, code, stderr)
			assert.NotEmpty(t, stderr)
			assertFamilyRolloutPrepared(t, db)
		})
	}
}

// This test observes PostgreSQL blocking before releasing a prior writer. It
// cannot pass merely because a goroutine happened to start after activation.
func TestLoginFamilyRolloutActivationWaitsForEarlyWriterBarrier(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	receipt := rolloutReceipt(t, db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() {
		err := tx.Rollback()
		if err != nil && err != sql.ErrTxDone {
			t.Errorf("rollback writer: %v", err)
		}
	}()
	var pid int
	require.NoError(t, tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	_, err = loginfamily.AcquireWriterBarrierTx(context.Background(), tx)
	require.NoError(t, err)
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	path := receiptFile(t, raw)
	done := make(chan int, 1)
	// Avoid the global stdout capture helper from the worker; status is checked
	// only after the worker exits, and no other test runs in parallel.
	go func() {
		done <- runVoiceEnforcementRollout([]string{"activate", "--confirm-drained", "--receipt-file", path})
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		queryErr := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
		return queryErr == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "activation must wait for the earlier shared writer")
	assertFamilyRolloutPrepared(t, db)
	require.NoError(t, tx.Commit())
	select {
	case code := <-done:
		require.Equal(t, 0, code)
	case <-time.After(12 * time.Second):
		t.Fatal("activation did not complete after writer committed")
	}
	var active bool
	require.NoError(t, db.QueryRow(`SELECT activated_at IS NOT NULL AND receipt IS NOT NULL FROM login_family_rollout WHERE id`).Scan(&active))
	assert.True(t, active)
}

func TestLoginFamilyRolloutRetainsEvidenceAndImmutableFirstReceipt(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	receipt := rolloutReceipt(t, db)
	_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, uuid.New())
	require.NoError(t, err)
	// Build retained evidence by copying a real parent and completing its
	// guarded release transaction; never fabricate a parentless room closure.
	boot := receiptString(t, firstReceiptSlot(t, receipt), "nodeBootId")
	addRetirementLiveConnection(t, db, boot)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer rollbackRolloutFixture(t, tx)
	_, err = tx.Exec(`INSERT INTO voice_auth_connection_closures(connection_generation,node_boot_id,socket_id,credential_epoch,variant,user_id,family_id,registration_token_sha256,registration_body_sha256,closed_at)
 SELECT connection_generation,node_boot_id,socket_id,credential_epoch,'bound',user_id,family_id,registration_token_sha256,registration_body_sha256,clock_timestamp() FROM voice_auth_connections WHERE node_boot_id=$1`, boot)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO voice_auth_room_closures(session_generation,connection_generation,node_boot_id,socket_id,credential_epoch,user_id,family_id,room_id,room_kind,closed_at)
 SELECT $2,connection_generation,node_boot_id,socket_id,credential_epoch,user_id,family_id,$3,'channel',clock_timestamp() FROM voice_auth_connections WHERE node_boot_id=$1`, boot, uuid.New(), uuid.New())
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM voice_auth_connections WHERE node_boot_id=$1`, boot)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	// Retained proof counts must match, but do not have to be zero.
	code, _, stderr := activateReceipt(t, receipt)
	require.NotEqual(t, 0, code, stderr)
	assertFamilyRolloutPrepared(t, db)
	receipt["retiredBootRows"] = 1
	receipt["authConnectionClosureRows"] = 1
	receipt["authRoomClosureRows"] = 1
	code, _, stderr = activateReceipt(t, receipt)
	require.Equal(t, 0, code, stderr)
	var firstTime time.Time
	var firstReceipt string
	require.NoError(t, db.QueryRow(`SELECT activated_at,receipt::text FROM login_family_rollout WHERE id`).Scan(&firstTime, &firstReceipt))
	status := familyRolloutStatus(t)
	assert.JSONEq(t, `"active"`, string(status["familyActivation"]))
	assert.JSONEq(t, `"`+receiptString(t, receipt, "activationId")+`"`, string(status["familyActivationId"]))
	for _, key := range []string{"retiredBootRows", "authConnectionClosureRows", "authRoomClosureRows"} {
		assert.Equal(t, "1", string(status[key]), key)
	}
	assert.Equal(t, "false", string(status["securityRoundFresh"]))
	assert.Equal(t, "false", string(status["productiveGrantEligible"]))
	code, _, stderr = activateReceipt(t, receipt)
	require.Equal(t, 0, code, stderr)
	for _, field := range []string{"activationId", "controlPlaneArtifactSHA256"} {
		old := receipt[field]
		if field == "activationId" {
			receipt[field] = uuid.New().String()
		} else {
			receipt[field] = strings.Repeat("d", 64)
		}
		code, _, stderr = activateReceipt(t, receipt)
		require.NotEqual(t, 0, code, stderr)
		receipt[field] = old
	}
	code, _, stderr = captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout([]string{"deactivate"}) })
	require.NotEqual(t, 0, code, stderr)
	var finalTime time.Time
	var finalReceipt string
	require.NoError(t, db.QueryRow(`SELECT activated_at,receipt::text FROM login_family_rollout WHERE id`).Scan(&finalTime, &finalReceipt))
	assert.Equal(t, firstTime, finalTime)
	assert.JSONEq(t, firstReceipt, finalReceipt)
}

func TestLoginFamilyRolloutPendingOriginalAgeAndRefusal(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	receipt := rolloutReceipt(t, db)
	user := dbtest.CreateUser(t, db)
	// The deliberately old pending operation still has a complete sealed
	// frontier, created and sealed atomically under the real SQL guards.
	operation, family := uuid.New(), uuid.New()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer rollbackRolloutFixture(t, tx)
	_, err = tx.Exec(`INSERT INTO login_family_revocations(operation_id,user_id,scope,reason,created_at,updated_at,reconcile_after) VALUES($1,$2,'user','displacement',clock_timestamp()-interval '90 seconds',clock_timestamp(),clock_timestamp())`, operation, user)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, user)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE login_families SET terminal_at=clock_timestamp(),auth_operation_id=$1 WHERE family_id=$2`, operation, family)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO login_family_revocation_targets(operation_id,family_id,user_id) VALUES($1,$2,$3)`, operation, family, user)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE login_family_revocations SET sealed_at=clock_timestamp() WHERE operation_id=$1`, operation)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	status := familyRolloutStatus(t)
	assert.Equal(t, "1", string(status["authParentRows"]))
	var age int64
	require.NoError(t, json.Unmarshal(status["oldestUnsettledSeconds"], &age))
	assert.GreaterOrEqual(t, age, int64(90))
	assert.Less(t, age, int64(100))
	code, _, stderr := activateReceipt(t, receipt)
	require.NotEqual(t, 0, code, stderr)
	assertFamilyRolloutPrepared(t, db)
}

func TestLoginFamilyRolloutNormalizesExpiredLegacyBeforeFreshLogin(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	receipt := rolloutReceipt(t, db)
	rdb := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), rdb))
	user := dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO user_keys(user_id,wrapped_private_key,key_derivation_salt,key_version,key_derivation_alg) VALUES($1,$2,$3,1,'argon2id')`, user, []byte{1}, []byte{2})
	require.NoError(t, err)
	expired, usable := uuid.New(), uuid.New()
	for _, row := range []struct {
		id     uuid.UUID
		expiry time.Time
	}{{expired, time.Now().Add(-time.Hour)}, {usable, time.Now().Add(time.Hour)}} {
		_, err := db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,ip_address,user_agent,machine_id) VALUES($1,$2,$3,$4,'192.0.2.10','rollout-fixture','rollout-machine')`, row.id, user, auth.HashRefreshToken(row.id.String()), row.expiry)
		require.NoError(t, err)
	}
	status := familyRolloutStatus(t)
	assert.Equal(t, "1", string(status["legacyUsableSIDs"]), "expired historical rows are excluded from the established usable-only counter")
	code, _, stderr := activateReceipt(t, receipt)
	require.Equal(t, 0, code, stderr)
	var revoked int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE id IN($1,$2) AND revoked_at IS NOT NULL`, expired, usable).Scan(&revoked))
	assert.Equal(t, 2, revoked)
	gin.SetMode(gin.TestMode)
	h := auth.NewHandler(db, rdb, logger.NewWithWriter(io.Discard), "rollout-test-signing-secret", nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	c.Request.RemoteAddr = "192.0.2.10:1234"
	c.Request.Header.Set("User-Agent", "rollout-fixture")
	require.True(t, h.CompleteLogin(c, user.String(), true, "", securityevent.AuthPassword), recorder.Body.String())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var body struct {
		Access string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.NotEmpty(t, body.Access)
	claims, err := auth.ValidateAccessToken(body.Access, "rollout-test-signing-secret")
	require.NoError(t, err)
	var valid bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM login_family_sids s JOIN login_families f USING(family_id,user_id) WHERE s.sid=$1 AND s.user_id=$2 AND s.access_expires_at=$3 AND f.terminal_at IS NULL)`, claims.SessionID, user, claims.ExpiresAt.Time).Scan(&valid))
	assert.True(t, valid, "post-cutover signed credentials must have real live SQL authority")
}

func TestLoginFamilyRolloutRejectsForeignMembershipWithoutNormalizing(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	receipt := rolloutReceipt(t, db)
	owner, foreign := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	sid, family := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, foreign)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 hour')`, sid, family, foreign)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 hour')`, sid, owner, auth.HashRefreshToken("rollout-foreign-membership"))
	require.NoError(t, err)
	code, _, stderr := activateReceipt(t, receipt)
	require.NotEqual(t, 0, code, stderr)
	assertFamilyRolloutPrepared(t, db)
	var revoked bool
	require.NoError(t, db.QueryRow(`SELECT revoked_at IS NOT NULL FROM refresh_tokens WHERE id=$1`, sid).Scan(&revoked))
	assert.False(t, revoked)
	var familyCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM login_families`).Scan(&familyCount))
	assert.Equal(t, 1, familyCount, "cutover must never guess a replacement family")
}
func TestLoginFamilyRolloutRejectsConfiguredInventorySubset(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	receipt := rolloutReceipt(t, db)
	t.Setenv("ROOM_PLACEMENT_ENABLED", "true")
	firstReceiptSlot(t, receipt)["slot"] = "media-plane-blue"
	code, _, stderr := activateReceipt(t, receipt)
	require.NotEqual(t, 0, code, stderr)
	assertFamilyRolloutPrepared(t, db)
}

func TestLoginFamilyRolloutRejectsLiveConnection(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	receipt := rolloutReceipt(t, db)
	user, family := dbtest.CreateUser(t, db), uuid.New()
	boot := firstReceiptSlot(t, receipt)["nodeBootId"]
	_, err := db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, user)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_auth_connections(connection_generation,family_id,user_id,credential_epoch,node_boot_id,socket_id,registration_token_sha256,registration_body_sha256) VALUES($1,$2,$3,'',$4,'live-socket',$5,$5)`, uuid.New(), family, user, boot, strings.Repeat("e", 64))
	require.NoError(t, err)
	status := familyRolloutStatus(t)
	assert.Equal(t, "1", string(status["authConnectionRows"]))
	code, _, stderr := activateReceipt(t, receipt)
	require.NotEqual(t, 0, code, stderr)
	assertFamilyRolloutPrepared(t, db)
	// Claiming the live count honestly still cannot authorize activation.
	receipt["authConnectionRows"] = 1
	code, _, stderr = activateReceipt(t, receipt)
	require.NotEqual(t, 0, code, stderr)
	assertFamilyRolloutPrepared(t, db)
}

func retirementStateSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var state string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'boots',(SELECT coalesce(jsonb_agg(to_jsonb(b) ORDER BY node_boot_id),'[]') FROM voice_auth_boot_fences b),
 'connections',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY connection_generation),'[]') FROM voice_auth_connections c),
 'sessions',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY session_generation),'[]') FROM voice_enforcement_sessions s),
 'bindings',(SELECT coalesce(jsonb_agg(to_jsonb(b) ORDER BY session_generation),'[]') FROM voice_auth_room_bindings b),
 'connectionClosures',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY connection_generation),'[]') FROM voice_auth_connection_closures c),
 'roomClosures',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY session_generation),'[]') FROM voice_auth_room_closures c),
 'familyRollout',(SELECT to_jsonb(r) FROM login_family_rollout r WHERE id),
 'voiceRollout',(SELECT to_jsonb(r) FROM voice_enforcement_rollout r WHERE id))::text`).Scan(&state))
	return state
}

func TestLoginFamilyRolloutOperatorRetirementCommitsPermanentAbsentFence(t *testing.T) {
	for _, variant := range []string{"container", "native"} {
		t.Run(variant, func(t *testing.T) {
			db := setupFamilyRolloutDB(t)
			boot := uuid.New().String()
			// Only the privileged local CLI may retire an absent identity. Service HTTP
			// unknown-boot refusal is independently exercised by the API test owner.
			args := operatorRetirementArgs(boot, "media-plane", variant)
			code, _, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(args) })
			require.Equal(t, 0, code, stderr)
			require.Empty(t, stderr)
			var retired bool
			require.NoError(t, db.QueryRow(`SELECT retired_at IS NOT NULL FROM voice_auth_boot_fences WHERE node_boot_id=$1`, boot).Scan(&retired))
			require.True(t, retired)
			before := retirementStateSnapshot(t, db)
			code, _, stderr = captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(args) })
			require.Equal(t, 0, code, stderr)
			assert.Equal(t, before, retirementStateSnapshot(t, db), "retry cannot replace the original permanent fence")
			// Permanent retirement must be obeyed by the real creation guard.
			user, family := dbtest.CreateUser(t, db), uuid.New()
			_, err := db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, user)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO voice_auth_connections(connection_generation,family_id,user_id,credential_epoch,node_boot_id,socket_id,registration_token_sha256,registration_body_sha256) VALUES($1,$2,$3,'',$4,'after-retirement',$5,$5)`, uuid.New(), family, user, boot, strings.Repeat("e", 64))
			require.ErrorContains(t, err, "boot is absent or retired")
			assert.Equal(t, before, retirementStateSnapshot(t, db))
			assertFamilyRolloutPrepared(t, db)
		})
	}
}

func TestLoginFamilyRolloutOperatorRetirementExactConfiguredSlots(t *testing.T) {
	cases := []struct {
		placement, slot, variant string
		want                     int
	}{
		{"false", "media-plane", "container", 0},
		{"false", "media-plane", "native", 0},
		{"true", "media-plane-blue", "container", 0},
		{"true", "media-plane-green", "container", 0},
		{"false", "media-plane-blue", "container", 64},
		{"false", "media-plane-green", "container", 64},
		{"true", "media-plane", "container", 64},
		{"true", "media-plane-blue", "native", 64},
		{"true", "media-plane-green", "native", 64},
		{"true", "media-plane", "native", 64},
		{"false", "guessed-slot", "container", 64},
	}
	for _, tc := range cases {
		t.Run(tc.placement+"/"+tc.slot+"/"+tc.variant, func(t *testing.T) {
			db := setupFamilyRolloutDB(t)
			t.Setenv("ROOM_PLACEMENT_ENABLED", tc.placement)
			before := retirementStateSnapshot(t, db)
			args := operatorRetirementArgs(uuid.New().String(), tc.slot, tc.variant)
			code, _, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(args) })
			require.Equal(t, tc.want, code, stderr)
			if tc.want != 0 {
				assert.NotEmpty(t, stderr)
				assert.Equal(t, before, retirementStateSnapshot(t, db))
			}
			assertFamilyRolloutPrepared(t, db)
		})
	}
}

func addRetirementLiveConnection(t *testing.T, db *sql.DB, boot string) {
	t.Helper()
	user, family := dbtest.CreateUser(t, db), uuid.New()
	_, err := db.Exec(`INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES($1,$2,'')`, family, user)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO voice_auth_connections(connection_generation,family_id,user_id,credential_epoch,node_boot_id,socket_id,registration_token_sha256,registration_body_sha256) VALUES($1,$2,$3,'',$4,'retirement-live',$5,$5)`, uuid.New(), family, user, boot, strings.Repeat("e", 64))
	require.NoError(t, err)
}

func TestLoginFamilyRolloutVerifyRetiredBootFreshGateAndReadOnly(t *testing.T) {
	for _, state := range []string{"absent", "active", "retired", "other-boot-live"} {
		t.Run(state, func(t *testing.T) {
			db := setupFamilyRolloutDB(t)
			boot := uuid.New().String()
			if state != "absent" {
				_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,CASE WHEN $2 THEN clock_timestamp() ELSE NULL END)`, boot, state != "active")
				require.NoError(t, err)
			}
			switch state {
			case "other-boot-live":
				other := uuid.New().String()
				_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, other)
				require.NoError(t, err)
				addRetirementLiveConnection(t, db, other)

			}
			before := retirementStateSnapshot(t, db)
			code, _, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout([]string{"verify-retired-boot", "--node-boot-id", boot}) })
			want := 3
			if state == "retired" || state == "other-boot-live" {
				want = 0
			}
			require.Equal(t, want, code, stderr)
			assert.Equal(t, before, retirementStateSnapshot(t, db), "read-only observation must neither insert nor update any authority/evidence")
		})
	}
}

func TestLoginFamilyRolloutOperatorRetirementRefusesLiveEvidenceWithoutMutation(t *testing.T) {
	for _, state := range []string{"absent", "active"} {
		t.Run(state, func(t *testing.T) {
			db := setupFamilyRolloutDB(t)
			boot := uuid.New().String()
			if state != "absent" {
				_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,CASE WHEN $2 THEN clock_timestamp() ELSE NULL END)`, boot, state == "retired")
				require.NoError(t, err)
			}
			if state == "absent" {
				_, err := db.Exec(`INSERT INTO voice_enforcement_sessions(session_generation,node_boot_id,room_id,room_kind,user_id,credential_epoch,socket_id) VALUES($1,$2,$3,'channel',$4,'','retirement-session')`, uuid.New(), boot, uuid.New(), uuid.New())
				require.NoError(t, err)
			} else {
				addRetirementLiveConnection(t, db, boot)
			}
			before := retirementStateSnapshot(t, db)
			args := operatorRetirementArgs(boot, "media-plane", "container")
			code, _, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(args) })
			require.Equal(t, 1, code, stderr)
			assert.NotEmpty(t, stderr)
			assert.Equal(t, before, retirementStateSnapshot(t, db))
		})
	}
}

func TestLoginFamilyRolloutVerifyRetiredBootObservationFailureDoesNotMutate(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	boot := uuid.New().String()
	_, err := db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, boot)
	require.NoError(t, err)
	before := retirementStateSnapshot(t, db)
	_, err = db.Exec(`ALTER TABLE voice_auth_connections RENAME TO rollout_test_connections_unavailable`)
	require.NoError(t, err)
	restored := false
	t.Cleanup(func() {
		if !restored {
			_, restoreErr := db.Exec(`ALTER TABLE rollout_test_connections_unavailable RENAME TO voice_auth_connections`)
			require.NoError(t, restoreErr)
		}
	})
	code, stdout, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout([]string{"verify-retired-boot", "--node-boot-id", boot}) })
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "observe retired boot failed")
	_, err = db.Exec(`ALTER TABLE rollout_test_connections_unavailable RENAME TO voice_auth_connections`)
	require.NoError(t, err)
	restored = true
	assert.Equal(t, before, retirementStateSnapshot(t, db))
}

func rollbackRolloutFixture(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
		t.Errorf("rollback rollout fixture: %v", err)
	}
}

func receiptSlots(t *testing.T, receipt map[string]any) []any {
	t.Helper()
	slots, ok := receipt["mediaSlots"].([]any)
	require.True(t, ok, "receipt mediaSlots must be an array")
	require.NotEmpty(t, slots)
	return slots
}

func firstReceiptSlot(t *testing.T, receipt map[string]any) map[string]any {
	t.Helper()
	slot, ok := receiptSlots(t, receipt)[0].(map[string]any)
	require.True(t, ok, "receipt media slot must be an object")
	return slot
}

func receiptString(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	require.True(t, ok, "receipt %s must be a string", key)
	return value
}
