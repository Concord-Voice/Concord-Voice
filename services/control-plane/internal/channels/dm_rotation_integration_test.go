package channels_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/e2eekeys"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathDMConversationsPrefix = "/api/v1/dm/conversations/"
	pathE2EEPendingKeys       = "/api/v1/e2ee/pending-keys"
	fmtRateLimitDMRotateKey   = "ratelimit:dm_rotate:%s"
	fmtUserRLKeyDMRotate      = "ratelimit:user:%s:POST:/api/v1/dm/conversations/:id/rotate-key"
)

// dmRotationBody builds a successor batch: one wrap per listed user at the
// claimed epoch. Wrap bytes are opaque to the server, so a label suffices.
func dmRotationBody(version int, userIDs ...string) map[string]interface{} {
	wrapped := make(map[string]string, len(userIDs))
	for _, id := range userIDs {
		wrapped[id] = fmt.Sprintf("wrapped-%s-v%d", id, version)
	}
	return map[string]interface{}{"wrapped_keys": wrapped, "key_version": version}
}

func dmKeyVersions(t *testing.T, ts *testhelpers.TestServer, convID, userID string) []int {
	t.Helper()
	rows, err := ts.DB.Query(
		`SELECT key_version FROM dm_channel_keys WHERE conversation_id = $1 AND user_id = $2 ORDER BY key_version`,
		convID, userID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var versions []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		versions = append(versions, v)
	}
	require.NoError(t, rows.Err())
	return versions
}

func dmRevocationCount(t *testing.T, ts *testhelpers.TestServer, convID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_key_revocations WHERE conversation_id = $1`, convID).Scan(&n))
	return n
}

// Two participants at epoch 1; user1 rotates. The successor wraps and the
// revocation land together, and the response names the new epoch.
func TestRotateDMKey_AtomicSuccess(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrot-a1")
	user2 := ts.CreateTestUser(t, "dmrot-a2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["new_key_version"])

	assert.Equal(t, []int{1, 2}, dmKeyVersions(t, ts, convID, user1.ID))
	assert.Equal(t, []int{1, 2}, dmKeyVersions(t, ts, convID, user2.ID))

	var revokedEpoch, successorEpoch int
	var reason, revokedBy string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_epoch, successor_epoch, reason, revoked_by FROM dm_key_revocations WHERE conversation_id = $1`,
		convID).Scan(&revokedEpoch, &successorEpoch, &reason, &revokedBy))
	assert.Equal(t, 1, revokedEpoch)
	assert.Equal(t, 2, successorEpoch)
	assert.Equal(t, "manual_rotation", reason)
	assert.Equal(t, user1.ID, revokedBy)

	// The new epoch is what a current-key fetch now serves, for both.
	w = ts.DoRequest(http.MethodGet, pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var keyResp e2eekeys.KeyResponse
	testhelpers.ParseJSON(t, w, &keyResp)
	assert.Equal(t, 2, keyResp.Key.KeyVersion)
}

// The contract the prod lockout came from: a rotation request carrying no
// key material. It must fail, and it must write nothing.
func TestRotateDMKey_RefusesBodylessRequest(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrot-b1")
	user2 := ts.CreateTestUser(t, "dmrot-b2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey, nil,
		testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user1.ID))
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2.ID))
}

// A batch that leaves a participant out would strand them at the revoked
// epoch the moment it committed. Refused, nothing written.
func TestRotateDMKey_RefusesIncompleteBatch(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrot-c1")
	user2 := ts.CreateTestUser(t, "dmrot-c2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, user1.ID), testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Key rotation must wrap the key for every participant", body["error"])
	assert.Equal(t, float64(1), body["missing"])
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user1.ID))
}

// Only current+1 may be claimed: a gap leaves versions no message references,
// and current+1 already taken means another claimant won.
func TestRotateDMKey_RefusesEpochOtherThanNext(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrot-d1")
	user2 := ts.CreateTestUser(t, "dmrot-d2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	headers := testhelpers.AuthHeaders(user1.AccessToken)

	for _, claimed := range []int{1, 3} {
		w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
			dmRotationBody(claimed, user1.ID, user2.ID), headers)
		assert.Equal(t, http.StatusConflict, w.Code, "claimed %d: %s", claimed, w.Body.String())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "Key rotation must claim the next epoch", body["error"])
		assert.Equal(t, float64(1), body["current_version"])
	}
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user1.ID))
}

