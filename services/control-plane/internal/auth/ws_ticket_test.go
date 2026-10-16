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

func testTicketValue(userID, sessionID, epoch string) string {
	return fmt.Sprintf("%s:%s:%s:%d", userID, sessionID, epoch, time.Now().Add(15*time.Minute).Unix())
}

// --- ValidateTicket Unit Tests (no HTTP, direct Redis) ---

func TestValidateTicketSuccess(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Seed a ticket directly in Redis
	ticket := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, testTicketValue("user-123", "", ""), 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-123", userID)
	assert.Empty(t, sessionID)
}

func TestValidateTicketWithSessionID(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "fedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, testTicketValue("user-456", "session-abc", ""), 30*time.Second).Err()
	require.NoError(t, err)

	userID, sessionID, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-456", userID)
	assert.Equal(t, "session-abc", sessionID)
}

func TestValidateTicketSingleUse(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "singleuse1234567890singleuse1234567890singleuse1234567890singleu"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, testTicketValue("user-789", "", ""), 30*time.Second).Err()
	require.NoError(t, err)

	// First use succeeds
	userID, _, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-789", userID)

	// Second use fails (ticket deleted)
	_, _, _, _, err = auth.ValidateTicket(ctx, ts.Redis, ticket)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid or expired")
}

func TestValidateTicketEmpty(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	_, _, _, _, err := auth.ValidateTicket(ctx, ts.Redis, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ticket")
}

func TestValidateTicketWhitespace(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	_, _, _, _, err := auth.ValidateTicket(ctx, ts.Redis, "   ")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ticket")
}

func TestValidateTicketExpired(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Nonexistent ticket (simulates expired)
	_, _, _, _, err := auth.ValidateTicket(ctx, ts.Redis, "nonexistent-ticket-value")
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
	userID, _, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
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
	userID, sessionID, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
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
	userID1, _, _, _, err := auth.ValidateTicket(ctx, ts.Redis, body1["ticket"].(string))
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID1)

	userID2, _, _, _, err := auth.ValidateTicket(ctx, ts.Redis, body2["ticket"].(string))
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID2)
}

func TestValidateTicketSessionIDWithColons(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Extra segments must not shift the authenticated epoch or deadline.
	ticket := "colontest12345678901234567890123456789012345678901234567890abcde"
	key := wsTicketKeyPrefix + ticket
	err := ts.Redis.Set(ctx, key, "user-x:session:with:colons", 30*time.Second).Err()
	require.NoError(t, err)

	_, _, _, _, err = auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.Error(t, err)
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

func TestValidateTicket_DeadlineAndEpochValue(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "epoch3seg1234567890abcdef1234567890abcdef1234567890abcdef123456"
	key := wsTicketKeyPrefix + ticket
	require.NoError(t, ts.Redis.Set(ctx, key, testTicketValue("user-789", "session-xyz", "epochE9"), 30*time.Second).Err())

	userID, sessionID, credEpoch, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-789", userID)
	assert.Equal(t, "session-xyz", sessionID)
	assert.Equal(t, "epochE9", credEpoch)
}

func TestValidateTicket_DeadlineAndEmptySession(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	ticket := "epochnosess234567890abcdef1234567890abcdef1234567890abcdef123456"
	key := wsTicketKeyPrefix + ticket
	require.NoError(t, ts.Redis.Set(ctx, key, testTicketValue("user-790", "", "epochE10"), 30*time.Second).Err())

	userID, sessionID, credEpoch, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, "user-790", userID)
	assert.Empty(t, sessionID)
	assert.Equal(t, "epochE10", credEpoch)
}

func TestValidateTicket_LegacyValuesWithoutDeadlineAreRejected(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Old tickets cannot establish a signed deadline; a client must fetch a new one.
	ticket := "legacyoneseg4567890abcdef1234567890abcdef1234567890abcdef1234567"
	require.NoError(t, ts.Redis.Set(ctx, wsTicketKeyPrefix+ticket, "user-legacy", 30*time.Second).Err())
	userID, sessionID, credEpoch, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.Error(t, err)
	assert.Empty(t, userID)
	assert.Empty(t, sessionID)
	assert.Empty(t, credEpoch)

	ticket2 := "legacytwoseg4567890abcdef1234567890abcdef1234567890abcdef1234567"
	require.NoError(t, ts.Redis.Set(ctx, wsTicketKeyPrefix+ticket2, "user-legacy2:sess-2", 30*time.Second).Err())
	userID, sessionID, credEpoch, _, err = auth.ValidateTicket(ctx, ts.Redis, ticket2)
	require.Error(t, err)
	assert.Empty(t, userID)
	assert.Empty(t, sessionID)
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
	claims, err := auth.ValidateAccessToken(tok, testhelpers.TestJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%s::tickepoch1:%d", user.ID, claims.ExpiresAt.Unix()), stored)
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

	userID, sessionID, _, _, err := auth.ValidateTicket(ctx, ts.Redis, ticket)
	require.NoError(t, err)
	assert.Equal(t, user.ID, userID)
	assert.Equal(t, "authsid-9", sessionID,
		"ticket must bind to the authenticated sid, not the spoofed X-Session-ID header")
}
