package auth_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const wsTicketKeyPrefix = "ws_ticket:"

// --- ValidateTicket Unit Tests (no HTTP, direct Redis) ---

func TestValidateTicketSuccess(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Seed a ticket directly in Redis
	ticket := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-123", 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-123", userID)
	assert.Empty(t, sessionID)
}

func TestValidateTicketWithSessionID(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "fedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-456:session-abc", 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-456", userID)
	assert.Equal(t, "session-abc", sessionID)
}

func TestValidateTicketSingleUse(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "singleuse1234567890singleuse1234567890singleuse1234567890singleu"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-789", 30*time.Second).Err()
	require.NoError(t, err)

	// First use succeeds
	userID, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-789", userID)

	// Second use fails (ticket deleted)
	_, _, _, err = auth.ValidateTicket(ctx, ts.Redis, ticket)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid or expired")
}

func TestValidateTicketEmpty(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	_, _, _, err := auth.ValidateTicket(ctx, ts.Redis, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ticket")
}

func TestValidateTicketWhitespace(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	_, _, _, err := auth.ValidateTicket(ctx, ts.Redis, "   ")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ticket")
}

func TestValidateTicketExpired(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Nonexistent ticket (simulates expired)
	_, _, _, err := auth.ValidateTicket(ctx, ts.Redis, "nonexistent-ticket-value")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid or expired")
}

// --- IssueTicket HTTP Tests ---

func TestIssueTicketSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket1")

	w := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket, ok := body["ticket"].(string)
	assert.True(t, ok, "response should contain ticket")
	assert.NotEmpty(t, ticket)

	// Verify ticket is valid in Redis
	ctx := context.Background()
	userID, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID)
}

func TestIssueTicketWithSessionID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket2")

	headers := testhelpers.AuthHeaders(user.AccessToken)
	headers.Set("X-Session-ID", "test-session-123")

	w := ts.DoRequest("POST", pathWSTicket, nil, headers)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket := body["ticket"].(string)

	// Validate and check session ID
	ctx := context.Background()
	userID, sessionID, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID)
	assert.Equal(t, "test-session-123", sessionID)
}

func TestIssueTicketNoAuth(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathWSTicket, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestIssueTicketMultipleTickets(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket3")

	// Issue two tickets
	w1 := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w2.Code)

	var body1, body2 map[string]interface{}
	testhelpers.ParseJSON(t, w1, &body1)
	testhelpers.ParseJSON(t, w2, &body2)

	// Tickets should be different
	assert.NotEqual(t, body1["ticket"], body2["ticket"])

	// Both should be valid
	ctx := context.Background()
	userID1, _, _, err := auth.ValidateTicket(ctx, ts.Redis, body1["ticket"].(string))
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID1)

	userID2, _, _, err := auth.ValidateTicket(ctx, ts.Redis, body2["ticket"].(string))
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID2)
}

func TestValidateTicketSessionIDWithColons(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Post-#2201 the value is 3-segment (userID:sessionID:credEpoch), so a
	// colon-bearing middle segment parses as sessionID + epoch tail. IssueTicket
	// discards colon-bearing X-Session-ID values (session IDs are UUIDs), so
	// this shape only documents the parser's defensive SplitN(3) behavior.
	ticket := "colontest12345678901234567890123456789012345678901234567890abcde"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-x:session:with:colons", 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, credEpoch, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-x", userID)
	assert.Equal(t, "session", sessionID)
	assert.Equal(t, "with:colons", credEpoch)
}

func TestIssueTicketStoresWithTTL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wsticket4")
	ctx := context.Background()

	w := ts.DoRequest("POST", pathWSTicket, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket := body["ticket"].(string)

	// Check TTL on the Redis key
	key := fmt.Sprintf("%s%s", wsTicketKeyPrefix, ticket)
	ttl, err := ts.Redis.TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0, "ticket should have a TTL")
	assert.True(t, ttl <= 30*time.Second, "ticket TTL should not exceed 30 seconds")
}

// --- #2201: credential epoch in the ticket value ---