// A participant who never held the current epoch cannot re-key the
// conversation onto material the established members never agreed to.
func TestRotateDMKey_RefusesClaimantWithoutCurrentKey(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrot-e1")
	user2 := ts.CreateTestUser(t, "dmrot-e2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user2.ID, 1) // user1 has no wrap at all

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))
	assert.Nil(t, dmKeyVersions(t, ts, convID, user1.ID))
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2.ID))
}

func TestRotateDMKey_NotParticipantAndInvalidID(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrot-f1")
	user2 := ts.CreateTestUser(t, "dmrot-f2")
	outsider := ts.CreateTestUser(t, "dmrot-f3")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, user1.ID, user2.ID), testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))

	w = ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+"not-a-uuid"+pathRotateKey,
		dmRotationBody(2, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// A conversation with no key yet: version 1 is an initial distribution, so no
// epoch is revoked. The old handler answered this with a bare 200 and wrote
// nothing; now the wraps land.
func TestRotateDMKey_InitialDistributionRevokesNothing(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrot-g1")
	user2 := ts.CreateTestUser(t, "dmrot-g2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(1, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["new_key_version"])
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user1.ID))
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2.ID))
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))
}

// The unified distribute route enforces the same claim fences, because the
// rotation coordinator on every participant's client posts through it.
func TestUnifiedDistributeDM_SuccessorClaimFences(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmclaim-1")
	user2 := ts.CreateTestUser(t, "dmclaim-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	headers := testhelpers.AuthHeaders(user1.AccessToken)

	// Incomplete successor batch → refused, nothing written.
	w := ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(2, user1.ID), headers)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2.ID))

	// A gap → refused with the epoch the conversation is at.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(3, user1.ID, user2.ID), headers)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// A rewrap at the CURRENT epoch is not a claim and stays allowed: a peer
	// fulfilling a pending request posts exactly this shape.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(1, user2.ID), headers)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The next epoch, every participant wrapped → the claim lands, and the
	// ledger row commits WITH it: the successor claim is the only writer of
	// dm_key_revocations, so an epoch is never revoked ahead of its
	// replacement (membership changes only cue the claim).
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(2, user1.ID, user2.ID), headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []int{1, 2}, dmKeyVersions(t, ts, convID, user1.ID))
	assert.Equal(t, []int{1, 2}, dmKeyVersions(t, ts, convID, user2.ID))
	var revokedEpoch, successorEpoch int
	var reason string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_epoch, successor_epoch, reason FROM dm_key_revocations WHERE conversation_id = $1`,
		convID).Scan(&revokedEpoch, &successorEpoch, &reason))
	assert.Equal(t, 1, revokedEpoch)
	assert.Equal(t, 2, successorEpoch)
	assert.Equal(t, "successor_claim", reason)

	// A wrap BELOW the current epoch is refused with the epoch to resync to.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(1, user2.ID), headers)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var below map[string]interface{}
	testhelpers.ParseJSON(t, w, &below)
	assert.Equal(t, float64(2), below["current_version"])

	// A participant without the current epoch cannot claim the one after it.
	outsiderKeyed := ts.CreateTestUser(t, "dmclaim-3")
	conv2 := ts.CreateDMConversation(t, outsiderKeyed.ID, user2.ID)
	ts.SeedDMKey(t, conv2, user2.ID, 1)
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+conv2, dmRotationBody(2, outsiderKeyed.ID, user2.ID),
		testhelpers.AuthHeaders(outsiderKeyed.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Nil(t, dmKeyVersions(t, ts, conv2, outsiderKeyed.ID))
}

// A revoked epoch blocks the current-key fetch, not a history read of a wrap
// the caller holds. Without this a rotated DM lost its history on any device
// that had to refetch.
func TestGetDMKey_VersionedFetchOfRevokedEpochIsServed(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrev-1")
	user2 := ts.CreateTestUser(t, "dmrev-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	ts.SeedDMKeyRevocation(t, convID, 1, 2)
	headers := testhelpers.AuthHeaders(user1.AccessToken)

	w := ts.DoRequest(http.MethodGet, pathE2EEKeys+convID, nil, headers)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var errResp e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &errResp)
	assert.Equal(t, e2eekeys.CodeRevokedEpoch, errResp.Code)

	w = ts.DoRequest(http.MethodGet, pathE2EEKeys+convID+"?version=1", nil, headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var keyResp e2eekeys.KeyResponse
	testhelpers.ParseJSON(t, w, &keyResp)
	assert.Equal(t, 1, keyResp.Key.KeyVersion)
}

// Only a holder of the conversation's current epoch is offered its pending
// requests: a holder of an older epoch would wrap the wrong key and the
// server would stamp it at the current version.
func TestPendingDMRequests_OnlyCurrentEpochHolderIsOffered(t *testing.T) {
	ts := setupTS(t)
	current := ts.CreateTestUser(t, "dmpend-1")
	stale := ts.CreateTestUser(t, "dmpend-2")
	convID := ts.CreateDMConversation(t, current.ID, stale.ID)
	ts.SeedDMKey(t, convID, current.ID, 1)
	ts.SeedDMKey(t, convID, current.ID, 2)
	ts.SeedDMKey(t, convID, stale.ID, 1)
	requester := ts.CreateTestUser(t, "dmpend-3")
	_, err := ts.DB.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, requester.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO dm_pending_key_requests (conversation_id, user_id) VALUES ($1, $2)`, convID, requester.ID)
	require.NoError(t, err)

	pendingFor := func(user testhelpers.TestUser) []string {
		w := ts.DoRequest(http.MethodGet, pathE2EEPendingKeys, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body struct {
			PendingRequests []struct {
				ChannelID string `json:"channel_id"`
			} `json:"pending_requests"`
		}
		testhelpers.ParseJSON(t, w, &body)
		ids := make([]string, 0, len(body.PendingRequests))
		for _, r := range body.PendingRequests {
			ids = append(ids, r.ChannelID)
		}
		return ids
	}
	assert.Contains(t, pendingFor(current), convID)
	assert.NotContains(t, pendingFor(stale), convID)
}

// --- Rate limit (moved from the dm package with the handler) ---

func TestDMRotateKeyRateLimitBlocks11th(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrl-user1")
	user2 := ts.CreateTestUser(t, "dmrl-user2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	headers := testhelpers.AuthHeaders(user1.AccessToken)
	endpoint := pathDMConversationsPrefix + convID + pathRotateKey
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitDMRotateKey, convID))

	// Ten rotations succeed, each claiming the next epoch.
	for i := 1; i <= 10; i++ {
		if i%4 == 0 {
			ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
		}
		w := ts.DoRequest(http.MethodPost, endpoint, dmRotationBody(i+1, user1.ID, user2.ID), headers)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should succeed: %s", i, w.Body.String())
	}
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))

	// The 11th is refused by the per-conversation budget the ten commits spent.
	w := ts.DoRequest(http.MethodPost, endpoint, dmRotationBody(12, user1.ID, user2.ID), headers)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Rate limit exceeded", body["error"])
	assert.Contains(t, body["message"], "Try again in")
	assert.NotNil(t, body["retry_after"])
}

func TestDMRotateKeyRateLimitIndependentConversations(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmrl-indep1")
	user2 := ts.CreateTestUser(t, "dmrl-indep2")
	user3 := ts.CreateTestUser(t, "dmrl-indep3")
	convA := ts.CreateDMConversation(t, user1.ID, user2.ID)
	convB := ts.CreateDMConversation(t, user1.ID, user3.ID)
	for _, seed := range []struct{ conv, user string }{{convA, user1.ID}, {convA, user2.ID}, {convB, user1.ID}, {convB, user3.ID}} {
		ts.SeedDMKey(t, seed.conv, seed.user, 1)
	}
	headers := testhelpers.AuthHeaders(user1.AccessToken)
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitDMRotateKey, convA))
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitDMRotateKey, convB))

	for i := 1; i <= 10; i++ {
		if i%4 == 0 {
			ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
		}
		w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convA+pathRotateKey,
			dmRotationBody(i+1, user1.ID, user2.ID), headers)
		assert.Equal(t, http.StatusOK, w.Code, "conv A request %d should succeed: %s", i, w.Body.String())
	}
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convA+pathRotateKey,
		dmRotationBody(12, user1.ID, user2.ID), headers)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
	w = ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convB+pathRotateKey,
		dmRotationBody(2, user1.ID, user3.ID), headers)
	assert.Equal(t, http.StatusOK, w.Code, "conv B should still work: %s", w.Body.String())
}