func TestValidateTicket_ThreeSegmentEpochValue(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "epoch3seg1234567890abcdef1234567890abcdef1234567890abcdef123456"
	key := wsTicketKeyPrefix + ticket
	require.NoError(t, ts.Redis.Set(ctx, key, "user-789:session-xyz:epochE9", 30*time.Second).Err())

	userID, sessionID, credEpoch, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-789", userID)
	assert.Equal(t, "session-xyz", sessionID)
	assert.Equal(t, "epochE9", credEpoch)
}

func TestValidateTicket_ThreeSegmentEmptySession(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "epochnosess234567890abcdef1234567890abcdef1234567890abcdef123456"
	key := wsTicketKeyPrefix + ticket
	require.NoError(t, ts.Redis.Set(ctx, key, "user-790::epochE10", 30*time.Second).Err())

	userID, sessionID, credEpoch, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-790", userID)
	assert.Empty(t, sessionID)
	assert.Equal(t, "epochE10", credEpoch)
}

func TestValidateTicket_LegacyValuesParseEmptyEpoch(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Legacy one- and two-segment values (pre-#2201 instances inside the 30s
	// deploy window) must parse with an empty epoch, not error.
	ticket := "legacyoneseg4567890abcdef1234567890abcdef1234567890abcdef1234567"
	require.NoError(t, ts.Redis.Set(ctx, wsTicketKeyPrefix+ticket, "user-legacy", 30*time.Second).Err())
	userID, sessionID, credEpoch, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-legacy", userID)
	assert.Empty(t, sessionID)
	assert.Empty(t, credEpoch)

	ticket2 := "legacytwoseg4567890abcdef1234567890abcdef1234567890abcdef1234567"
	require.NoError(t, ts.Redis.Set(ctx, wsTicketKeyPrefix+ticket2, "user-legacy2:sess-2", 30*time.Second).Err())
	userID, sessionID, credEpoch, err = auth.ValidateTicket(ctx, ts.Redis, ticket2)
	require.NoError(t, err)
	assert.Equal(t, "user-legacy2", userID)
	assert.Equal(t, "sess-2", sessionID)
	assert.Empty(t, credEpoch)
}

func TestIssueTicket_StoresEpochFromRequestClaims(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()
	user := ts.CreateTestUser(t, "wsticketepoch")

	// Give the user an active epoch and a token carrying it, so the issued
	// ticket must embed that epoch as its third segment.
	_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = 'tickepoch1' WHERE id = $1`, user.ID)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Del(ctx, "cred_epoch:"+user.ID).Err())

	tok, err := auth.GenerateAccessToken(user.ID, testhelpers.TestJWTSecret, true, "tickepoch1", "")
	require.NoError(t, err)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+tok)
	w := ts.DoRequest("POST", pathWSTicket, nil, headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket, ok := body["ticket"].(string)
	require.True(t, ok)
	require.NotEmpty(t, ticket)

	stored, err := ts.Redis.Get(ctx, wsTicketKeyPrefix+ticket).Result()
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%s::tickepoch1", user.ID), stored)
}

// #2201 (Codex #2397 review): the ticket must bind to the authenticated `sid`
// claim, never a client-supplied X-Session-ID. Otherwise a patched client could
// register its ticket socket under an empty/foreign session and dodge
// DisconnectSession on targeted revocation.
func TestIssueTicket_PrefersAuthenticatedSidOverSpoofedHeader(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()
	user := ts.CreateTestUser(t, "wsticketsid")

	tok, err := auth.GenerateAccessToken(user.ID, testhelpers.TestJWTSecret, true, "", "authsid-9")
	require.NoError(t, err)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+tok)
	headers.Set("X-Session-ID", "spoofed-attacker-session")

	w := ts.DoRequest("POST", pathWSTicket, nil, headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	ticket := body["ticket"].(string)

	userID, sessionID, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID)
	assert.Equal(t, "authsid-9", sessionID,
		"ticket must bind to the authenticated sid, not the spoofed X-Session-ID header")
}