// The per-conversation rotation limiter (10/24h) was keyed on the raw path
// parameter, so re-spelling the conversation UUID minted a fresh counter with a
// full budget (#1218 red-team, same class as the DM key-distribution limiter).
// uuid.Parse accepts upper-case, hyphen-less and braced forms and PostgreSQL's
// uuid_in accepts the same set, so every gate still passed against the same row.
// The handler canonicalizes at its parse; moved here with the handler.
func TestRotateDMKey_LimitSurvivesUUIDRespelling(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "rotrespell1")
	user2 := ts.CreateTestUser(t, "rotrespell2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	ctx := context.Background()
	ts.Redis.Del(ctx, fmt.Sprintf(fmtRateLimitDMRotateKey, convID))

	// Spend the whole per-conversation rotation budget.
	for i := 0; i < 10; i++ {
		w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
			dmRotationBody(i+2, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, "rotation %d of 10 should be within budget: %s", i+1, w.Body.String())
		// The route also caps 5/min per user; clear it so only the per-conversation
		// limiter can answer, or this test proves the wrong limiter.
		ts.Redis.Del(ctx, fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
	}
	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(12, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusTooManyRequests, w.Code, "the 11th rotation must be blocked")

	upper := strings.ToUpper(convID)
	require.NotEqual(t, convID, upper, "fixture UUID must contain hex letters to re-spell")

	for name, spelling := range map[string]string{
		"upper-case":  upper,
		"hyphen-less": strings.ReplaceAll(convID, "-", ""),
		"braced":      "{" + convID + "}",
	} {
		t.Run(name, func(t *testing.T) {
			ts.Redis.Del(ctx, fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
			// The budget is read after the body checks (a refusal must not
			// spend it), so the request carries a valid successor batch: the
			// only thing left to refuse it is the canonical conversation key.
			got := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+spelling+pathRotateKey,
				dmRotationBody(12, user1.ID, user2.ID), testhelpers.AuthHeaders(user1.AccessToken))
			assert.Equal(t, http.StatusTooManyRequests, got.Code,
				"re-spelling the conversation id must not mint a second rotation budget")
			assert.Equal(t, "10", got.Header().Get("X-RateLimit-Limit"),
				"the 429 must come from the per-conversation limiter")
		})
	}
}

// A DM peer with no wrap yet is enrolled as pending on its first key fetch,
// and the row is served only when a holder runs its pending queue. Nothing
// paged a holder who was online the whole time — key_needed was a
// server-channel push — so the requester waited on the holder's next
// reconnect. The first enrollment now pages every other participant.
func TestGetDMKey_FirstEnrollmentPagesHolders(t *testing.T) {
	ts := setupTS(t)
	holder := ts.CreateTestUser(t, "dmkn-holder")
	requester := ts.CreateTestUser(t, "dmkn-req")
	convID := ts.CreateDMConversation(t, holder.ID, requester.ID)
	ts.SeedDMKey(t, convID, holder.ID, 1)

	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(wsServer.URL, "http")+"/api/v1/ws?token="+url.QueryEscape(holder.AccessToken), nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(holder.ID)) > 0
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err = conn.ReadMessage() // connection bootstrap
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodGet, pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(requester.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var errResp e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &errResp)
	require.Equal(t, e2eekeys.CodeNoKeyYet, errResp.Code)
	require.True(t, errResp.Pending)

	var keyNeeded map[string]interface{}
	deadline := time.Now().Add(2 * time.Second)
	for keyNeeded == nil && time.Now().Before(deadline) {
		require.NoError(t, conn.SetReadDeadline(deadline))
		_, frame, readErr := conn.ReadMessage()
		if readErr != nil {
			if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
				break
			}
			require.NoError(t, readErr)
		}
		var event struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(frame, &event))
		if event.Type == "key_needed" {
			keyNeeded = event.Data
		}
	}
	require.NotNil(t, keyNeeded, "the holder must be paged on the requester's first enrollment")
	assert.Equal(t, requester.ID, keyNeeded["user_id"])
	assert.Equal(t, []interface{}{convID}, keyNeeded["channel_ids"])
	_, hasServer := keyNeeded["server_id"]
	assert.False(t, hasServer, "a DM has no server; the client schema makes the field optional")

	// A second fetch hits ON CONFLICT DO NOTHING and must not page again.
	// Drain everything that arrives inside the window rather than judge the
	// first frame: an unrelated frame first would otherwise satisfy this.
	w = ts.DoRequest(http.MethodGet, pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(requester.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	quiet := time.Now().Add(500 * time.Millisecond)
	for {
		require.NoError(t, conn.SetReadDeadline(quiet))
		_, frame, readErr := conn.ReadMessage()
		if readErr != nil {
			netErr, ok := readErr.(net.Error)
			require.True(t, ok && netErr.Timeout(), "unexpected read error: %v", readErr)
			break
		}
		var event struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(frame, &event))
		assert.NotEqual(t, "key_needed", event.Type, "a re-fetch must not re-page every holder")
	}
}

// A participant holding NO key at the live epoch could write an invented wrap
// into it — for themselves and for anyone else still missing a row — because
// the successor fences ran only for a claim ABOVE the current epoch. The row
// planted that way also satisfied the holder check, so it was a self-service
// bypass of errDMEpochClaimNotHolder. Red-team PoC 1 on this PR, inverted.
func TestUnifiedDistributeDM_NonHolderCannotWriteLiveEpoch(t *testing.T) {
	ts := setupTS(t)
	holder := ts.CreateTestUser(t, "dmlive-holder")
	stranded := ts.CreateTestUser(t, "dmlive-stranded")
	convID := ts.CreateDMConversation(t, holder.ID, stranded.ID)
	ts.SeedDMKey(t, convID, holder.ID, 1)

	w := ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(1, stranded.ID),
		testhelpers.AuthHeaders(stranded.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Nil(t, dmKeyVersions(t, ts, convID, stranded.ID))

	// And they cannot claim the epoch after it either — the pending queue a
	// holder serves is their only way back in.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(2, holder.ID, stranded.ID),
		testhelpers.AuthHeaders(stranded.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Nil(t, dmKeyVersions(t, ts, convID, stranded.ID))

	// The holder's rewrap at the live epoch for the stranded peer is what
	// the equal-epoch path exists for.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(1, stranded.ID),
		testhelpers.AuthHeaders(holder.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, stranded.ID))
}

// The completeness fence checked that the batch NAMED every participant, but
// delivery then skipped anyone whose client-declared wrapped_key_versions
// entry no longer matched (#2420) — and the revocation committed regardless.
// Naming a participant with a bogus version therefore produced the exact
// post-state of the 2026-09-17 lockout for them. Red-team PoC 2, inverted.
func TestRotateDMKey_RefusesWhenARecipientWrapIsStale(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmstale-1")
	user2 := ts.CreateTestUser(t, "dmstale-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	body := dmRotationBody(2, user1.ID, user2.ID)
	body["wrapped_key_versions"] = map[string]int{user2.ID: 0}
	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey, body,
		testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, float64(1), resp["stale_recipients"])
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user1.ID), "nothing committed")
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2.ID))
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID), "no epoch revoked ahead of its successor")

	// The same batch on the unified route is refused the same way.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, body, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))
}

// The revoked-epoch refusal names the successor the ledger recorded, so a
// device that never saw the epoch number (fresh launch) claims the right one
// instead of guessing — a guess at or below the current epoch is a no-op
// rewrap that the client would repeat every backoff window.
func TestGetDMKey_RevokedEpochNamesItsSuccessor(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmsucc-1")
	user2 := ts.CreateTestUser(t, "dmsucc-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	ts.SeedDMKeyRevocation(t, convID, 1, 2)

	w := ts.DoRequest(http.MethodGet, pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var errResp e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &errResp)
	assert.Equal(t, e2eekeys.CodeRevokedEpoch, errResp.Code)
	assert.Equal(t, 2, errResp.SuccessorEpoch)

	// The claim it cues lands, and heals the conversation for both.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID, dmRotationBody(2, user1.ID, user2.ID),
		testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = ts.DoRequest(http.MethodGet, pathE2EEKeys+convID, nil, testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var keyResp e2eekeys.KeyResponse
	testhelpers.ParseJSON(t, w, &keyResp)
	assert.Equal(t, 2, keyResp.Key.KeyVersion)
	// A NO_KEY_YET refusal carries no successor.
	other := ts.CreateTestUser(t, "dmsucc-3")
	conv2 := ts.CreateDMConversation(t, other.ID, user2.ID)
	w = ts.DoRequest(http.MethodGet, pathE2EEKeys+conv2, nil, testhelpers.AuthHeaders(other.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code)
	var noKey e2eekeys.ErrorResponse
	testhelpers.ParseJSON(t, w, &noKey)
	assert.Equal(t, e2eekeys.CodeNoKeyYet, noKey.Code)
	assert.Equal(t, 0, noKey.SuccessorEpoch)
}

func TestRotateDMKey_Unauthenticated(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmunauth-1")
	user2 := ts.CreateTestUser(t, "dmunauth-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, user1.ID, user2.ID), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// The renderer offers Rotate Encryption Key to a group's creator only; the
// server admitted any participant, so a direct POST from any member could
// re-key the group for everyone (CWE-602). Creator or admin role, server-side.
func TestRotateDMKey_GroupRequiresCreatorOrAdmin(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "dmgrp-creator")
	admin := ts.CreateTestUser(t, "dmgrp-admin")
	member := ts.CreateTestUser(t, "dmgrp-member")
	convID := ts.CreateGroupDMConversation(t, creator.ID, admin.ID, member.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, admin.ID)
	require.NoError(t, err)
	for _, u := range []testhelpers.TestUser{creator, admin, member} {
		ts.SeedDMKey(t, convID, u.ID, 1)
	}
	all := []string{creator.ID, admin.ID, member.ID}

	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, all...), testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, member.ID))
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID))

	w = ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, all...), testhelpers.AuthHeaders(admin.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, creator.ID))
	w = ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(3, all...), testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []int{1, 2, 3}, dmKeyVersions(t, ts, convID, member.ID))
}

// The per-conversation budget is spent by committed rotations only. Every
// participant shares it and the route now refuses several request shapes;
// if refusals spent it, one member — or one client still speaking the old
// bodyless contract — could exhaust it and block the owner's
// incident-response rotation for a day.
func TestRotateDMKey_RefusalsDoNotSpendTheConversationBudget(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmbudget-1")
	user2 := ts.CreateTestUser(t, "dmbudget-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)
	headers := testhelpers.AuthHeaders(user1.AccessToken)
	endpoint := pathDMConversationsPrefix + convID + pathRotateKey
	budgetKey := fmt.Sprintf(fmtRateLimitDMRotateKey, convID)
	ts.Redis.Del(context.Background(), budgetKey)

	// Refusals: bodyless, incomplete, wrong epoch, non-holder.
	require.Equal(t, http.StatusBadRequest, ts.DoRequest(http.MethodPost, endpoint, nil, headers).Code)
	require.Equal(t, http.StatusBadRequest, ts.DoRequest(http.MethodPost, endpoint, dmRotationBody(2, user1.ID), headers).Code)
	require.Equal(t, http.StatusConflict, ts.DoRequest(http.MethodPost, endpoint, dmRotationBody(5, user1.ID, user2.ID), headers).Code)
	stranded := ts.CreateTestUser(t, "dmbudget-3")
	conv2 := ts.CreateDMConversation(t, stranded.ID, user2.ID)
	ts.SeedDMKey(t, conv2, user2.ID, 1)
	require.Equal(t, http.StatusForbidden, ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+conv2+pathRotateKey,
		dmRotationBody(2, stranded.ID, user2.ID), testhelpers.AuthHeaders(stranded.AccessToken)).Code)
	spent, err := ts.Redis.Exists(context.Background(), budgetKey).Result()
	require.NoError(t, err)
	assert.Zero(t, spent, "a refused request must not spend the conversation's rotation budget")
	conv2Spent, err := ts.Redis.Exists(context.Background(), fmt.Sprintf(fmtRateLimitDMRotateKey, conv2)).Result()
	require.NoError(t, err)
	assert.Zero(t, conv2Spent, "a 403 non-holder refusal must not spend the target conversation's rotation budget")

	// A committed rotation spends exactly one.
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
	require.Equal(t, http.StatusOK, ts.DoRequest(http.MethodPost, endpoint, dmRotationBody(2, user1.ID, user2.ID), headers).Code)
	count, err := ts.Redis.Get(context.Background(), budgetKey).Int()
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// An exhausted budget refuses before the transaction.
	require.NoError(t, ts.Redis.Set(context.Background(), budgetKey, 10, 24*time.Hour).Err())
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtUserRLKeyDMRotate, user1.ID))
	w := ts.DoRequest(http.MethodPost, endpoint, dmRotationBody(3, user1.ID, user2.ID), headers)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Equal(t, []int{1, 2}, dmKeyVersions(t, ts, convID, user1.ID))
}

// TestUnifiedDistributeDM_InitialSelfOnlyClaimIsRefused pins the B1 fix
// (red-team, PR #3343): on a keyless conversation a participant could claim
// epoch 1 with a self-only batch, becoming the sole holder while the keyed
// peer was left a permanent non-holder behind the holder fence. A batch that
// wraps the actor's own row must now cover every other keyed participant; the
// legitimate #1023 bootstrap (wrap the peer, omit self) is untouched.
func TestUnifiedDistributeDM_InitialSelfOnlyClaimIsRefused(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dminit-1")
	user2 := ts.CreateTestUser(t, "dminit-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)

	// user2 claims epoch 1 wrapping ONLY itself -> refused (user1 is keyed and omitted).
	w := ts.DoRequest(http.MethodPost, pathE2EEKeys+convID,
		map[string]interface{}{"wrapped_keys": map[string]string{user2.ID: testhelpers.ValidCiphertext()}},
		testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "every participant")
	assert.Empty(t, dmKeyVersions(t, ts, convID, user1.ID), "nothing established")
	assert.Empty(t, dmKeyVersions(t, ts, convID, user2.ID))

	// The legitimate bootstrap -- wrap the PEER and omit self -- still works.
	w = ts.DoRequest(http.MethodPost, pathE2EEKeys+convID,
		map[string]interface{}{"wrapped_keys": map[string]string{user2.ID: testhelpers.ValidCiphertext()}},
		testhelpers.AuthHeaders(user1.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []int{1}, dmKeyVersions(t, ts, convID, user2.ID))
}

// TestRotateDMKey_PeerParticipantMayRotateOneToOne pins the 1:1 authority
// short-circuit. dmRotationAuthority returns true for a 1:1 via `!isGroup`, but
// every other rotate test uses the creator (user1) as actor, so a mutation
// dropping that short-circuit would pass them all while wrongly 403-ing the
// non-creator peer. This drives the peer.
func TestRotateDMKey_PeerParticipantMayRotateOneToOne(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmpeer-1")
	user2 := ts.CreateTestUser(t, "dmpeer-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	// user2 is the NON-creator peer (CreateDMConversation sets created_by=user1).
	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, user1.ID, user2.ID), testhelpers.AuthHeaders(user2.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []int{1, 2}, dmKeyVersions(t, ts, convID, user1.ID))
	assert.Equal(t, []int{1, 2}, dmKeyVersions(t, ts, convID, user2.ID))
}

// TestRotateDMKey_RefusesMoreThanMaxWrappedKeys pins the rotate route's batch
// size fence (the unified route's is covered separately). An oversized batch
// is a 400 before the transaction and writes nothing.
func TestRotateDMKey_RefusesMoreThanMaxWrappedKeys(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "dmmax-1")
	user2 := ts.CreateTestUser(t, "dmmax-2")
	convID := ts.CreateDMConversation(t, user1.ID, user2.ID)
	ts.SeedDMKey(t, convID, user1.ID, 1)
	ts.SeedDMKey(t, convID, user2.ID, 1)

	ids := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		ids = append(ids, uuid.NewString())
	}
	w := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathRotateKey,
		dmRotationBody(2, ids...), testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Too many wrapped keys")
	assert.Equal(t, 0, dmRevocationCount(t, ts, convID), "an oversized batch writes nothing")
}
